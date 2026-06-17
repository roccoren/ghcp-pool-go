package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/github/copilot-sdk/go"
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

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
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

func copilotModelFixture(id string, endpoints []string) map[string]any {
	return map[string]any{
		"id":                   id,
		"name":                 id,
		"version":              id + "-2026-06-01",
		"model_picker_enabled": true,
		"supported_endpoints":  endpoints,
		"capabilities": map[string]any{
			"family": "test",
			"limits": map[string]any{
				"max_context_window_tokens": 128000,
				"max_output_tokens":         16384,
				"max_prompt_tokens":         128000,
			},
			"supports": map[string]any{
				"streaming":   true,
				"tool_calls":  true,
				"vision":      false,
				"reasoning":   false,
				"attachments": false,
			},
		},
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
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

func TestEnvAPIKeysAcceptMultipleValues(t *testing.T) {
	t.Setenv("GHCP_API_KEYS", "sk-env-a, sk-env-b\nsk-env-c")
	settings, err := LoadSettings("/does/not/exist.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.APIKeys) != 3 {
		t.Fatalf("api keys=%d", len(settings.APIKeys))
	}
	gw, err := NewGateway(settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"sk-env-a", "sk-env-b", "sk-env-c"} {
		principal, ok := gw.Authenticator.Authenticate("Bearer " + key)
		if !ok || !principal.HasScope("admin") || !principal.HasScope("inference") {
			t.Fatalf("expected env API key %q to authenticate with admin and inference scopes", key)
		}
	}
	if _, ok := gw.Authenticator.Authenticate("Bearer sk-missing"); ok {
		t.Fatal("unexpected authentication with missing key")
	}
}

func TestLoadSettingsDefaultsCopilotProviderLogin(t *testing.T) {
	t.Setenv("GHCP_MODEL_MAP_PATH", ":memory:")
	settings, err := LoadSettings("/does/not/exist.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if settings.Login.ClientID != defaultCopilotOAuthClientID {
		t.Fatalf("client id=%q", settings.Login.ClientID)
	}
	if settings.Login.DeviceCodeURL != "https://github.com/login/device/code" || settings.Login.TokenURL != "https://github.com/login/oauth/access_token" {
		t.Fatalf("login urls=%q %q", settings.Login.DeviceCodeURL, settings.Login.TokenURL)
	}
	if settings.Copilot.Mode != copilotBackendModeSDK {
		t.Fatalf("mode=%q", settings.Copilot.Mode)
	}
	if settings.Copilot.AuthMode != copilotAuthModeExchange {
		t.Fatalf("auth mode=%q", settings.Copilot.AuthMode)
	}
}

func TestLoadSettingsOpencodeModeDefaultsOAuthProvider(t *testing.T) {
	t.Setenv("GHCP_MODEL_MAP_PATH", ":memory:")
	t.Setenv("GHCP_COPILOT_MODE", "opencode")
	settings, err := LoadSettings("/does/not/exist.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if settings.Copilot.Mode != copilotBackendModeOpencode {
		t.Fatalf("mode=%q", settings.Copilot.Mode)
	}
	if settings.Copilot.AuthMode != copilotAuthModeOAuth {
		t.Fatalf("auth mode=%q", settings.Copilot.AuthMode)
	}
	if settings.Copilot.BaseURL != copilotPublicAPIBaseURL {
		t.Fatalf("base url=%q", settings.Copilot.BaseURL)
	}
}

func TestLoadSettingsSDKWebSearchAndToolsFlags(t *testing.T) {
	t.Setenv("GHCP_MODEL_MAP_PATH", ":memory:")
	t.Setenv("GHCP_COPILOT_SDK_WEB_SEARCH", "true")
	t.Setenv("GHCP_COPILOT_SDK_AVAILABLE_TOOLS", "view,web_search,view")
	settings, err := LoadSettings("/does/not/exist.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Copilot.SDKWebSearch {
		t.Fatalf("sdk web search disabled")
	}
	if strings.Join(settings.Copilot.SDKTools, ",") != "view,web_search" {
		t.Fatalf("sdk tools=%v", settings.Copilot.SDKTools)
	}
	if settings.Copilot.SDKWebSearchMode != "empty" {
		t.Fatalf("sdk web search mode=%q", settings.Copilot.SDKWebSearchMode)
	}
	t.Setenv("GHCP_COPILOT_SDK_WEB_SEARCH_MODE", "cli")
	settings, err = LoadSettings("/does/not/exist.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if settings.Copilot.SDKWebSearchMode != "cli" {
		t.Fatalf("sdk web search mode override=%q", settings.Copilot.SDKWebSearchMode)
	}
	t.Setenv("GHCP_COPILOT_SDK_WEB_SEARCH_MODE", "native")
	settings, err = LoadSettings("/does/not/exist.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if settings.Copilot.SDKWebSearchMode != "native_cli" {
		t.Fatalf("sdk web search mode native alias=%q", settings.Copilot.SDKWebSearchMode)
	}
}

func TestLoadSettingsDerivesEnterpriseCopilotProviderURLs(t *testing.T) {
	t.Setenv("GHCP_MODEL_MAP_PATH", ":memory:")
	t.Setenv("GHCP_GITHUB_ENTERPRISE_URL", "https://ghe.example.com/org")
	t.Setenv("GHCP_COPILOT_MODE", "opencode")
	settings, err := LoadSettings("/does/not/exist.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if settings.Copilot.EnterpriseURL != "ghe.example.com" {
		t.Fatalf("enterprise=%q", settings.Copilot.EnterpriseURL)
	}
	if settings.Copilot.BaseURL != "https://copilot-api.ghe.example.com" {
		t.Fatalf("base url=%q", settings.Copilot.BaseURL)
	}
	if settings.Login.DeviceCodeURL != "https://ghe.example.com/login/device/code" || settings.Login.TokenURL != "https://ghe.example.com/login/oauth/access_token" {
		t.Fatalf("login urls=%q %q", settings.Login.DeviceCodeURL, settings.Login.TokenURL)
	}
	if settings.Copilot.Mode != copilotBackendModeOpencode {
		t.Fatalf("mode=%q", settings.Copilot.Mode)
	}
	if settings.Copilot.AuthMode != copilotAuthModeOAuth {
		t.Fatalf("auth mode=%q", settings.Copilot.AuthMode)
	}
}

func TestCopilotBackendOptionsAllowPerAccountModeOverride(t *testing.T) {
	options, err := copilotBackendOptions(CopilotConfig{Mode: copilotBackendModeSDK, AuthMode: copilotAuthModeExchange}, &AccountConfig{
		ID:          "acct-opencode",
		CopilotMode: "opencode",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.Mode != copilotBackendModeOpencode || options.AuthMode != copilotAuthModeOAuth || options.BaseURL != copilotPublicAPIBaseURL {
		t.Fatalf("options=%+v", options)
	}
}

func TestCopilotBackendOptionsAllowPerAccountSDKWebSearchOverride(t *testing.T) {
	enabled := true
	options, err := copilotBackendOptions(CopilotConfig{
		Mode:         copilotBackendModeSDK,
		AuthMode:     copilotAuthModeExchange,
		SDKWebSearch: false,
		SDKTools:     []string{"view"},
	}, &AccountConfig{
		ID:                      "acct-sdk",
		CopilotSDKWebSearch:     &enabled,
		CopilotSDKWebSearchMode: "native-cli",
		CopilotSDKTools:         []string{"grep"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !options.SDKWebSearch || options.SDKWebSearchMode != "native_cli" || strings.Join(options.SDKTools, ",") != "grep" {
		t.Fatalf("options=%+v", options)
	}
}

func TestLoginManagerUsesAccountEnterpriseURLs(t *testing.T) {
	settings := testSettings()
	pool, err := NewPoolManager(settings)
	if err != nil {
		t.Fatal(err)
	}
	account := pool.Get("acct_a")
	account.Config.GitHubEnterpriseURL = "https://ghe.example.com/org"
	login := NewLoginManager(settings, pool)
	deviceCodeURL, tokenURL := login.loginURLs(account)
	if deviceCodeURL != "https://ghe.example.com/login/device/code" || tokenURL != "https://ghe.example.com/login/oauth/access_token" {
		t.Fatalf("login urls=%q %q", deviceCodeURL, tokenURL)
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

func TestModelAliasesPersistAndLoad(t *testing.T) {
	path := t.TempDir() + "/model_map.json"
	settings := testSettings()
	settings.ModelMapPath = path
	gw, err := NewGateway(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.SetModelAliases(map[string]string{"display-model": "gpt-4.1"}); err != nil {
		t.Fatal(err)
	}
	aliases, err := LoadModelAliases(path)
	if err != nil {
		t.Fatal(err)
	}
	if aliases["display-model"] != "gpt-4.1" {
		t.Fatalf("aliases=%v", aliases)
	}

	t.Setenv("GHCP_MODEL_MAP_PATH", path)
	loaded, err := LoadSettings("/does/not/exist.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ModelAliases["display-model"] != "gpt-4.1" {
		t.Fatalf("loaded aliases=%v", loaded.ModelAliases)
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
	badPayload := decodeBody(t, bad)
	if badPayload["type"] != "error" || badPayload["error"].(map[string]any)["type"] != "invalid_request_error" {
		t.Fatalf("bad anthropic error shape=%v", badPayload)
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

func TestResponsesLifecycleAPI(t *testing.T) {
	gw, h := testServer(t)
	resp := request(t, h, "POST", "/v1/responses", map[string]any{"model": "gpt-4.1", "input": "remember me", "store": true}, userHeaders)
	if resp.Code != 200 {
		t.Fatal(resp.Body.String())
	}
	body := decodeBody(t, resp)
	id := body["id"].(string)

	got := request(t, h, "GET", "/v1/responses/"+id, nil, userHeaders)
	if got.Code != 200 {
		t.Fatalf("get response: %d %s", got.Code, got.Body.String())
	}
	retrieved := decodeBody(t, got)
	if retrieved["id"] != id || retrieved["object"] != "response" {
		t.Fatalf("retrieved=%v", retrieved)
	}

	itemsResp := request(t, h, "GET", "/v1/responses/"+id+"/input_items", nil, userHeaders)
	if itemsResp.Code != 200 {
		t.Fatalf("input items: %d %s", itemsResp.Code, itemsResp.Body.String())
	}
	items := decodeBody(t, itemsResp)
	data := items["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["type"] != "message" {
		t.Fatalf("input items=%v", items)
	}

	queuedID := "resp_cancel_test"
	gw.Responses.Store(map[string]any{"id": queuedID, "object": "response", "status": "in_progress", "model": "gpt-4.1", "output": []any{}, "output_text": ""}, nil)
	cancel := request(t, h, "POST", "/v1/responses/"+queuedID+"/cancel", nil, userHeaders)
	if cancel.Code != 200 {
		t.Fatalf("cancel: %d %s", cancel.Code, cancel.Body.String())
	}
	if decodeBody(t, cancel)["status"] != "cancelled" {
		t.Fatalf("cancel body=%v", decodeBody(t, cancel))
	}

	del := request(t, h, "DELETE", "/v1/responses/"+id, nil, userHeaders)
	if del.Code != 200 || decodeBody(t, del)["deleted"] != true {
		t.Fatalf("delete: %d %s", del.Code, del.Body.String())
	}
	missing := request(t, h, "GET", "/v1/responses/"+id, nil, userHeaders)
	if missing.Code != 404 || !strings.Contains(missing.Body.String(), "response not found") {
		t.Fatalf("missing: %d %s", missing.Code, missing.Body.String())
	}
}

func TestNativeOutputBlocksArePreserved(t *testing.T) {
	responses := responseResponse(ChatResult{
		ID:      "resp_native",
		Model:   "gpt-5.5",
		Content: "answer",
		Usage:   Usage{InputTokens: 1, OutputTokens: 1}.Normalized(),
		ResponsesOutput: []map[string]any{
			{"id": "rs_1", "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "thought"}}},
			{"id": "ws_1", "type": "web_search_call", "status": "completed"},
			{"id": "msg_1", "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "answer", "annotations": []any{}}}},
		},
	}, nil)
	output := responses["output"].([]any)
	if output[0].(map[string]any)["type"] != "reasoning" || output[1].(map[string]any)["type"] != "web_search_call" {
		t.Fatalf("responses output=%v", output)
	}

	anthropic := anthropicResponse(ChatResult{
		ID:           "msg_native",
		Model:        "claude-sonnet-4.6",
		Content:      "final",
		Usage:        Usage{InputTokens: 1, OutputTokens: 1}.Normalized(),
		FinishReason: "stop",
		AnthropicContent: []map[string]any{
			{"type": "thinking", "thinking": "private summary", "signature": "sig"},
			{"type": "text", "text": "final", "citations": []any{map[string]any{"type": "webpage_location", "url": "https://example.com"}}},
		},
	}, nil)
	blocks := anthropic["content"].([]any)
	if blocks[0].(map[string]any)["type"] != "thinking" || blocks[1].(map[string]any)["citations"] == nil {
		t.Fatalf("anthropic blocks=%v", blocks)
	}
}

func TestSDKSearchResultHelpers(t *testing.T) {
	raw := `https://duckduckgo.com/l/?uddg=https%3A%2F%2Fnews.microsoft.com%2Fbuild-2026-book-of-news%2F`
	if got := decodeDuckDuckGoURL(raw); got != "https://news.microsoft.com/build-2026-book-of-news/" {
		t.Fatalf("decoded=%q", got)
	}
	if got := normalizeSDKWebFetchURL("https://gh.hereis.app/microsoft/Build26-news/blob/main/news.md"); got != "https://raw.githubusercontent.com/microsoft/Build26-news/main/news.md" {
		t.Fatalf("normalized fetch url=%q", got)
	}
	if got := cleanHTMLText(`<b>Build</b> &amp; Fabric&nbsp;news`); got != "Build & Fabric news" {
		t.Fatalf("cleaned=%q", got)
	}
}

func TestAnthropicAuthAndModelNormalization(t *testing.T) {
	_, h := testServer(t)
	unauth := request(t, h, "POST", "/v1/messages", map[string]any{"model": "claude-3.5", "max_tokens": 1, "messages": []map[string]any{{"role": "user", "content": "hi"}}}, nil)
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d body=%s", unauth.Code, unauth.Body.String())
	}
	body := decodeBody(t, unauth)
	if body["type"] != "error" || body["error"].(map[string]any)["type"] != "authentication_error" {
		t.Fatalf("auth error shape=%v", body)
	}

	if got := normalizeAnthropicRequestedModel("claude-sonnet-4-6-20260101", "context-1m-2025-08-07"); got != "claude-sonnet-4.6-1m" {
		t.Fatalf("normalized=%q", got)
	}
	if got := normalizeAnthropicRequestedModel("claude-haiku-4-5-20251001", ""); got != "claude-haiku-4.5" {
		t.Fatalf("normalized=%q", got)
	}

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader("null"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-user")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "JSON object") {
		t.Fatalf("null body response=%d %s", rr.Code, rr.Body.String())
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

func TestContextTierCanBeRequestedOrConfigured(t *testing.T) {
	settings := testSettings()
	settings.Accounts[1].Models = append(settings.Accounts[1].Models, "claude-opus-4.8")
	settings.ContextTiers = map[string]string{"claude-opus-4.8": "long_context"}
	gw, err := NewGateway(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Shutdown)
	principal := Principal{config: APIKeyConfig{Scopes: []string{"inference"}, ModelAllow: []string{"*"}, CacheNamespace: "test"}}
	msg := ChatMessage{Role: "user", Content: "hi", Raw: map[string]any{"role": "user", "content": "hi"}}

	plan, err := gw.Prepare(ChatCompletionRequest{Model: "claude-opus-4.8", Messages: []ChatMessage{msg}}, principal, "bypass")
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Params["context_tier"]; got != "long_context" {
		t.Fatalf("configured context_tier=%v", got)
	}

	plan, err = gw.Prepare(ChatCompletionRequest{Model: "gpt-4.1", ContextTier: "long_context", Messages: []ChatMessage{msg}}, principal, "bypass")
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Params["context_tier"]; got != "long_context" {
		t.Fatalf("request context_tier=%v", got)
	}

	if _, err := gw.Prepare(ChatCompletionRequest{Model: "gpt-4.1", ContextTier: "huge", Messages: []ChatMessage{msg}}, principal, "bypass"); err == nil {
		t.Fatalf("expected invalid context_tier error")
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

func TestSmartLoadBalancingUsesRecentRequestsAnd429s(t *testing.T) {
	settings := testSettings()
	gw, err := NewGateway(settings)
	if err != nil {
		t.Fatal(err)
	}
	a := gw.Pool.Get("acct_a")
	b := gw.Pool.Get("acct_b")
	for i := 0; i < 10; i++ {
		a.RecordSuccess()
	}
	if selected := gw.Router.pick([]*Account{a, b}, "smart"); selected.ID() != "acct_b" {
		t.Fatalf("selected=%s want acct_b", selected.ID())
	}
	b.RecordFailure("copilot upstream error 429: too many requests")
	if selected := gw.Router.pick([]*Account{a, b}, "smart"); selected.ID() != "acct_a" {
		t.Fatalf("selected=%s want acct_a after 429 penalty", selected.ID())
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

func TestWriteStreamEmitsKeepaliveDuringSilence(t *testing.T) {
	orig := streamKeepaliveInterval
	streamKeepaliveInterval = 20 * time.Millisecond
	defer func() { streamKeepaliveInterval = orig }()

	stream := make(chan string)
	go func() {
		time.Sleep(80 * time.Millisecond) // stay silent across several intervals
		stream <- "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		close(stream)
	}()
	rr := httptest.NewRecorder()
	writeStream(rr, map[string]string{}, stream, func() {})
	body := rr.Body.String()
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("expected keepalive heartbeat during silence, got %q", body)
	}
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("expected real data to follow keepalive, got %q", body)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestResponseReplayEventsEmitsWebSearchCall(t *testing.T) {
	seq := 0
	events, output := responseReplayEvents("Here are the results.", nil, true, &seq)
	if len(output) < 2 {
		t.Fatalf("expected web_search_call and message output items, got %v", output)
	}
	first, _ := output[0].(map[string]any)
	if first["type"] != "web_search_call" || first["status"] != "completed" {
		t.Fatalf("first output item should be a completed web_search_call, got %v", output[0])
	}
	var sawAdded, sawDone bool
	for _, event := range events {
		if event["type"] == "response.output_item.added" {
			if item, ok := event["item"].(map[string]any); ok && item["type"] == "web_search_call" {
				sawAdded = true
			}
		}
		if event["type"] == "response.output_item.done" {
			if item, ok := event["item"].(map[string]any); ok && item["type"] == "web_search_call" {
				sawDone = true
			}
		}
	}
	if !sawAdded || !sawDone {
		t.Fatalf("expected web_search_call output_item.added and done events, added=%v done=%v", sawAdded, sawDone)
	}
	// Without the flag the search item must not appear (parity with non-search turns).
	seq = 0
	_, plainOutput := responseReplayEvents("Plain answer.", nil, false, &seq)
	for _, item := range plainOutput {
		if m, ok := item.(map[string]any); ok && m["type"] == "web_search_call" {
			t.Fatalf("web_search_call must not be emitted when webSearch is false")
		}
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

func TestWebSearchToolPreservedAcrossPayloads(t *testing.T) {
	tools := normalizeTools([]map[string]any{
		{"type": "web_search_preview", "search_context_size": "low"},
		{"type": "function", "function": map[string]any{"name": "lookup", "description": "Lookup", "parameters": map[string]any{"type": "object"}}},
	})
	if len(tools) != 2 || tools[0]["type"] != "web_search_preview" || tools[1]["name"] != "lookup" {
		t.Fatalf("normalized tools=%v", tools)
	}
	responses := responsesPayload("gpt-5.5", []NeutralMessage{{Role: "user", Content: "hi"}}, map[string]any{"tools": tools}, false)
	responseTools := responses["tools"].([]map[string]any)
	if responseTools[0]["type"] != "web_search_preview" || responseTools[0]["search_context_size"] != "low" {
		t.Fatalf("responses tools=%v", responseTools)
	}
	anthropic := anthropicPayload("claude-sonnet-4.6", []NeutralMessage{{Role: "user", Content: "hi"}}, map[string]any{"tools": []map[string]any{{"type": "web_search_20250305", "name": "web_search"}}, "response_options": map[string]any{"max_tokens": 64}}, false)
	anthropicTools := anthropic["tools"].([]map[string]any)
	if anthropicTools[0]["type"] != "web_search_20250305" || anthropicTools[0]["name"] != "web_search" {
		t.Fatalf("anthropic tools=%v", anthropicTools)
	}
}

func TestResponsesPayloadPreservesNativeOptions(t *testing.T) {
	payload := responsesPayload("gpt-5.5", []NeutralMessage{{Role: "user", Content: "hi"}}, map[string]any{
		"reasoning_effort": "high",
		"response_options": map[string]any{
			"context_tier":     "long_context",
			"include":          []any{"custom.include"},
			"metadata":         map[string]any{"session": "s1"},
			"prompt_cache_key": "cache-key",
			"service_tier":     "flex",
			"store":            true,
			"text":             map[string]any{"verbosity": "low"},
			"truncation":       "auto",
		},
	}, false)
	if payload["store"] != true || payload["prompt_cache_key"] != "cache-key" || payload["service_tier"] != "flex" {
		t.Fatalf("native options not preserved: %v", payload)
	}
	if payload["context_tier"] != "long_context" {
		t.Fatalf("context_tier=%v", payload["context_tier"])
	}
	if payload["text"].(map[string]any)["verbosity"] != "low" {
		t.Fatalf("text options=%v", payload["text"])
	}
	reasoning := payload["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning=%v", reasoning)
	}
	include := payload["include"].([]any)
	if len(include) != 1 || include[0] != "custom.include" {
		t.Fatalf("include should preserve caller value: %v", include)
	}
}

func TestResponsesPayloadPreservesRawNativeShape(t *testing.T) {
	raw := map[string]any{
		"model":            "display-model",
		"stream":           false,
		"cache":            "bypass",
		"max_tokens":       123,
		"reasoning_effort": "high",
		"input": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "look"},
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64,abc"},
			}},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "done"},
		},
		"tools": []any{
			map[string]any{"type": "local_shell"},
			map[string]any{"type": "function", "name": "read_file", "parameters": map[string]any{"type": "object"}},
		},
	}
	payload := responsesPayload("gpt-5.5", []NeutralMessage{{Role: "user", Content: "lossy fallback"}}, map[string]any{
		"reasoning_effort":        "high",
		internalResponsesRawParam: raw,
	}, true)

	if payload["model"] != "gpt-5.5" || payload["stream"] != true {
		t.Fatalf("routing fields not overridden: %v", payload)
	}
	if payload["cache"] != nil || payload["reasoning_effort"] != nil || payload["max_tokens"] != nil {
		t.Fatalf("unsupported fields leaked upstream: %v", payload)
	}
	if payload["max_output_tokens"] != 123 {
		t.Fatalf("max_output_tokens=%v", payload["max_output_tokens"])
	}
	reasoning := payload["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning=%v", reasoning)
	}
	input := payload["input"].([]any)
	if input[1].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("native input was not preserved: %v", input)
	}
	tools := payload["tools"].([]any)
	if tools[0].(map[string]any)["type"] != "local_shell" {
		t.Fatalf("native tools were not preserved: %v", tools)
	}
}

func TestResponsesStreamErrorUsesNestedMessage(t *testing.T) {
	body := strings.Join([]string{
		"event: response.failed",
		"data: {\"type\":\"response.failed\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"bad native field\"}}",
		"",
	}, "\n")
	out := make(chan StreamItem)
	go func() {
		defer close(out)
		streamResponsesSSE(context.Background(), strings.NewReader(body), out)
	}()
	for item := range out {
		if item.Err != nil {
			if !strings.Contains(item.Err.Error(), "bad native field") {
				t.Fatalf("error=%v", item.Err)
			}
			return
		}
	}
	t.Fatal("expected stream error")
}

func TestCopilotHeadersUseProtocolMetadata(t *testing.T) {
	responsesBody := map[string]any{
		"input": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "look"}}},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "ok"}}},
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/responses", nil)
	addCopilotHeaders(req, "tok", endpointResponses, copilotRequestMetadata(responsesBody, endpointResponses))
	if req.Header.Get("Openai-Intent") != "conversation-edits" || req.Header.Get("X-Github-Api-Version") != "2026-06-01" {
		t.Fatalf("headers=%v", req.Header)
	}
	if req.Header.Get("x-initiator") != "agent" {
		t.Fatalf("x-initiator=%q", req.Header.Get("x-initiator"))
	}
	if got := copilotRequestMetadata(map[string]any{"input": "plain prompt"}, endpointResponses).Initiator; got != "user" {
		t.Fatalf("string-input initiator=%q", got)
	}
	toolOutput := map[string]any{
		"input": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "look"}}},
			map[string]any{"type": "function_call_output", "call_id": "call", "output": "done"},
		},
	}
	if got := copilotRequestMetadata(toolOutput, endpointResponses).Initiator; got != "agent" {
		t.Fatalf("tool-output initiator=%q", got)
	}

	visionBody := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,abc"}}}}},
	}
	visionReq := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
	addCopilotHeaders(visionReq, "tok", endpointChatCompletions, copilotRequestMetadata(visionBody, endpointChatCompletions))
	if visionReq.Header.Get("x-initiator") != "user" || visionReq.Header.Get("Copilot-Vision-Request") != "true" {
		t.Fatalf("vision headers=%v", visionReq.Header)
	}

	anthropicReq := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	addCopilotHeaders(anthropicReq, "tok", endpointMessages, copilotRequestMetadata(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call", "content": "done"}}}},
	}, endpointMessages))
	if anthropicReq.Header.Get("x-initiator") != "agent" || anthropicReq.Header.Get("anthropic-beta") == "" {
		t.Fatalf("anthropic headers=%v", anthropicReq.Header)
	}
}

func TestCopilotDirectOAuthModeUsesProviderBaseURL(t *testing.T) {
	backend := NewCopilotBackendWithOptions("acct", "gh-oauth-token", "", CopilotBackendOptions{
		Mode:    copilotBackendModeOpencode,
		BaseURL: "https://api.githubcopilot.com/",
	})
	access, err := backend.validAccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if backend.mode != copilotBackendModeOpencode || backend.authMode != copilotAuthModeOAuth {
		t.Fatalf("backend mode/auth=%q/%q", backend.mode, backend.authMode)
	}
	if access.Token != "gh-oauth-token" || access.BaseURL != "https://api.githubcopilot.com" {
		t.Fatalf("access=%+v", access)
	}
}

func TestCopilotSDKModeListsModelsOnlyThroughSDK(t *testing.T) {
	oldClient := copilotHTTPClient
	defer func() {
		copilotHTTPClient = oldClient
	}()
	copilotHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("sdk model discovery should not call Copilot HTTP: %s", req.URL.String())
		return nil, nil
	})}

	backend := NewCopilotBackend("acct", "gho_oauth_token", "")
	backend.sdkClient = sdk.NewClient(&sdk.ClientOptions{OnListModels: func(context.Context) ([]sdk.ModelInfo, error) {
		return []sdk.ModelInfo{
			{ID: "claude-sonnet-4.6", Name: "Claude Sonnet 4.6"},
			{ID: "gpt-5.5", Name: "GPT-5.5"},
		}, nil
	}})
	backend.sdkReady = true
	specs, err := backend.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("specs=%v", specs)
	}
	if specs[0].ID != "claude-sonnet-4.6" || !supportedEndpoint(specs[0], endpointMessages) {
		t.Fatalf("claude spec=%+v", specs[0])
	}
	if specs[1].ID != "gpt-5.5" || !supportedEndpoint(specs[1], endpointResponses) {
		t.Fatalf("gpt spec=%+v", specs[1])
	}
}

