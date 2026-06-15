package gateway

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

type RoutingError struct{ message string }

func (e RoutingError) Error() string { return e.message }

type NoAccountForModel struct{ message string }

func (e NoAccountForModel) Error() string { return e.message }

type RateLimitExceeded struct {
	message    string
	RetryAfter float64
}

func (e RateLimitExceeded) Error() string { return e.message }

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

func (r *Router) CandidateAccounts(model string, endpoints ...string) []*Account {
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
	endpoint := ""
	if len(endpoints) > 0 {
		endpoint = endpoints[0]
	}
	out := []*Account{}
	for _, id := range ids {
		account := r.Pool.Get(id)
		if account == nil || !account.Enabled || !account.AllowsModel(model) {
			continue
		}
		if registryReady && !serving[id] {
			continue
		}
		if registryReady && endpoint != "" && !r.Registry.AccountSupportsEndpoint(model, id, endpoint) {
			continue
		}
		out = append(out, account)
	}
	return out
}

func (r *Router) Select(model string, endpoints ...string) (*Account, error) {
	route := r.matchingRoute(model)
	strategy := "least_busy"
	if route != nil && route.Strategy != "" {
		strategy = route.Strategy
	}
	endpoint := ""
	if len(endpoints) > 0 {
		endpoint = endpoints[0]
	}
	eligible := r.CandidateAccounts(model, endpoint)
	if len(eligible) == 0 {
		if endpoint != "" {
			return nil, NoAccountForModel{message: fmt.Sprintf("no account serves model %q on endpoint %q", model, endpoint)}
		}
		return nil, NoAccountForModel{message: fmt.Sprintf("no account serves model %q", model)}
	}
	if retry := r.Pool.GlobalRetryAfter(); retry > 0 {
		return nil, RateLimitExceeded{message: "global rate limit exceeded", RetryAfter: retry}
	}
	candidates := append([]*Account{}, eligible...)
	for len(candidates) > 0 {
		picked := r.pick(candidates, strategy)
		if r.Pool.TryAcquire(picked) {
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
	rateLimited := 0
	retryAfter := 0.0
	for _, account := range eligible {
		if retry := account.RateRetryAfter(); retry > 0 {
			rateLimited++
			if retryAfter == 0 || retry < retryAfter {
				retryAfter = retry
			}
		}
	}
	if rateLimited == len(eligible) {
		return nil, RateLimitExceeded{message: fmt.Sprintf("all accounts for model %q are rate limited", model), RetryAfter: retryAfter}
	}
	return nil, RoutingError{message: fmt.Sprintf("all accounts for model %q are busy or cooling down", model)}
}

func (r *Router) PickEndpoint(model string, preferred []string) string {
	if len(preferred) == 0 {
		return endpointChatCompletions
	}
	for _, endpoint := range preferred {
		if len(r.CandidateAccounts(model, endpoint)) > 0 {
			return endpoint
		}
	}
	return preferred[0]
}

func (r *Router) SelectExcluding(model string, exclude map[string]bool, endpoints ...string) *Account {
	route := r.matchingRoute(model)
	strategy := "least_busy"
	if route != nil && route.Strategy != "" {
		strategy = route.Strategy
	}
	if r.Pool.GlobalRetryAfter() > 0 {
		return nil
	}
	endpoint := ""
	if len(endpoints) > 0 {
		endpoint = endpoints[0]
	}
	available := []*Account{}
	for _, account := range r.CandidateAccounts(model, endpoint) {
		if !exclude[account.ID()] {
			available = append(available, account)
		}
	}
	for len(available) > 0 {
		picked := r.pick(available, strategy)
		if r.Pool.TryAcquire(picked) {
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
		now := time.Now()
		sort.Slice(accounts, func(i, j int) bool {
			pi := ratePenalty(accounts[i], now)
			pj := ratePenalty(accounts[j], now)
			if pi != pj {
				return pi < pj
			}
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

func ratePenalty(account *Account, now time.Time) float64 {
	account.mu.Lock()
	last := account.LastRateLimitedAt
	account.mu.Unlock()
	if last.IsZero() {
		return 0
	}
	remaining := 300 - now.Sub(last).Seconds()
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (r *Router) Explain(model string) map[string]any {
	return r.ExplainEndpoint(model, "")
}

func (r *Router) ExplainEndpoint(model, endpoint string) map[string]any {
	route := r.matchingRoute(model)
	eligible := r.CandidateAccounts(model, endpoint)
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
		"endpoint":           nilIfEmptyString(endpoint),
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
