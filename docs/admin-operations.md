# Admin operations

This guide covers day-to-day administration for ghcp-pool-go: managing Copilot
user accounts, viewing admin status/usage, and managing models.

## Setup

Set the gateway host and admin API key in your shell. Do not print or commit the
API key.

```bash
FQDN=<gateway-fqdn>

# Example for the current Azure deployment:
FQDN=ghcp-pool-go-vnet.ambitiousbay-5a038d28.eastus.azurecontainerapps.io

# Set this from your secret manager, Key Vault retrieval path, or local secure
# deployment metadata.
export GHCP_API_KEY="<gateway-admin-api-key>"
```

For a local GitHub CLI account token:

```bash
export GH_TOKEN="$(gh auth token)"

# Or select a specific locally logged-in GitHub account.
export GH_TOKEN="$(gh auth token --hostname github.com --user <github-user>)"
```

## Manage user accounts

### List users

```bash
curl -sS "https://$FQDN/admin/users" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

Useful fields:

| Field | Meaning |
| --- | --- |
| `id` | Gateway account ID used in routes and per-account endpoints. |
| `enabled` | Whether routing may use this account. |
| `started` | Whether the backend client started successfully. |
| `authenticated` | Whether a token or credential source exists. |
| `auth_method` | `key_vault`, `token_env`, `api_token`, `device_login`, etc. |
| `models` | Models currently discovered for this account. |
| `last_error` | Last startup/model-refresh error, if any. |

### Add a Copilot user with a Key Vault-backed token

For Azure Container Apps private deployments, prefer
`token_key_vault_secret`. The gateway stores the supplied token in Key Vault
from inside the VNet/private endpoint path, then starts the account.

```bash
NEW_USER_ID=acct-c
NEW_GH_TOKEN="$(gh auth token --hostname github.com --user <github-user>)"

curl -sS -X POST "https://$FQDN/admin/users" \
  -H "Authorization: Bearer $GHCP_API_KEY" \
  -H "Content-Type: application/json" \
  -d "{
    \"id\": \"$NEW_USER_ID\",
    \"label\": \"$NEW_USER_ID\",
    \"token\": \"$NEW_GH_TOKEN\",
    \"token_key_vault_secret\": \"ghcp-copilot-token-$NEW_USER_ID\",
    \"enabled\": true,
    \"max_concurrency\": 8,
    \"allow\": [\"*\"]
  }" | jq
```

If the response contains `ForbiddenByRbac` for `setSecret`, grant the Container
App managed identity a Key Vault role that can write secrets, such as
`Key Vault Secrets Officer`, at the vault scope.

Runtime user changes update the running gateway process. The token secret
persists in Key Vault, but the account entry itself should also be added to your
deployment config/startup template if it must survive a Container App revision
restart.

### Rotate an existing user's token

If the account has `token_key_vault_secret`, this updates that Key Vault secret.

```bash
USER_ID=acct-c
NEW_GH_TOKEN="$(gh auth token --hostname github.com --user <github-user>)"

curl -sS -X POST "https://$FQDN/admin/users/$USER_ID/token" \
  -H "Authorization: Bearer $GHCP_API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"token\":\"$NEW_GH_TOKEN\"}" | jq
```

### Remove a user

```bash
USER_ID=acct-c

curl -sS -X DELETE "https://$FQDN/admin/users/$USER_ID" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

Removing a user removes it from the running gateway pool. It does not purge any
Key Vault secret with the same token value; rotate or delete secrets separately
if required by your operations policy. Also remove the account from deployment
config/startup templates if it was made persistent there.

### Enable or disable an account

```bash
USER_ID=acct-c

curl -sS -X POST "https://$FQDN/admin/accounts/$USER_ID/disable" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq

curl -sS -X POST "https://$FQDN/admin/accounts/$USER_ID/enable" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

### Device login flow

The gateway defaults to the public Copilot OAuth client ID used by OpenCode, so
admin device login works without creating your own GitHub OAuth App. For custom
branding or stricter operations control, create or choose a GitHub OAuth App
that supports the device authorization flow, then override the client ID:

```yaml
gateway:
  login:
    client_id: "<github-oauth-app-client-id>"
    scopes: "read:user"
    device_code_url: "https://github.com/login/device/code"
    token_url: "https://github.com/login/oauth/access_token"
```

`client_id`, `scopes`, `device_code_url`, and `token_url` all have defaults, so
the minimal public GitHub.com config no longer needs a `login:` block.

To use the OpenCode-style direct provider path instead of the default SDK-first
mode, configure:

```yaml
gateway:
  copilot:
    mode: opencode
    auth_mode: oauth
    base_url: "https://api.githubcopilot.com"
```

`mode: sdk` is the default. It uses the official SDK for authentication, model
discovery, and eligible simple chat turns. It never calls direct Copilot APIs;
SDK-unsupported request shapes fail explicitly instead of falling back to HTTP.
`mode: opencode` disables SDK use and talks directly to the Copilot provider API;
when `auth_mode` is omitted, it defaults to `oauth`.

SDK mode uses the Copilot client's empty mode, so SDK built-in tools are off by
default. To handle OpenAI/Anthropic web-search requests through the SDK without
direct Copilot API calls, enable the explicit CLI-mode bridge:

```yaml
gateway:
  copilot:
    mode: sdk
    sdk_web_search_mode: cli
