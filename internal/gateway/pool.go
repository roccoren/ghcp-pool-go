package gateway

import (
	"context"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const recentRequestWindow = time.Hour

type TokenBucket struct {
	RPM       int
	tokens    float64
	updatedAt time.Time
	mu        sync.Mutex
}

func NewTokenBucket(rpm int) *TokenBucket {
	if rpm <= 0 {
		return nil
	}
	return &TokenBucket{RPM: rpm, tokens: float64(rpm), updatedAt: time.Now()}
}

func (b *TokenBucket) refillLocked(now time.Time) {
	if b == nil || b.RPM <= 0 {
		return
	}
	elapsed := now.Sub(b.updatedAt).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	b.tokens = math.Min(float64(b.RPM), b.tokens+elapsed*(float64(b.RPM)/60.0))
	b.updatedAt = now
}

func (b *TokenBucket) Available() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(time.Now())
	return b.tokens >= 1
}

func (b *TokenBucket) Consume() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(time.Now())
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func (b *TokenBucket) RetryAfter() float64 {
	if b == nil || b.RPM <= 0 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(time.Now())
	if b.tokens >= 1 {
		return 0
	}
	return math.Ceil((1 - b.tokens) / (float64(b.RPM) / 60.0))
}

type Account struct {
	Config            *AccountConfig
	Backend           Backend
	Enabled           bool
	Models            []string
	HomeDir           string
	Started           bool
	LastError         string
	Cooldown          time.Time
	Failures          int
	InFlight          int
	RateLimiter       *TokenBucket
	LastRateLimitedAt time.Time
	RecentRequests    []RequestRecord
	mu                sync.Mutex
	maxParallel       chan struct{}
}

type RequestRecord struct {
	At     time.Time
	Failed bool
	Is429  bool
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
	ok := a.Enabled && time.Now().After(a.Cooldown) && a.InFlight < a.MaxConcurrency()
	a.mu.Unlock()
	return ok && (a.RateLimiter == nil || a.RateLimiter.Available())
}

func (a *Account) RateRetryAfter() float64 {
	if a.RateLimiter == nil {
		return 0
	}
	return a.RateLimiter.RetryAfter()
}

func (a *Account) IsRateLimited() bool {
	return a.RateRetryAfter() > 0
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
	if a.RateLimiter != nil && !a.RateLimiter.Consume() {
		a.LastRateLimitedAt = time.Now()
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
	a.recordRequestLocked(false, false, time.Now())
	a.mu.Unlock()
}

func (a *Account) RecordFailure(message string) {
	a.mu.Lock()
	a.LastError = message
	if isNonRetryableClientError(message) {
		a.recordRequestLocked(true, false, time.Now())
		a.mu.Unlock()
		return
	}
	a.Failures++
	base := 30
	if isRateLimitError(message) {
		base = 120
		a.LastRateLimitedAt = time.Now()
	}
	a.recordRequestLocked(true, isRateLimitError(message), time.Now())
	backoff := time.Duration(min(a.Failures*base, 300)) * time.Second
	a.Cooldown = time.Now().Add(backoff)
	a.mu.Unlock()
}

func (a *Account) ClearError() {
	a.mu.Lock()
	a.Failures = 0
	a.LastError = ""
	a.mu.Unlock()
}

func (a *Account) recordRequestLocked(failed, is429 bool, now time.Time) {
	a.RecentRequests = append(a.RecentRequests, RequestRecord{At: now, Failed: failed, Is429: is429})
	a.trimRecentRequestsLocked(now)
}

func (a *Account) trimRecentRequestsLocked(now time.Time) {
	cutoff := now.Add(-recentRequestWindow)
	i := sort.Search(len(a.RecentRequests), func(i int) bool {
		return !a.RecentRequests[i].At.Before(cutoff)
	})
	if i > 0 {
		a.RecentRequests = append([]RequestRecord{}, a.RecentRequests[i:]...)
	}
}

func (a *Account) RecentStats(now time.Time) (requests int, failures int, last429 time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.trimRecentRequestsLocked(now)
	for _, record := range a.RecentRequests {
		requests++
		if record.Failed {
			failures++
		}
		if record.Is429 && record.At.After(last429) {
			last429 = record.At
		}
	}
	if a.LastRateLimitedAt.After(last429) {
		last429 = a.LastRateLimitedAt
	}
	return requests, failures, last429
}

func (a *Account) Status() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	status := map[string]any{
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
		"rate_limit_rpm":       rateLimitRPM(a.RateLimiter),
		"rate_limited":         a.IsRateLimited(),
		"rate_retry_after":     a.RateRetryAfter(),
		"last_rate_limited_at": nilIfZeroTime(a.LastRateLimitedAt),
	}
	a.trimRecentRequestsLocked(time.Now())
	recentFailures := 0
	for _, record := range a.RecentRequests {
		if record.Failed {
			recentFailures++
		}
	}
	status["recent_requests"] = len(a.RecentRequests)
	status["recent_failures"] = recentFailures
	return status
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
	Settings          Settings
	Homes             *HomeRegistry
	Accounts          map[string]*Account
	GlobalRateLimiter *TokenBucket
	mu                sync.RWMutex
}

