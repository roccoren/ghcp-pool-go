package gateway

import (
	"context"
	"os"
	"sync"
	"time"
)

type Account struct {
	Config      *AccountConfig
	Backend     Backend
	Enabled     bool
	Models      []string
	HomeDir     string
	Started     bool
	LastError   string
	Cooldown    time.Time
	Failures    int
	InFlight    int
	mu          sync.Mutex
	maxParallel chan struct{}
}

func (a *Account) ID() string { return a.Config.ID }

func (a *Account) MaxConcurrency() int {
	if a.Config.MaxConcurrency <= 0 {
		return 32
	}
	return a.Config.MaxConcurrency
}

func (a *Account) Weight() int {
	if a.Config.Weight <= 0 {
		return 1
	}
	return a.Config.Weight
}

func (a *Account) Available() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Enabled && time.Now().After(a.Cooldown) && a.InFlight < a.MaxConcurrency()
}

func (a *Account) AllowsModel(model string) bool {
	if matchesAny(model, a.Config.Deny) {
		return false
	}
	allow := a.Config.Allow
	if len(allow) == 0 {
		allow = []string{"*"}
	}
	return matchesAny(model, allow)
}

func (a *Account) Acquire() {
	a.mu.Lock()
	a.InFlight++
	a.mu.Unlock()
}

func (a *Account) TryAcquire() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.Enabled || time.Now().Before(a.Cooldown) || a.InFlight >= a.MaxConcurrency() {
		return false
	}
	a.InFlight++
	return true
}

func (a *Account) Release() {
	a.mu.Lock()
	if a.InFlight > 0 {
		a.InFlight--
	}
	a.mu.Unlock()
}

func (a *Account) RecordSuccess() {
	a.mu.Lock()
	a.Failures = 0
	a.LastError = ""
	a.mu.Unlock()
}

func (a *Account) RecordFailure(message string) {
	a.mu.Lock()
	a.Failures++
	a.LastError = message
	backoff := time.Duration(min(a.Failures*30, 300)) * time.Second
	a.Cooldown = time.Now().Add(backoff)
	a.mu.Unlock()
}

func (a *Account) Status() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return map[string]any{
		"id":                   a.ID(),
		"label":                a.Config.Label,
		"enabled":              a.Enabled,
		"in_flight":            a.InFlight,
		"max_concurrency":      a.MaxConcurrency(),
		"cooldown_remaining":   max(0, time.Until(a.Cooldown).Seconds()),
		"consecutive_failures": a.Failures,
		"last_error":           nilIfEmptyString(a.LastError),
		"models":               a.Models,
		"home_directory":       nilIfEmptyString(a.HomeDir),
		"auth_method":          a.Config.AuthMethod(),
		"authenticated":        a.authenticated(),
		"started":              a.Started,
	}
}

func (a *Account) authenticated() bool {
	cfg := a.Config
	if cfg.RuntimeToken != "" || cfg.Token != "" || cfg.TokenKeyVaultSecret != "" {
		return true
	}
	if cfg.TokenEnv != "" && os.Getenv(cfg.TokenEnv) != "" {
		return true
	}
	return homeHasCredential(firstNonEmpty(a.HomeDir, cfg.BaseDirectory))
}

type PoolManager struct {
	Settings Settings
	Homes    *HomeRegistry
	Accounts map[string]*Account
	mu       sync.RWMutex
}

func NewPoolManager(settings Settings) (*PoolManager, error) {
	pm := &PoolManager{
		Settings: settings,
		Homes:    NewHomeRegistry(settings.HomeRoot),
		Accounts: map[string]*Account{},
	}
	for i := range settings.Accounts {
		cfg := settings.Accounts[i]
		if _, err := pm.install(&cfg); err != nil {
			return nil, err
		}
	}
	return pm, nil
}

