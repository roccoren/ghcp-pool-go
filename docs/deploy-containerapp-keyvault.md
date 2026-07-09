# Deploy ghcp-pool-go with Azure Container Apps and private Key Vault

This guide shows two supported patterns:

1. Deploy a full private Azure stack with Container Apps in a VNet, Key Vault
   public access disabled, and a Key Vault private endpoint/private DNS link.
2. Retrofit an existing Container App by using Container Apps Key Vault secret
   references.

The full stack is preferred for private deployments because ghcp-pool-go reads
tokens directly from Key Vault at runtime with managed identity. With additional
write RBAC, admin token updates can also persist rotated Copilot tokens back to
Key Vault.

The two secrets have different purposes:

| Secret | Runtime config | Used by | Purpose |
| --- | --- | --- | --- |
| Gateway API key | `GHCP_API_KEY` or `GHCP_API_KEY_KEY_VAULT_SECRET` | Clients -> ghcp-pool-go | Authenticates clients calling `/v1/*`, `/v1/messages`, `/v1beta/*`, and admin endpoints. |
| GitHub/Copilot token | `GHCP_COPILOT_TOKEN` or `GHCP_COPILOT_TOKEN_KEY_VAULT_SECRET` | ghcp-pool-go -> GitHub/Copilot | Exchanged by the gateway for a Copilot API token, then used to call Copilot upstream. |

Do not put either value in `config.yaml`, Docker image layers, shell history, or
source control.

## Minimum Azure resources

For the smallest Azure deployment that is still suitable for shared use, run
ghcp-pool-go on Azure Container Apps and keep the gateway API key and Copilot
tokens in Key Vault. The runtime-minimum Azure resource set is:

| Azure resource | Minimum shape | Required for |
| --- | --- | --- |
| Resource group | One resource group in an Azure region that supports Container Apps and private endpoints. | Deployment boundary for all resources below. |
| Container Apps environment | One managed environment. For the private stack, attach it to a delegated `Microsoft.App/environments` subnet; the template uses a `/23`. | Hosts Container Apps revisions and networking. |
| Container App | One app running the ghcp-pool-go image. The template defaults to 0.5 vCPU, 1.0 GiB memory, `minReplicas=1`, `maxReplicas=3`, target port 8000. | Runs the gateway. |
| Log Analytics workspace | One workspace, `PerGB2018`, 30-day retention in the template. | Container Apps logs. |
| Managed identity | One user-assigned identity on the Container App. | Lets the app read Key Vault without storing Azure credentials. |
| Key Vault | One Standard vault with Azure RBAC. For the private stack, disable public network access. | Stores `ghcp-api-key`, `ghcp-copilot-token`, and any per-account Copilot tokens. |
| Key Vault role assignment | `Key Vault Secrets User` for the app identity at vault scope. Add `Key Vault Secrets Officer` only if admin token update APIs must write tokens back to Key Vault. | Runtime secret reads; optional token rotation writes. |
| Virtual network | One VNet with a Container Apps delegated subnet and a private endpoint subnet. | Required when Key Vault public network access is disabled. |
| Key Vault private endpoint | One private endpoint in the private endpoint subnet. | Private network path from Container Apps to Key Vault. |
| Private DNS zone/link | `privatelink.vaultcore.azure.net` linked to the VNet. | Resolves the Key Vault hostname to the private endpoint. |
| Container image source | Any reachable registry image, for example GHCR or ACR. | Supplies the container image. ACR is optional unless you want Azure-native image builds/storage. |

Not required for the minimal gateway deployment: Azure OpenAI/Cognitive
Services, Storage accounts, App Service, Azure Functions, Azure SQL, Redis, or a
custom domain. Add a custom domain, ACR, persistent storage, or extra monitoring
only if your operational requirements need them.

## Runtime Key Vault settings

The gateway can resolve secrets directly from Azure Key Vault:

```yaml
gateway:
  key_vault_url: https://<vault-name>.vault.azure.net/
  api_keys:
    - key_vault_secret: ghcp-api-key
      scopes: [admin, inference]
      model_allow: ["*"]
      cache_namespace: default
accounts:
  - id: default
    token_key_vault_secret: ghcp-copilot-token
    enabled: true
```

