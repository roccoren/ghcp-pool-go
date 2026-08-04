#!/usr/bin/env bash
# End-to-end validation of structured output (response_format) against a live
# Copilot backend in SDK mode.
#
# Verifies the four-layer contract:
#   1. json_schema is delivered via the synthetic ghcp_structured_output tool
#   2. fenced / prose-wrapped output is unwrapped
#   3. output is validated against the caller schema
#   4. non-conforming output is repaired or fails explicitly (never a 200 with prose)
#
# Usage: scripts/structured-output-smoke.sh [--gh-user USER] [--port PORT] [--model MODEL]
set -euo pipefail

gh_user="${GHCP_SMOKE_GH_USER:-roccoren}"
port="${GHCP_STRUCTURED_SMOKE_PORT:-18190}"
host="${GHCP_SMOKE_HOST:-127.0.0.1}"
api_key="${GHCP_SMOKE_API_KEY:-sk-structured}"
model="${GHCP_SMOKE_MODEL:-gpt-5-mini}"
mode="${GHCP_COPILOT_MODE:-sdk}"
backend="${GHCP_SMOKE_BACKEND:-copilot}"
bin="${GHCP_SMOKE_BIN:-./ghcp-pool}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --gh-user) gh_user="${2:-}"; shift 2 ;;
    --port) port="${2:-}"; shift 2 ;;
    --model) model="${2:-}"; shift 2 ;;
    --mode) mode="${2:-}"; shift 2 ;;
    --backend) backend="${2:-}"; shift 2 ;;
    -h|--help) sed -n '2,10p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

command -v gh >/dev/null 2>&1 || { echo "gh CLI is required" >&2; exit 1; }
token="$(gh auth token --hostname github.com --user "$gh_user")"
[[ -n "$token" ]] || { echo "empty token for $gh_user" >&2; exit 1; }

if [[ ! -x "$bin" ]]; then
  go build -o ghcp-pool ./cmd/ghcp-pool
  bin="./ghcp-pool"
fi

tmpdir="$(mktemp -d)"
server_pid=""
cleanup() {
  [[ -n "$server_pid" ]] && { kill "$server_pid" >/dev/null 2>&1 || true; wait "$server_pid" >/dev/null 2>&1 || true; }
  [[ "${GHCP_SMOKE_KEEP_TMP:-false}" == "true" ]] && echo "artifacts kept in $tmpdir" >&2 || rm -rf "$tmpdir"
}
trap cleanup EXIT

cat > "$tmpdir/config.yaml" <<YAML
backend: ${backend}
gateway:
  host: ${host}
  port: ${port}
  model_refresh_seconds: 300
  cache:
    enabled: false
  usage:
    sqlite_path: "${tmpdir}/usage.sqlite"
  api_keys:
    - key: ${api_key}
      scopes: [admin, inference]
      model_allow: ["*"]
accounts:
  - id: acct_structured
    label: Structured
    enabled: true
    max_concurrency: 8
    token_env: GHCP_COPILOT_TOKEN
routes:
  - model: "*"
    accounts: [acct_structured]
    strategy: least_busy
YAML

echo "starting gateway (backend=${backend} mode=${mode} model=${model}) on ${host}:${port}"
env GHCP_CONFIG="$tmpdir/config.yaml" \
    GHCP_BACKEND="$backend" \
    GHCP_COPILOT_MODE="$mode" \
    GHCP_COPILOT_TOKEN="$token" \
    GHCP_HOME_ROOT="$tmpdir/home" \
    GHCP_MODEL_MAP_PATH="$tmpdir/model-map.json" \
    "$bin" > "$tmpdir/server.log" 2>&1 &
server_pid=$!

for _ in $(seq 1 60); do
  if curl -fsS "http://${host}:${port}/healthz" >/dev/null 2>&1; then break; fi
  if ! kill -0 "$server_pid" 2>/dev/null; then
    echo "server died during startup:" >&2; tail -40 "$tmpdir/server.log" >&2; exit 1
  fi
  sleep 1
done
curl -fsS "http://${host}:${port}/healthz" >/dev/null || { echo "gateway never became healthy" >&2; tail -40 "$tmpdir/server.log" >&2; exit 1; }