```

Only web-search requests use the temporary Copilot CLI-mode SDK client; normal
SDK turns still use empty mode. The legacy `sdk_web_search: true` setting keeps
the older empty-mode allowlist behavior, which may not expose a usable web
search tool in current SDK runtimes.

For advanced SDK tool allowlists, use `sdk_available_tools`:

```yaml
gateway:
  copilot:
    sdk_available_tools:
      - web_search
```

For models that expose a long-context tier, such as Claude Opus variants in
Copilot, clients can send `context_tier: "long_context"` in Chat Completions,
Responses, or Anthropic Messages requests. To pin a model by default:

```yaml
gateway:
  context_tiers:
    "claude-opus-4.8": long_context
```

For GitHub Enterprise or data-residency deployments, set the enterprise domain.
The gateway derives both device-login endpoints and
`https://copilot-api.<enterprise-domain>`:

```yaml
gateway:
  copilot:
    mode: opencode
    auth_mode: oauth
    enterprise_url: "ghe.example.com"
```

Per-account overrides are also available as `copilot_mode`, `copilot_auth_mode`,
`copilot_base_url`, `copilot_sdk_web_search`,
`copilot_sdk_available_tools`, and `github_enterprise_url`.

For the Azure Container Apps deployment in this repository, add the `login:`
and/or `copilot:` block to the generated `/tmp/config.yaml` startup template or
to the mounted `config.yaml`, then restart the Container App revision.

```bash
USER_ID=acct-c

curl -sS -X POST "https://$FQDN/admin/users/$USER_ID/login" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq

curl -sS -X POST "https://$FQDN/admin/users/$USER_ID/login/poll" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

## View admin information

### Dashboard

Open:

```text
https://ghcp-pool-go-vnet.ambitiousbay-5a038d28.eastus.azurecontainerapps.io/dashboard
```

The dashboard data API requires an admin bearer token:

```bash
curl -sS "https://$FQDN/admin/dashboard/data" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

### Usage

Aggregate usage:

```bash
curl -sS "https://$FQDN/admin/usage/aggregate" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

Raw/queryable usage:

```bash
curl -sS "https://$FQDN/admin/usage" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

Per-user usage:

```bash
USER_ID=acct-c

curl -sS "https://$FQDN/admin/users/$USER_ID/usage" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

### Rate limits

View current limits and account rate-limit state:

```bash
curl -sS "https://$FQDN/admin/rate-limits" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

Update global and per-account RPM limits:

```bash
curl -sS -X PUT "https://$FQDN/admin/rate-limits" \
  -H "Authorization: Bearer $GHCP_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"global_rpm": 0, "per_account_rpm": 60}' | jq
```

Use `0` for unlimited.

### Cache and debug

```bash
curl -sS "https://$FQDN/admin/cache/stats" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq

curl -sS -X DELETE "https://$FQDN/admin/cache" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq

curl -sS "https://$FQDN/admin/debug" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

## Manage models

### List visible client models

```bash
curl -sS "https://$FQDN/v1/models" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

### View full model registry

```bash
curl -sS "https://$FQDN/admin/models" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

This shows:

| Field | Meaning |
| --- | --- |
| `models_count` | Number of distinct discovered upstream models. |
| `models` | Model-to-account serving map. |
| `capabilities` | Per-account model capabilities and supported endpoints. |
| `model_aliases` | Friendly aliases mapped to upstream model IDs. |

### View models for one account

```bash
USER_ID=acct-c

curl -sS "https://$FQDN/admin/users/$USER_ID/models" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

If an account only shows `gpt-4.1`, that is what SDK model discovery advertised
for that token. `allow: ["*"]` permits all advertised models; it does not grant
additional Copilot model entitlement.

### Refresh model discovery

Refresh all accounts:

```bash
curl -sS -X POST "https://$FQDN/admin/models/refresh" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

Refresh one account:

```bash
USER_ID=acct-c

curl -sS -X POST "https://$FQDN/admin/models/refresh/$USER_ID" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

### Manage model aliases

Aliases let clients request friendly names while the gateway routes to upstream
model IDs.

View aliases:

```bash
curl -sS "https://$FQDN/admin/model-aliases" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

Replace aliases:

```bash
curl -sS -X PUT "https://$FQDN/admin/model-aliases" \
  -H "Authorization: Bearer $GHCP_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "gpt-default": "gpt-4.1",
    "haiku": "claude-3-5-haiku-latest"
  }' | jq
```

Only map aliases to models that are actually discovered for at least one
enabled account.

### Manage routes

View routes:

```bash
curl -sS "https://$FQDN/admin/routes" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

Replace routes:

```bash
curl -sS -X PUT "https://$FQDN/admin/routes" \
  -H "Authorization: Bearer $GHCP_API_KEY" \
  -H "Content-Type: application/json" \
  -d '[
    {"model": "claude-*", "accounts": ["acct-c"], "strategy": "smart", "priority": 10},
    {"model": "*", "accounts": ["acct-a", "acct-b", "acct-c"], "strategy": "smart"}
  ]' | jq
```

Supported route strategies are `round_robin`, `least_busy`, `weighted`,
`quota_aware`, and `smart`.

Explain how a model would route:

```bash
curl -sS "https://$FQDN/admin/routes/resolve?model=gpt-default&endpoint=/chat/completions" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

### Manage reasoning effort defaults

```bash
curl -sS "https://$FQDN/admin/reasoning-efforts" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq

curl -sS -X PUT "https://$FQDN/admin/reasoning-efforts" \
  -H "Authorization: Bearer $GHCP_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"gpt-*": "medium", "claude-*": "low"}' | jq
```

Valid effort values are `none`, `low`, `medium`, `high`, `xhigh`, and `max`.