Equivalent environment-only defaults are supported for container deployments:

| Env var | Purpose |
| --- | --- |
| `AZURE_KEY_VAULT_URL` | Default vault URL for API keys and account tokens. |
| `AZURE_CLIENT_ID` | User-assigned managed identity client ID, when using one. |
| `GHCP_API_KEY_KEY_VAULT_SECRET` | Gateway API key secret name. |
| `GHCP_API_KEY_KEY_VAULT_SECRETS` | Optional comma/whitespace-separated gateway API key secret names. |
| `GHCP_ADMIN_API_KEY_KEY_VAULT_SECRET` | Optional separate admin key secret name. |
| `GHCP_COPILOT_TOKEN_KEY_VAULT_SECRET` | Default account Copilot token secret name. |

For multi-account pools, set `accounts[].token_key_vault_secret` per account.
Gateway API key secret values can contain a single key or multiple keys
separated by commas, spaces, tabs, or newlines.
When admin login or `POST /admin/users/{id}/token` sets a token for an account
with `token_key_vault_secret`, the gateway stores the new token in that Key
Vault secret before rebuilding the account backend.

## Deploy the private Container Apps + Key Vault stack

The Bicep template at `infra/containerapp-keyvault.bicep` creates the private
minimum resource set:

- VNet with a delegated Container Apps subnet and a private endpoint subnet.
- Key Vault with Azure RBAC enabled and public network access disabled.
- Key Vault private endpoint plus `privatelink.vaultcore.azure.net` private DNS.
- User-assigned managed identity with `Key Vault Secrets User` on the vault.
- Container Apps environment and app configured with `AZURE_KEY_VAULT_URL`,
  `AZURE_CLIENT_ID`, `GHCP_API_KEY_KEY_VAULT_SECRET`, and
  `GHCP_COPILOT_TOKEN_KEY_VAULT_SECRET`.

Deploy with Azure Deployment Stacks:

```bash
RG=ghcp-pool-rg
LOCATION=eastus
STACK=ghcp-pool-private
IMAGE=ghcr.io/<owner>/ghcp-pool-go:latest

export GHCP_API_KEY_VALUE="sk-$(openssl rand -hex 24)"
export GHCP_COPILOT_TOKEN_VALUE="$(gh auth token)"

az group create -n "$RG" -l "$LOCATION"

az stack group create \
  -g "$RG" \
  -n "$STACK" \
  --template-file infra/containerapp-keyvault.bicep \
  --parameters \
    location="$LOCATION" \
    containerImage="$IMAGE" \
    gatewayApiKey="$GHCP_API_KEY_VALUE" \
    copilotToken="$GHCP_COPILOT_TOKEN_VALUE" \
  --action-on-unmanage detachAll \
  --deny-settings-mode none
```

The secure parameters create the initial `ghcp-api-key` and
`ghcp-copilot-token` Key Vault secrets. The Container App does not receive those
values as plain environment variables; it receives only the vault URL and secret
names, then resolves them through the managed identity over the VNet/private
endpoint path.

If RBAC propagation delays cause the first app revision to start before the
identity can read Key Vault, restart the latest revision after role assignment
propagates.

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

Manual one-off token-validity check during setup — the gateway itself never calls this endpoint; it delegates all Copilot access to the SDK/CLI.

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
configure each account with its own `token_key_vault_secret`.

Example account layout:

| Account | Key Vault secret | Gateway config |
| --- | --- | --- |
| `user-a` | `ghcp-copilot-token-user-a` | `token_key_vault_secret: ghcp-copilot-token-user-a` |
| `user-b` | `ghcp-copilot-token-user-b` | `token_key_vault_secret: ghcp-copilot-token-user-b` |

### 6.1 Store each user's token in Key Vault

Repeat this for each user. The example assumes you have obtained the user's
GitHub token out of band and stored it in a local shell variable.

