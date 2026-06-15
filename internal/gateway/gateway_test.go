package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var adminHeaders = map[string]string{"Authorization": "Bearer sk-admin"}
var userHeaders = map[string]string{"Authorization": "Bearer sk-user"}

func testSettings() Settings {
	on := true
	return Settings{
		Backend:              "fake",
		Host:                 "127.0.0.1",
		Port:                 8000,
		ModelRefreshSeconds:  300,
		RouteBusyWaitSeconds: 0.1,
		APIKeys: []APIKeyConfig{
			{Key: "sk-admin", Scopes: []string{"admin", "inference"}, ModelAllow: []string{"*"}, CacheNamespace: "default"},
			{Key: "sk-user", Scopes: []string{"inference"}, ModelAllow: []string{"gpt-*", "claude-*"}, CacheNamespace: "default"},
		},
		Accounts: []AccountConfig{
			{ID: "acct_a", Label: "A", Enabled: &on, MaxConcurrency: 32, Weight: 1, Allow: []string{"*"}, Models: []string{"gpt-4.1", "gpt-4o-mini"}},
			{ID: "acct_b", Label: "B", Enabled: &on, MaxConcurrency: 32, Weight: 1, Allow: []string{"*"}, Models: []string{"claude-3.5"}},
		},
		Routes: []RouteConfig{
			{Model: "claude-*", Accounts: []string{"acct_b"}, Strategy: "weighted", Priority: 10},
			{Model: "*", Accounts: []string{"acct_a", "acct_b"}, Strategy: "least_busy"},
		},
		Cache:            CacheConfig{Enabled: &on, TTLSeconds: 600, MaxEntries: 100, Salt: "t"},
		Usage:            UsageConfig{SQLitePath: ":memory:"},
		ReasoningEfforts: map[string]string{"gpt-4o-mini": "low"},
		Debug:            DebugConfig{Enabled: false, MaxEntries: 10, MaxBodyChars: 20000},
		Login:            LoginConfig{Scopes: "read:user", DeviceCodeURL: "https://github.com/login/device/code", TokenURL: "https://github.com/login/oauth/access_token"},
	}
}

func testServer(t *testing.T) (*Gateway, http.Handler) {
	t.Helper()
	gw, err := NewGateway(testSettings())
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Shutdown)
	return gw, NewServer(gw)
}

func request(t *testing.T, h http.Handler, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func decodeBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &data); err != nil {
		t.Fatalf("decode %s: %v", rr.Body.String(), err)
	}
	return data
}

func chatBody(model, text string) map[string]any {
	return map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": "be terse"},
			{"role": "user", "content": text},
		},
	}
}

func TestHealthAndAuth(t *testing.T) {
	_, h := testServer(t)
	if rr := request(t, h, "GET", "/healthz", nil, nil); rr.Code != 200 {
		t.Fatalf("healthz status %d", rr.Code)
	}

	if rr := request(t, h, "GET", "/v1/models", nil, nil); rr.Code != 401 {
		t.Fatalf("models without auth status %d", rr.Code)
	}
	rr := request(t, h, "GET", "/v1/models", nil, userHeaders)
	body := decodeBody(t, rr)
	if rr.Code != 200 || len(body["data"].([]any)) < 2 {
		t.Fatalf("models response: status=%d body=%v", rr.Code, body)
	}
}

func TestEnvAPIKeyOverridesDefaultKey(t *testing.T) {
	t.Setenv("GHCP_API_KEY", "sk-env")
	settings, err := LoadSettings("/does/not/exist.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := settings.APIKeys[0].Key; got != "sk-env" {
		t.Fatalf("api key=%q", got)
	}
}

