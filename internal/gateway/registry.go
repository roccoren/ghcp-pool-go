package gateway

import (
	"context"
	"sort"
	"sync"
	"time"
)

type ModelRegistry struct {
	Pool        *PoolManager
	index       map[string]map[string]ModelSpec
	LastRefresh float64
	mu          sync.RWMutex
}

func NewModelRegistry(pool *PoolManager) *ModelRegistry {
	return &ModelRegistry{Pool: pool, index: map[string]map[string]ModelSpec{}}
}

func (r *ModelRegistry) Refresh(ctx context.Context) {
	next := map[string]map[string]ModelSpec{}
	r.Pool.mu.RLock()
	accounts := make([]*Account, 0, len(r.Pool.Accounts))
	for _, account := range r.Pool.Accounts {
		accounts = append(accounts, account)
	}
	r.Pool.mu.RUnlock()
	for _, account := range accounts {
		if !account.Enabled {
			account.Models = nil
			continue
		}
		specs, err := account.Backend.ListModels(ctx)
		if err != nil {
			account.RecordFailure("list_models: " + err.Error())
			continue
		}
		account.ClearError()
		models := make([]string, 0, len(specs))
		for _, spec := range specs {
			models = append(models, spec.ID)
			if next[spec.ID] == nil {
				next[spec.ID] = map[string]ModelSpec{}
			}
			next[spec.ID][account.ID()] = spec
		}
		account.Models = models
	}
	r.mu.Lock()
	r.index = next
	r.LastRefresh = nowUnix()
	r.mu.Unlock()
}

func (r *ModelRegistry) RefreshAccount(ctx context.Context, id string) {
	account := r.Pool.Get(id)
	r.mu.Lock()
	for _, ids := range r.index {
		delete(ids, id)
	}
	r.mu.Unlock()
	if account != nil && account.Enabled {
		_ = r.Pool.StartAccount(ctx, id)
		specs, err := account.Backend.ListModels(ctx)
		if err != nil {
			account.RecordFailure("list_models: " + err.Error())
			specs = nil
		} else {
			account.ClearError()
		}
		models := make([]string, 0, len(specs))
		r.mu.Lock()
		for _, spec := range specs {
			models = append(models, spec.ID)
			if r.index[spec.ID] == nil {
				r.index[spec.ID] = map[string]ModelSpec{}
			}
			r.index[spec.ID][id] = spec
		}
		r.mu.Unlock()
		account.Models = models
	} else if account != nil {
		account.Models = nil
	}
	r.mu.Lock()
	for model, ids := range r.index {
		if len(ids) == 0 {
			delete(r.index, model)
		}
	}
	if r.LastRefresh == 0 {
		r.LastRefresh = nowUnix()
	}
	r.mu.Unlock()
}

func (r *ModelRegistry) AccountsFor(model string) map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]bool{}
	for id := range r.index[model] {
		out[id] = true
	}
	return out
}

func (r *ModelRegistry) AccountSupportsEndpoint(model, accountID, endpoint string) bool {
	if endpoint == "" {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	spec, ok := r.index[model][accountID]
	if !ok {
		return false
	}
	return supportedEndpoint(spec, endpoint)
}

func (r *ModelRegistry) ModelSupportsEndpoint(model, endpoint string) bool {
	if endpoint == "" {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, spec := range r.index[model] {
		if supportedEndpoint(spec, endpoint) {
			return true
		}
	}
	return false
}

func (r *ModelRegistry) PickEndpoint(model string, preferred []string) string {
	if len(preferred) == 0 {
		return endpointChatCompletions
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	specs := r.index[model]
	if len(specs) == 0 {
		return preferred[0]
	}
	for _, endpoint := range preferred {
		for _, spec := range specs {
			if supportedEndpoint(spec, endpoint) {
				return endpoint
			}
		}
	}
	return preferred[0]
}

func (r *ModelRegistry) ModelsIndex() map[string][]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string][]string{}
	for model, ids := range r.index {
		for id := range ids {
			out[model] = append(out[model], id)
		}
		sort.Strings(out[model])
	}
	return out
}

func (r *ModelRegistry) CapabilitiesIndex() map[string]map[string]ModelSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]map[string]ModelSpec{}
	for model, byAccount := range r.index {
		out[model] = map[string]ModelSpec{}
		for accountID, spec := range byAccount {
			out[model][accountID] = spec
		}
	}
	return out
}

func (r *ModelRegistry) AllModels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	models := make([]string, 0, len(r.index))
	for model := range r.index {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

func (r *ModelRegistry) VisibleModels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	models := []string{}
	for model, ids := range r.index {
		for id := range ids {
			if account := r.Pool.Get(id); account != nil && account.Enabled {
				models = append(models, model)
				break
			}
		}
	}
	sort.Strings(models)
	return models
}

func (r *ModelRegistry) RefreshLoop(ctx context.Context, intervalSeconds int) {
	ticker := time.NewTicker(time.Duration(max(5, intervalSeconds)) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.Refresh(ctx)
		case <-ctx.Done():
			return
		}
	}
}