func TestCopilotSDKModeEmptyModelListFails(t *testing.T) {
	backend := NewCopilotBackend("acct", "gho_oauth_token", "")
	backend.sdkClient = sdk.NewClient(&sdk.ClientOptions{OnListModels: func(context.Context) ([]sdk.ModelInfo, error) {
		return nil, nil
	}})
	backend.sdkReady = true
	_, err := backend.ListModels(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sdk model discovery returned no models") {
		t.Fatalf("err=%v", err)
	}
}

func TestCopilotSDKModeBestEffortRequestsWithoutHTTP(t *testing.T) {
	oldClient := copilotHTTPClient
	defer func() { copilotHTTPClient = oldClient }()
	copilotHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("sdk mode should not call Copilot HTTP: %s", req.URL.String())
		return nil, nil
	})}

	backend := NewCopilotBackend("acct", "gh-token", "")
	cases := []struct {
		name     string
		messages []NeutralMessage
		params   map[string]any
	}{
		{
			name:     "multi-message chat",
			messages: []NeutralMessage{{Role: "system", Content: "rules"}, {Role: "user", Content: "hello"}},
		},
		{
			name:     "sampling param",
			messages: []NeutralMessage{{Role: "user", Content: "hello"}},
			params:   map[string]any{"max_tokens": 10},
		},
		{
			name:     "responses endpoint",
			messages: []NeutralMessage{{Role: "user", Content: "hello"}},
			params:   map[string]any{internalEndpointParam: endpointResponses},
		},
		{
			name:     "messages endpoint",
			messages: []NeutralMessage{{Role: "user", Content: "hello"}},
			params:   map[string]any{internalEndpointParam: endpointMessages},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !backend.canUseSDK(tc.messages, tc.params) {
				t.Fatalf("expected SDK best-effort support")
			}
		})
	}

	result := applySDKOutputConstraints(ChatResult{Content: "one two three stop four", FinishReason: "stop", Usage: Usage{InputTokens: 1, OutputTokens: 5}}, map[string]any{"max_tokens": 2, "stop": []any{"stop"}})
	if result.Content != "one two" || result.FinishReason != "length" {
		t.Fatalf("constrained result=%+v", result)
	}
}