func (p *PoolManager) install(cfg *AccountConfig) (*Account, error) {
	home := ""
	if p.Settings.Backend == "copilot" {
		var err error
		home, err = p.Homes.Resolve(cfg.ID, cfg.BaseDirectory)
		if err != nil {
			return nil, err
		}
	}
	backend := buildBackend(cfg, p.Settings, home)
	account := &Account{
		Config:  cfg,
		Backend: backend,
		Enabled: cfg.enabled(),
		Models:  append([]string{}, cfg.Models...),
		HomeDir: home,
	}
	p.mu.Lock()
	p.Accounts[cfg.ID] = account
	p.mu.Unlock()
	return account, nil
}

func buildBackend(cfg *AccountConfig, settings Settings, homeDir string) Backend {
	if settings.Backend == "copilot" {
		token, _ := cfg.ResolveToken(settings.KeyVaultURL)
		if token == "" && cfg.BaseDirectory == "" {
			token = firstNonEmpty(
				os.Getenv("GHCP_COPILOT_TOKEN"),
				os.Getenv("COPILOT_GITHUB_TOKEN"),
				os.Getenv("GH_TOKEN"),
				os.Getenv("GITHUB_TOKEN"),
			)
		}
		return NewCopilotBackend(cfg.ID, token, homeDir)
	}
	return NewFakeBackend(cfg.ID, cfg.Models)
}

func (p *PoolManager) Start(ctx context.Context) error {
	p.mu.RLock()
	accounts := make([]*Account, 0, len(p.Accounts))
	for _, account := range p.Accounts {
		accounts = append(accounts, account)
	}
	p.mu.RUnlock()
	for _, account := range accounts {
		if account.Enabled {
			if err := p.StartAccount(ctx, account.ID()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *PoolManager) StartAccount(_ context.Context, id string) error {
	account := p.Get(id)
	if account == nil {
		return nil
	}
	account.mu.Lock()
	account.Started = true
	account.mu.Unlock()
	return nil
}

func (p *PoolManager) Close() {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, account := range p.Accounts {
		_ = account.Backend.Close()
		account.Started = false
	}
}

func (p *PoolManager) Get(id string) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.Accounts[id]
}

func (p *PoolManager) EnabledAccounts() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := []*Account{}
	for _, account := range p.Accounts {
		if account.Enabled {
			out = append(out, account)
		}
	}
	return out
}

func (p *PoolManager) Snapshot() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Account, 0, len(p.Accounts))
	for _, account := range p.Accounts {
		out = append(out, account)
	}
	return out
}

func (p *PoolManager) SetEnabled(id string, enabled bool) bool {
	account := p.Get(id)
	if account == nil {
		return false
	}
	account.mu.Lock()
	account.Enabled = enabled
	account.mu.Unlock()
	return true
}

func (p *PoolManager) AddAccount(ctx context.Context, cfg AccountConfig) (*Account, error) {
	p.mu.RLock()
	_, exists := p.Accounts[cfg.ID]
	p.mu.RUnlock()
	if exists {
		return nil, &conflictError{message: "account '" + cfg.ID + "' already exists"}
	}
	account, err := p.install(&cfg)
	if err != nil {
		return nil, err
	}
	if account.Enabled {
		if err := p.StartAccount(ctx, account.ID()); err != nil {
			return nil, err
		}
	}
	p.Settings.Accounts = append(p.Settings.Accounts, cfg)
	return account, nil
}

func (p *PoolManager) RemoveAccount(id string) bool {
	p.mu.Lock()
	account := p.Accounts[id]
	if account == nil {
		p.mu.Unlock()
		return false
	}
	delete(p.Accounts, id)
	p.mu.Unlock()
	_ = account.Backend.Close()
	filtered := p.Settings.Accounts[:0]
	for _, cfg := range p.Settings.Accounts {
		if cfg.ID != id {
			filtered = append(filtered, cfg)
		}
	}
	p.Settings.Accounts = filtered
	return true
}

func (p *PoolManager) RebuildBackend(ctx context.Context, id string) (*Account, error) {
	account := p.Get(id)
	if account == nil {
		return nil, nil
	}
	_ = account.Backend.Close()
	account.Backend = buildBackend(account.Config, p.Settings, account.HomeDir)
	account.Started = false
	if account.Enabled {
		if err := p.StartAccount(ctx, id); err != nil {
			return nil, err
		}
	}
	return account, nil
}

func nilIfEmptyString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
