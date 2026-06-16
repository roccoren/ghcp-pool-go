#!/usr/bin/env bash
set -euo pipefail

backend="copilot"
gh_user="roccoren"
port="${GHCP_SMOKE_PORT:-18080}"
host="${GHCP_SMOKE_HOST:-127.0.0.1}"
api_key="${GHCP_SMOKE_API_KEY:-sk-smoke}"
bin="${GHCP_SMOKE_BIN:-./ghcp-pool}"
keep_tmp="${GHCP_SMOKE_KEEP_TMP:-false}"

usage() {
  cat <<'EOF'
Usage: scripts/smoke.sh [--backend fake|copilot] [--gh-user USER] [--port PORT]

Runs reusable smoke tests against a local ghcp-pool-go server:
  - /healthz and /readyz
  - /v1/models
  - /v1/chat/completions
  - /v1/responses text response
  - /v1/responses with web_search tool preserved
  - GET /v1/responses/{id}
  - GET /v1/responses/{id}/input_items
  - DELETE /v1/responses/{id} and follow-up 404
  - POST /v1/responses/{id}/cancel for completed response returns native 400
  - /v1/messages Anthropic response when a Claude model is available

For --backend copilot, the script uses: gh auth token --user USER
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --backend)
      backend="${2:-}"
      shift 2
      ;;
    --gh-user)
      gh_user="${2:-}"
      shift 2
      ;;
    --port)
      port="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ "$backend" != "fake" && "$backend" != "copilot" ]]; then
  echo "--backend must be fake or copilot" >&2
  exit 2
fi

if [[ ! -x "$bin" ]]; then
  if command -v go >/dev/null 2>&1; then
    go build -o ghcp-pool ./cmd/ghcp-pool
    bin="./ghcp-pool"
  else
    echo "binary $bin not found and go is not available to build it" >&2
    exit 1
  fi
fi

tmpdir="$(mktemp -d)"
server_pid=""
cleanup() {
  if [[ -n "$server_pid" ]]; then
    kill "$server_pid" >/dev/null 2>&1 || true
    wait "$server_pid" >/dev/null 2>&1 || true
  fi
  if [[ "$keep_tmp" != "true" ]]; then
    rm -rf "$tmpdir"
  else
    echo "kept smoke artifacts in $tmpdir" >&2
  fi
}
trap cleanup EXIT

cfg="$tmpdir/config.yaml"
cat > "$cfg" <<YAML
backend: ${backend}
gateway:
  host: ${host}
  port: ${port}
  model_refresh_seconds: 300
  route_busy_wait_seconds: 0.1
  cache:
    enabled: false
    ttl_seconds: 600
    max_entries: 100
    salt: smoke
  usage:
    sqlite_path: "${tmpdir}/usage.sqlite"
  api_keys:
    - key: ${api_key}
      scopes: [admin, inference]
      model_allow: ["*"]
      cache_namespace: smoke
accounts:
  - id: acct_smoke
    label: Smoke
    enabled: true
    max_concurrency: 32
    weight: 1
    allow: ["*"]
    token_env: GHCP_COPILOT_TOKEN
    models: [gpt-4.1, gpt-5.5, claude-3.5, claude-sonnet-4.6]
routes:
  - model: "*"
    accounts: [acct_smoke]
    strategy: least_busy
YAML

extra_env=()
if [[ "$backend" == "copilot" ]]; then
  if ! command -v gh >/dev/null 2>&1; then
    echo "gh CLI is required for --backend copilot" >&2
    exit 1
  fi
  token="$(gh auth token --hostname github.com --user "$gh_user")"
  if [[ -z "$token" ]]; then
    echo "gh auth token returned empty token for user $gh_user" >&2
    exit 1
  fi
  extra_env+=(GHCP_COPILOT_TOKEN="$token" GHCP_COPILOT_MODE="${GHCP_COPILOT_MODE:-sdk}")
fi

env \
  GHCP_CONFIG="$cfg" \
  GHCP_BACKEND="$backend" \
  GHCP_HOST="$host" \
  GHCP_PORT="$port" \
  GHCP_MODEL_MAP_PATH="$tmpdir/model-map.json" \
  GHCP_HOME_ROOT="$tmpdir/home" \
  "${extra_env[@]}" \
  "$bin" > "$tmpdir/server.log" 2>&1 &
server_pid=$!

base="http://${host}:${port}"
auth_header="Authorization: Bearer ${api_key}"

for _ in $(seq 1 200); do
  if curl -fsS "$base/healthz" > "$tmpdir/health.json" 2> "$tmpdir/curl.err"; then
    break
  fi
  sleep 0.1
done

if ! curl -fsS "$base/healthz" > "$tmpdir/health.json"; then
  echo "server did not become healthy" >&2
  cat "$tmpdir/server.log" >&2
  exit 1
fi

curl -fsS "$base/readyz" > "$tmpdir/ready.json"
curl -fsS -H "$auth_header" "$base/v1/models" > "$tmpdir/models.json"