func TestCopilotSDKModeAcceptsToolRequestsWithoutDirectHTTP(t *testing.T) {
	oldClient := copilotHTTPClient
	defer func() { copilotHTTPClient = oldClient }()
	copilotHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("sdk mode should not call Copilot HTTP: %s", req.URL.String())
		return nil, nil
	})}

	backend := NewCopilotBackend("acct", "gh-token", "")
	params := map[string]any{
		"tools": []map[string]any{{"type": "function", "function": map[string]any{"name": "lookup"}}},
	}
	req := ChatCompletionRequest{Model: "gpt-5-mini", Messages: []ChatMessage{{Role: "user", Content: "hello", Raw: map[string]any{"role": "user", "content": "hello"}}}, Tools: params["tools"].([]map[string]any)}
	cfg := backend.sdkSessionConfig("gpt-5-mini", req.SamplingParams(), false)
	if len(cfg.Tools) != 1 || cfg.Tools[0].Name != "lookup" {
		t.Fatalf("sdk tools=%v", cfg.Tools)
	}
	if strings.Join(cfg.AvailableTools, ",") != "lookup" {
		t.Fatalf("available tools=%v", cfg.AvailableTools)
	}
	if backend.canUseSDK([]NeutralMessage{{Role: "user", Content: "hello"}}, map[string]any{"n": 2}) {
		t.Fatalf("n>1 cannot be satisfied through SDK")
	}
	if _, err := backend.Embeddings(context.Background(), "text-embedding", []string{"hello"}, nil); err != ErrEmbeddingsUnsupported {
		t.Fatalf("embeddings err=%v", err)
	}
}

