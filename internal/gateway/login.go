package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

type LoginState struct {
	AccountID       string
	DeviceCode      string
	UserCode        string
	VerificationURI string
	Interval        int
	ExpiresAt       time.Time
	Status          string
	Error           string
}

func (s LoginState) Public() map[string]any {
	return map[string]any{
		"account_id":       s.AccountID,
		"status":           s.Status,
		"user_code":        s.UserCode,
		"verification_uri": s.VerificationURI,
		"expires_in":       max(0, int(time.Until(s.ExpiresAt).Seconds())),
		"interval":         s.Interval,
		"error":            nilIfEmptyString(s.Error),
	}
}

type LoginManager struct {
	settings Settings
	pool     *PoolManager
	states   map[string]*LoginState
	mu       sync.Mutex
}

func NewLoginManager(settings Settings, pool *PoolManager) *LoginManager {
	return &LoginManager{settings: settings, pool: pool, states: map[string]*LoginState{}}
}

func (m *LoginManager) Start(ctx context.Context, accountID string) (map[string]any, error) {
	if m.pool.Get(accountID) == nil {
		return nil, errors.New("account not found")
	}
	if m.settings.Login.ClientID == "" {
		return nil, errors.New("login.client_id is not configured")
	}
	data, err := postForm(ctx, m.settings.Login.DeviceCodeURL, url.Values{
		"client_id": {m.settings.Login.ClientID},
		"scope":     {m.settings.Login.Scopes},
	})
	if err != nil {
		return nil, err
	}
	state := &LoginState{
		AccountID:       accountID,
		DeviceCode:      stringFromMap(data, "device_code"),
		UserCode:        stringFromMap(data, "user_code"),
		VerificationURI: firstNonEmpty(stringFromMap(data, "verification_uri"), stringFromMap(data, "verification_uri_complete"), "https://github.com/login/device"),
		Interval:        intFromMap(data, "interval", 5),
		ExpiresAt:       time.Now().Add(time.Duration(intFromMap(data, "expires_in", 900)) * time.Second),
		Status:          "pending",
	}
	m.mu.Lock()
	m.states[accountID] = state
	m.mu.Unlock()
	return state.Public(), nil
}

func (m *LoginManager) Poll(ctx context.Context, accountID string) (map[string]any, error) {
	m.mu.Lock()
	state := m.states[accountID]
	m.mu.Unlock()
	if state == nil {
		return nil, errors.New("no pending login")
	}
	if state.Status == "authorized" || state.Status == "error" || state.Status == "expired" {
		return state.Public(), nil
	}
	if time.Now().After(state.ExpiresAt) {
		state.Status = "expired"
		return state.Public(), nil
	}
	data, err := postForm(ctx, m.settings.Login.TokenURL, url.Values{
		"client_id":   {m.settings.Login.ClientID},
		"device_code": {state.DeviceCode},
		"grant_type":  {deviceGrant},
	})
	if err != nil {
		return nil, err
	}
	if token := stringFromMap(data, "access_token"); token != "" {
		if err := m.applyToken(ctx, accountID, token, "device_login"); err != nil {
			return nil, err
		}
		state.Status = "authorized"
		return state.Public(), nil
	}
	errCode := stringFromMap(data, "error")
	if errCode == "" || errCode == "authorization_pending" || errCode == "slow_down" {
		if errCode == "slow_down" {
			state.Interval = intFromMap(data, "interval", state.Interval+5)
		}
		state.Status = "pending"
		return state.Public(), nil
	}
	state.Status = "error"
	state.Error = errCode
	return state.Public(), nil
}

func (m *LoginManager) SetToken(ctx context.Context, accountID, token string) error {
	if stringsTrim(token) == "" {
		return errors.New("token must be a non-empty string")
	}
	return m.applyToken(ctx, accountID, stringsTrim(token), "api_token")
}

func (m *LoginManager) Logout(ctx context.Context, accountID string) bool {
	account := m.pool.Get(accountID)
	if account == nil {
		return false
	}
	hadToken := account.Config.RuntimeToken != ""
	account.Config.RuntimeToken = ""
	account.Config.RuntimeTokenSource = ""
	m.mu.Lock()
	delete(m.states, accountID)
	m.mu.Unlock()
	if hadToken {
		_, _ = m.pool.RebuildBackend(ctx, accountID)
	}
	return true
}

func (m *LoginManager) Status(accountID string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state := m.states[accountID]; state != nil {
		return state.Public()
	}
	return nil
}

func (m *LoginManager) applyToken(ctx context.Context, accountID, token, source string) error {
	account := m.pool.Get(accountID)
	if account == nil {
		return errors.New("account not found")
	}
	account.Config.RuntimeToken = token
	account.Config.RuntimeTokenSource = source
	_, err := m.pool.RebuildBackend(ctx, accountID)
	return err
}

func postForm(ctx context.Context, endpoint string, values url.Values) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, stringsNewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, errors.New(resp.Status)
	}
	var data map[string]any
	return data, json.NewDecoder(resp.Body).Decode(&data)
}

func stringFromMap(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func intFromMap(m map[string]any, key string, fallback int) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return fallback
	}
}