func TestChatCompletionCacheAndRouting(t *testing.T) {
	_, h := testServer(t)
	body := chatBody("gpt-4.1", "hello world")
	first := request(t, h, "POST", "/v1/chat/completions", body, userHeaders)
	if first.Code != 200 {
		t.Fatal(first.Body.String())
	}
	if got := first.Header().Get("x-ghcp-account"); got != "acct_a" {
		t.Fatalf("account=%q", got)
	}
	if got := first.Header().Get("x-ghcp-cache"); got != "miss" {
		t.Fatalf("cache=%q", got)
	}
	payload := decodeBody(t, first)
	choice := payload["choices"].([]any)[0].(map[string]any)
	content := choice["message"].(map[string]any)["content"].(string)
	if !strings.Contains(content, "hello world") {
		t.Fatalf("content=%q", content)
	}
	second := request(t, h, "POST", "/v1/chat/completions", body, userHeaders)
	if got := second.Header().Get("x-ghcp-cache"); got != "hit" {
		t.Fatalf("cache hit header=%q", got)
	}
	if got := second.Header().Get("x-ghcp-account"); got != "cache" {
		t.Fatalf("cache account=%q", got)
	}
}

func TestModelAliasesListAndRoute(t *testing.T) {
	settings := testSettings()
	settings.ModelAliases = map[string]string{
		"gpt-friendly":    "gpt-4.1",
		"claude-friendly": "claude-3.5",
	}
	gw, err := NewGateway(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Shutdown)
	h := NewServer(gw)

	modelsResp := request(t, h, "GET", "/v1/models", nil, userHeaders)
	models := decodeBody(t, modelsResp)["data"].([]any)
	ids := map[string]bool{}
	for _, item := range models {
		ids[item.(map[string]any)["id"].(string)] = true
	}
	if !ids["gpt-friendly"] || ids["gpt-4.1"] {
		t.Fatalf("model ids=%v", ids)
	}

	resp := request(t, h, "POST", "/v1/chat/completions", chatBody("gpt-friendly", "alias hello"), userHeaders)
	if resp.Code != 200 {
		t.Fatal(resp.Body.String())
	}
	if got := resp.Header().Get("x-ghcp-model"); got != "gpt-friendly" {
		t.Fatalf("display model header=%q", got)
	}
	if got := resp.Header().Get("x-ghcp-upstream-model"); got != "gpt-4.1" {
		t.Fatalf("upstream model header=%q", got)
	}
	payload := decodeBody(t, resp)
	if payload["model"] != "gpt-friendly" {
		t.Fatalf("payload model=%v", payload["model"])
	}
}

func TestAdminModelAliasesRoundtrip(t *testing.T) {
	_, h := testServer(t)
	resp := request(t, h, "PUT", "/admin/model-aliases", map[string]string{"gpt-display": "gpt-4.1"}, adminHeaders)
	if resp.Code != 200 {
		t.Fatal(resp.Body.String())
	}
	aliases := decodeBody(t, request(t, h, "GET", "/admin/model-aliases", nil, adminHeaders))["model_aliases"].(map[string]any)
	if aliases["gpt-display"] != "gpt-4.1" {
		t.Fatalf("aliases=%v", aliases)
	}
	resolved := decodeBody(t, request(t, h, "GET", "/admin/routes/resolve?model=gpt-display", nil, adminHeaders))
	if resolved["resolved_model"] != "gpt-4.1" {
		t.Fatalf("resolved=%v", resolved)
	}
}

func TestResponsesAndAnthropicShapes(t *testing.T) {
	_, h := testServer(t)
	resp := request(t, h, "POST", "/v1/responses", map[string]any{"model": "gpt-4.1", "input": "responses hello", "max_output_tokens": 64}, userHeaders)
	if resp.Code != 200 {
		t.Fatal(resp.Body.String())
	}
	body := decodeBody(t, resp)
	if body["object"] != "response" || body["status"] != "completed" || !strings.Contains(body["output_text"].(string), "responses hello") {
		t.Fatalf("bad response body=%v", body)
	}
	usage := body["usage"].(map[string]any)
	if usage["input_tokens"] == nil || usage["output_tokens"] == nil {
		t.Fatalf("bad usage=%v", usage)
	}

	bad := request(t, h, "POST", "/v1/messages", map[string]any{"model": "claude-3.5", "messages": []map[string]any{{"role": "user", "content": "hi"}}}, userHeaders)
	if bad.Code != 400 || !strings.Contains(bad.Body.String(), "requires max_tokens") {
		t.Fatalf("bad max_tokens response: %d %s", bad.Code, bad.Body.String())
	}
	msg := request(t, h, "POST", "/v1/messages", map[string]any{"model": "claude-3.5", "max_tokens": 64, "messages": []map[string]any{{"role": "user", "content": "anthropic hello"}}}, userHeaders)
	if msg.Code != 200 {
		t.Fatal(msg.Body.String())
	}
	payload := decodeBody(t, msg)
	content := payload["content"].([]any)[0].(map[string]any)["text"].(string)
	if payload["type"] != "message" || !strings.Contains(content, "anthropic hello") {
		t.Fatalf("anthropic payload=%v", payload)
	}
	if got := msg.Header().Get("x-ghcp-account"); got != "acct_b" {
		t.Fatalf("anthropic account=%q", got)
	}
}

