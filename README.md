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
Copilot backend uses direct Copilot HTTP for protocol-compatible API calls and
also starts an official `github.com/github/copilot-sdk/go` client per account,
using SDK sessions for simple Chat Completions turns and SDK model discovery
fallbacks.

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

For public deployments, set `GHCP_API_KEY` (or `GHCP_ADMIN_API_KEY`) so the
container does not expose the local-development default key.

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
managed identity.

For Azure Container Apps deployments that keep `GHCP_API_KEY` and
`GHCP_COPILOT_TOKEN` in Azure Key Vault, including a private VNet/Private Link
stack, see
[`docs/deploy-containerapp-keyvault.md`](docs/deploy-containerapp-keyvault.md).

Useful admin controls:

- `GET/PUT /admin/model-aliases` maps friendly client model IDs to backend IDs.
  Requests resolve aliases before routing/cache; responses echo the requested ID
  and include `x-ghcp-upstream-model` when it differs. Updates are persisted to
  `GHCP_MODEL_MAP_PATH` (default `model_map.json`).
- `GET/PUT /admin/rate-limits` configures global and per-account RPM token
  buckets. Exhausted buckets return HTTP 429 with `Retry-After`.

Set `GHCP_BACKEND=copilot` plus `GHCP_COPILOT_TOKEN` to route through the
Go-native Copilot HTTP backend. `GHCP_COPILOT_TOKEN` should be a GitHub OAuth
token; the backend exchanges it for a Copilot API token, caches it until expiry,
detects the Copilot API base URL from the token response, and proxies supported
Copilot endpoints directly from Go.

Docker builds run the official SDK bundler before compiling so the Copilot CLI
runtime is embedded in the gateway binary. Local source builds need Go 1.24+.

Model metadata from `/models` is retained per account, including
`supported_endpoints`. The router uses those capabilities to choose between
Chat Completions, Responses, Anthropic Messages, and embeddings endpoints when a
model does not support the client-requested protocol directly.

Route strategies include `round_robin`, `least_busy`, `weighted`,
`quota_aware`, and `smart`. The `smart`/`quota_aware` scoring considers recent
request volume, recent failures, current in-flight requests, and a decaying
penalty for recent 429/rate-limit events.
