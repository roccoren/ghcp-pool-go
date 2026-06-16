# ghcp-pool-go

Go rewrite of `ghcp-pool`, optimized for high-volume concurrent gateway traffic.

## Why Go

Go is a better fit than the current Python/FastAPI implementation for large
batch, high-concurrency gateway workloads because goroutines are cheap, the
standard HTTP server is highly concurrent, binaries deploy as a single artifact,
and there is no interpreter GIL limiting CPU-bound request coordination.

The rewrite keeps the same core contract:

- OpenAI Chat Completions: `POST /v1/chat/completions`
- OpenAI Responses API: `POST /v1/responses`
- Anthropic Messages API for `claude*`: `POST /v1/messages`
- Anthropic token count estimates: `POST /v1/messages/count_tokens`
- Gemini-compatible API: `GET /v1beta/models`,
  `POST /v1beta/models/{model}:generateContent`,
  `POST /v1beta/models/{model}:streamGenerateContent`, and
  `POST /v1beta/models/{model}:countTokens`
- OpenAI-compatible embeddings: `POST /v1/embeddings`
- pooled accounts, routing, model registry, exact-match cache, usage accounting,
  admin routes, user management, debug capture, model aliases, RPM rate limits,
  health and metrics endpoints

The offline `fake` backend is fully implemented for local testing. The real
Copilot backend starts an official `github.com/github/copilot-sdk/go` client per
account, using SDK authentication and SDK model discovery in the default mode.
OpenCode mode is available when direct Copilot provider API behavior is needed.

## Run

```bash
go test ./...
go run ./cmd/ghcp-pool

curl -s localhost:8000/healthz
curl -s localhost:8000/v1/chat/completions \
  -H 'Authorization: Bearer sk-local-dev' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}]}'

curl -s localhost:8000/v1/embeddings \
  -H 'Authorization: Bearer sk-local-dev' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4.1","input":["hi"],"dimensions":8}'

curl -s localhost:8000/v1beta/models/gpt-4.1:generateContent \
  -H 'x-goog-api-key: sk-local-dev' \
  -H 'Content-Type: application/json' \
  -d '{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}'
```

Configuration defaults match the Python project: without `config.yaml`, the
server starts on port 8000 with one fake account exposing `gpt-4.1` and
`gpt-4o-mini`, and API key `sk-local-dev`.

For public deployments, set `GHCP_API_KEY`/`GHCP_API_KEYS` (or
`GHCP_ADMIN_API_KEY`/`GHCP_ADMIN_API_KEYS`) so the container does not expose the
local-development default key. Multiple keys can be separated with commas,
spaces, tabs, or newlines. YAML config can also list multiple
`gateway.api_keys` entries with separate scopes and model allow-lists.

## Container

```bash
docker build -t ghcp-pool-go .
docker run --rm -p 8000:8000 -e GHCP_API_KEY=sk-change-me ghcp-pool-go
```

Supported deployment environment overrides include `GHCP_HOST`, `GHCP_PORT`,
`GHCP_BACKEND`, `GHCP_CACHE_SALT`, `GHCP_USAGE_SQLITE_PATH`,
`GHCP_MODEL_MAP_PATH`, `GHCP_GLOBAL_RATE_LIMIT_RPM`, and
`GHCP_PER_ACCOUNT_RATE_LIMIT_RPM`. For Azure Key Vault-backed deployments, set
`AZURE_KEY_VAULT_URL` plus `GHCP_API_KEY_KEY_VAULT_SECRET` and/or
`GHCP_COPILOT_TOKEN_KEY_VAULT_SECRET` to resolve secrets at runtime through
managed identity. API-key secrets may contain one key or a comma/whitespace
separated list.

For Azure Container Apps deployments that keep `GHCP_API_KEY` and
`GHCP_COPILOT_TOKEN` in Azure Key Vault, including the minimum Azure resource
list and a private VNet/Private Link stack, see
[`docs/deploy-containerapp-keyvault.md`](docs/deploy-containerapp-keyvault.md).

Useful admin controls:

- [`docs/project-overview-cn.md`](docs/project-overview-cn.md) is a Chinese
  user-facing project overview covering SDK/API advantages and simple deployment
  paths.
- [`docs/usage-guide-cn.md`](docs/usage-guide-cn.md) is a Chinese quick usage
  guide with OpenAI, Anthropic, Gemini, embeddings, and admin examples.
- [`docs/admin-operations.md`](docs/admin-operations.md) has copy/paste
  workflows for managing Copilot users, viewing admin status/usage, and managing
  models.
- `GET/PUT /admin/model-aliases` maps friendly client model IDs to backend IDs.
  Requests resolve aliases before routing/cache; responses echo the requested ID
  and include `x-ghcp-upstream-model` when it differs. Updates are persisted to
  `GHCP_MODEL_MAP_PATH` (default `model_map.json`).
- `GET/PUT /admin/rate-limits` configures global and per-account RPM token
  buckets. Exhausted buckets return HTTP 429 with `Retry-After`.

Set `GHCP_BACKEND=copilot` plus `GHCP_COPILOT_TOKEN` to route through the
Copilot backend. `GHCP_COPILOT_TOKEN` should be a GitHub OAuth token. In the
default SDK mode, the token is passed to the SDK runtime and model discovery is
performed by the SDK rather than by the direct Copilot API.

Choose the Copilot implementation with `GHCP_COPILOT_MODE` or
`gateway.copilot.mode`:

- `sdk` (default): official SDK behavior for authentication, model discovery,
  and eligible simple chat turns. This mode never calls direct Copilot APIs; SDK
  unsupported request shapes return an error instead of falling back to HTTP.
- `opencode`: OpenCode-inspired direct provider behavior. This defaults
  `auth_mode` to `oauth` and sends the GitHub OAuth token directly to
  `https://api.githubcopilot.com` unless another Copilot API base URL is set.

SDK mode runs the Copilot client in multi-tenant-safe empty mode, so no SDK
built-in tools are exposed by default. Set `GHCP_COPILOT_SDK_AVAILABLE_TOOLS`
or `gateway.copilot.sdk_available_tools` for explicit allowlists. For SDK-only
web search without direct Copilot API calls, set
`GHCP_COPILOT_SDK_WEB_SEARCH_MODE=cli` or
`gateway.copilot.sdk_web_search_mode: cli`; web-search requests then use a
temporary Copilot CLI-mode SDK client with controlled web handlers.

Models that support a long-context tier, such as Copilot-exposed Claude Opus
variants, can be pinned with request field `context_tier: "long_context"` or
configured by pattern:

```yaml
gateway:
  context_tiers:
    "claude-opus-4.8": long_context
```

For GitHub Enterprise, set `GHCP_GITHUB_ENTERPRISE_URL=<enterprise-domain>`;
the gateway derives `https://copilot-api.<enterprise-domain>` plus enterprise
device-login URLs. The admin device-login flow defaults to the same public
Copilot OAuth client ID used by OpenCode and can be overridden with
`GHCP_LOGIN_CLIENT_ID` or `gateway.login.client_id`.

Docker builds run the official SDK bundler before compiling so the Copilot CLI
runtime is embedded in the gateway binary. Local source builds need Go 1.24+.

Model metadata from SDK discovery is retained per account. In OpenCode mode,
direct `/models` metadata such as `supported_endpoints` and
`model_picker_enabled` is also retained. The router uses those capabilities to
choose between Chat Completions, Responses, Anthropic Messages, and embeddings
endpoints when a model does not support the client-requested protocol directly;
non-picker utility models remain routable but are not advertised from
`/v1/models`.

Use OpenCode mode for direct API-compatible features such as native Responses,
Anthropic Messages, embeddings, or request parameters that the SDK simple-chat
path does not support.

Route strategies include `round_robin`, `least_busy`, `weighted`,
`quota_aware`, and `smart`. The `smart`/`quota_aware` scoring considers recent
request volume, recent failures, current in-flight requests, and a decaying
penalty for recent 429/rate-limit events.