func NewPoolManager(settings Settings) (*PoolManager, error) {
	pm := &PoolManager{
		Settings:          settings,
		Homes:             NewHomeRegistry(settings.HomeRoot),
		Accounts:          map[string]*Account{},
		GlobalRateLimiter: NewTokenBucket(settings.RateLimits.GlobalRPM),
	}
	for i := range settings.Accounts {
		cfg := settings.Accounts[i]
		if _, err := pm.install(context.Background(), &cfg); err != nil {
			return nil, err
		}
	}
	return pm, nil
}

func (p *PoolManager) install(ctx context.Context, cfg *AccountConfig) (*Account, error) {
	home := ""
	if p.Settings.Backend == "copilot" {
		var err error
		home, err = p.Homes.Resolve(cfg.ID, cfg.BaseDirectory)
		if err != nil {
			return nil, err
		}
	}
	backend, err := buildBackend(ctx, cfg, p.Settings, home)
	if err != nil {
		return nil, err
	}
	account := &Account{
		Config:      cfg,
		Backend:     backend,
		Enabled:     cfg.enabled(),
		Models:      append([]string{}, cfg.Models...),
		HomeDir:     home,
		RateLimiter: NewTokenBucket(p.accountRateLimit(cfg)),
	}
	p.mu.Lock()
	p.Accounts[cfg.ID] = account
	p.mu.Unlock()
	return account, nil
}

func (p *PoolManager) accountRateLimit(cfg *AccountConfig) int {
	if cfg.RateLimitRPM != nil {
		return *cfg.RateLimitRPM
	}
	return p.Settings.RateLimits.PerAccountRPM
}

func (p *PoolManager) TryAcquire(account *Account) bool {
	if account == nil || !account.Available() {
		return false
	}
	if p.GlobalRateLimiter != nil && !p.GlobalRateLimiter.Consume() {
		return false
	}
	return account.TryAcquire()
}

func (p *PoolManager) GlobalRetryAfter() float64 {
	if p.GlobalRateLimiter == nil {
		return 0
	}
	return p.GlobalRateLimiter.RetryAfter()
}

func (p *PoolManager) ConfigureRateLimits(config RateLimitConfig) {
	p.Settings.RateLimits = config
	p.GlobalRateLimiter = NewTokenBucket(config.GlobalRPM)
	for _, account := range p.Snapshot() {
		account.mu.Lock()
		account.RateLimiter = NewTokenBucket(p.accountRateLimit(account.Config))
		account.mu.Unlock()
	}
}

func buildBackend(ctx context.Context, cfg *AccountConfig, settings Settings, homeDir string) (Backend, error) {
	if settings.Backend == "copilot" {
		token, err := cfg.ResolveToken(ctx, settings.KeyVaultURL)
		if err != nil {
			return nil, err
		}
		if token == "" && cfg.BaseDirectory == "" {
			token = firstNonEmpty(
				os.Getenv("GHCP_COPILOT_TOKEN"),
				os.Getenv("COPILOT_GITHUB_TOKEN"),
				os.Getenv("GH_TOKEN"),
				os.Getenv("GITHUB_TOKEN"),
			)
		}
		return NewCopilotBackend(cfg.ID, token, homeDir), nil
	}
	return NewFakeBackend(cfg.ID, cfg.Models), nil
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

func (p *PoolManager) StartAccount(ctx context.Context, id string) error {
	account := p.Get(id)
	if account == nil {
		return nil
	}
	if err := account.Backend.Start(ctx); err != nil {
		account.RecordFailure("start: " + err.Error())
		return err
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
	account, err := p.install(ctx, &cfg)
	if err != nil {
		return nil, err
	}
	if account.Enabled {
		if err := p.StartAccount(ctx, account.ID()); err != nil {
			p.mu.Lock()
			delete(p.Accounts, account.ID())
			p.mu.Unlock()
			_ = account.Backend.Close()
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
	backend, err := buildBackend(ctx, account.Config, p.Settings, account.HomeDir)
	if err != nil {
		return nil, err
	}
	_ = account.Backend.Close()
	account.Backend = backend
	account.Started = false
	if account.Enabled {
		if err := p.StartAccount(ctx, id); err != nil {
			return nil, err
		}
	}
	return account, nil
}

func isRateLimitError(message string) bool {
	text := strings.ToLower(message)
	return strings.Contains(text, "429") || strings.Contains(text, "too many requests") || strings.Contains(text, "rate limit")
}

func isNonRetryableClientError(message string) bool {
	text := strings.ToLower(message)
	return strings.Contains(text, "copilot upstream error 400") ||
		strings.Contains(text, "invalid_request_error") ||
		strings.Contains(text, "bad request")
}

func rateLimitRPM(bucket *TokenBucket) int {
	if bucket == nil {
		return 0
	}
	return bucket.RPM
}

func nilIfEmptyString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