func TestAnthropicCountTokens(t *testing.T) {
	_, h := testServer(t)
	resp := request(t, h, "POST", "/v1/messages/count_tokens", map[string]any{
		"model":    "claude-3.5",
		"system":   "count system",
		"messages": []map[string]any{{"role": "user", "content": "count these tokens"}},
		"tools":    []map[string]any{{"name": "get_weather", "input_schema": map[string]any{"type": "object"}}},
	}, userHeaders)
	if resp.Code != 200 {
		t.Fatal(resp.Body.String())
	}
	body := decodeBody(t, resp)
	if body["input_tokens"].(float64) <= 0 {
		t.Fatalf("count body=%v", body)
	}

	bad := request(t, h, "POST", "/v1/messages/count_tokens", map[string]any{
		"model": "gpt-4.1", "messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, userHeaders)
	if bad.Code != 400 || !strings.Contains(bad.Body.String(), "count_tokens is only available") {
		t.Fatalf("bad count response=%d %s", bad.Code, bad.Body.String())
	}
}

func TestEmbeddingsEndpoint(t *testing.T) {
	_, h := testServer(t)
	resp := request(t, h, "POST", "/v1/embeddings", map[string]any{
		"model": "gpt-4.1", "input": []string{"hello", "world"}, "dimensions": 8,
	}, userHeaders)
	if resp.Code != 200 {
		t.Fatal(resp.Body.String())
	}
	body := decodeBody(t, resp)
	if body["object"] != "list" || body["model"] != "gpt-4.1" {
		t.Fatalf("embedding body=%v", body)
	}
	data := body["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("embedding data=%v", data)
	}
	vector := data[0].(map[string]any)["embedding"].([]any)
	if len(vector) != 8 {
		t.Fatalf("embedding vector len=%d", len(vector))
	}
}

func TestStreamingAndToolCalls(t *testing.T) {
	_, h := testServer(t)
	body := chatBody("gpt-4.1", "stream this")
	body["stream"] = true
	body["stream_options"] = map[string]any{"include_usage": true}
	rr := request(t, h, "POST", "/v1/chat/completions", body, userHeaders)
	if rr.Code != 200 || !strings.Contains(rr.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream status=%d content-type=%s", rr.Code, rr.Header().Get("Content-Type"))
	}
	if !strings.Contains(rr.Body.String(), "chat.completion.chunk") || !strings.Contains(rr.Body.String(), "[DONE]") || !strings.Contains(rr.Body.String(), "usage") {
		t.Fatalf("stream body=%s", rr.Body.String())
	}

	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object"}}}}
	toolResp := request(t, h, "POST", "/v1/chat/completions", map[string]any{"model": "gpt-4.1", "messages": []map[string]any{{"role": "user", "content": "weather?"}}, "tools": tools}, userHeaders)
	payload := decodeBody(t, toolResp)
	choice := payload["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("tool choice=%v", choice)
	}
}

func TestGeminiProtocol(t *testing.T) {
	_, h := testServer(t)
	models := request(t, h, "GET", "/v1beta/models", nil, map[string]string{"x-goog-api-key": "sk-user"})
	if models.Code != 200 {
		t.Fatal(models.Body.String())
	}
	if !strings.Contains(models.Body.String(), "models/gpt-4.1") {
		t.Fatalf("gemini models=%s", models.Body.String())
	}

	body := map[string]any{"contents": []map[string]any{{"role": "user", "parts": []map[string]any{{"text": "gemini hello"}}}}}
	resp := request(t, h, "POST", "/v1beta/models/gpt-4.1:generateContent", body, map[string]string{"x-goog-api-key": "sk-user"})
	if resp.Code != 200 {
		t.Fatal(resp.Body.String())
	}
	payload := decodeBody(t, resp)
	candidates := payload["candidates"].([]any)
	text := candidates[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "gemini hello") {
		t.Fatalf("gemini response=%v", payload)
	}

	count := request(t, h, "POST", "/v1beta/models/gpt-4.1:countTokens", body, map[string]string{"x-goog-api-key": "sk-user"})
	if count.Code != 200 || decodeBody(t, count)["totalTokens"].(float64) <= 0 {
		t.Fatalf("count=%d %s", count.Code, count.Body.String())
	}

	streamBody := map[string]any{"contents": []map[string]any{{"role": "user", "parts": []map[string]any{{"text": "gemini stream"}}}}}
	stream := request(t, h, "POST", "/v1beta/models/gpt-4.1:streamGenerateContent", streamBody, map[string]string{"x-goog-api-key": "sk-user"})
	if stream.Code != 200 || !strings.Contains(stream.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream=%d content-type=%s body=%s", stream.Code, stream.Header().Get("Content-Type"), stream.Body.String())
	}
	if !strings.Contains(stream.Body.String(), "gemini ") || !strings.Contains(stream.Body.String(), "stream ") {
		t.Fatalf("stream body=%s", stream.Body.String())
	}
}

func TestCapabilityRoutingSelectsSupportedEndpoint(t *testing.T) {
	settings := testSettings()
	settings.Accounts[0].Models = []string{"responses-only-model", "chat-only-model"}
	gw, err := NewGateway(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Shutdown)
	principal := Principal{config: APIKeyConfig{Scopes: []string{"inference"}, ModelAllow: []string{"*"}, CacheNamespace: "test"}}

	chatReq := ChatCompletionRequest{Model: "responses-only-model", PreferredEndpoint: endpointChatCompletions, FallbackEndpoints: []string{endpointResponses}, Messages: []ChatMessage{{Role: "user", Content: "hi", Raw: map[string]any{"role": "user", "content": "hi"}}}}
	plan, err := gw.Prepare(chatReq, principal, "bypass")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Endpoint != endpointResponses {
		t.Fatalf("endpoint=%q want %q", plan.Endpoint, endpointResponses)
	}

	responsesReq := ChatCompletionRequest{Model: "chat-only-model", PreferredEndpoint: endpointResponses, FallbackEndpoints: []string{endpointChatCompletions}, Messages: []ChatMessage{{Role: "user", Content: "hi", Raw: map[string]any{"role": "user", "content": "hi"}}}}
	plan, err = gw.Prepare(responsesReq, principal, "bypass")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Endpoint != endpointChatCompletions {
		t.Fatalf("endpoint=%q want %q", plan.Endpoint, endpointChatCompletions)
	}
}

func TestCapabilityRoutingHonorsRoutesBeforeEndpointChoice(t *testing.T) {
	settings := testSettings()
	settings.Accounts[0].Models = []string{"mixed-model"}
	settings.Accounts[1].Models = []string{"mixed-model"}
	settings.Routes = []RouteConfig{{Model: "mixed-model", Accounts: []string{"acct_a"}, Strategy: "least_busy"}}
	gw, err := NewGateway(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Shutdown)
	gw.Registry.mu.Lock()
	gw.Registry.index = map[string]map[string]ModelSpec{
		"mixed-model": {
			"acct_a": {ID: "mixed-model", SupportedEndpoints: []string{endpointResponses}},
			"acct_b": {ID: "mixed-model", SupportedEndpoints: []string{endpointChatCompletions}},
		},
		"messages-only-model": {
			"acct_a": {ID: "messages-only-model", SupportedEndpoints: []string{endpointMessages}},
		},
	}
	gw.Registry.LastRefresh = nowUnix()
	gw.Registry.mu.Unlock()

	principal := Principal{config: APIKeyConfig{Scopes: []string{"inference"}, ModelAllow: []string{"*"}, CacheNamespace: "test"}}
	req := ChatCompletionRequest{Model: "mixed-model", PreferredEndpoint: endpointChatCompletions, FallbackEndpoints: []string{endpointResponses}, Messages: []ChatMessage{{Role: "user", Content: "hi", Raw: map[string]any{"role": "user", "content": "hi"}}}}
	plan, err := gw.Prepare(req, principal, "bypass")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Endpoint != endpointResponses || plan.Account.ID() != "acct_a" {
		t.Fatalf("endpoint/account=%q/%q, want %q/acct_a", plan.Endpoint, plan.Account.ID(), endpointResponses)
	}

	messagesReq := ChatCompletionRequest{Model: "messages-only-model", PreferredEndpoint: endpointResponses, FallbackEndpoints: []string{endpointChatCompletions, endpointMessages}, Messages: []ChatMessage{{Role: "user", Content: "hi", Raw: map[string]any{"role": "user", "content": "hi"}}}}
	plan, err = gw.Prepare(messagesReq, principal, "bypass")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Endpoint != endpointMessages {
		t.Fatalf("endpoint=%q want %q", plan.Endpoint, endpointMessages)
	}
}

func TestGeminiAllowedFunctionNamesFilterTools(t *testing.T) {
	req := GeminiGenerateContentRequest{
		Contents: []GeminiContent{{Role: "user", Parts: []GeminiPart{{Text: "call a tool"}}}},
		Tools: []GeminiTool{{FunctionDeclarations: []GeminiFunctionDeclaration{
			{Name: "allowed", Parameters: map[string]any{"type": "object"}},
			{Name: "blocked", Parameters: map[string]any{"type": "object"}},
		}}},
		ToolConfig: &GeminiToolConfig{FunctionCallingConfig: &GeminiFunctionCallingConfig{Mode: "ANY", AllowedFunctionNames: []string{"allowed"}}},
	}
	chat, err := geminiToChatRequest("gpt-4.1", req, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Tools) != 1 {
		t.Fatalf("tools=%v", chat.Tools)
	}
	fn := chat.Tools[0]["function"].(map[string]any)
	if fn["name"] != "allowed" {
		t.Fatalf("tool=%v", chat.Tools[0])
	}
}

func TestChatSSERequiresDone(t *testing.T) {
	out := make(chan StreamItem)
	go func() {
		defer close(out)
		streamChatSSE(context.Background(), strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"), out)
	}()
	items := []StreamItem{}
	for item := range out {
		items = append(items, item)
	}
	if len(items) == 0 || items[len(items)-1].Err == nil {
		t.Fatalf("items=%v", items)
	}
}

func TestResponsesStreamMergesToolCallByOutputIndex(t *testing.T) {
	body := strings.Join([]string{
		"event: response.function_call_arguments.delta",
		"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{\\\"city\\\"\"}",
		"",
		"event: response.output_item.done",
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"weather\",\"arguments\":\"{\\\"city\\\":\\\"SEA\\\"}\"}}",
		"",
		"event: response.completed",
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2},\"output\":[{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"weather\",\"arguments\":\"{\\\"city\\\":\\\"SEA\\\"}\"}]}}",
		"",
	}, "\n")
	out := make(chan StreamItem)
	go func() {
		defer close(out)
		streamResponsesSSE(context.Background(), strings.NewReader(body), out)
	}()
	toolCalls := []ToolCall{}
	for item := range out {
		if item.Err != nil {
			t.Fatal(item.Err)
		}
		if item.Kind == "tool_call" {
			toolCalls = append(toolCalls, item.ToolCall)
		}
	}
	if len(toolCalls) != 1 || toolCalls[0].ID != "call_1" || toolCalls[0].Arguments != "{\"city\":\"SEA\"}" {
		t.Fatalf("toolCalls=%v", toolCalls)
	}
}

func TestToolChoiceShapeMatchesSelectedEndpoint(t *testing.T) {
	chatChoice := map[string]any{"type": "function", "function": map[string]any{"name": "allowed"}}
	responses := responsesPayload("responses-only-model", []NeutralMessage{{Role: "user", Content: "hi"}}, map[string]any{"tool_choice": chatChoice}, false)
	choice := responses["tool_choice"].(map[string]any)
	if choice["name"] != "allowed" || choice["function"] != nil {
		t.Fatalf("responses tool_choice=%v", choice)
	}

	responseChoice := map[string]any{"type": "function", "name": "allowed"}
	chat := chatPayload("chat-only-model", []NeutralMessage{{Role: "user", Content: "hi"}}, map[string]any{"tool_choice": responseChoice}, false)
	chatToolChoice := chat["tool_choice"].(map[string]any)
	fn := chatToolChoice["function"].(map[string]any)
	if fn["name"] != "allowed" {
		t.Fatalf("chat tool_choice=%v", chatToolChoice)
	}

	customChoice := map[string]any{"type": "custom", "name": "shell"}
	custom := responsesPayload("responses-only-model", []NeutralMessage{{Role: "user", Content: "hi"}}, map[string]any{"tool_choice": customChoice}, false)
	if custom["tool_choice"].(map[string]any)["type"] != "custom" {
		t.Fatalf("custom responses tool_choice=%v", custom["tool_choice"])
	}

	anthropicChoice := anthropicPayload("claude-3.5", []NeutralMessage{{Role: "user", Content: "hi"}}, map[string]any{"tool_choice": chatChoice, "response_options": map[string]any{"max_tokens": 64}}, false)
	if anthropicChoice["tool_choice"].(map[string]any)["type"] != "tool" {
		t.Fatalf("anthropic tool_choice=%v", anthropicChoice["tool_choice"])
	}
}

func TestRateLimits(t *testing.T) {
	settings := testSettings()
	off := false
	settings.Cache.Enabled = &off
	settings.RateLimits = RateLimitConfig{PerAccountRPM: 1}
	gw, err := NewGateway(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Shutdown)
	h := NewServer(gw)

	first := request(t, h, "POST", "/v1/chat/completions", chatBody("gpt-4.1", "rate one"), userHeaders)
	if first.Code != 200 {
		t.Fatal(first.Body.String())
	}
	second := request(t, h, "POST", "/v1/chat/completions", chatBody("gpt-4.1", "rate two"), userHeaders)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("rate status=%d body=%s", second.Code, second.Body.String())
	}
	if second.Header().Get("Retry-After") == "" {
		t.Fatalf("missing Retry-After")
	}

	status := decodeBody(t, request(t, h, "GET", "/admin/rate-limits", nil, adminHeaders))
	if status["rate_limits"].(map[string]any)["per_account_rpm"].(float64) != 1 {
		t.Fatalf("rate status=%v", status)
	}

	updated := request(t, h, "PUT", "/admin/rate-limits", map[string]any{"global_rpm": 10, "per_account_rpm": 5}, adminHeaders)
	if updated.Code != 200 {
		t.Fatal(updated.Body.String())
	}
}

func TestUsageAndUsers(t *testing.T) {
	_, h := testServer(t)
	request(t, h, "POST", "/v1/chat/completions", chatBody("gpt-4.1", "credit one"), userHeaders)
	request(t, h, "POST", "/v1/chat/completions", chatBody("gpt-4.1", "credit one"), userHeaders)
	usage := decodeBody(t, request(t, h, "GET", "/admin/usage?model=gpt-4.1", nil, adminHeaders))
	credits := usage["credits"].(float64)
	if usage["calls"].(float64) != 2 || credits < 0.009 || credits > 0.011 {
		t.Fatalf("usage=%v", usage)
	}
	created := request(t, h, "POST", "/admin/users", map[string]any{"id": "u_alice", "models": []string{"gpt-4.1"}}, adminHeaders)
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	models := decodeBody(t, request(t, h, "GET", "/admin/users/u_alice/models", nil, adminHeaders))
	if models["models"].([]any)[0] != "gpt-4.1" {
		t.Fatalf("models=%v", models)
	}
	deleted := request(t, h, "DELETE", "/admin/users/u_alice", nil, adminHeaders)
	if deleted.Code != 200 {
		t.Fatalf("delete=%d %s", deleted.Code, deleted.Body.String())
	}
}
