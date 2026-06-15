package gateway

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

const keyVaultRequestTimeout = 30 * time.Second

type keyVaultSecretClient interface {
	GetSecret(context.Context, string) (string, error)
	SetSecret(context.Context, string, string) error
}

type azureKeyVaultSecretClient struct {
	client *azsecrets.Client
}

var (
	keyVaultClientCache sync.Map
	newKeyVaultClient   = newAzureKeyVaultSecretClient
)

func newAzureKeyVaultSecretClient(vaultURL string) (keyVaultSecretClient, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}
	client, err := azsecrets.NewClient(vaultURL, credential, nil)
	if err != nil {
		return nil, err
	}
	return &azureKeyVaultSecretClient{client: client}, nil
}

func (c *azureKeyVaultSecretClient) GetSecret(ctx context.Context, name string) (string, error) {
	resp, err := c.client.GetSecret(ctx, name, "", nil)
	if err != nil {
		return "", err
	}
	if resp.Value == nil {
		return "", fmt.Errorf("key vault secret %q has no value", name)
	}
	return *resp.Value, nil
}

func (c *azureKeyVaultSecretClient) SetSecret(ctx context.Context, name, value string) error {
	_, err := c.client.SetSecret(ctx, name, azsecrets.SetSecretParameters{Value: &value}, nil)
	return err
}

func getKeyVaultSecret(ctx context.Context, vaultURL, name string) (string, error) {
	client, normalizedURL, err := keyVaultClientFor(vaultURL)
	if err != nil {
		return "", err
	}
	ctx, cancel := keyVaultContext(ctx)
	defer cancel()
	value, err := client.GetSecret(ctx, name)
	if err != nil {
		return "", fmt.Errorf("get Key Vault secret %q from %s: %w", name, normalizedURL, err)
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("Key Vault secret %q from %s is empty", name, normalizedURL)
	}
	return value, nil
}

func setKeyVaultSecret(ctx context.Context, vaultURL, name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("Key Vault secret %q value must be non-empty", name)
	}
	client, normalizedURL, err := keyVaultClientFor(vaultURL)
	if err != nil {
		return err
	}
	ctx, cancel := keyVaultContext(ctx)
	defer cancel()
	if err := client.SetSecret(ctx, name, value); err != nil {
		return fmt.Errorf("set Key Vault secret %q in %s: %w", name, normalizedURL, err)
	}
	return nil
}

func keyVaultClientFor(vaultURL string) (keyVaultSecretClient, string, error) {
	normalizedURL, err := normalizeKeyVaultURL(vaultURL)
	if err != nil {
		return nil, "", err
	}
	if client, ok := keyVaultClientCache.Load(normalizedURL); ok {
		return client.(keyVaultSecretClient), normalizedURL, nil
	}
	client, err := newKeyVaultClient(normalizedURL)
	if err != nil {
		return nil, "", fmt.Errorf("create Key Vault client for %s: %w", normalizedURL, err)
	}
	actual, _ := keyVaultClientCache.LoadOrStore(normalizedURL, client)
	return actual.(keyVaultSecretClient), normalizedURL, nil
}

func keyVaultContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, keyVaultRequestTimeout)
}

func normalizeKeyVaultURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("key_vault_url is required")
	}
	if !strings.Contains(value, "://") {
		if !strings.Contains(value, ".") {
			value += ".vault.azure.net"
		}
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("invalid key_vault_url %q: %w", value, err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("key_vault_url must be an https URL, got %q", value)
	}
	parsed.Path = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}