python3 - "$tmpdir/models.json" "$backend" > "$tmpdir/selected-models.env" <<'PY'
import json, sys
models = [item["id"] for item in json.load(open(sys.argv[1])).get("data", [])]
backend = sys.argv[2]
preferred_chat = ["gpt-4.1", "gpt-5.5", "gpt-4o-mini", "gpt-5-mini"]
chat = next((m for m in preferred_chat if m in models), None)
if chat is None:
    chat = next((m for m in models if m.startswith("gpt-")), None)
claude = next((m for m in models if m.startswith("claude-")), None)
if chat is None:
    raise SystemExit(f"no GPT chat model found in /v1/models for {backend}: {models}")
print(f"CHAT_MODEL={chat}")
print(f"CLAUDE_MODEL={claude or ''}")
PY
# shellcheck disable=SC1091
source "$tmpdir/selected-models.env"

curl_json() {
  local path="$1"
  local body="$2"
  curl -fsS -H "$auth_header" -H "Content-Type: application/json" "$base$path" -d "$body"
}

curl_json /v1/chat/completions \
  "{\"model\":\"${CHAT_MODEL}\",\"messages\":[{\"role\":\"user\",\"content\":\"smoke chat\"}]}" \
  > "$tmpdir/chat.json"

curl_json /v1/responses \
  "{\"model\":\"${CHAT_MODEL}\",\"input\":\"smoke responses text\",\"store\":true}" \
  > "$tmpdir/response-text.json"
text_id="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$tmpdir/response-text.json")"

curl -fsS -H "$auth_header" "$base/v1/responses/$text_id" > "$tmpdir/response-get.json"
curl -fsS -H "$auth_header" "$base/v1/responses/$text_id/input_items" > "$tmpdir/input-items.json"

curl_json /v1/responses \
  "{\"model\":\"${CHAT_MODEL}\",\"input\":\"smoke responses tool\",\"store\":true,\"tools\":[{\"type\":\"web_search_preview\",\"search_context_size\":\"low\"}]}" \
  > "$tmpdir/response-tool.json"

cancel_code="$(curl -sS -o "$tmpdir/cancel-completed.json" -w '%{http_code}' -X POST -H "$auth_header" "$base/v1/responses/$text_id/cancel")"

curl -fsS -X DELETE -H "$auth_header" "$base/v1/responses/$text_id" > "$tmpdir/delete.json"
deleted_code="$(curl -sS -o "$tmpdir/deleted-get.json" -w '%{http_code}' -H "$auth_header" "$base/v1/responses/$text_id")"

if [[ -n "${CLAUDE_MODEL:-}" ]]; then
  curl_json /v1/messages \
    "{\"model\":\"${CLAUDE_MODEL}\",\"max_tokens\":64,\"messages\":[{\"role\":\"user\",\"content\":\"smoke anthropic\"}]}" \
    > "$tmpdir/anthropic.json"
fi

python3 - "$tmpdir" "$cancel_code" "$deleted_code" "${CLAUDE_MODEL:-}" <<'PY'
import json, sys
root, cancel_code, deleted_code, claude_model = sys.argv[1:5]
def load(name):
    with open(f"{root}/{name}.json") as f:
        return json.load(f)
checks = []
checks.append(("ready", load("ready")["status"] == "ready"))
models = load("models")
checks.append(("models", models["object"] == "list" and len(models["data"]) > 0))
chat = load("chat")
checks.append(("chat", chat["object"] == "chat.completion" and len(chat["choices"]) == 1))
text = load("response-text")
checks.append(("responses_text", text["object"] == "response" and text["status"] == "completed" and ("smoke responses text" in text.get("output_text", "") or len(text.get("output", [])) > 0)))
got = load("response-get")
checks.append(("responses_get", got["id"] == text["id"]))
items = load("input-items")
checks.append(("input_items", items["object"] == "list" and len(items["data"]) >= 1))
tool = load("response-tool")
checks.append(("responses_tool", tool["object"] == "response" and tool["status"] == "completed" and len(tool.get("output", [])) >= 1))
checks.append(("web_search_tool_preserved", tool["tools"][0]["type"] == "web_search_preview"))
cancel = load("cancel-completed")
checks.append(("cancel_completed_400", cancel_code == "400" and cancel.get("error", {}).get("type") == "invalid_request_error"))
deleted = load("delete")
checks.append(("responses_delete", deleted.get("deleted") is True))
checks.append(("responses_deleted_404", deleted_code == "404"))
if claude_model:
    msg = load("anthropic")
    checks.append(("anthropic", msg["type"] == "message" and len(msg.get("content", [])) >= 1))
failed = [name for name, ok in checks if not ok]
if failed:
    raise SystemExit("failed smoke checks: " + ", ".join(failed))
print("smoke checks passed:", ", ".join(name for name, _ in checks))
PY

