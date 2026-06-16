#!/usr/bin/env bash
set -euo pipefail

gh_user="roccoren"
base_port="${GHCP_FOCUSED_SMOKE_PORT:-18180}"
host="${GHCP_SMOKE_HOST:-127.0.0.1}"
api_key="${GHCP_SMOKE_API_KEY:-sk-focus}"
bin="${GHCP_SMOKE_BIN:-./ghcp-pool}"
keep_tmp="${GHCP_SMOKE_KEEP_TMP:-false}"

usage() {
  cat <<'EOF'
Usage: scripts/focused-smoke.sh [--gh-user USER] [--port PORT]

Runs three focused Copilot-backend smoke checks:
  1. Web Search: Copilot SDK mode with CLI-mode web_search session, /v1/responses web_search request.
  2. Tool Use: Copilot SDK mode, forced custom function tool_call.
  3. Shell: Copilot SDK mode, builtin bash shell tool_call is surfaced.

The script uses: gh auth token --hostname github.com --user USER
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --gh-user)
      gh_user="${2:-}"
      shift 2
      ;;
    --port)
      base_port="${2:-}"
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

if [[ ! -x "$bin" ]]; then
  if command -v go >/dev/null 2>&1; then
    go build -o ghcp-pool ./cmd/ghcp-pool
    bin="./ghcp-pool"
  else
    echo "binary $bin not found and go is not available to build it" >&2
    exit 1
  fi
fi

if ! command -v gh >/dev/null 2>&1; then
  echo "gh CLI is required" >&2
  exit 1
fi
token="$(gh auth token --hostname github.com --user "$gh_user")"
if [[ -z "$token" ]]; then
  echo "gh auth token returned empty token for user $gh_user" >&2
  exit 1
fi

tmpdir="$(mktemp -d)"
pids=()
cleanup() {
  for pid in "${pids[@]:-}"; do
    kill "$pid" >/dev/null 2>&1 || true
    wait "$pid" >/dev/null 2>&1 || true
  done
  if [[ "$keep_tmp" == "true" ]]; then
    echo "kept focused smoke artifacts in $tmpdir" >&2
  else
    rm -rf "$tmpdir"
  fi
}
trap cleanup EXIT

write_config() {
  local dir="$1"
  local port="$2"
  local key="$3"
  cat > "$dir/config.yaml" <<YAML
backend: copilot
gateway:
  host: ${host}
  port: ${port}
  model_refresh_seconds: 300
  route_busy_wait_seconds: 0.1
  cache:
    enabled: false
  usage:
    sqlite_path: "${dir}/usage.sqlite"
  api_keys:
    - key: ${key}
      scopes: [admin, inference]
      model_allow: ["*"]
      cache_namespace: focus
accounts:
  - id: acct_focus
    label: Focus
    enabled: true
    max_concurrency: 32
    weight: 1
    allow: ["*"]
    token_env: GHCP_COPILOT_TOKEN
routes:
  - model: "*"
    accounts: [acct_focus]
    strategy: least_busy
YAML
}

start_server() {
  local name="$1"
  local port="$2"
  local mode="$3"
  local tools="${4:-}"
  local dir="$tmpdir/$name"
  mkdir -p "$dir"
  write_config "$dir" "$port" "$api_key"
  local -a envs=(
    GHCP_CONFIG="$dir/config.yaml"
    GHCP_BACKEND=copilot
    GHCP_COPILOT_MODE="$mode"
    GHCP_COPILOT_TOKEN="$token"
    GHCP_HOME_ROOT="$dir/home"
    GHCP_MODEL_MAP_PATH="$dir/model-map.json"
  )
  if [[ "$mode" == "opencode" ]]; then
    envs+=(GHCP_COPILOT_AUTH_MODE=oauth)
  fi
  if [[ "$name" == "web" ]]; then
    envs+=(GHCP_COPILOT_SDK_WEB_SEARCH_MODE=cli)
  fi
  if [[ -n "$tools" ]]; then
    envs+=(GHCP_COPILOT_SDK_AVAILABLE_TOOLS="$tools")
  fi
  env "${envs[@]}" "$bin" > "$dir/server.log" 2>&1 &
  pids+=("$!")
  local base="http://${host}:${port}"
  for _ in $(seq 1 200); do
    if curl -fsS "$base/readyz" > "$dir/ready.json" 2> "$dir/curl.err"; then
      echo "$base"
      return 0
    fi
    sleep 0.1
  done
  cat "$dir/server.log" >&2
  return 1
}

