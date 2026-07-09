# SDK/CLI-Only Copilot Access — Migration Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `ghcp-pool-go` reach every Copilot model exclusively through the official `github.com/github/copilot-sdk/go` SDK (which spawns the bundled Copilot CLI subprocess), removing all hand-rolled direct HTTP to `api.githubcopilot.com`, while keeping the four client-facing API shapes (OpenAI Chat Completions, OpenAI Responses, Anthropic Messages, OpenAI Embeddings) working through the existing neutral pipeline.

**Architecture:** The gateway already has a provider-neutral seam — `Backend` (`Start/ListModels/Chat/ChatStream/Embeddings/Close`) returning neutral `ChatResult` and `<-chan StreamItem`, and `Gateway.Stream{Chat,Responses,Anthropic}` serializing those neutral items into client SSE. Today two Copilot code paths coexist: `sdk` mode (official SDK, but under-used — fake streaming, prompt-flattened tools) and `opencode` mode (raw direct HTTP). We will (1) make the SDK backend emit *proper* neutral items (real streaming, native tool calls), (2) confirm the server-side Anthropic/Responses serializers build correct output from neutral `ChatResult`/`StreamItem` (not from opencode-only fields), (3) resolve embeddings (no SDK support), (4) flip the default to SDK, then (5) delete the opencode direct-HTTP code.

**Tech Stack:** Go 1.24+, `github.com/github/copilot-sdk/go` (v1.0.0 in `go.mod:8`; v1.0.6 available), `sdk.ModeEmpty` (multi-tenant safe) and `sdk.ModeCopilotCli`, standard `net/http` test servers, `go test ./...`. Optional client-wire modeling: `github.com/openai/openai-go` and `github.com/anthropics/anthropic-sdk-go` (types only, not their HTTP clients).

## Global Constraints

