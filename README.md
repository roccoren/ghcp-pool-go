# ghcp-pool-go

Go rewrite of `ghcp-pool`, optimized for high-volume concurrent gateway traffic.

## Why Go

Go is a better fit than the current Python/FastAPI implementation for large
batch, high-concurrency gateway workloads because goroutines are cheap, the
standard HTTP server is highly concurrent, binaries deploy as a single artifact,
and there is no interpreter GIL limiting CPU-bound request coordination.

The rewrite keeps the same core contract:

- OpenAI Chat Completions: `POST /v1/chat/completions`
- OpenAI Responses API for `gpt*`: `POST /v1/responses`
- Anthropic Messages API for `claude*`: `POST /v1/messages`
- pooled accounts, routing, model registry, exact-match cache, usage accounting,
  admin routes, user management, debug capture, health and metrics endpoints

The offline `fake` backend is fully implemented for local testing. The backend
interface is isolated so a real Copilot SDK adapter can be added behind the same
pool/router/cache/meter pipeline.

## Run

```bash
go test ./...
go run ./cmd/ghcp-pool

curl -s localhost:8000/healthz
curl -s localhost:8000/v1/chat/completions \
  -H 'Authorization: Bearer sk-local-dev' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}]}'
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
`GHCP_BACKEND`, `GHCP_CACHE_SALT`, and `GHCP_USAGE_SQLITE_PATH`.
