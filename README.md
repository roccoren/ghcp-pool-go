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

### Published image (GitHub Container Registry)

The [`Build and Publish Container Image`](.github/workflows/docker-publish.yml)
workflow builds the container on every push to `main` and every `v*` tag, then
publishes it to GitHub Container Registry at
`ghcr.io/roccoren/ghcp-pool-go`. Image tags include `latest` (default branch),
the branch name, the commit SHA, and semantic-version tags for releases.

```bash
docker pull ghcr.io/roccoren/ghcp-pool-go:latest
docker run --rm -p 8000:8000 -e GHCP_API_KEY=sk-change-me \
  ghcr.io/roccoren/ghcp-pool-go:latest
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

Alternatively, set `GHCP_BACKEND=copilot-cli` to use the dedicated CLI backend.
This backend uses the Copilot SDK exclusively in `ModeCopilotCli` mode, providing
maximum compatibility with Copilot CLI features. The CLI backend is a
simplified alternative to the full `copilot` backend that always runs in CLI
mode without OpenCode fallback. Configuration options like
`GHCP_COPILOT_SDK_WEB_SEARCH_MODE` and `GHCP_COPILOT_SDK_AVAILABLE_TOOLS` are
supported.

SDK mode runs the Copilot client in multi-tenant-safe empty mode, so no SDK
built-in tools are exposed by default. Set `GHCP_COPILOT_SDK_AVAILABLE_TOOLS`
or `gateway.copilot.sdk_available_tools` for explicit allowlists. For SDK-only
web search without direct Copilot API calls, set
`GHCP_COPILOT_SDK_WEB_SEARCH_MODE=cli` or
`gateway.copilot.sdk_web_search_mode: cli`; web-search requests then use a
temporary Copilot CLI-mode SDK client with controlled `ghcp_web_search` and
`ghcp_web_fetch` handlers. To experiment with Copilot CLI native search instead,
set `GHCP_COPILOT_SDK_WEB_SEARCH_MODE=native_cli`; this exposes native
`web_search` through the CLI-mode SDK client, while `web_fetch` is handled by
the gateway so GitHub blob/raw URLs can be normalized before fetching.

### Reasoning Effort and Context Tiers

High-capability models automatically get optimal defaults for reasoning effort
and context tier. These expose their 1M+ token context windows and advanced
reasoning capabilities without any configuration required.

**Automatic Defaults:**
- **Claude Opus 4.x**: `reasoning_effort: high`, `context_tier: long_context` (premium quality)
- **Claude Sonnet 4.6+**: `reasoning_effort: medium`, `context_tier: long_context` (balanced)
- **GPT-5.x, GPT-6.x**: `reasoning_effort: medium`, `context_tier: long_context` (balanced)
- **Gemini 3.x/4.x**: `context_tier: long_context` (context only, no reasoning effort)

**Full Reasoning Effort Range:**
Choose from `none`, `low`, `medium`, `high`, `xhigh` (extra high), `max`:
- `low` - Fast, basic reasoning for simple tasks
- `medium` - Balanced performance and quality (default for most models)
- `high` - Enhanced reasoning for complex problems (default for Opus)
- `xhigh` - Extra high reasoning for very difficult tasks
- `max` - Maximum reasoning capability for the most challenging problems

**Override via Config:**

```yaml
gateway:
  reasoning_efforts:
    "claude-opus-4.*": max         # Maximum reasoning for Opus
    "claude-sonnet-4.6": high      # Upgrade Sonnet from default "medium"
    "gpt-5.5": low                 # Faster responses for specific model
  context_tiers:
    "claude-opus-4.8": long_context  # Already defaulted, shown for reference
    "gpt-5.*": default               # Override to use default context
```

**Override per Request:**

```bash
# Simple task with low reasoning (faster)
curl -X POST http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer $GHCP_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4.6",
    "messages": [{"role": "user", "content": "What is 2+2?"}],
    "reasoning_effort": "low"
  }'

# Complex analysis with maximum reasoning
curl -X POST http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer $GHCP_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-opus-4.8",
    "messages": [{"role": "user", "content": "Design a distributed system..."}],
    "reasoning_effort": "max",
    "context_tier": "long_context"
  }'