pass=0; fail=0
call() {
  curl -sS -m 180 -X POST "http://${host}:${port}$1" \
    -H "Authorization: Bearer ${api_key}" \
    -H 'Content-Type: application/json' \
    -d "$2"
}
report() {
  if [[ "$1" == "pass" ]]; then pass=$((pass+1)); printf '  \033[32mPASS\033[0m %s\n' "$2"
  else fail=$((fail+1)); printf '  \033[31mFAIL\033[0m %s\n     %s\n' "$2" "${3:-}"; fi
}

echo
echo "--- 1. json_schema: strict object, must conform exactly ---"
resp="$(call /v1/chat/completions "$(cat <<JSON
{"model":"${model}",
 "messages":[{"role":"user","content":"Ada Lovelace was a mathematician born in 1815 in London."}],
 "response_format":{"type":"json_schema","json_schema":{"name":"person","strict":true,
   "schema":{"type":"object",
     "properties":{"name":{"type":"string"},"birth_year":{"type":"integer"},"city":{"type":"string"}},
     "required":["name","birth_year","city"],"additionalProperties":false}}}}
JSON
)")"
content="$(printf '%s' "$resp" | python3 -c 'import sys,json;print(json.load(sys.stdin)["choices"][0]["message"]["content"] or "")' 2>/dev/null || echo '')"
echo "     content: ${content:0:200}"
if printf '%s' "$content" | python3 -c '
import sys, json
d = json.load(sys.stdin)
assert isinstance(d, dict), "not an object"
assert set(d) == {"name","birth_year","city"}, "keys=" + repr(sorted(d))
assert isinstance(d["birth_year"], int), "birth_year not integer"
' 2>"$tmpdir/e1"; then report pass "schema-conforming object returned"
else report fail "schema-conforming object returned" "$(cat "$tmpdir/e1"); raw=${resp:0:300}"; fi

if printf '%s' "$resp" | python3 -c '
import sys,json; d=json.load(sys.stdin)
assert not d["choices"][0]["message"].get("tool_calls"), "synthetic tool call leaked to client"
' 2>"$tmpdir/e2"; then report pass "synthetic tool call hidden from client"
else report fail "synthetic tool call hidden from client" "$(cat "$tmpdir/e2")"; fi

if printf '%s' "$resp" | python3 -c '
import sys,json; u=json.load(sys.stdin)["usage"]
assert u.get("completion_tokens",0) > 0, "no output tokens billed: " + repr(u)
' 2>"$tmpdir/e3"; then report pass "output tokens billed"
else report fail "output tokens billed" "$(cat "$tmpdir/e3")"; fi

echo
echo "--- 2. json_object: must be a bare JSON object, no fences ---"
resp="$(call /v1/chat/completions "$(cat <<JSON
{"model":"${model}",
 "messages":[{"role":"user","content":"Give me a JSON object with keys a and b set to 1 and 2."}],
 "response_format":{"type":"json_object"}}
JSON
)")"
content="$(printf '%s' "$resp" | python3 -c 'import sys,json;print(json.load(sys.stdin)["choices"][0]["message"]["content"] or "")' 2>/dev/null || echo '')"
echo "     content: ${content:0:200}"
if printf '%s' "$content" | python3 -c '
import sys,json
raw = sys.stdin.read()
assert not raw.strip().startswith("```"), "content still fenced"
d = json.loads(raw)
assert isinstance(d, dict), "not a top-level object"
' 2>"$tmpdir/e4"; then report pass "unfenced top-level JSON object"
else report fail "unfenced top-level JSON object" "$(cat "$tmpdir/e4")"; fi

echo
echo "--- 3. nested schema with enum + array ---"
resp="$(call /v1/chat/completions "$(cat <<JSON
{"model":"${model}",
 "messages":[{"role":"user","content":"Summarize: the deploy failed twice on Tuesday due to an expired TLS cert."}],
 "response_format":{"type":"json_schema","json_schema":{"name":"incident","strict":true,
   "schema":{"type":"object",
     "properties":{"severity":{"type":"string","enum":["low","medium","high"]},
                   "causes":{"type":"array","items":{"type":"string"}},
                   "occurrences":{"type":"integer"}},
     "required":["severity","causes","occurrences"],"additionalProperties":false}}}}
JSON
)")"
content="$(printf '%s' "$resp" | python3 -c 'import sys,json;print(json.load(sys.stdin)["choices"][0]["message"]["content"] or "")' 2>/dev/null || echo '')"
echo "     content: ${content:0:250}"
if printf '%s' "$content" | python3 -c '
import sys,json
d=json.load(sys.stdin)
assert d["severity"] in ("low","medium","high"), "enum violated: " + repr(d.get("severity"))
assert isinstance(d["causes"], list), "causes not array"
assert isinstance(d["occurrences"], int), "occurrences not integer"
' 2>"$tmpdir/e5"; then report pass "nested enum+array schema honored"
else report fail "nested enum+array schema honored" "$(cat "$tmpdir/e5"); raw=${resp:0:300}"; fi

