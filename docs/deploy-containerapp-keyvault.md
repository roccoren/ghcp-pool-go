# Deploy ghcp-pool-go with Azure Key Vault secrets

This guide shows how to keep the gateway API key and the GitHub/Copilot token
in Azure Key Vault, then inject them into an existing Azure Container App by
using Container Apps Key Vault secret references.

The two secrets have different purposes:

| Secret | Runtime env var | Used by | Purpose |
| --- | --- | --- | --- |
| Gateway API key | `GHCP_API_KEY` | Clients -> ghcp-pool-go | Authenticates clients calling `/v1/*`, `/v1/messages`, `/v1beta/*`, and admin endpoints. |
| GitHub/Copilot token | `GHCP_COPILOT_TOKEN` | ghcp-pool-go -> GitHub/Copilot | Exchanged by the gateway for a Copilot API token, then used to call Copilot upstream. |

Do not put either value in `config.yaml`, Docker image layers, shell history, or
source control.

## Assumptions

These examples use the current deployment names:

```bash
RG=ghcp-pool-rg
APP=ghcp-pool-go
KV=ghcppoolkvb3nvpcl2jk3js
API_SECRET_NAME=ghcp-api-key
COPILOT_SECRET_NAME=ghcp-copilot-token
```

If your deployment uses different names, change the variables before running the
commands.

## 1. Verify the Container App identity

The Container App should have a managed identity so it can read Key Vault
secrets without storing Key Vault credentials.

```bash
az containerapp identity assign \
  -g "$RG" \
  -n "$APP" \
  --system-assigned

PRINCIPAL_ID="$(az containerapp show \
  -g "$RG" \
  -n "$APP" \
  --query identity.principalId \
  -o tsv)"

echo "$PRINCIPAL_ID"
```

## 2. Store/rotate the secrets in Key Vault

Generate or choose a gateway API key:

```bash
export GHCP_API_KEY_VALUE="sk-$(openssl rand -hex 24)"
```

Use the currently logged-in GitHub CLI token for Copilot, after verifying that
it can be exchanged for a Copilot token:

```bash
export GHCP_COPILOT_TOKEN_VALUE="$(gh auth token)"

curl -fsS \
  -H "Authorization: Bearer $GHCP_COPILOT_TOKEN_VALUE" \
  -H "User-Agent: GitHubCopilotChat/0.39.0" \
  -H "Accept: application/json" \
  https://api.github.com/copilot_internal/v2/token \
  >/dev/null
```

Write both values to Key Vault. The `--query` avoids printing secret values.

```bash
az keyvault secret set \
  --vault-name "$KV" \
  --name "$API_SECRET_NAME" \
  --value "$GHCP_API_KEY_VALUE" \
  --query '{name:name,id:id}' \
  -o json

az keyvault secret set \
  --vault-name "$KV" \
  --name "$COPILOT_SECRET_NAME" \
  --value "$GHCP_COPILOT_TOKEN_VALUE" \
  --query '{name:name,id:id}' \
  -o json
```

## 3. Grant the Container App identity access to Key Vault

If the Key Vault uses Azure RBAC authorization, assign
`Key Vault Secrets User` to the Container App system identity:

```bash
KV_ID="$(az keyvault show \
  -g "$RG" \
  -n "$KV" \
  --query id \
  -o tsv)"

az role assignment create \
  --assignee "$PRINCIPAL_ID" \
  --role "Key Vault Secrets User" \
  --scope "$KV_ID"
```

If the Key Vault uses access policies instead of Azure RBAC:

```bash
az keyvault set-policy \
  -g "$RG" \
  -n "$KV" \
  --object-id "$PRINCIPAL_ID" \
  --secret-permissions get list
```

Use only one permission model. Do not mix RBAC and access-policy assumptions for
the same vault.

## 4. Point Container App secrets at Key Vault

Get versionless Key Vault secret URIs:

