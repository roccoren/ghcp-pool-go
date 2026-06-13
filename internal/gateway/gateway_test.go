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