func TestCopilotModelParserFiltersIncompleteAndDisabledRecords(t *testing.T) {
	disabled := copilotModelFixture("disabled-model", []string{"/chat/completions"})
	disabled["policy"] = map[string]any{"state": "disabled"}
	incomplete := map[string]any{"id": "incomplete-internal-model", "capabilities": map[string]any{"supports": map[string]any{}}}
	hidden := copilotModelFixture("utility-model", []string{"/chat/completions"})
	hidden["model_picker_enabled"] = false
	specs, err := parseCopilotModels(mustJSON(t, map[string]any{"data": []any{
		copilotModelFixture("gpt-5.5", []string{"/responses"}),
		disabled,
		incomplete,
		hidden,
	}}))
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	var hiddenSpec ModelSpec
	for _, spec := range specs {
		ids = append(ids, spec.ID)
		if spec.ID == "utility-model" {
			hiddenSpec = spec
		}
	}
	if strings.Join(ids, ",") != "gpt-5.5,utility-model" {
		t.Fatalf("ids=%v", ids)
	}
	if hiddenSpec.ModelPickerEnabled == nil || *hiddenSpec.ModelPickerEnabled {
		t.Fatalf("hidden picker flag=%v", hiddenSpec.ModelPickerEnabled)
	}
}

func TestVisibleModelsHideNonPickerUtilityModels(t *testing.T) {
	gw, err := NewGateway(testSettings())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Shutdown)
	hidden := false
	gw.Registry.index = map[string]map[string]ModelSpec{
		"gpt-4.1":       {"acct_a": {ID: "gpt-4.1"}},
		"utility-model": {"acct_a": {ID: "utility-model", ModelPickerEnabled: &hidden}},
	}
	models := strings.Join(gw.Registry.VisibleModels(), ",")
	if models != "gpt-4.1" {
		t.Fatalf("visible models=%q", models)
	}
}

