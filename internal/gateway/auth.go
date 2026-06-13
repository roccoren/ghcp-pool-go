package gateway

import "strings"

type Principal struct {
	config APIKeyConfig
}

func (p Principal) HasScope(scope string) bool {
	for _, s := range p.config.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

func (p Principal) CacheNamespace() string {
	if p.config.CacheNamespace == "" {
		return "default"
	}
	return p.config.CacheNamespace
}

func (p Principal) MayUseModel(model string) bool {
	patterns := p.config.ModelAllow
	if len(patterns) == 0 {
		patterns = []string{"*"}
	}
	return matchesAny(model, patterns)
}

type Authenticator struct {
	keys map[string]APIKeyConfig
}

func NewAuthenticator(settings Settings) *Authenticator {
	keys := make(map[string]APIKeyConfig, len(settings.APIKeys))
	for _, key := range settings.APIKeys {
		keys[key.Key] = key
	}
	return &Authenticator{keys: keys}
}

func (a *Authenticator) Authenticate(value string) (Principal, bool) {
	if value == "" {
		return Principal{}, false
	}
	token := value
	if strings.HasPrefix(strings.ToLower(value), "bearer ") {
		token = strings.TrimSpace(value[7:])
	}
	config, ok := a.keys[token]
	if !ok {
		return Principal{}, false
	}
	return Principal{config: config}, true
}