```bash
USER_A_SECRET_NAME=ghcp-copilot-token-user-a

# Do not echo this value.
export USER_A_GH_TOKEN="<user-a-github-token>"

# Manual one-off token-validity check during setup — the gateway itself never calls this endpoint; it delegates all Copilot access to the SDK/CLI.

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

### 6.2 Configure gateway accounts for direct Key Vault resolution

The private stack already sets `AZURE_KEY_VAULT_URL`, `AZURE_CLIENT_ID`, and the
managed identity role assignment. For a custom config, map each account to its
Key Vault secret name:

```yaml
backend: copilot
gateway:
  key_vault_url: https://<vault-name>.vault.azure.net/
  api_keys:
    - key_vault_secret: ghcp-api-key
      scopes: [admin, inference]
      model_allow: ["*"]
      cache_namespace: default
accounts:
  - id: user-a
    label: User A
    token_key_vault_secret: ghcp-copilot-token-user-a
    enabled: true
    max_concurrency: 8
    allow: ["claude-*", "gpt-*", "gemini-*"]
  - id: user-b
    label: User B
    token_key_vault_secret: ghcp-copilot-token-user-b
    enabled: true
    max_concurrency: 8
    allow: ["claude-*", "gpt-*", "gemini-*"]
routes:
  - model: "*"
    accounts: ["user-a", "user-b"]
    strategy: smart
```

### 6.3 Alternative: Container App secret references

If you prefer to let Container Apps inject each token as an environment variable
instead of having the gateway call Key Vault directly, create Key Vault
references for each user token:

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
USER_A_ENV_NAME=GHCP_COPILOT_TOKEN_USER_A

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

Then mount or bake a `config.yaml` that maps accounts to those env vars:

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
> rendered config, use `GHCP_API_KEY`, or use `gateway.api_keys[].key_vault_secret`.
> The per-account `token_env` values are environment variable names and are
> resolved by the gateway at runtime.

If you prefer to add accounts after startup, use the admin API and keep the
runtime token in the gateway process memory. For persistent multi-user private
deployments, `token_key_vault_secret` is easier to recreate after restart and
lets admin token updates persist back to Key Vault.

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

## Durable usage telemetry (Azure Monitor / Log Analytics)

The gateway keeps per-request usage in a local SQLite file for immediate admin
reads, but that file is ephemeral in a container. To persist usage across
restarts and scale events, stream every usage event to a Log Analytics custom
table through the Azure Monitor Logs Ingestion API, then set three env vars on
the Container App. SQLite stays the fast read cache; Log Analytics is the
durable, cross-replica record. See the "Durable usage persistence" section of
the README for the runtime behavior and KQL query examples.

The Bicep template `infra/containerapp-keyvault.bicep` provisions all of this and
wires the env vars automatically. The steps below reproduce the same setup with
the `az` CLI against an existing deployment.

| Azure resource | Minimum shape | Required for |
| --- | --- | --- |
| Log Analytics workspace | One workspace (the private stack already creates one for Container Apps logs; a dedicated `ghcp-usage-law` keeps usage data isolated). | Stores the `UsageEvent_CL` custom table. |
| Custom table | `UsageEvent_CL` with the usage columns. | Destination for usage events. |
| Data Collection Endpoint | One DCE in the workspace region. | Ingestion endpoint the gateway POSTs to. |
| Data Collection Rule | One `Direct` DCR with a `Custom-UsageEvent` stream to `UsageEvent_CL`. | Routes and transforms incoming events. |
| Role assignment | `Monitoring Metrics Publisher` for the app identity at DCR scope. | Lets the managed identity ingest data. |

### 1. Create the workspace and custom table

```bash
RG=ghcp-pool-rg
LOCATION=eastus
LAW=ghcp-usage-law

az monitor log-analytics workspace create \
  -g "$RG" -n "$LAW" -l "$LOCATION" --sku PerGB2018 --retention-time 30

az monitor log-analytics workspace table create \
  -g "$RG" --workspace-name "$LAW" -n UsageEvent_CL --retention-time 30 \
  --columns \
    TimeGenerated=datetime AccountId=string Model=string ApiEndpoint=string \
    InputTokens=long OutputTokens=long CachedTokens=long TotalTokens=long \
    Credits=real DurationMs=long CacheResult=string Success=boolean ErrorType=string