func TestCopilotEndpointHeuristicMatchesOpenCodeProvider(t *testing.T) {
	if !copilotPrefersResponses("gpt-5") || !copilotPrefersResponses("gpt-5.1-codex") {
		t.Fatalf("expected gpt-5 class models to prefer responses")
	}
	if copilotPrefersResponses("gpt-5-mini") || copilotPrefersResponses("gpt-5-mini-2025-08-07") || copilotPrefersResponses("gpt-4.1") {
		t.Fatalf("expected mini/older models to stay chat-first")
	}
}

func TestCopilotPayloadsAddNoopToolWhenReplayingToolCalls(t *testing.T) {
	messages := []NeutralMessage{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_1", "name": "weather", "arguments": "{}"}}},
		{Role: "tool", ToolCallID: "call_1", Content: "sunny"},
	}
	chat := chatPayload("gpt-4.1", messages, map[string]any{}, false)
	if tools, ok := chat["tools"].([]map[string]any); !ok || len(tools) != 1 || tools[0]["type"] != "function" {
		t.Fatalf("chat tools=%v", chat["tools"])
	}
	responses := responsesPayload("gpt-5", messages, map[string]any{}, false)
	if tools, ok := responses["tools"].([]map[string]any); !ok || len(tools) != 1 || tools[0]["name"] != "_noop" {
		t.Fatalf("responses tools=%v", responses["tools"])
	}
	anthropic := anthropicPayload("claude-sonnet-4.5", messages, map[string]any{"response_options": map[string]any{"max_tokens": 64}}, false)
	if tools, ok := anthropic["tools"].([]map[string]any); !ok || len(tools) != 1 || tools[0]["name"] != "_noop" {
		t.Fatalf("anthropic tools=%v", anthropic["tools"])
	}
}