select_models() {
  local base="$1"
  local out="$2"
  curl -fsS -H "Authorization: Bearer ${api_key}" "$base/v1/models" > "$out/models.json"
  python3 - "$out/models.json" > "$out/models.env" <<'PY'
import json, sys
models = [item["id"] for item in json.load(open(sys.argv[1])).get("data", [])]
chat = next((m for m in ["gpt-5.5", "gpt-5.4", "gpt-5-mini"] if m in models), None)
if chat is None:
    chat = next((m for m in models if m.startswith("gpt-")), None)
if chat is None:
    raise SystemExit(f"no GPT model available: {models}")
print(f"CHAT_MODEL={chat}")
PY
}

auth_header="Authorization: Bearer ${api_key}"
web_port="$base_port"
tool_port="$((base_port + 1))"
shell_port="$((base_port + 2))"

web_base="$(start_server web "$web_port" sdk)"
select_models "$web_base" "$tmpdir/web"
source "$tmpdir/web/models.env"
cat > "$tmpdir/web/request.json" <<JSON
{"model":"${CHAT_MODEL}","input":"Use web search to answer in one sentence: what is the current headline on github.blog? Include WEB_SEARCH_DONE in your final answer.","tools":[{"type":"web_search_preview","search_context_size":"low"}],"tool_choice":"required","max_output_tokens":250}
JSON
curl --max-time 150 -fsS -H "$auth_header" -H "Content-Type: application/json" "$web_base/v1/responses" --data @"$tmpdir/web/request.json" > "$tmpdir/web/response.json"
python3 - "$tmpdir/web/response.json" <<'PY'
import json, sys
body = json.load(open(sys.argv[1]))
if not any(item.get("type") == "web_search_call" for item in body.get("output", [])):
    raise SystemExit("web search smoke did not produce web_search_call")
if "WEB_SEARCH_DONE" not in body.get("output_text", ""):
    raise SystemExit("web search smoke missing sentinel")
if "no executable web" in body.get("output_text", "").lower() or "no web search tool" in body.get("output_text", "").lower():
    raise SystemExit("web search smoke reported unavailable web tooling")
print("WEB_SEARCH_OK", body["output_text"][:200])
PY

tool_base="$(start_server tool "$tool_port" sdk)"
select_models "$tool_base" "$tmpdir/tool"
source "$tmpdir/tool/models.env"
cat > "$tmpdir/tool/request.json" <<JSON
{"model":"${CHAT_MODEL}","messages":[{"role":"user","content":"Use the get_weather tool for Paris. Do not answer directly."}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],"tool_choice":{"type":"function","function":{"name":"get_weather"}}}
JSON
curl --max-time 120 -fsS -H "$auth_header" -H "Content-Type: application/json" "$tool_base/v1/chat/completions" --data @"$tmpdir/tool/request.json" > "$tmpdir/tool/response.json"
python3 - "$tmpdir/tool/response.json" <<'PY'
import json, sys
body = json.load(open(sys.argv[1]))
choice = body["choices"][0]
calls = choice["message"].get("tool_calls") or []
if choice.get("finish_reason") != "tool_calls" or not calls or calls[0]["function"]["name"] != "get_weather":
    raise SystemExit("tool use smoke did not produce get_weather tool_call")
print("TOOL_USE_OK", calls[0]["function"]["arguments"])
PY

shell_base="$(start_server shell "$shell_port" sdk builtin:bash)"
select_models "$shell_base" "$tmpdir/shell"
source "$tmpdir/shell/models.env"
cat > "$tmpdir/shell/request.json" <<JSON
{"model":"${CHAT_MODEL}","messages":[{"role":"user","content":"Run exactly this shell command with bash: printf GHCP_SHELL_OK. Return the shell tool call; do not answer directly."}],"max_tokens":200}
JSON
curl --max-time 120 -fsS -H "$auth_header" -H "Content-Type: application/json" "$shell_base/v1/chat/completions" --data @"$tmpdir/shell/request.json" > "$tmpdir/shell/response.json"
python3 - "$tmpdir/shell/response.json" <<'PY'
import json, sys
body = json.load(open(sys.argv[1]))
choice = body["choices"][0]
calls = choice["message"].get("tool_calls") or []
if choice.get("finish_reason") != "tool_calls" or not calls or calls[0]["function"]["name"] != "bash":
    raise SystemExit("shell smoke did not produce bash tool_call")
if "GHCP_SHELL_OK" not in calls[0]["function"].get("arguments", ""):
    raise SystemExit("shell smoke missing command sentinel")
print("SHELL_OK", calls[0]["function"]["arguments"])
PY