```

Valid `context_tier` values: `default`, `long_context`.

`/v1/models` adapts to the client protocol. OpenAI-style requests get
compatibility fields such as `context_window`, `max_context_window_tokens`,
`supported_reasoning_efforts`, and nested `capabilities.limits` /
`capabilities.supports`. Anthropic-style requests (for example, requests to
`/models`, requests with `anthropic-version` or `x-api-key`, or Claude/Anthropic
user agents) get the official Anthropic Models API shape: `type`,
`display_name`, `created_at`, `max_input_tokens`, `max_tokens`, and
`capabilities.effort.{low,medium,high,xhigh,max}.supported`. Anthropic-style
responses advertise the official dash-style model IDs (for example
`claude-opus-4-8`) so Claude clients recognize them; OpenAI-style responses keep
the dot-style IDs (`claude-opus-4.8`). Both forms resolve to the same upstream
model on input. The model list only advertises real upstream model IDs plus
aliases explicitly configured in `gateway.model_aliases`; long context is
represented by `max_input_tokens` and `context_tier`, not by extra generated
model IDs.

### Using with Claude Code / Claude Desktop (Anthropic clients)

Point the client at the gateway with the standard Anthropic settings (env vars
for the Claude Code CLI, or the managed config for the Claude Desktop app):

```bash
export ANTHROPIC_BASE_URL=http://localhost:8000   # your gateway URL
export ANTHROPIC_AUTH_TOKEN=$GHCP_API_KEY         # sent as Authorization: Bearer
```

Important asymmetry: **reasoning effort is an advertisable capability** that
clients read from `/v1/models` (`capabilities.effort`), but the **1M context
window is not discovered from the gateway at all** — clients treat it as a
client-side opt-in. That is why effort can appear automatically while the 1M
window stays hidden until you enable it in the client. The gateway already
returns `max_input_tokens: 1000000` and honors the
`anthropic-beta: context-1m-2025-08-07` header (and the `[1m]` model suffix) by
routing to the upstream long-context tier; the missing piece is the client-side
toggle below.

**Reasoning effort (both clients).** Use `/effort low|medium|high|xhigh|max` in
the CLI, or the effort control in Desktop. The client sends
`output_config: {effort: "..."}`, which the gateway maps to the upstream
`reasoning_effort`. The option appears for models the client recognizes as
effort-capable (Opus 4.6+/4.7/4.8, Sonnet 4.6+), keyed by the official dash-style
ID, or when declared with
`ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES=effort,xhigh_effort,max_effort,thinking,adaptive_thinking,interleaved_thinking`.

**1M context window — Claude Code CLI.** Select a `[1m]` variant, most reliably
by pinning the alias:

```bash
export ANTHROPIC_DEFAULT_OPUS_MODEL='claude-opus-4-8[1m]'
export ANTHROPIC_DEFAULT_SONNET_MODEL='claude-sonnet-4-6[1m]'
```

or pick it live with `/model opus[1m]` (or type the full name
`/model claude-opus-4-8[1m]` even if it is not listed). Claude Code strips the
`[1m]` suffix and adds the `context-1m-2025-08-07` beta; the gateway honors it.
`CLAUDE_CODE_DISABLE_1M_CONTEXT=1` hides it.

**1M context window — Claude Desktop (Cowork on 3P).** Desktop never probes the
gateway for 1M support; you assert it per model with `supports1m: true` in the
`inferenceModels` managed configuration. The `name` **must be the exact ID the
gateway's `/v1/models` returns** — which for Anthropic clients is the dash-style
ID (for example `claude-opus-4-8`):

```json
"inferenceModels": [
  { "name": "claude-opus-4-8",  "supports1m": true, "anthropicFamilyTier": "opus",   "isFamilyDefault": true },
  { "name": "claude-sonnet-4-6", "supports1m": true, "anthropicFamilyTier": "sonnet" },
  { "name": "claude-haiku-4-5",  "anthropicFamilyTier": "haiku" }
]
```

Set this via **Developer → Configure third-party inference** in the app, or in
the exported `.mobileconfig` / registry policy, then fully quit and reopen
Desktop. A separate 1M picker entry then appears for each `supports1m` model.

**Populate the picker from the gateway.** The Claude Desktop app auto-discovers
models from `/v1/models` by default; the Claude Code CLI needs
`export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1` (v2.1.129+). Either way the
gateway already exposes dash-style IDs with full `capabilities`; discovery does
not synthesize the 1M variant, so still apply the CLI `[1m]` pin or the Desktop
`supports1m` entry above.

The gateway also stubs `POST /api/event_logging/batch` (returns
`{"status":"ok"}`) so Claude Code telemetry posts do not error.

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