func TestModelSupportsEndpointUsesRegistryMetadata(t *testing.T) {
	registry := NewModelRegistry(nil)
	registry.index = map[string]map[string]ModelSpec{
		"anthropic-bridge": {
			"acct": {ID: "anthropic-bridge", SupportedEndpoints: []string{endpointMessages}},
		},
	}
	if !registry.ModelSupportsEndpoint("anthropic-bridge", endpointMessages) {
		t.Fatalf("expected /v1/messages support")
	}
	if registry.ModelSupportsEndpoint("anthropic-bridge", endpointResponses) {
		t.Fatalf("did not expect /responses support")
	}
}

func TestCopilotNativeThinkingNormalizesClaudeCodeShape(t *testing.T) {
	thinking, outputConfig := copilotNativeThinking(map[string]any{"type": "enabled", "budget_tokens": float64(1024)}, nil, "claude-sonnet-4.6")
	tm := thinking.(map[string]any)
	if tm["type"] != "adaptive" {
		t.Fatalf("thinking=%v", thinking)
	}
	if _, ok := tm["budget_tokens"]; ok {
		t.Fatalf("budget_tokens should be removed: %v", thinking)
	}
	om := outputConfig.(map[string]any)
	if om["effort"] != "low" {
		t.Fatalf("output_config=%v", outputConfig)
	}

	haikuThinking, haikuConfig := copilotNativeThinking(map[string]any{"type": "enabled", "budget_tokens": float64(8000)}, map[string]any{"effort": "medium"}, "claude-haiku-4.5")
	if haikuThinking != nil {
		t.Fatalf("haiku thinking should be stripped: %v", haikuThinking)
	}
	if hm, ok := haikuConfig.(map[string]any); ok {
		if _, hasEffort := hm["effort"]; hasEffort {
			t.Fatalf("haiku output_config should not include effort: %v", haikuConfig)
		}
	}
}

func TestSDKReasoningEffortStrippedForHaiku(t *testing.T) {
	backend := NewCopilotBackend("acct", "gh-token", "")
	haiku := &sdk.SessionConfig{Model: "claude-haiku-4.5"}
	backend.applySDKModelOptions(haiku, map[string]any{"reasoning_effort": "medium"})
	if haiku.ReasoningEffort != "" {
		t.Fatalf("haiku reasoning effort should be stripped, got %q", haiku.ReasoningEffort)
	}

	sonnet := &sdk.SessionConfig{Model: "claude-sonnet-4.6"}
	backend.applySDKModelOptions(sonnet, map[string]any{"reasoning_effort": "medium"})
	if sonnet.ReasoningEffort != "medium" {
		t.Fatalf("sonnet reasoning effort=%q", sonnet.ReasoningEffort)
	}
}

func TestNativeAnthropicPayloadPreservesRawBlocks(t *testing.T) {
	raw := map[string]any{
		"model": "claude-sonnet-4.6",
		"cache": "no-store",
		"system": []any{
			map[string]any{"type": "text", "text": "system", "cache_control": map[string]any{"type": "ephemeral", "scope": "workspace"}},
		},
		"context_management": []any{map[string]any{"type": "clear"}},
		"thinking":           map[string]any{"type": "enabled", "budget_tokens": float64(1024)},
		"messages": []any{
			map[string]any{"role": "system", "content": "system in messages"},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "hello", "cache_control": map[string]any{"type": "ephemeral", "scope": "tool"}},
					map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "abc"}},
				},
			},
		},
	}
	payload := normalizeNativeAnthropicPayload(raw, "claude-sonnet-4.6", true, map[string]any{"context_tier": "long_context"})
	if payload["stream"] != true {
		t.Fatalf("stream=%v", payload["stream"])
	}
	if payload["context_tier"] != "long_context" {
		t.Fatalf("context_tier=%v", payload["context_tier"])
	}
	if _, ok := payload["context_management"]; ok {
		t.Fatalf("context_management should be stripped: %v", payload)
	}
	if _, ok := payload["cache"]; ok {
		t.Fatalf("gateway cache control should be stripped: %v", payload)
	}
	msgs := payload["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] == "system" {
		t.Fatalf("system messages should be hoisted: %v", msgs)
	}
	systemBlocks := payload["system"].([]any)
	if len(systemBlocks) == 0 {
		t.Fatalf("system should be preserved/hoisted: %v", payload["system"])
	}
	content := msgs[0].(map[string]any)["content"].([]any)
	if content[1].(map[string]any)["type"] != "image" {
		t.Fatalf("raw image block not preserved: %v", content)
	}
	cc := content[0].(map[string]any)["cache_control"].(map[string]any)
	if _, ok := cc["scope"]; ok {
		t.Fatalf("cache_control.scope should be stripped: %v", cc)
	}
	thinking := payload["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" || thinking["budget_tokens"] != nil {
		t.Fatalf("thinking=%v", thinking)
	}
	outputConfig := payload["output_config"].(map[string]any)
	if outputConfig["effort"] != "low" {
		t.Fatalf("output_config=%v", outputConfig)
	}
}

func TestSanitizedAnthropicRawCacheKeyStability(t *testing.T) {
	withScope := map[string]any{
		"model": "claude-sonnet-4.6",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":          "text",
				"text":          "hello",
				"cache_control": map[string]any{"type": "ephemeral", "scope": "tool"},
			}},
		}},
	}
	withoutScope := map[string]any{
		"model": "claude-sonnet-4.6",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":          "text",
				"text":          "hello",
				"cache_control": map[string]any{"type": "ephemeral"},
			}},
		}},
	}
	reqA := ChatCompletionRequest{Model: "claude-sonnet-4.6", AnthropicRaw: sanitizeNativeAnthropicRaw(withScope)}
	reqB := ChatCompletionRequest{Model: "claude-sonnet-4.6", AnthropicRaw: sanitizeNativeAnthropicRaw(withoutScope)}
	if toJSONString(reqA.SamplingParams()[internalAnthropicRawParam]) != toJSONString(reqB.SamplingParams()[internalAnthropicRawParam]) {
		t.Fatalf("sanitized raw payloads should match:\n%s\n%s", toJSONString(reqA.SamplingParams()[internalAnthropicRawParam]), toJSONString(reqB.SamplingParams()[internalAnthropicRawParam]))
	}
}

func TestClientBadRequestDoesNotCooldownAccount(t *testing.T) {
	settings := testSettings()
	gw, err := NewGateway(settings)
	if err != nil {
		t.Fatal(err)
	}
	account := gw.Pool.Get("acct_a")
	account.RecordFailure("copilot upstream error 400: invalid_request_error")
	if account.Failures != 0 {
		t.Fatalf("failures=%d want 0", account.Failures)
	}
	if remaining := time.Until(account.Cooldown); remaining > 0 {
		t.Fatalf("unexpected cooldown %v", remaining)
	}
	if !account.Available() {
		t.Fatalf("account should remain available")
	}
}

