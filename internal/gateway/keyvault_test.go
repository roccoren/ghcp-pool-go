package gateway

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

type memoryKeyVaultClient struct {
	mu      sync.Mutex
	secrets map[string]string
}

func (m *memoryKeyVaultClient) GetSecret(_ context.Context, name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.secrets[name]
	if !ok {
		return "", fmt.Errorf("secret %q not found", name)
	}
	return value, nil
}

func (m *memoryKeyVaultClient) SetSecret(_ context.Context, name, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.secrets[name] = value
	return nil
}

func withMemoryKeyVault(t *testing.T, client *memoryKeyVaultClient) {
	t.Helper()
	oldFactory := newKeyVaultClient
	keyVaultClientCache = sync.Map{}
	newKeyVaultClient = func(string) (keyVaultSecretClient, error) {
		return client, nil
	}
	t.Cleanup(func() {
		newKeyVaultClient = oldFactory
		keyVaultClientCache = sync.Map{}
	})
}

func TestAPIKeyResolvesFromKeyVault(t *testing.T) {
	kv := &memoryKeyVaultClient{secrets: map[string]string{"ghcp-api-key": "sk-from-kv"}}
	withMemoryKeyVault(t, kv)

	settings := testSettings()
	settings.KeyVaultURL = "test-kv"
	settings.APIKeys = []APIKeyConfig{{
		KeyVaultSecret: "ghcp-api-key",
		Scopes:         []string{"admin", "inference"},
		ModelAllow:     []string{"*"},
		CacheNamespace: "default",
	}}
	gw, err := NewGateway(settings)
	if err != nil {
		t.Fatal(err)
	}
	principal, ok := gw.Authenticator.Authenticate("Bearer sk-from-kv")
	if !ok || !principal.HasScope("admin") || !principal.HasScope("inference") {
		t.Fatalf("expected Key Vault API key to authenticate with admin and inference scopes")
	}
}

func TestEnvKeyVaultSecretDefaults(t *testing.T) {
	t.Setenv("GHCP_BACKEND", "copilot")
	t.Setenv("AZURE_KEY_VAULT_URL", "test-kv")
	t.Setenv("GHCP_API_KEY_KEY_VAULT_SECRET", "ghcp-api-key")
	t.Setenv("GHCP_COPILOT_TOKEN_KEY_VAULT_SECRET", "ghcp-copilot-token")

	settings, err := LoadSettings("/does/not/exist.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if settings.KeyVaultURL != "test-kv" {
		t.Fatalf("key vault url=%q", settings.KeyVaultURL)
	}
	if got := settings.APIKeys[0].KeyVaultSecret; got != "ghcp-api-key" {
		t.Fatalf("api key secret=%q", got)
	}
	if got := settings.Accounts[0].TokenKeyVaultSecret; got != "ghcp-copilot-token" {
		t.Fatalf("copilot token secret=%q", got)
	}
}

func TestLoginManagerStoresTokenInKeyVault(t *testing.T) {
	kv := &memoryKeyVaultClient{secrets: map[string]string{}}
	withMemoryKeyVault(t, kv)

	settings := testSettings()
	settings.KeyVaultURL = "https://test-kv.vault.azure.net/"
	settings.Accounts[0].TokenKeyVaultSecret = "ghcp-copilot-token-user-a"
	gw, err := NewGateway(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.LoginManager.SetToken(context.Background(), "acct_a", "gho-copilot-token"); err != nil {
		t.Fatal(err)
	}
	if got := kv.secrets["ghcp-copilot-token-user-a"]; got != "gho-copilot-token" {
		t.Fatalf("stored token=%q", got)
	}
	account := gw.Pool.Get("acct_a")
	if account == nil {
		t.Fatal("account not found")
	}
	if got := account.Config.RuntimeTokenSource; got != "api_token_key_vault" {
		t.Fatalf("runtime token source=%q", got)
	}
}
