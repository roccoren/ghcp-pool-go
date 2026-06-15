package gateway

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var ValidReasoningEfforts = map[string]bool{
	"none": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

var ValidStrategies = map[string]bool{
	"round_robin": true, "least_busy": true, "weighted": true, "quota_aware": true, "smart": true,
}

type APIKeyConfig struct {
	Key            string   `yaml:"key" json:"key"`
	KeyEnv         string   `yaml:"key_env" json:"key_env"`
	KeyVaultSecret string   `yaml:"key_vault_secret" json:"key_vault_secret"`
	KeyVaultURL    string   `yaml:"key_vault_url" json:"key_vault_url"`
	Scopes         []string `yaml:"scopes" json:"scopes"`
	ModelAllow     []string `yaml:"model_allow" json:"model_allow"`
	CacheNamespace string   `yaml:"cache_namespace" json:"cache_namespace"`
}

type AccountConfig struct {
	ID                  string   `yaml:"id" json:"id"`
	Label               string   `yaml:"label" json:"label"`
	Token               string   `yaml:"token" json:"token"`
	TokenEnv            string   `yaml:"token_env" json:"token_env"`
	TokenKeyVaultSecret string   `yaml:"token_key_vault_secret" json:"token_key_vault_secret"`
	KeyVaultURL         string   `yaml:"key_vault_url" json:"key_vault_url"`
	BaseDirectory       string   `yaml:"base_directory" json:"base_directory"`
	Enabled             *bool    `yaml:"enabled" json:"enabled"`
	MaxConcurrency      int      `yaml:"max_concurrency" json:"max_concurrency"`
	Weight              int      `yaml:"weight" json:"weight"`
	RateLimitRPM        *int     `yaml:"rate_limit_rpm" json:"rate_limit_rpm"`
	Allow               []string `yaml:"allow" json:"allow"`
	Deny                []string `yaml:"deny" json:"deny"`
	Models              []string `yaml:"models" json:"models"`

	RuntimeToken       string `yaml:"-" json:"-"`
	RuntimeTokenSource string `yaml:"-" json:"-"`
}

func (c *AccountConfig) enabled() bool {
	return c.Enabled == nil || *c.Enabled
}

func (c *APIKeyConfig) ResolveKey(ctx context.Context, defaultKeyVaultURL string) (string, error) {
	if c.Key != "" {
		return c.Key, nil
	}
	if c.KeyEnv != "" {
		if key := os.Getenv(c.KeyEnv); key != "" {
			return key, nil
		}
	}
	if c.KeyVaultSecret != "" {
		vaultURL := firstNonEmpty(c.KeyVaultURL, defaultKeyVaultURL)
		if vaultURL == "" {
			return "", fmt.Errorf("api key uses key_vault_secret %q but no key_vault_url is configured", c.KeyVaultSecret)
		}
		return getKeyVaultSecret(ctx, vaultURL, c.KeyVaultSecret)
	}
	return "", nil
}

func (c *AccountConfig) ResolveToken(ctx context.Context, defaultKeyVaultURL string) (string, error) {
	if c.RuntimeToken != "" {
		return c.RuntimeToken, nil
	}
	if c.Token != "" {
		return c.Token, nil
	}
	if c.TokenEnv != "" {
		if token := os.Getenv(c.TokenEnv); token != "" {
			return token, nil
		}
	}
	if c.TokenKeyVaultSecret != "" {
		vaultURL := firstNonEmpty(c.KeyVaultURL, defaultKeyVaultURL)
		if vaultURL == "" {
			return "", fmt.Errorf("account %q uses token_key_vault_secret but no key_vault_url is configured", c.ID)
		}
		return getKeyVaultSecret(ctx, vaultURL, c.TokenKeyVaultSecret)
	}
	return "", nil
}

func (c *AccountConfig) StoreToken(ctx context.Context, defaultKeyVaultURL, token string) error {
	if c.TokenKeyVaultSecret == "" {
		return nil
	}
	vaultURL := firstNonEmpty(c.KeyVaultURL, defaultKeyVaultURL)
	if vaultURL == "" {
		return fmt.Errorf("account %q uses token_key_vault_secret but no key_vault_url is configured", c.ID)
	}
	return setKeyVaultSecret(ctx, vaultURL, c.TokenKeyVaultSecret, token)
}

func (c AccountConfig) AuthMethod() string {
	switch {
	case c.RuntimeToken != "":
		if c.RuntimeTokenSource != "" {
			return c.RuntimeTokenSource
		}
		return "device_login"
	case c.Token != "":
		return "inline_token"
	case c.TokenEnv != "":
		return "token_env"
	case c.TokenKeyVaultSecret != "":
		return "key_vault"
	case c.BaseDirectory != "":
		return "credential_home"
	default:
		return "auto_home"
	}
}

type RouteConfig struct {
	Model    string   `yaml:"model" json:"model"`
	Accounts []string `yaml:"accounts" json:"accounts"`
	Strategy string   `yaml:"strategy" json:"strategy"`
	Priority int      `yaml:"priority" json:"priority"`
}

type RateLimitConfig struct {
	GlobalRPM     int `yaml:"global_rpm" json:"global_rpm"`
	PerAccountRPM int `yaml:"per_account_rpm" json:"per_account_rpm"`
}

type CacheConfig struct {
	Enabled    *bool  `yaml:"enabled" json:"enabled"`
	TTLSeconds int    `yaml:"ttl_seconds" json:"ttl_seconds"`
	MaxEntries int    `yaml:"max_entries" json:"max_entries"`
	Salt       string `yaml:"salt" json:"salt"`
	RedisURL   string `yaml:"redis_url" json:"redis_url"`
}

func (c CacheConfig) enabled() bool { return c.Enabled == nil || *c.Enabled }

type UsageConfig struct {
	SQLitePath string `yaml:"sqlite_path" json:"sqlite_path"`
}

type DebugConfig struct {
	Enabled      bool `yaml:"enabled" json:"enabled"`
	MaxEntries   int  `yaml:"max_entries" json:"max_entries"`
	MaxBodyChars int  `yaml:"max_body_chars" json:"max_body_chars"`
}

type LoginConfig struct {
	ClientID      string `yaml:"client_id" json:"client_id"`
	Scopes        string `yaml:"scopes" json:"scopes"`
	DeviceCodeURL string `yaml:"device_code_url" json:"device_code_url"`
	TokenURL      string `yaml:"token_url" json:"token_url"`
}

type GatewayConfig struct {
	Host                 string            `yaml:"host" json:"host"`
	Port                 int               `yaml:"port" json:"port"`
	ModelRefreshSeconds  int               `yaml:"model_refresh_seconds" json:"model_refresh_seconds"`
	OTLPEndpoint         string            `yaml:"otlp_endpoint" json:"otlp_endpoint"`
	KeyVaultURL          string            `yaml:"key_vault_url" json:"key_vault_url"`
	HomeRoot             string            `yaml:"home_root" json:"home_root"`
	RouteBusyWaitSeconds float64           `yaml:"route_busy_wait_seconds" json:"route_busy_wait_seconds"`
	ModelAliases         map[string]string `yaml:"model_aliases" json:"model_aliases"`
	ModelMapPath         string            `yaml:"model_map_path" json:"model_map_path"`
	RateLimits           RateLimitConfig   `yaml:"rate_limits" json:"rate_limits"`
	APIKeys              []APIKeyConfig    `yaml:"api_keys" json:"api_keys"`
	Cache                CacheConfig       `yaml:"cache" json:"cache"`
	Usage                UsageConfig       `yaml:"usage" json:"usage"`
	Login                LoginConfig       `yaml:"login" json:"login"`
	Debug                DebugConfig       `yaml:"debug" json:"debug"`
	ReasoningEfforts     map[string]string `yaml:"reasoning_efforts" json:"reasoning_efforts"`
}

type rawSettings struct {
	Backend  string          `yaml:"backend" json:"backend"`
	Gateway  GatewayConfig   `yaml:"gateway" json:"gateway"`
	Accounts []AccountConfig `yaml:"accounts" json:"accounts"`
	Routes   []RouteConfig   `yaml:"routes" json:"routes"`
}

type Settings struct {
	Backend              string
	Host                 string
	Port                 int
	ModelRefreshSeconds  int
	OTLPEndpoint         string
	KeyVaultURL          string
	HomeRoot             string
	RouteBusyWaitSeconds float64
	ModelAliases         map[string]string
	ModelMapPath         string
	RateLimits           RateLimitConfig
	APIKeys              []APIKeyConfig
	Accounts             []AccountConfig
	Routes               []RouteConfig
	Cache                CacheConfig
	Usage                UsageConfig
	Login                LoginConfig
	Debug                DebugConfig
	ReasoningEfforts     map[string]string
}

func (s Settings) Addr() string {
	return net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
}

func (s Settings) ResolveModelAlias(model string) string {
	if s.ModelAliases == nil {
		return model
	}
	if target := s.ModelAliases[model]; target != "" {
		return target
	}
	return model
}

func (s Settings) DisplayIDsForModel(model string) []string {
	aliases := []string{}
	for alias, target := range s.ModelAliases {
		if target == model {
			aliases = append(aliases, alias)
		}
	}
	if len(aliases) == 0 {
		return []string{model}
	}
	sort.Strings(aliases)
	return aliases
}

func LoadSettings(path string) (Settings, error) {
	if path == "" {
		path = firstNonEmpty(os.Getenv("GHCP_CONFIG"), "config.yaml")
	}
	raw := rawSettings{}
	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return Settings{}, err
		}
	} else if !os.IsNotExist(err) {
		return Settings{}, err
	}

	g := raw.Gateway
	settings := Settings{
		Backend:              firstNonEmpty(os.Getenv("GHCP_BACKEND"), raw.Backend, "fake"),
		Host:                 firstNonEmpty(os.Getenv("GHCP_HOST"), g.Host, "0.0.0.0"),
		Port:                 firstInt(os.Getenv("GHCP_PORT"), g.Port, 8000),
		ModelRefreshSeconds:  firstInt("", g.ModelRefreshSeconds, 300),
		OTLPEndpoint:         firstNonEmpty(os.Getenv("OTLP_ENDPOINT"), g.OTLPEndpoint),
		KeyVaultURL:          firstNonEmpty(os.Getenv("AZURE_KEY_VAULT_URL"), g.KeyVaultURL),
		HomeRoot:             firstNonEmpty(os.Getenv("GHCP_HOME_ROOT"), g.HomeRoot, "runtime-home"),
		RouteBusyWaitSeconds: firstFloat("", g.RouteBusyWaitSeconds, 4.0),
		ModelAliases:         map[string]string{},
		ModelMapPath:         firstNonEmpty(os.Getenv("GHCP_MODEL_MAP_PATH"), g.ModelMapPath, "model_map.json"),
		RateLimits: RateLimitConfig{
			GlobalRPM:     firstInt(os.Getenv("GHCP_GLOBAL_RATE_LIMIT_RPM"), g.RateLimits.GlobalRPM),
			PerAccountRPM: firstInt(os.Getenv("GHCP_PER_ACCOUNT_RATE_LIMIT_RPM"), g.RateLimits.PerAccountRPM),
		},
		APIKeys:          g.APIKeys,
		Accounts:         raw.Accounts,
		Routes:           raw.Routes,
		Cache:            g.Cache,
		Usage:            g.Usage,
		Login:            g.Login,
		Debug:            g.Debug,
		ReasoningEfforts: map[string]string{},
	}
	for alias, target := range g.ModelAliases {
		alias = strings.TrimSpace(alias)
		target = strings.TrimSpace(target)
		if alias != "" && target != "" {
			settings.ModelAliases[alias] = target
		}
	}
	if aliases, err := LoadModelAliases(settings.ModelMapPath); err == nil {
		for alias, target := range aliases {
			settings.ModelAliases[alias] = target
		}
	} else if !os.IsNotExist(err) {
		return Settings{}, err
	}
	if settings.Cache.TTLSeconds == 0 {
		settings.Cache.TTLSeconds = 3600
	}
	if settings.Cache.MaxEntries == 0 {
		settings.Cache.MaxEntries = 1000
	}
	settings.Cache.Salt = firstNonEmpty(os.Getenv("GHCP_CACHE_SALT"), settings.Cache.Salt, "change-me")
	settings.Usage.SQLitePath = firstNonEmpty(os.Getenv("GHCP_USAGE_SQLITE_PATH"), settings.Usage.SQLitePath, "usage.sqlite")
	if settings.Login.Scopes == "" {
		settings.Login.Scopes = "read:user"
	}
	if settings.Login.DeviceCodeURL == "" {
		settings.Login.DeviceCodeURL = "https://github.com/login/device/code"
	}
	if settings.Login.TokenURL == "" {
		settings.Login.TokenURL = "https://github.com/login/oauth/access_token"
	}
	if settings.Debug.MaxEntries == 0 {
		settings.Debug.MaxEntries = 200
	}
	if settings.Debug.MaxBodyChars == 0 {
		settings.Debug.MaxBodyChars = 20000
	}
	settings.Debug.Enabled = envBool("GHCP_DEBUG", settings.Debug.Enabled)
	if settings.Accounts == nil {
		models := []string{"gpt-4.1", "gpt-4o-mini"}
		tokenEnv := ""
		tokenKeyVaultSecret := ""
		if settings.Backend == "copilot" {
			models = nil
			tokenEnv = "GHCP_COPILOT_TOKEN"
			tokenKeyVaultSecret = firstNonEmpty(os.Getenv("GHCP_COPILOT_TOKEN_KEY_VAULT_SECRET"), os.Getenv("GHCP_COPILOT_TOKEN_KEYVAULT_SECRET"))
		}
		settings.Accounts = []AccountConfig{{
			ID:                  "acct_a",
			Label:               "Default account",
			TokenEnv:            tokenEnv,
			TokenKeyVaultSecret: tokenKeyVaultSecret,
			Models:              models,
		}}
	}
	if len(settings.Routes) == 0 {
		ids := make([]string, 0, len(settings.Accounts))
		for _, account := range settings.Accounts {
			ids = append(ids, account.ID)
		}
		settings.Routes = []RouteConfig{{Model: "*", Accounts: ids, Strategy: "least_busy"}}
	}
	if len(settings.APIKeys) == 0 {
		apiKey := os.Getenv("GHCP_API_KEY")
		adminKey := os.Getenv("GHCP_ADMIN_API_KEY")
		apiKeySecret := firstNonEmpty(os.Getenv("GHCP_API_KEY_KEY_VAULT_SECRET"), os.Getenv("GHCP_API_KEY_KEYVAULT_SECRET"))
		adminKeySecret := firstNonEmpty(os.Getenv("GHCP_ADMIN_API_KEY_KEY_VAULT_SECRET"), os.Getenv("GHCP_ADMIN_API_KEY_KEYVAULT_SECRET"))
		if apiKey != "" || adminKey != "" || apiKeySecret != "" || adminKeySecret != "" {
			if apiKey != "" || apiKeySecret != "" {
				settings.APIKeys = append(settings.APIKeys, APIKeyConfig{Key: apiKey, KeyVaultSecret: apiKeySecret, Scopes: []string{"admin", "inference"}, ModelAllow: []string{"*"}, CacheNamespace: "default"})
			}
			if adminKey != "" || adminKeySecret != "" || len(settings.APIKeys) == 0 {
				settings.APIKeys = append(settings.APIKeys, APIKeyConfig{Key: adminKey, KeyVaultSecret: adminKeySecret, Scopes: []string{"admin", "inference"}, ModelAllow: []string{"*"}, CacheNamespace: "default"})
			}
		}
	}
	if len(settings.APIKeys) == 0 {
		settings.APIKeys = []APIKeyConfig{{Key: "sk-local-dev", Scopes: []string{"admin", "inference"}, ModelAllow: []string{"*"}, CacheNamespace: "default"}}
	}
	for i := range settings.APIKeys {
		if len(settings.APIKeys[i].Scopes) == 0 {
			settings.APIKeys[i].Scopes = []string{"inference"}
		}
		if len(settings.APIKeys[i].ModelAllow) == 0 {
			settings.APIKeys[i].ModelAllow = []string{"*"}
		}
		if settings.APIKeys[i].CacheNamespace == "" {
			settings.APIKeys[i].CacheNamespace = "default"
		}
	}
	for i := range settings.Accounts {
		if settings.Accounts[i].Label == "" {
			settings.Accounts[i].Label = settings.Accounts[i].ID
		}
		if settings.Accounts[i].MaxConcurrency == 0 {
			settings.Accounts[i].MaxConcurrency = 32
		}
		if settings.Accounts[i].Weight == 0 {
			settings.Accounts[i].Weight = 1
		}
		if len(settings.Accounts[i].Allow) == 0 {
			settings.Accounts[i].Allow = []string{"*"}
		}
	}
	for i := range settings.Routes {
		if settings.Routes[i].Model == "" {
			settings.Routes[i].Model = "*"
		}
		if settings.Routes[i].Strategy == "" {
			settings.Routes[i].Strategy = "least_busy"
		}
	}
	for pattern, effort := range g.ReasoningEfforts {
		effort = strings.ToLower(effort)
		if !ValidReasoningEfforts[effort] {
			return Settings{}, fmt.Errorf("invalid reasoning effort %q for model pattern %q", effort, pattern)
		}
		settings.ReasoningEfforts[pattern] = effort
	}
	if settings.Usage.SQLitePath != ":memory:" {
		if dir := filepath.Dir(settings.Usage.SQLitePath); dir != "." && dir != "" {
			_ = os.MkdirAll(dir, 0o755)
		}
	}
	if settings.ModelMapPath != "" && settings.ModelMapPath != ":memory:" {
		if dir := filepath.Dir(settings.ModelMapPath); dir != "." && dir != "" {
			_ = os.MkdirAll(dir, 0o755)
		}
	}
	return settings, nil
}

func resolveReasoningEffort(model string, overrides map[string]string) string {
	bestScore := -1
	best := ""
	for pattern, effort := range overrides {
		if !globMatch(pattern, model) {
			continue
		}
		score := len(pattern)
		if !strings.ContainsAny(pattern, "*?") {
			score += 1000
		}
		if score > bestScore {
			bestScore = score
			best = effort
		}
	}
	return best
}