func TestAnthropicBackendInvalidRequestPreservesStatusAndShape(t *testing.T) {
	err := &CopilotUpstreamError{
		StatusCode: http.StatusBadRequest,
		Body:       []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad thinking"}}`),
	}
	if !isNonRetryableBackendError(err) {
		t.Fatalf("expected non-retryable")
	}
	status, errorType, message := anthropicErrorFromBackend(err)
	if status != http.StatusBadRequest || errorType != "invalid_request_error" || message != "bad thinking" {
		t.Fatalf("status=%d type=%q message=%q", status, errorType, message)
	}
	forbidden := &CopilotUpstreamError{
		StatusCode: http.StatusForbidden,
		Body:       []byte(`{"type":"error","error":{"type":"permission_error","message":"account forbidden"}}`),
	}
	if isNonRetryableBackendError(forbidden) {
		t.Fatalf("403 should remain retryable across accounts")
	}
	status, errorType, message = anthropicErrorFromBackend(&CopilotUpstreamError{
		StatusCode: http.StatusBadRequest,
		Body:       []byte(`{"message":"adaptive thinking is not supported on this model"}`),
	})
	if status != http.StatusBadRequest || errorType != "invalid_request_error" || !strings.Contains(message, "adaptive thinking") {
		t.Fatalf("status=%d type=%q message=%q", status, errorType, message)
	}
}