echo
echo "--- 4. streaming with json_schema ---"
stream="$(curl -sS -m 180 -N -X POST "http://${host}:${port}/v1/chat/completions" \
  -H "Authorization: Bearer ${api_key}" -H 'Content-Type: application/json' \
  -d "$(cat <<JSON
{"model":"${model}","stream":true,
 "messages":[{"role":"user","content":"Marie Curie, physicist, born 1867 in Warsaw."}],
 "response_format":{"type":"json_schema","json_schema":{"name":"person","strict":true,
   "schema":{"type":"object","properties":{"name":{"type":"string"},"birth_year":{"type":"integer"}},
     "required":["name","birth_year"],"additionalProperties":false}}}}
JSON
)")"
assembled="$(printf '%s' "$stream" | python3 -c '
import sys, json
out=[]
for line in sys.stdin:
    line=line.strip()
    if not line.startswith("data: "): continue
    body=line[6:]
    if body=="[DONE]": break
    try: d=json.loads(body)
    except Exception: continue
    for ch in d.get("choices",[]):
        out.append(ch.get("delta",{}).get("content") or "")
print("".join(out))
')"
echo "     assembled: ${assembled:0:200}"
if printf '%s' "$assembled" | python3 -c '
import sys,json
d=json.load(sys.stdin)
assert set(d)=={"name","birth_year"}, "keys=" + repr(sorted(d))
assert isinstance(d["birth_year"], int)
' 2>"$tmpdir/e6"; then report pass "streamed deltas assemble to schema-conforming JSON"
else report fail "streamed deltas assemble to schema-conforming JSON" "$(cat "$tmpdir/e6")"; fi

echo
echo "--- 5. reserved tool name collision must be rejected ---"
resp="$(call /v1/chat/completions "$(cat <<JSON
{"model":"${model}",
 "messages":[{"role":"user","content":"hi"}],
 "tools":[{"type":"function","function":{"name":"ghcp_structured_output","description":"mine",
   "parameters":{"type":"object","properties":{}}}}],
 "response_format":{"type":"json_schema","json_schema":{"name":"p",
   "schema":{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}}}}
JSON
)")"
echo "     resp: ${resp:0:220}"
if printf '%s' "$resp" | grep -qi "reserved"; then report pass "reserved name collision rejected"
else report fail "reserved name collision rejected" "expected a 'reserved' error, got: ${resp:0:200}"; fi

echo
echo "--- 6. /v1/responses text.format (Responses API path) ---"
resp="$(call /v1/responses "$(cat <<JSON
{"model":"${model}",
 "input":"Alan Turing, born 1912 in London.",
 "text":{"format":{"type":"json_schema","name":"person",
   "schema":{"type":"object","properties":{"name":{"type":"string"},"birth_year":{"type":"integer"}},
     "required":["name","birth_year"],"additionalProperties":false}}}}
JSON
)")"
rcontent="$(printf '%s' "$resp" | python3 -c '
import sys,json
d=json.load(sys.stdin)
if d.get("output_text"): print(d["output_text"]); raise SystemExit
for item in d.get("output",[]):
    for c in item.get("content",[]) or []:
        if c.get("text"): print(c["text"]); raise SystemExit
print("")
' 2>/dev/null || echo '')"
echo "     content: ${rcontent:0:220}"
if printf '%s' "$rcontent" | python3 -c '
import sys,json
d=json.load(sys.stdin)
assert set(d)=={"name","birth_year"}, "keys=" + repr(sorted(d))
' 2>"$tmpdir/e7"; then report pass "Responses text.format enforced"
else report fail "Responses text.format enforced" "$(cat "$tmpdir/e7"); raw=${resp:0:300}"; fi

echo
echo "======================================"
printf 'passed: %d   failed: %d\n' "$pass" "$fail"
echo "server log: $tmpdir/server.log (set GHCP_SMOKE_KEEP_TMP=true to retain)"
[[ "$fail" -eq 0 ]] || { echo; echo "--- server log tail ---"; tail -30 "$tmpdir/server.log"; }
exit $(( fail > 0 ? 1 : 0 ))