```

### 2. Create the Data Collection Endpoint and Rule

```bash
az extension add -n monitor-control-service -y

az monitor data-collection endpoint create \
  -g "$RG" -n ghcp-usage-dce -l "$LOCATION" --public-network-access Enabled

DCE_ID=$(az monitor data-collection endpoint show -g "$RG" -n ghcp-usage-dce --query id -o tsv)
WS_ID=$(az monitor log-analytics workspace show -g "$RG" -n "$LAW" --query id -o tsv)

cat > /tmp/ghcp-usage-dcr.json <<JSON
{
  "dataCollectionEndpointId": "${DCE_ID}",
  "streamDeclarations": {
    "Custom-UsageEvent": {
      "columns": [
        { "name": "TimeGenerated", "type": "datetime" },
        { "name": "AccountId", "type": "string" },
        { "name": "Model", "type": "string" },
        { "name": "ApiEndpoint", "type": "string" },
        { "name": "InputTokens", "type": "long" },
        { "name": "OutputTokens", "type": "long" },
        { "name": "CachedTokens", "type": "long" },
        { "name": "TotalTokens", "type": "long" },
        { "name": "Credits", "type": "real" },
        { "name": "DurationMs", "type": "long" },
        { "name": "CacheResult", "type": "string" },
        { "name": "Success", "type": "boolean" },
        { "name": "ErrorType", "type": "string" }
      ]
    }
  },
  "destinations": {
    "logAnalytics": [
      { "workspaceResourceId": "${WS_ID}", "name": "ghcpUsageWorkspace" }
    ]
  },
  "dataFlows": [
    {
      "streams": [ "Custom-UsageEvent" ],
      "destinations": [ "ghcpUsageWorkspace" ],
      "transformKql": "source",
      "outputStream": "Custom-UsageEvent_CL"
    }
  ]
}
JSON

az monitor data-collection rule create \
  -g "$RG" -n ghcp-usage-dcr -l "$LOCATION" \
  --rule-file /tmp/ghcp-usage-dcr.json --kind Direct
```

### 3. Grant the app identity ingestion rights

```bash
DCR_ID=$(az monitor data-collection rule show -g "$RG" -n ghcp-usage-dcr --query id -o tsv)

# Principal ID of the app's user-assigned managed identity.
PRINCIPAL_ID=$(az containerapp show -g "$RG" -n "$APP" \
  --query "identity.userAssignedIdentities.*.principalId | [0]" -o tsv)

az role assignment create \
  --assignee-object-id "$PRINCIPAL_ID" \
  --assignee-principal-type ServicePrincipal \
  --role "Monitoring Metrics Publisher" \
  --scope "$DCR_ID"
```

### 4. Wire the env vars and restart

```bash
DCE_URI=$(az monitor data-collection endpoint show -g "$RG" -n ghcp-usage-dce \
  --query properties.logsIngestion.endpoint -o tsv)
DCR_IMMUTABLE=$(az monitor data-collection rule show -g "$RG" -n ghcp-usage-dcr \
  --query immutableId -o tsv)

az containerapp update -g "$RG" -n "$APP" --set-env-vars \
  "GHCP_USAGE_AZMON_ENDPOINT=$DCE_URI" \
  "GHCP_USAGE_AZMON_RULE_ID=$DCR_IMMUTABLE" \
  "GHCP_USAGE_AZMON_STREAM=Custom-UsageEvent"
```

Ingestion has a few-minutes latency. After the app serves traffic, confirm rows
arrive:

```bash
WS_GUID=$(az monitor log-analytics workspace show -g "$RG" -n "$LAW" --query customerId -o tsv)

az monitor log-analytics query --workspace "$WS_GUID" --analytics-query \
  "UsageEvent_CL | sort by TimeGenerated desc | take 10" -o table
```

Deleting rows from a Log Analytics custom table uses the management `/purge`
REST API (there is no row-level delete in KQL); the operation is asynchronous.