func TestCopilotDirectRetriesTransient529(t *testing.T) {
	oldClient := copilotHTTPClient
	defer func() { copilotHTTPClient = oldClient }()
	attempts := 0
	copilotHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return &http.Response{
				StatusCode: 529,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"Overloaded"}}`)),
			}, nil
		}
		return &http.Response{
			StatusCode: 200,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"model":"gpt-5.5","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)),
		}, nil
	})}
	backend := NewCopilotBackendWithOptions("acct", "gh-token", "", CopilotBackendOptions{Mode: copilotBackendModeOpencode, AuthMode: copilotAuthModeOAuth, BaseURL: "https://example.test"})
	result, err := backend.Chat(context.Background(), "gpt-5.5", []NeutralMessage{{Role: "user", Content: "hi"}}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || result.Content != "ok" {
		t.Fatalf("attempts=%d result=%+v", attempts, result)
	}
}

func TestTransientOverloadClassification(t *testing.T) {
	if !isTransientOverloadError(&CopilotUpstreamError{StatusCode: 529, Body: []byte("overloaded")}) {
		t.Fatalf("529 should be transient overload")
	}
	if !isTransientOverloadError(errors.New("session error: Repeated 529 Overloaded errors. The API is at capacity")) {
		t.Fatalf("sdk overload string should be transient")
	}
	if isTransientOverloadError(&CopilotUpstreamError{StatusCode: 400, Body: []byte("bad request")}) {
		t.Fatalf("400 should not be transient overload")
	}
}

func TestSDKEligibilityRequiresSinglePlainUserMessage(t *testing.T) {
	backend := NewCopilotBackend("acct", "gh-token", "")
	if !backend.canUseSDK([]NeutralMessage{{Role: "user", Content: "hello"}}, map[string]any{}) {
		t.Fatalf("single plain user prompt should be SDK-eligible")
	}
	if !backend.canUseSDK([]NeutralMessage{{Role: "system", Content: "rules"}, {Role: "user", Content: "hello"}}, map[string]any{}) {
		t.Fatalf("roleful chat should be SDK-eligible via prompt folding")
	}
	if !backend.canUseSDK([]NeutralMessage{{Role: "user", Content: "hello"}}, map[string]any{"temperature": 0.1}) {
		t.Fatalf("sampling params should be SDK-eligible best-effort")
	}
	if !backend.canUseSDK([]NeutralMessage{{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_1", "name": "lookup", "arguments": "{}"}}}, {Role: "tool", ToolCallID: "call_1", Content: "ok"}}, map[string]any{"tools": []map[string]any{{"type": "function", "name": "lookup"}}}) {
		t.Fatalf("tool call replay should be SDK-eligible via prompt folding")
	}
	opencode := NewCopilotBackendWithOptions("acct", "gh-token", "", CopilotBackendOptions{Mode: copilotBackendModeOpencode})
	if opencode.canUseSDK([]NeutralMessage{{Role: "user", Content: "hello"}}, map[string]any{}) {
		t.Fatalf("opencode mode should not use SDK")
	}
	toolParams := map[string]any{"tools": []map[string]any{
		{"type": "function", "name": "lookup", "description": "Lookup", "parameters": map[string]any{"type": "object"}},
		{"type": "web_search_preview", "search_context_size": "low"},
	}}
	cfg := backend.sdkSessionConfig("gpt-4.1", toolParams, false)
	if len(cfg.Tools) != 1 || cfg.Tools[0].Name != "lookup" {
		t.Fatalf("sdk custom tools=%v", cfg.Tools)
	}
	if strings.Join(cfg.AvailableTools, ",") != "lookup" {
		t.Fatalf("request tools should be available to SDK, got %v", cfg.AvailableTools)
	}
	cliWeb := NewCopilotBackendWithOptions("acct", "gh-token", "", CopilotBackendOptions{SDKWebSearchMode: "cli"})
	cfg = cliWeb.sdkSessionConfig("gpt-4.1", toolParams, false)
	if strings.Join(cfg.AvailableTools, ",") != "ghcp_web_search,ghcp_web_fetch,ghcp_report_intent,lookup" {
		t.Fatalf("cli web search tools=%v", cfg.AvailableTools)
	}
	if !cliWeb.useSDKCLIWebSearch(toolParams) {
		t.Fatalf("expected cli web search path")
	}
	if !strings.Contains(cliWeb.sdkPrompt([]NeutralMessage{{Role: "user", Content: "search"}}, toolParams), "ghcp_web_search") {
		t.Fatalf("expected controlled cli web prompt")
	}
	nativeWeb := NewCopilotBackendWithOptions("acct", "gh-token", "", CopilotBackendOptions{SDKWebSearchMode: "native_cli"})
	cfg = nativeWeb.sdkSessionConfig("gpt-4.1", toolParams, false)
	if len(cfg.Tools) != 2 || cfg.Tools[0].Name != "web_fetch" || cfg.Tools[1].Name != "lookup" {
		t.Fatalf("native cli custom tools=%v", cfg.Tools)
	}
	if strings.Join(cfg.AvailableTools, ",") != "builtin:web_search,web_fetch,lookup" {
		t.Fatalf("native cli web search tools=%v", cfg.AvailableTools)
	}
	if cfg.OnPermissionRequest == nil {
		t.Fatalf("native cli web search should approve allowed native web tool permissions")
	}
	if !nativeWeb.useSDKCLIWebSearch(toolParams) || !nativeWeb.useSDKNativeCLIWebSearch(toolParams) || nativeWeb.useSDKControlledCLIWebSearch(toolParams) {
		t.Fatalf("expected native cli web search path")
	}
	if !isSDKNativeWebTool("web_fetch") || !isSDKNativeWebTool("builtin:web_search") || isSDKNativeWebTool("lookup") {
		t.Fatalf("native web tool filter mismatch")
	}
	if !strings.Contains(nativeWeb.sdkPrompt([]NeutralMessage{{Role: "user", Content: "search"}}, toolParams), "native web_search") {
		t.Fatalf("expected native cli web prompt")
	}
	cfg = backend.sdkSessionConfig("gpt-4.1", map[string]any{}, false)
	if cfg.AvailableTools == nil || len(cfg.AvailableTools) != 0 {
		t.Fatalf("empty SDK tool allowlist must be explicit, got %#v", cfg.AvailableTools)
	}
	withSearch := NewCopilotBackendWithOptions("acct", "gh-token", "", CopilotBackendOptions{SDKWebSearch: true})
	cfg = withSearch.sdkSessionConfig("gpt-4.1", map[string]any{}, false)
	if strings.Join(cfg.AvailableTools, ",") != "web_search" {
		t.Fatalf("available tools=%v", cfg.AvailableTools)
	}
	cfg = withSearch.sdkSessionConfig("gpt-4.1", map[string]any{"tool_choice": "none", "tools": []map[string]any{{"type": "web_search_preview"}}}, false)
	if len(cfg.AvailableTools) != 0 || len(cfg.Tools) != 0 {
		t.Fatalf("tool_choice none should disable SDK tools, available=%v custom=%v", cfg.AvailableTools, cfg.Tools)
	}
	withTools := NewCopilotBackendWithOptions("acct", "gh-token", "", CopilotBackendOptions{SDKWebSearch: true, SDKTools: []string{"view", "web_search", "view"}})
	cfg = withTools.sdkSessionConfig("gpt-4.1", map[string]any{}, false)
	if strings.Join(cfg.AvailableTools, ",") != "view,web_search" {
		t.Fatalf("available tools=%v", cfg.AvailableTools)
	}
	cfg = backend.sdkSessionConfig("claude-opus-4.8", map[string]any{"context_tier": "long_context"}, false)
	if cfg.ContextTier != sdk.ContextTierLongContext {
		t.Fatalf("context tier=%q", cfg.ContextTier)
	}
}

func TestCopilotOpenCodeListModelsFallsBackToPublicAPIBase(t *testing.T) {
	oldClient := copilotHTTPClient
	oldFallbacks := copilotModelListFallbackBaseURLs
	defer func() {
		copilotHTTPClient = oldClient
		copilotModelListFallbackBaseURLs = oldFallbacks
	}()

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusMisdirectedRequest, map[string]any{"error": "wrong host"}, nil)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer cp-token" {
			t.Fatalf("authorization=%q", got)
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{
			copilotModelFixture("gpt-5.5", []string{"/chat/completions"}),
			copilotModelFixture("claude-haiku-4.5", []string{"/v1/messages"}),
		}}, nil)
	}))
	defer fallback.Close()

	copilotModelListFallbackBaseURLs = []string{fallback.URL}
	backend := NewCopilotBackendWithOptions("acct", "", "", CopilotBackendOptions{Mode: copilotBackendModeOpencode})
	backend.access = copilotAccessToken{Token: "cp-token", BaseURL: primary.URL, ExpiresAt: time.Now().Add(time.Hour)}
	specs, err := backend.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, spec := range specs {
		ids = append(ids, spec.ID)
	}
	if strings.Join(ids, ",") != "gpt-5.5,claude-haiku-4.5" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestCopilotOpenCodeListModelsMergesPublicAPIBase(t *testing.T) {
	oldClient := copilotHTTPClient
	oldFallbacks := copilotModelListFallbackBaseURLs
	defer func() {
		copilotHTTPClient = oldClient
		copilotModelListFallbackBaseURLs = oldFallbacks
	}()

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{
			copilotModelFixture("gpt-4.1", []string{"/chat/completions"}),
		}}, nil)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{
			copilotModelFixture("gpt-4.1", []string{"/chat/completions"}),
			copilotModelFixture("gpt-5.5", []string{"/chat/completions"}),
			copilotModelFixture("claude-haiku-4.5", []string{"/v1/messages"}),
		}}, nil)
	}))
	defer fallback.Close()

	copilotModelListFallbackBaseURLs = []string{fallback.URL}
	backend := NewCopilotBackendWithOptions("acct", "", "", CopilotBackendOptions{Mode: copilotBackendModeOpencode})
	backend.access = copilotAccessToken{Token: "cp-token", BaseURL: primary.URL, ExpiresAt: time.Now().Add(time.Hour)}
	specs, err := backend.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, spec := range specs {
		ids = append(ids, spec.ID)
	}
	if strings.Join(ids, ",") != "gpt-4.1,gpt-5.5,claude-haiku-4.5" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestCopilotModelHeadersMatchKnownGoodModelDiscovery(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/models", nil)
	addCopilotModelHeaders(req, "tok")
	if got := req.Header.Get("Openai-Intent"); got != "" {
		t.Fatalf("Openai-Intent=%q", got)
	}
	if got := req.Header.Get("X-Github-Api-Version"); got != "2026-06-01" {
		t.Fatalf("X-Github-Api-Version=%q", got)
	}
	if got := req.Header.Get("x-initiator"); got != "" {
		t.Fatalf("x-initiator should not be set for model discovery, got %q", got)
	}
}

func TestMergeModelEndpointMetadata(t *testing.T) {
	specs := []ModelSpec{{
		ID:                 "gpt-5.5",
		SupportedEndpoints: []string{endpointChatCompletions},
		Capabilities:       map[string]any{"sdk": true},
	}}
	httpSpecs := []ModelSpec{{
		ID:                 "gpt-5.5",
		SupportedEndpoints: []string{endpointResponses, endpointChatCompletions},
		Capabilities:       map[string]any{"streaming": true, "sdk": false},
	}}
	mergeModelEndpointMetadata(specs, httpSpecs)
	if strings.Join(specs[0].SupportedEndpoints, ",") != endpointResponses+","+endpointChatCompletions {
		t.Fatalf("endpoints=%v", specs[0].SupportedEndpoints)
	}
	if specs[0].Capabilities["sdk"] != true || specs[0].Capabilities["streaming"] != true {
		t.Fatalf("capabilities=%v", specs[0].Capabilities)
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
	gw, h := testServer(t)
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
	overrides := request(t, h, "POST", "/admin/users", map[string]any{
		"id":                          "u_copilot",
		"models":                      []string{"gpt-4.1"},
		"copilot_mode":                "opencode",
		"copilot_auth_mode":           "oauth",
		"copilot_base_url":            "https://api.githubcopilot.com",
		"copilot_sdk_web_search":      true,
		"copilot_sdk_available_tools": []string{"web_search", "view"},
		"github_enterprise_url":       "https://ghe.example.com/org",
	}, adminHeaders)
	if overrides.Code != 200 {
		t.Fatal(overrides.Body.String())
	}
	cfg := gw.Pool.Get("u_copilot").Config
	if cfg.CopilotMode != "opencode" || cfg.CopilotAuthMode != "oauth" || cfg.CopilotBaseURL != "https://api.githubcopilot.com" {
		t.Fatalf("copilot overrides=%+v", cfg)
	}
	if cfg.CopilotSDKWebSearch == nil || !*cfg.CopilotSDKWebSearch || strings.Join(cfg.CopilotSDKTools, ",") != "web_search,view" {
		t.Fatalf("sdk overrides=%+v tools=%v", cfg.CopilotSDKWebSearch, cfg.CopilotSDKTools)
	}
	if cfg.GitHubEnterpriseURL != "https://ghe.example.com/org" {
		t.Fatalf("enterprise=%q", cfg.GitHubEnterpriseURL)
	}
	deleted := request(t, h, "DELETE", "/admin/users/u_alice", nil, adminHeaders)
	if deleted.Code != 200 {
		t.Fatalf("delete=%d %s", deleted.Code, deleted.Body.String())
	}
}