- Go 1.24+ (SDK requirement; source builds already need it).
- `go test ./...` MUST stay green after every task. Pre-existing failures, if any, are noted per task, never newly introduced.
- Do NOT change the `Backend` interface signature in `internal/gateway/backend.go:15-22`. All work happens behind it.
- `FakeBackend` MUST keep working unchanged — the whole unit-test suite depends on it (`GHCP_BACKEND=fake` default).
- No type suppression, no empty catch/`_ =` swallowing of real errors; match existing error-wrapping style (`fmt.Errorf("...: %w", err)`).
- Docker base stays glibc (Debian slim) — the bundled Copilot CLI is a Node.js runtime and fails on musl.
- Copilot integration tests run only with `GHCP_BACKEND=copilot` + a real GitHub token; they are opt-in and gated behind `scripts/smoke.sh` / `scripts/focused-smoke.sh`.
- Commit steps below are the plan's suggested checkpoints and use the existing repo style (`feat:`, `fix:`, `refactor:`, `test:`). Honor the team's "commit only when explicitly authorized" policy — if the executor is not cleared to commit, treat each `Commit` step as a "stage + checkpoint" boundary instead and let the human commit.
- "No direct HTTP to Copilot" means: no `net/http` call from our Go code to `api*.githubcopilot.com` or `api.github.com/copilot_internal/*`. The CLI subprocess making HTTPS to Copilot is expected and allowed (that is GitHub's official client).

---

## File Structure

**Modified:**
- `internal/gateway/copilot_http.go` — `CopilotBackend`. Phase 1-3 add real SDK streaming + native tools; Phase 7 deletes all opencode/direct-HTTP code (`doCopilot`, `stream*SSE`, `*Payload` direct dispatch, `addCopilot*Headers`, `listModelsHTTP`, `validAccessToken`/token mint, `copilotHTTPClient`).
- `internal/gateway/copilot_cli.go` — `CopilotCLIBackend`. Phase 1 gives it real streaming; Phase 2 wires native tools.
- `internal/gateway/config.go` — Phase 6 flips default mode, rejects/deprecates `opencode`.
- `internal/gateway/pool.go` — Phase 6 backend construction default.
- `internal/gateway/gateway.go` — Phase 3 only if a neutral-serialization gap is found; otherwise untouched.
- `internal/gateway/openai.go` — Phase 3 (ensure `anthropicResponse`/responses serialization is neutral-complete), Phase 5 (optional common-package types).
- `internal/gateway/server.go` — Phase 4 embeddings behavior; Phase 6 wiring.
- `go.mod` / `go.sum` — Phase 5 optional deps; Phase 6 optional SDK bump.
- `README.md`, `.github/copilot-instructions.md` — Phase 6/7 doc updates.

**Created:**
- `internal/gateway/copilot_sdk_stream.go` — shared SDK streaming event loop used by both `CopilotBackend` and `CopilotCLIBackend` (Phase 1).
- `internal/gateway/copilot_sdk_tools.go` — client-tool → SDK-tool declaration + tool-request interception (Phase 2).
- `internal/gateway/copilot_sdk_stream_test.go`, `copilot_sdk_tools_test.go` — unit tests.
- `internal/gateway/embeddings_external.go` — optional non-Copilot embeddings provider (Phase 4, only if "external provider" option chosen).

**Deleted (Phase 7):** the opencode-only helpers listed above (kept until the SDK path fully replaces them).

---

## Phase 0 — Safety net: characterization tests

**Goal:** Freeze the *client-observable* output of all four API shapes so later backend-internal refactors cannot silently regress. Run against `FakeBackend` (deterministic) at the server layer.

### Task 0.1: Golden server-output tests for the four shapes

**Files:**
- Test: `internal/gateway/migration_characterization_test.go` (create)

**Interfaces:**
- Consumes: existing `newTestServer`/`request` helpers in `gateway_test.go` (reuse the same harness; confirm exact helper names before writing).
- Produces: `TestCharacterizationChatCompletions`, `TestCharacterizationResponses`, `TestCharacterizationAnthropicMessages`, `TestCharacterizationChatStreamSSE`, `TestCharacterizationAnthropicStreamSSE` — later phases must keep these green.

- [ ] **Step 1: Write characterization tests** capturing, for the fake backend: (a) non-stream `/v1/chat/completions` JSON keys + finish_reason, (b) `/v1/responses` output item shape, (c) `/v1/messages` Anthropic response shape (`type:"message"`, `content[]`, `stop_reason`), (d) streamed `/v1/chat/completions` SSE event sequence (`data:` chunks + `[DONE]`), (e) streamed `/v1/messages` SSE sequence (`message_start` → `content_block_delta` → `message_stop`). Assert on structural invariants (keys present, event order), not exact token text.

- [ ] **Step 2: Run** `go test ./internal/gateway -run TestCharacterization -v` → all PASS (they pin current behavior).

- [ ] **Step 3: Commit** `test: add client-output characterization tests for migration safety net`.

**Verification (phase gate):** `go test ./internal/gateway -run TestCharacterization` PASS.

---

## Phase 1 — Real streaming from the SDK/CLI backend

**Goal:** Replace the fake/buffered streaming in both SDK-mode (`chatStreamSDK`, `copilot_http.go:926`) and CLI-mode (`CopilotCLIBackend.ChatStream`, `copilot_cli.go:120`) with a real event loop that emits incremental `StreamItem{Kind:"delta"}` as the SDK sends `AssistantMessageDeltaData` / `AssistantReasoningDeltaData`, then a terminal `done`.

**Background:** `sendAndCollect` (`copilot_cli.go:361`) already subscribes via `session.On(func(event sdk.SessionEvent))` and handles `*sdk.AssistantMessageData` (final), `*sdk.ExternalToolRequestedData`, `*sdk.AssistantUsageData`, `*sdk.SessionIdleData`, `*sdk.SessionErrorData`. The SDK also emits `*sdk.AssistantMessageDeltaData` (field `DeltaContent`) and `*sdk.AssistantReasoningDeltaData` when `SessionConfig.Streaming = sdk.Bool(true)` — currently unhandled. We add a streaming collector that forwards deltas to a channel.

### Task 1.1: Shared SDK streaming collector

**Files:**
- Create: `internal/gateway/copilot_sdk_stream.go`
- Test: `internal/gateway/copilot_sdk_stream_test.go`

**Interfaces:**
- Produces:
  ```go
  // streamSDKSession sends prompt on an already-created streaming session and
  // forwards SDK events as neutral StreamItems until idle/error/ctx-done.
  // It closes out when the turn completes. Tool requests are surfaced as
  // StreamItem{Kind:"tool_call"}; final usage as the terminal "done" item.
  func streamSDKSession(
      ctx context.Context,
      session *sdk.Session,
      prompt string,
      endpoint string,
      out chan<- StreamItem,
  )
  ```
- Consumes: `sdk.Session`, `sdk.MessageOptions`, `sdk.AssistantMessageDeltaData`, `sdk.AssistantMessageData`, `sdk.AssistantUsageData`, `sdk.ExternalToolRequestedData`, `sdk.SessionIdleData`, `sdk.SessionErrorData`; helpers `sdkUsageEndpoint`, `mergeSDKUsage`, `sdkUsageFromEvent`, `toolCallFromSDKExternalTool`, `appendToolCallUnique` (already in `copilot_cli.go`).

- [ ] **Step 1: Write the failing test** in `copilot_sdk_stream_test.go`. Because a real `*sdk.Session` needs the CLI, test the *delta-ordering and finalization logic* by extracting it into a pure helper `reduceSDKStreamEvents(events []sdk.SessionEvent, out chan<- StreamItem)` and feeding synthetic events:

  ```go
  func TestReduceSDKStreamEventsEmitsDeltasThenDone(t *testing.T) {
      out := make(chan StreamItem, 8)
      m := "claude-sonnet-4.6"
      events := []sdk.SessionEvent{
          {Data: &sdk.AssistantMessageDeltaData{DeltaContent: "Hel"}},
          {Data: &sdk.AssistantMessageDeltaData{DeltaContent: "lo"}},
          {Data: &sdk.AssistantMessageData{Content: "Hello", Model: &m}},
          {Data: &sdk.AssistantUsageData{Model: m}},
          {Data: &sdk.SessionIdleData{}},
      }
      reduceSDKStreamEvents(events, out)
      close(out)
      var kinds []string
      var text string
      for it := range out {
          kinds = append(kinds, it.Kind)
          if it.Kind == "delta" { text += it.Text }
      }
      if text != "Hello" { t.Fatalf("delta text=%q", text) }
      if kinds[len(kinds)-1] != "done" { t.Fatalf("last kind=%v", kinds) }
  }
  ```
  (Confirm exact field names on `AssistantMessageDeltaData`/`AssistantUsageData` from the vendored SDK before finalizing — `go doc github.com/github/copilot-sdk/go.AssistantMessageDeltaData`.)

- [ ] **Step 2: Run** `go test ./internal/gateway -run TestReduceSDKStreamEvents -v` → FAIL (`reduceSDKStreamEvents` undefined).

- [ ] **Step 3: Implement** `reduceSDKStreamEvents` (pure reducer: delta→`StreamItem{Kind:"delta",Text:DeltaContent}`; accumulate usage/model; on tool request → `StreamItem{Kind:"tool_call"}`; on idle → emit terminal `StreamItem{Kind:"done",Usage:...,FinishReason:...}`) and `streamSDKSession` (subscribes via `session.On`, pushes each event through the reducer to `out`, calls `session.Send`, returns on idle/error/ctx). De-dupe suppressed web-search internal tools as `sendAndCollect` does.

- [ ] **Step 4: Run** `go test ./internal/gateway -run TestReduceSDKStreamEvents -v` → PASS.

- [ ] **Step 5: Commit** `feat: add real SDK streaming event reducer`.

### Task 1.2: Wire CLI backend `ChatStream` to real streaming

**Files:**
- Modify: `internal/gateway/copilot_cli.go:119-144` (`ChatStream`)

**Interfaces:**
- Consumes: `streamSDKSession` (Task 1.1), `b.clientForParams`, `b.sessionConfig(model, params, /*stream*/ true)`.

- [ ] **Step 1: Write a failing integration-style test** guarded by copilot env (skips without token): `TestCLIBackendChatStreamEmitsIncrementalDeltas` — asserts ≥2 `delta` items arrive before `done` for a multi-word prompt. Mark `t.Skip` when `os.Getenv("GHCP_BACKEND") != "copilot"`.

- [ ] **Step 2: Run** `GHCP_BACKEND=copilot go test ./internal/gateway -run TestCLIBackendChatStream -v` → FAIL (currently one buffered delta).

- [ ] **Step 3: Reimplement `ChatStream`** to create a streaming session (`sessionConfig(..., true)`), then `go streamSDKSession(ctx, session, prompt, endpoint, out)` instead of calling `chatCLI(..., false)` and replaying. Preserve the tool-call abort-and-return path (reuse the settle/abort logic path from `sendAndCollect`, or share it).

- [ ] **Step 4: Run** the guarded test with a token → PASS; and `go test ./internal/gateway` (fake) → PASS (no regression).

- [ ] **Step 5: Commit** `feat: real incremental streaming for copilot CLI backend`.

### Task 1.3: Wire SDK-mode `chatStreamSDK` to real streaming

**Files:**
- Modify: `internal/gateway/copilot_http.go:926-950` (`chatStreamSDK`)

- [ ] **Step 1: Write failing guarded test** `TestSDKBackendChatStreamEmitsIncrementalDeltas` (same shape as 1.2, `mode=sdk`).

- [ ] **Step 2: Run** → FAIL (buffered).

- [ ] **Step 3: Reimplement `chatStreamSDK`** to obtain a streaming SDK session via `b.sdkForParams`, build the prompt via `b.sdkPrompt(messages, params)` (unchanged for now — Phase 2 replaces tool handling), and delegate to `streamSDKSession`. Remove the `chatSDK(..., false)`-then-replay body.

- [ ] **Step 4: Run** guarded test → PASS; `go test ./internal/gateway` → PASS; `go test ./internal/gateway -run TestCharacterization` → PASS.

- [ ] **Step 5: Commit** `feat: real incremental streaming for copilot SDK backend`.

**Verification (phase gate):** `go test ./internal/gateway` green; with a token, both `Test*ChatStreamEmitsIncrementalDeltas` PASS; characterization SSE tests unchanged.

---

## Phase 2 — Native tool calling in the SDK/CLI backend

**Goal:** Stop degrading tools to text instructions (`sdkToolChoiceInstruction`, `copilot_http.go:969`). Declare the client's tool schemas to the SDK session so the model emits real tool requests, intercept them, and return OpenAI/Anthropic-shaped `tool_calls` to the client (abort-and-return, matching the proxy model — the *client* executes tools and sends `tool_result` in the next request).

### Task 2.1: Declare client tools to the SDK session

**Files:**
- Create: `internal/gateway/copilot_sdk_tools.go`
- Test: `internal/gateway/copilot_sdk_tools_test.go`
- Modify: `internal/gateway/copilot_cli.go` (`sessionConfig`), `internal/gateway/copilot_http.go` (`sdkClientOptions`/session creation)

**Interfaces:**
- Produces:
  ```go
  // sdkToolsFromParams converts OpenAI/Anthropic tool schemas in params["tools"]
  // into SDK tool declarations whose handlers are never expected to run
  // (the turn is aborted on first tool request and the call is returned to
  // the client). Returns nil when no tools are present.
  func sdkToolsFromParams(params map[string]any) []sdk.Tool
  ```
- Consumes: `sdk.Tool`/`sdk.DefineTool` (confirm the exact constructor + schema type via `go doc github.com/github/copilot-sdk/go.DefineTool`), the normalized tool shape produced by `normalizeTools` (`types.go:175`).

- [ ] **Step 1: Write failing test** `TestSDKToolsFromParamsMapsNamesAndSchemas` — feed a params map with two function tools; assert two `sdk.Tool` returned with matching names/descriptions and that JSON-schema parameters round-trip. `nil` in → `nil` out.

- [ ] **Step 2: Run** → FAIL (undefined).

- [ ] **Step 3: Implement `sdkToolsFromParams`.** Map each `{type:"function",function:{name,description,parameters}}` (and Anthropic `{name,description,input_schema}`) to an `sdk.Tool` with a no-op/error handler (handler body returns an error so an accidental in-session invocation is never silently mis-executed; real interception happens at the event layer). Attach `parameters`/`input_schema` as the tool's JSON schema.

- [ ] **Step 4: Run** → PASS.

- [ ] **Step 5: Commit** `feat: map client tool schemas to SDK tool declarations`.

### Task 2.2: Attach tools to sessions + keep abort-and-return

**Files:**
- Modify: `internal/gateway/copilot_cli.go:263-305` (`sessionConfig`), `internal/gateway/copilot_http.go` (SDK session creation for `CopilotBackend`)

**Interfaces:**
- Consumes: `sdkToolsFromParams`. Sets `SessionConfig.Tools = sdkToolsFromParams(params)` and, when tools are present, sets `AvailableTools` to include them (ModeEmpty defaults to no tools).

- [ ] **Step 1a: Write failing deterministic unit test** `TestSDKPromptOmitsToolInstruction` (no live model) — call `b.sdkPrompt(messages, params)` with `params["tools"]` set and assert the returned prompt does NOT contain the tool-choice instruction text that `sdkToolChoiceInstruction` currently appends (pick a distinctive substring from that instruction). This locks the "no text-instruction leak" behavior without a token.

- [ ] **Step 1b: Write failing guarded integration test** `TestSDKBackendReturnsToolCallsForClientTool` (skips without `GHCP_BACKEND=copilot`) — send a prompt + one function tool the model is very likely to call ("call get_weather for Tokyo"); assert the result has `FinishReason=="tool_calls"` and a `ToolCall{Name:"get_weather"}` with JSON args.

- [ ] **Step 2: Run** `go test ./internal/gateway -run TestSDKPromptOmitsToolInstruction -v` → FAIL (instruction still appended); with a token, `TestSDKBackendReturnsToolCallsForClientTool` → FAIL (tools not declared).

- [ ] **Step 3: Wire tools into `sessionConfig`** and the `CopilotBackend` session path; remove the `sdkToolChoiceInstruction` append from `sdkPrompt` (leave `response_format` JSON hint). Ensure the existing tool-request interception (`ExternalToolRequestedData`/`AssistantMessageData.ToolRequests` → abort → return) still fires with declared tools.

- [ ] **Step 4: Run** `go test ./internal/gateway -run TestSDKPromptOmitsToolInstruction` → PASS; guarded test (with token) → PASS; `go test ./internal/gateway` (fake) → PASS.

- [ ] **Step 5: Commit** `feat: native SDK tool calling in copilot backends`.

**Verification (phase gate):** `go test ./internal/gateway` green; with a token, `scripts/focused-smoke.sh` tool-use case passes; no `sdkToolChoiceInstruction` references remain (`grep -rn sdkToolChoiceInstruction internal/`).

---

## Phase 3 — Neutral-complete Anthropic & Responses serialization

**Goal:** Guarantee the server serializes correct Anthropic-Messages and OpenAI-Responses output from a neutral `ChatResult`/`StreamItem` produced by the SDK backend — without depending on opencode-only fields (`ChatResult.AnthropicContent`, `ChatResult.ResponsesOutput`) that only the direct-HTTP path populated.

### Task 3.1: Audit + test `anthropicResponse` on chat-shaped results

**Files:**
- Modify (only if gap found): `internal/gateway/openai.go:525` (`anthropicResponse`)
- Test: `internal/gateway/anthropic_serialization_test.go` (create)

- [ ] **Step 1: Write test** `TestAnthropicResponseFromChatShapedResult` — build a `ChatResult{Content:"hi", ToolCalls:[…], FinishReason:"tool_calls", AnthropicContent:nil}` and assert `anthropicResponse` yields valid `{type:"message", role:"assistant", content:[{type:"text",…} / {type:"tool_use",…}], stop_reason:"tool_use"}`. Add a `stop`→`end_turn` case.

- [ ] **Step 2: Run** → PASS or FAIL. If it depends on `AnthropicContent`, FAIL.

- [ ] **Step 3: If FAIL, extend `anthropicResponse`** to synthesize `content[]` from `Content` + `ToolCalls` when `AnthropicContent` is empty, and map finish reasons (`stop`→`end_turn`, `tool_calls`→`tool_use`, `length`→`max_tokens`, `content_filter`→`refusal`).

- [ ] **Step 4: Run** → PASS.

- [ ] **Step 5: Commit** `fix: build anthropic response from neutral chat result`.

### Task 3.2: Same audit for Responses output + streaming serializers

**Files:**
- Test: `internal/gateway/responses_serialization_test.go` (create); modify responses serializer in `openai.go` if a gap is found.

- [ ] **Step 1: Write tests** for `/v1/responses` non-stream serialization from a chat-shaped `ChatResult` (assert `output[]` item with `type:"message"`/`content`), and a streaming test that drives `Gateway.StreamResponses`/`StreamAnthropic` with a synthetic `<-chan StreamItem` (`delta`×2 → `tool_call` → `done`) and asserts the emitted SSE event sequence matches the characterization goldens.

- [ ] **Step 2: Run** → identify gaps.

- [ ] **Step 3: Fill gaps** so both serializers are total over neutral items.

- [ ] **Step 4: Run** → PASS; `go test ./internal/gateway -run TestCharacterization` → PASS.

- [ ] **Step 5: Commit** `fix: neutral-complete responses/anthropic streaming serialization`.

**Verification (phase gate):** With `mode=sdk` and a token, `scripts/smoke.sh --backend copilot` passes chat, responses, and anthropic messages (stream + non-stream).

---

## Phase 4 — Embeddings: drop under SDK/CLI (DECISION: Option A, locked)

**Goal:** The SDK/CLI cannot produce embeddings (confirmed: no method in SDK or CLI). **Decision (locked by product owner): Option A — drop embeddings from the SDK-only build.** `/v1/embeddings` returns a clean OpenAI-shaped unsupported error when the backend is SDK/CLI. Zero Copilot direct HTTP. Options B (isolated Copilot direct-HTTP exception) and C (external BYOK embeddings provider) are recorded below as NOT chosen, for future reference only — do not implement them.

- **Option A — Drop (CHOSEN):** `/v1/embeddings` returns `501`/`ErrEmbeddingsUnsupported` when backend is SDK/CLI. Simplest; removes a feature.
- **Option B — Isolated Copilot direct-HTTP exception (NOT chosen):** keep a single flagged `embeddingsDirectHTTP` behind config. Do not implement.
- **Option C — External provider BYOK (NOT chosen):** route `/v1/embeddings` to a non-Copilot provider. Do not implement.

### Task 4.1: Clean unsupported behavior (Option A)

**Files:** `internal/gateway/server.go` (embeddings handler), test in `gateway_test.go`.

- [ ] **Step 1: Write test** `TestEmbeddingsUnsupportedReturns501WhenSDK` — SDK-mode backend → `/v1/embeddings` returns a clean OpenAI-shaped error (`501` or `400 invalid_request_error`, not a 500).
- [ ] **Step 2: Run** → FAIL if it currently 500s.
- [ ] **Step 3: Map `ErrEmbeddingsUnsupported`** to a clean client error in the handler.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat: clean 501 for embeddings under sdk/cli backend`.

**Verification (phase gate):** `TestEmbeddingsUnsupportedReturns501WhenSDK` PASS; `grep -rn "githubcopilot.com" internal/` shows no embeddings-related Copilot HTTP.

---

## Phase 5 — (Optional, parallelizable) Adopt common packages for client wire types

**Goal:** Replace hand-rolled client-facing request/response structs with maintained libraries, one shape at a time. Pilot with Anthropic Messages. Types only — never their HTTP clients.

### Task 5.1: Add deps + pilot Anthropic request parsing

**Files:** `go.mod`/`go.sum`; `internal/gateway/openai.go` (Anthropic request type); test.

- [ ] **Step 1:** `go get github.com/anthropics/anthropic-sdk-go@latest github.com/openai/openai-go@latest` (record versions).
- [ ] **Step 2: Write test** `TestAnthropicRequestParsesViaSDKTypes` decoding a representative Claude Code `/v1/messages` body (system blocks, tool_use, tool_result, cache_control, thinking) into the library type and asserting our `NeutralMessage` conversion is unchanged vs the hand-rolled path (compare against a golden).
- [ ] **Step 3: Run** → establish baseline (may pass immediately if only additive).
- [ ] **Step 4: Introduce** the library type behind `AnthropicMessagesRequest` decoding, keeping `ToChatRequest()` output byte-identical (guard with the golden).
- [ ] **Step 5: Commit** `refactor: model anthropic messages wire types via anthropic-sdk-go`.

> Note: `openai-go` request types use `param.Field` wrappers (built for *sending*); for server-side *parsing* prefer `github.com/sashabaranov/go-openai` plain structs or keep hand-rolled OpenAI types. Decide per-shape; this phase is optional and can trail the rest.

**Verification (phase gate):** `go test ./internal/gateway` green; goldens unchanged.

---

## Phase 6 — Flip default to SDK, deprecate opencode

**Goal:** Make `sdk` the default and reject `opencode` (or warn-and-map to `sdk`) so no request path can take direct HTTP.

### Task 6.1: Default + validation

**Files:** `internal/gateway/config.go:430` (mode validation), `internal/gateway/pool.go:423`, `README.md`, `.github/copilot-instructions.md`.

- [ ] **Step 1: Write test** `TestOpencodeModeRejected` — loading `gateway.copilot.mode: opencode` (or `GHCP_COPILOT_MODE=opencode`) returns a clear error (or logs a deprecation and coerces to `sdk`; pick one and test it).
- [ ] **Step 2: Run** → FAIL (currently accepted).
- [ ] **Step 3: Update** `ValidCopilotBackendModes` to drop `opencode`; default mode `sdk`; update `LoadSettings` validation message.
- [ ] **Step 4: Run** `go test ./internal/gateway` → PASS; update README/instructions to state SDK/CLI-only + embeddings decision.
- [ ] **Step 5: Commit** `feat!: sdk/cli-only — deprecate opencode direct-http mode`.

**Verification (phase gate):** `go test ./...` green; `go run ./cmd/ghcp-pool` with `GHCP_COPILOT_MODE=opencode` errors/deprecates as specified; full `scripts/smoke.sh --backend copilot` passes end-to-end in `sdk` mode.

---

## Phase 7 — Delete opencode direct-HTTP code

**Goal:** Remove every hand-rolled Copilot HTTP path now that SDK mode covers the surface. Do this only after Phase 6 is green.

### Task 7.1: Remove direct-HTTP inference + SSE parsers

**Files:** `internal/gateway/copilot_http.go`

- [ ] **Step 1:** Delete `doCopilot` and its dispatch branches in `Chat`/`ChatStream`/`Embeddings` (`copilot_http.go:336-437`), the raw parsers `streamChatSSE`/`streamResponsesSSE`/`streamAnthropicSSE` (2969/3027/3119), and the direct payload builders only used by them (`anthropicPayload`, `responsesPayload`, `chatPayload` direct dispatch) if unreferenced.
- [ ] **Step 2:** Delete direct request headers/token: `addCopilotHeaders`, `addCopilotModelHeaders`, `addGitHubCopilotTokenHeaders`, `copilotRequestMetadata`, `validAccessToken` + token-mint (`copilotTokenURL` flow), `listModelsHTTP`/`listModelsDirect`, `copilotHTTPClient`, and constants that become unused (`defaultCopilotAPIBaseURL`, `copilotPublicAPIBaseURL`, `copilotUserAgent`, `copilotEditorVersion`, `copilotPluginVersion`, `copilotIntent`, `copilotAPIVersion`) — keeping any still used by the SDK web-search client (`sdkWebHTTPClient` hits external search URLs, not Copilot; keep it).
- [ ] **Step 3: Run** `go build ./... && go vet ./...` → 0 errors; fix references (`ListModels` now always `listModelsSDK`; `Embeddings` per Phase 4).
- [ ] **Step 4: Run** `go test ./...` → PASS (delete now-obsolete opencode-only tests in `gateway_test.go`, e.g. the `addCopilotHeaders` header tests, since that code is gone; keep behavior tests that run through the backend).
- [ ] **Step 5: Commit** `refactor: remove opencode direct-http copilot code path`.

### Task 7.2: Final sweep

- [ ] **Step 1: Verify no Copilot direct HTTP remains:** `grep -rnE "githubcopilot\.com|copilot_internal" internal/ | grep -v _test.go` returns nothing (or only the Phase-4 Option-B opt-in).
- [ ] **Step 2: Run** `lsp_diagnostics` on all changed files → clean; `go test ./...` → PASS.
- [ ] **Step 3:** Confirm the mode enum, config docs, and README reflect SDK/CLI-only.
- [ ] **Step 4: Commit** `chore: finalize sdk/cli-only migration`.

**Verification (phase gate):** `go test ./...` green; `grep` sweep clean; `scripts/smoke.sh --backend copilot` and `scripts/focused-smoke.sh` pass; container builds on glibc base.

---

## Cross-cutting verification (run after each phase)

```bash
go build ./...
go vet ./...
go test ./internal/gateway -run TestCharacterization   # never regress client output
go test ./...
# With a real token (opt-in):
GHCP_BACKEND=copilot GHCP_COPILOT_MODE=sdk scripts/smoke.sh --backend copilot --gh-user <user>
GHCP_BACKEND=copilot scripts/focused-smoke.sh --gh-user <user>
```

## Risks & open decisions

1. **Embeddings (Phase 4)** — a product decision (drop vs Copilot opt-in vs external). Blocks a strict "zero direct HTTP" claim only under Option B.
2. **Stateful session ↔ stateless API** — SDK sessions are per-turn prompt-based; multi-turn history is replayed as a rendered prompt (`renderPrompt`). Tool-result turns re-enter as prompt text. Acceptable, but tool-call fidelity across many turns should be smoke-tested (covered in Phase 2 verification).
3. **SDK version** — consider bumping `v1.0.0`→`v1.0.6` at Phase 6 (no breaking notes observed); do it as its own commit with full test run.
4. **Prompt-flattening fidelity** — if downstream users need exact multi-turn/system fidelity beyond what `renderPrompt` gives, that is a separate enhancement, out of scope here.