```bash
API_SECRET_URI="$(az keyvault secret show \
  --vault-name "$KV" \
  --name "$API_SECRET_NAME" \
  --query id \
  -o tsv | sed 's|/[0-9a-fA-F-]*$||')"

COPILOT_SECRET_URI="$(az keyvault secret show \
  --vault-name "$KV" \
  --name "$COPILOT_SECRET_NAME" \
  --query id \
  -o tsv | sed 's|/[0-9a-fA-F-]*$||')"
```

Configure Container App secret references:

```bash
az containerapp secret set \
  -g "$RG" \
  -n "$APP" \
  --secrets \
    "$API_SECRET_NAME=keyvaultref:$API_SECRET_URI,identityref:system" \
    "$COPILOT_SECRET_NAME=keyvaultref:$COPILOT_SECRET_URI,identityref:system"
```

Then bind those Container App secrets to runtime environment variables:

```bash
az containerapp update \
  -g "$RG" \
  -n "$APP" \
  --set-env-vars \
    GHCP_BACKEND=copilot \
    GHCP_API_KEY=secretref:"$API_SECRET_NAME" \
    GHCP_COPILOT_TOKEN=secretref:"$COPILOT_SECRET_NAME"
```

Restart the active revision so the new secret references are read:

```bash
REV="$(az containerapp show \
  -g "$RG" \
  -n "$APP" \
  --query properties.latestRevisionName \
  -o tsv)"

az containerapp revision restart \
  -g "$RG" \
  -n "$APP" \
  --revision "$REV"
```

## 5. Validate without revealing secrets

Check that environment variables reference Container App secrets:

```bash
az containerapp show \
  -g "$RG" \
  -n "$APP" \
  --query 'properties.template.containers[0].env' \
  -o table
```

Check health and readiness:

```bash
FQDN="$(az containerapp show \
  -g "$RG" \
  -n "$APP" \
  --query properties.configuration.ingress.fqdn \
  -o tsv)"

curl -fsS "https://$FQDN/healthz"
curl -fsS "https://$FQDN/readyz"
```

Use the new API key value from your current shell to test models:

```bash
curl -sS "https://$FQDN/v1/models" \
  -H "Authorization: Bearer $GHCP_API_KEY_VALUE" \
  | jq '{count: (.data | length), first10: [.data[0:10][]?.id]}'
```

## 6. Multi-user / multi-account Copilot tokens

For a pool with multiple Copilot users, do not share one global
`GHCP_COPILOT_TOKEN`. Store one GitHub/Copilot token per gateway account and
reference each token through a separate environment variable.

Example account layout:

| Account | Key Vault secret | Container App env var | Gateway config |
| --- | --- | --- | --- |
| `user-a` | `ghcp-copilot-token-user-a` | `GHCP_COPILOT_TOKEN_USER_A` | `token_env: GHCP_COPILOT_TOKEN_USER_A` |
| `user-b` | `ghcp-copilot-token-user-b` | `GHCP_COPILOT_TOKEN_USER_B` | `token_env: GHCP_COPILOT_TOKEN_USER_B` |

### 6.1 Store each user's token in Key Vault

Repeat this for each user. The example assumes you have obtained the user's
GitHub token out of band and stored it in a local shell variable.

```bash
USER_A_SECRET_NAME=ghcp-copilot-token-user-a
USER_A_ENV_NAME=GHCP_COPILOT_TOKEN_USER_A

# Do not echo this value.
export USER_A_GH_TOKEN="<user-a-github-token>"

curl -fsS \
  -H "Authorization: Bearer $USER_A_GH_TOKEN" \
  -H "User-Agent: GitHubCopilotChat/0.39.0" \
  -H "Accept: application/json" \
  https://api.github.com/copilot_internal/v2/token \
  >/dev/null

az keyvault secret set \
  --vault-name "$KV" \
  --name "$USER_A_SECRET_NAME" \
  --value "$USER_A_GH_TOKEN" \
  --query '{name:name,id:id}' \
  -o json
```

