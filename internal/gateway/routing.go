package gateway

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
)

type RoutingError struct{ message string }

func (e RoutingError) Error() string { return e.message }

type NoAccountForModel struct{ message string }

func (e NoAccountForModel) Error() string { return e.message }

type Router struct {
	Pool     *PoolManager
	Registry *ModelRegistry
	Routes   []RouteConfig
	rr       uint64
}

func NewRouter(pool *PoolManager, registry *ModelRegistry, routes []RouteConfig) *Router {
	return &Router{Pool: pool, Registry: registry, Routes: routes}
}

func (r *Router) SetRoutes(routes []RouteConfig) {
	r.Routes = routes
}

func (r *Router) matchingRoute(model string) *RouteConfig {
	candidates := []RouteConfig{}
	for _, route := range r.Routes {
		if globMatch(route.Model, model) {
			candidates = append(candidates, route)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		ai := candidates[i].Priority*10000 + specificity(candidates[i].Model)
		aj := candidates[j].Priority*10000 + specificity(candidates[j].Model)
		return ai > aj
	})
	return &candidates[0]
}

func specificity(pattern string) int {
	score := len(pattern)
	if !strings.ContainsAny(pattern, "*?") {
		score += 1000
	}
	return score
}

func (r *Router) CandidateAccounts(model string) []*Account {
	route := r.matchingRoute(model)
	ids := []string{}
	if route == nil || len(route.Accounts) == 0 {
		r.Pool.mu.RLock()
		for id := range r.Pool.Accounts {
			ids = append(ids, id)
		}
		r.Pool.mu.RUnlock()
		sort.Strings(ids)
	} else {
		ids = append(ids, route.Accounts...)
	}
	serving := r.Registry.AccountsFor(model)
	registryReady := r.Registry.LastRefresh > 0
	out := []*Account{}
	for _, id := range ids {
		account := r.Pool.Get(id)
		if account == nil || !account.Enabled || !account.AllowsModel(model) {
			continue
		}
		if registryReady && !serving[id] {
			continue
		}
		out = append(out, account)
	}
	return out
}

func (r *Router) Select(model string) (*Account, error) {
	route := r.matchingRoute(model)
	strategy := "least_busy"
	if route != nil && route.Strategy != "" {
		strategy = route.Strategy
	}
	eligible := r.CandidateAccounts(model)
	candidates := append([]*Account{}, eligible...)
	for len(candidates) > 0 {
		picked := r.pick(candidates, strategy)
		if picked.TryAcquire() {
			return picked, nil
		}
		next := candidates[:0]
		for _, account := range candidates {
			if account.ID() != picked.ID() {
				next = append(next, account)
			}
		}
		candidates = next
	}
	if len(eligible) == 0 {
		return nil, NoAccountForModel{message: fmt.Sprintf("no account serves model %q", model)}
	}
	return nil, RoutingError{message: fmt.Sprintf("all accounts for model %q are busy or cooling down", model)}
}

func (r *Router) SelectExcluding(model string, exclude map[string]bool) *Account {
	route := r.matchingRoute(model)
	strategy := "least_busy"
	if route != nil && route.Strategy != "" {
		strategy = route.Strategy
	}
	available := []*Account{}
	for _, account := range r.CandidateAccounts(model) {
		if !exclude[account.ID()] {
			available = append(available, account)
		}
	}
	for len(available) > 0 {
		picked := r.pick(available, strategy)
		if picked.TryAcquire() {
			return picked
		}
		next := available[:0]
		for _, account := range available {
			if account.ID() != picked.ID() {
				next = append(next, account)
			}
		}
		available = next
	}
	return nil
}

func (r *Router) pick(accounts []*Account, strategy string) *Account {
	switch strategy {
	case "round_robin":
		n := atomic.AddUint64(&r.rr, 1)
		return accounts[int(n-1)%len(accounts)]
	case "weighted":
		return minBy(accounts, func(a *Account) float64 { return float64(a.InFlight) / float64(a.Weight()) })
	case "quota_aware":
		sort.Slice(accounts, func(i, j int) bool {
			if accounts[i].Failures == accounts[j].Failures {
				return accounts[i].InFlight < accounts[j].InFlight
			}
			return accounts[i].Failures < accounts[j].Failures
		})
		return accounts[0]
	default:
		return minBy(accounts, func(a *Account) float64 { return float64(a.InFlight) })
	}
}

func (r *Router) Explain(model string) map[string]any {
	route := r.matchingRoute(model)
	eligible := r.CandidateAccounts(model)
	servingMap := r.Registry.AccountsFor(model)
	serving := make([]string, 0, len(servingMap))
	for id := range servingMap {
		serving = append(serving, id)
	}
	sort.Strings(serving)
	eligibleIDs := []string{}
	availableIDs := []string{}
	for _, account := range eligible {
		eligibleIDs = append(eligibleIDs, account.ID())
		if account.Available() {
			availableIDs = append(availableIDs, account.ID())
		}
	}
	var matched any
	if route != nil {
		matched = map[string]any{
			"model":    route.Model,
			"accounts": route.Accounts,
			"strategy": route.Strategy,
			"priority": route.Priority,
		}
	}
	return map[string]any{
		"model":              model,
		"matched_route":      matched,
		"serving_accounts":   serving,
		"eligible_accounts":  eligibleIDs,
		"available_accounts": availableIDs,
	}
}

func minBy[T any](items []T, score func(T) float64) T {
	best := items[0]
	bestScore := score(best)
	for _, item := range items[1:] {
		if s := score(item); s < bestScore {
			best = item
			bestScore = s
		}
	}
	return best
}
