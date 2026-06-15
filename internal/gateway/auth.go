package gateway

import (
	"context"
	"fmt"
	"strings"
)

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

func NewAuthenticator(ctx context.Context, settings Settings) (*Authenticator, error) {
	keys := make(map[string]APIKeyConfig, len(settings.APIKeys))
	for i, key := range settings.APIKeys {
		resolved, err := key.ResolveKey(ctx, settings.KeyVaultURL)
		if err != nil {
			return nil, err
		}
		if resolved == "" {
			return nil, fmt.Errorf("api key %d has no key, key_env, or key_vault_secret", i)
		}
		key.Key = resolved
		keys[resolved] = key
	}
	return &Authenticator{keys: keys}, nil
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