### 6.2 Create Key Vault references for each user token

```bash
USER_A_SECRET_URI="$(az keyvault secret show \
  --vault-name "$KV" \
  --name "$USER_A_SECRET_NAME" \
  --query id \
  -o tsv | sed 's|/[0-9a-fA-F-]*$||')"

az containerapp secret set \
  -g "$RG" \
  -n "$APP" \
  --secrets \
    "$USER_A_SECRET_NAME=keyvaultref:$USER_A_SECRET_URI,identityref:system"
```

Bind the secret reference to a per-account environment variable:

```bash
az containerapp update \
  -g "$RG" \
  -n "$APP" \
  --set-env-vars \
    "$USER_A_ENV_NAME=secretref:$USER_A_SECRET_NAME"
```

For many users, you can pass multiple `--set-env-vars` entries in one command:

```bash
az containerapp update \
  -g "$RG" \
  -n "$APP" \
  --set-env-vars \
    GHCP_COPILOT_TOKEN_USER_A=secretref:ghcp-copilot-token-user-a \
    GHCP_COPILOT_TOKEN_USER_B=secretref:ghcp-copilot-token-user-b
```

### 6.3 Configure gateway accounts

Mount or bake a `config.yaml` that maps accounts to those env vars:

```yaml
backend: copilot
gateway:
  api_keys:
    - key: sk-replace-with-your-gateway-api-key
      scopes: [admin, inference]
      model_allow: ["*"]
      cache_namespace: default
accounts:
  - id: user-a
    label: User A
    token_env: GHCP_COPILOT_TOKEN_USER_A
    enabled: true
    max_concurrency: 8
    allow: ["claude-*", "gpt-*", "gemini-*"]
  - id: user-b
    label: User B
    token_env: GHCP_COPILOT_TOKEN_USER_B
    enabled: true
    max_concurrency: 8
    allow: ["claude-*", "gpt-*", "gemini-*"]
routes:
  - model: "*"
    accounts: ["user-a", "user-b"]
    strategy: smart
```

> Note: the gateway config loader does not expand `${...}` placeholders inside
> YAML. Set `gateway.api_keys[].key` to the actual gateway API key value in your
> rendered config, or use `GHCP_API_KEY` for a single global API key. The
> per-account `token_env` values are environment variable names and are resolved
> by the gateway at runtime.

If you prefer to add accounts after startup, use the admin API and keep the
runtime token in the gateway process memory. For persistent multi-user
deployments, `token_env` + Key Vault references are easier to recreate after
restart.

### 6.4 Restart and validate

Restart after adding or rotating any per-user secret:

```bash
REV="$(az containerapp show \
  -g "$RG" \
  -n "$APP" \
  --query properties.latestRevisionName \
  -o tsv)"

az containerapp revision restart \
  -g "$RG" \
  -n "$APP" \
  --revision "$REV"
```

Then verify account/model state without printing tokens:

```bash
curl -sS "https://$FQDN/admin/users" \
  -H "Authorization: Bearer $GHCP_API_KEY_VALUE" \
  | jq '.users[] | {id, authenticated, started, enabled, models_count: (.models | length), last_error}'
```

## 7. Rotate later

To rotate either secret:

1. Set a new value in Key Vault with `az keyvault secret set`.
2. Restart the Container App revision.
3. Test with the new API key.

When the Container App secret reference uses a versionless Key Vault URI, the
same Container App secret name can follow the latest Key Vault secret version
after restart.

## Private Key Vault networking note

If the Key Vault has `publicNetworkAccess` disabled, the Container App
environment must be able to reach the vault through a private endpoint and
private DNS for `privatelink.vaultcore.azure.net`. Without that network path,
Key Vault secret references can fail even when RBAC permissions are correct.

For this repository's Azure environment, prefer private endpoint access over
enabling public Key Vault access.
