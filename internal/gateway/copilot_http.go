package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/github/copilot-sdk/go"
)

const (
	copilotTokenURL             = "https://api.github.com/copilot_internal/v2/token"
	defaultCopilotAPIBaseURL    = "https://api.individual.githubcopilot.com"
	copilotPublicAPIBaseURL     = "https://api.githubcopilot.com"
	defaultCopilotOAuthClientID = "Ov23li8tweQw6odWQebz"
	copilotBackendModeSDK       = "sdk"
	copilotBackendModeOpencode  = "opencode"
	copilotAuthModeExchange     = "exchange"
	copilotAuthModeOAuth        = "oauth"
	copilotUserAgent            = "GitHubCopilotChat/0.39.0"
	copilotEditorVersion        = "vscode/1.111.0"
	copilotPluginVersion        = "copilot-chat/0.39.0"
	copilotAPIVersion           = "2026-06-01"
	copilotIntent               = "conversation-edits"
)

var ValidCopilotBackendModes = map[string]bool{
	copilotBackendModeSDK:      true,
	copilotBackendModeOpencode: true,
}

var ValidCopilotAuthModes = map[string]bool{
	copilotAuthModeExchange: true,
	copilotAuthModeOAuth:    true,
}

var copilotHTTPClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       120 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
	},
}

var sdkWebHTTPClient = &http.Client{Timeout: 20 * time.Second}

var (
	ddgResultRE    = regexp.MustCompile(`(?is)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippetRE   = regexp.MustCompile(`(?is)<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	bingResultRE   = regexp.MustCompile(`(?is)<li class="b_algo".*?<a href="([^"]+)"[^>]*>(.*?)</a>.*?(?:<p>(.*?)</p>)?`)
	htmlTagStripRE = regexp.MustCompile(`(?is)<[^>]+>`)
)

var copilotModelListFallbackBaseURLs = []string{
	copilotPublicAPIBaseURL,
	defaultCopilotAPIBaseURL,
}

type CopilotBackend struct {
	accountID        string
	githubToken      string
	homeDir          string
	mode             string
	authMode         string
	baseURL          string
	sdkWebSearch     bool
	sdkWebSearchMode string
	sdkTools         []string

	mu        sync.Mutex
	access    copilotAccessToken
	sdkClient *sdk.Client
	sdkReady  bool
	sdkErr    error
}

type copilotAccessToken struct {
	Token     string
	BaseURL   string
	ExpiresAt time.Time
}

type copilotTokenExchangeError struct {
	StatusCode int
	Body       []byte
}

func (e *copilotTokenExchangeError) Error() string {
	return fmt.Sprintf("Copilot token request failed with status %d: %s", e.StatusCode, strings.TrimSpace(string(e.Body)))
}

type CopilotBackendOptions struct {
	Mode             string
	AuthMode         string
	BaseURL          string
	SDKWebSearch     bool
	SDKWebSearchMode string
	SDKTools         []string
}

func NewCopilotBackend(accountID, token, homeDir string) *CopilotBackend {
	return NewCopilotBackendWithOptions(accountID, token, homeDir, CopilotBackendOptions{})
}

func NewCopilotBackendWithOptions(accountID, token, homeDir string, options CopilotBackendOptions) *CopilotBackend {
	return &CopilotBackend{
		accountID:        accountID,
		githubToken:      strings.TrimSpace(token),
		homeDir:          homeDir,
		mode:             normalizeCopilotBackendMode(options.Mode),
		authMode:         normalizeCopilotAuthMode(defaultCopilotAuthMode(options.Mode, options.AuthMode)),
		baseURL:          normalizeBaseURLOrEmpty(options.BaseURL),
		sdkWebSearch:     options.SDKWebSearch,
		sdkWebSearchMode: normalizeCopilotSDKWebSearchMode(options.SDKWebSearchMode, options.SDKWebSearch),
		sdkTools:         normalizeStringList(options.SDKTools),
	}
}

func (b *CopilotBackend) Start(ctx context.Context) error {
	if !b.sdkConfigured() {
		return nil
	}
	b.mu.Lock()
	if b.sdkReady {
		b.mu.Unlock()
		return nil
	}
	if b.sdkClient == nil {
		b.sdkClient = sdk.NewClient(b.sdkClientOptions(sdk.ModeEmpty, ""))
	}
	client := b.sdkClient
	b.mu.Unlock()

	if err := client.Start(ctx); err != nil {
		b.mu.Lock()
		b.sdkReady = false
		b.sdkErr = err
		b.mu.Unlock()
		if b.githubToken != "" {
			return nil
		}
		return err
	}
	b.mu.Lock()
	b.sdkReady = true
	b.sdkErr = nil
	b.mu.Unlock()
	return nil
}

func (b *CopilotBackend) ListModels(ctx context.Context) ([]ModelSpec, error) {
	if b.mode != copilotBackendModeOpencode {
		specs, err := b.listModelsSDK(ctx)
		if err != nil {
			return nil, err
		}
		if len(specs) == 0 {
			return nil, fmt.Errorf("sdk model discovery returned no models")
		}
		return specs, nil
	}
	return b.listModelsDirect(ctx)
}

func (b *CopilotBackend) listModelsDirect(ctx context.Context) ([]ModelSpec, error) {
	access, err := b.validAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	merged := []ModelSpec{}
	seen := map[string]bool{}
	addSpecs := func(specs []ModelSpec) {
		for _, spec := range specs {
			if spec.ID == "" || seen[spec.ID] {
				continue
			}
			seen[spec.ID] = true
			merged = append(merged, spec)
		}
	}
	specs, err := b.listModelsHTTP(ctx, access.BaseURL, access.Token)
	directErr := err
	if err == nil {
		addSpecs(specs)
	}
	for _, baseURL := range copilotModelListFallbackBaseURLs {
		baseURL = normalizeBaseURL(baseURL)
		if baseURL == "" || baseURL == access.BaseURL {
			continue
		}
		if fallbackSpecs, fallbackErr := b.listModelsHTTP(ctx, baseURL, access.Token); fallbackErr == nil {
			addSpecs(fallbackSpecs)
		}
	}
	if len(merged) > 0 {
		return merged, nil
	}
	if directErr == nil {
		return nil, fmt.Errorf("copilot model discovery returned no models")
	}
	return nil, directErr
}

func mergeModelEndpointMetadata(specs []ModelSpec, httpSpecs []ModelSpec) {
	byID := map[string]ModelSpec{}
	for _, spec := range httpSpecs {
		if spec.ID != "" {
			byID[spec.ID] = spec
		}
	}
	for i := range specs {
		httpSpec := byID[specs[i].ID]
		if len(httpSpec.SupportedEndpoints) > 0 {
			specs[i].SupportedEndpoints = httpSpec.SupportedEndpoints
		}
		if httpSpec.ModelPickerEnabled != nil {
			specs[i].ModelPickerEnabled = httpSpec.ModelPickerEnabled
		}
		if httpSpec.Name != "" {
			specs[i].Name = httpSpec.Name
		}
		if httpSpec.Version != "" {
			specs[i].Version = httpSpec.Version
		}
		if len(httpSpec.Capabilities) > 0 {
			if specs[i].Capabilities == nil {
				specs[i].Capabilities = map[string]any{}
			}
			for key, value := range httpSpec.Capabilities {
				if _, exists := specs[i].Capabilities[key]; !exists {
					specs[i].Capabilities[key] = value
				}
			}
		}
	}
}

func (b *CopilotBackend) listModelsHTTP(ctx context.Context, baseURL, token string) ([]ModelSpec, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, normalizeBaseURL(baseURL)+"/models", nil)
	if err != nil {
		return nil, err
	}
	addCopilotModelHeaders(req, token)
	req.Header.Set("Accept", "application/json")
	resp, err := copilotHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("copilot models request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if err != nil {
		return nil, fmt.Errorf("read copilot models response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &CopilotUpstreamError{StatusCode: resp.StatusCode, Body: data}
	}
	return parseCopilotModels(data)
}

func parseCopilotModels(data []byte) ([]ModelSpec, error) {
	var payload struct {
		Data []struct {
			ID                 string   `json:"id"`
			Name               string   `json:"name"`
			Version            string   `json:"version"`
			ModelPickerEnabled *bool    `json:"model_picker_enabled"`
			SupportedEndpoints []string `json:"supported_endpoints"`
			Policy             struct {
				State string `json:"state"`
			} `json:"policy"`
			Capabilities map[string]any `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}
	specs := make([]ModelSpec, 0, len(payload.Data))
	for _, item := range payload.Data {
		if !usableCopilotModel(item.ID, item.Policy.State, item.Capabilities) {
			continue
		}
		capabilities := cloneMap(item.Capabilities)
		if capabilities == nil {
			capabilities = map[string]any{}
		}
		if item.Name != "" {
			capabilities["name"] = item.Name
		}
		if item.Version != "" {
			capabilities["version"] = item.Version
		}
		if item.ModelPickerEnabled != nil {
			capabilities["model_picker_enabled"] = *item.ModelPickerEnabled
		}
		specs = append(specs, ModelSpec{
			ID:                 item.ID,
			Name:               item.Name,
			Version:            item.Version,
			ModelPickerEnabled: item.ModelPickerEnabled,
			SupportedEndpoints: item.SupportedEndpoints,
			Capabilities:       capabilities,
		})
	}
	return specs, nil
}

func (b *CopilotBackend) Chat(ctx context.Context, model string, messages []NeutralMessage, params map[string]any) (ChatResult, error) {
	endpoint := endpointFromParams(params, endpointChatCompletions)
	clean := cleanBackendParams(params)
	if b.mode != copilotBackendModeOpencode {
		if b.canUseSDK(messages, clean) {
			return b.chatSDK(ctx, model, messages, clean, false)
		}
		return ChatResult{}, unsupportedSDKModeRequest(endpoint)
	}
	var body any
	switch {
	case endpointMatches(endpoint, endpointResponses):
		body = responsesPayload(model, messages, clean, false)
	case endpointMatches(endpoint, endpointMessages):
		body = anthropicPayload(model, messages, clean, false)
	default:
		endpoint = endpointChatCompletions
		body = chatPayload(model, messages, clean, false)
	}
	_, data, err := b.doCopilot(ctx, http.MethodPost, endpoint, body, false)
	if err != nil {
		return ChatResult{}, err
	}
	switch {
	case endpointMatches(endpoint, endpointResponses):
		return parseResponsesResult(data, model)
	case endpointMatches(endpoint, endpointMessages):
		return parseAnthropicResult(data, model)
	default:
		return parseChatResult(data, model)
	}
}

func (b *CopilotBackend) Embeddings(ctx context.Context, model string, inputs []string, params map[string]any) (EmbeddingResult, error) {
	if b.mode != copilotBackendModeOpencode {
		return EmbeddingResult{}, ErrEmbeddingsUnsupported
	}
	body := map[string]any{"model": model, "input": inputs}
	for _, key := range []string{"encoding_format", "dimensions", "user"} {
		if value := params[key]; value != nil {
			body[key] = value
		}
	}
	_, data, err := b.doCopilot(ctx, http.MethodPost, endpointEmbeddings, body, false)
	if err != nil {
		return EmbeddingResult{}, err
	}
	var payload struct {
		Model string `json:"model"`
		Data  []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage openAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return EmbeddingResult{}, fmt.Errorf("decode embeddings response: %w", err)
	}
	vectors := make([][]float64, 0, len(payload.Data))
	for _, item := range payload.Data {
		vectors = append(vectors, item.Embedding)
	}
	return EmbeddingResult{
		Model:      firstNonEmpty(payload.Model, model),
		Embeddings: vectors,
		Usage: Usage{
			InputTokens:  payload.Usage.PromptTokens,
			TotalTokens:  payload.Usage.TotalTokens,
			CachedTokens: payload.Usage.CachedTokens(),
			APIEndpoint:  endpointEmbeddings,
		}.Normalized(),
	}, nil
}

func (b *CopilotBackend) ChatStream(ctx context.Context, model string, messages []NeutralMessage, params map[string]any) (<-chan StreamItem, error) {
	endpoint := endpointFromParams(params, endpointChatCompletions)
	clean := cleanBackendParams(params)
	if b.mode != copilotBackendModeOpencode {
		if b.canUseSDK(messages, clean) {
			return b.chatStreamSDK(ctx, model, messages, clean)
		}
		return nil, unsupportedSDKModeRequest(endpoint)
	}
	var body any
	switch {
	case endpointMatches(endpoint, endpointResponses):
		body = responsesPayload(model, messages, clean, true)
	case endpointMatches(endpoint, endpointMessages):
		body = anthropicPayload(model, messages, clean, true)
	default:
		endpoint = endpointChatCompletions
		body = chatPayload(model, messages, clean, true)
	}
	resp, _, err := b.doCopilot(ctx, http.MethodPost, endpoint, body, true)
	if err != nil {
		return nil, err
	}
	out := make(chan StreamItem)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		switch {
		case endpointMatches(endpoint, endpointResponses):
			streamResponsesSSE(ctx, resp.Body, out)
		case endpointMatches(endpoint, endpointMessages):
			streamAnthropicSSE(ctx, resp.Body, out)
		default:
			streamChatSSE(ctx, resp.Body, out)
		}
	}()
	return out, nil
}

func unsupportedSDKModeRequest(endpoint string) error {
	return fmt.Errorf("copilot sdk mode does not support this request shape on endpoint %q; use sdk-compatible single-turn chat or explicit opencode mode for direct API behavior", endpoint)
}

func (b *CopilotBackend) Close() error {
	b.mu.Lock()
	client := b.sdkClient
	b.sdkClient = nil
	b.sdkReady = false
	b.mu.Unlock()
	if client != nil {
		return client.Stop()
	}
	return nil
}

func (b *CopilotBackend) sdkGitHubToken() string {
	if looksLikeCopilotBearer(b.githubToken) {
		return ""
	}
	return b.githubToken
}

func (b *CopilotBackend) sdkConfigured() bool {
	if b.mode == copilotBackendModeOpencode {
		return false
	}
	return b.sdkGitHubToken() != "" || b.homeDir != ""
}

func (b *CopilotBackend) sdk(ctx context.Context) (*sdk.Client, error) {
	b.mu.Lock()
	if b.sdkReady && b.sdkClient != nil {
		client := b.sdkClient
		b.mu.Unlock()
		return client, nil
	}
	b.mu.Unlock()
	if err := b.Start(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sdkReady && b.sdkClient != nil {
		return b.sdkClient, nil
	}
	if b.sdkErr != nil {
		return nil, b.sdkErr
	}
	if b.sdkClient == nil {
		return nil, fmt.Errorf("sdk client not available")
	}
	return nil, fmt.Errorf("sdk client not connected")
}

func (b *CopilotBackend) sdkForParams(ctx context.Context, params map[string]any) (*sdk.Client, func(), error) {
	if !b.useSDKCLIWebSearch(params) {
		client, err := b.sdk(ctx)
		return client, func() {}, err
	}
	client := sdk.NewClient(b.sdkClientOptions(sdk.ModeCopilotCli, "cli-web-search"))
	if err := client.Start(ctx); err != nil {
		return nil, func() {}, err
	}
	return client, func() { _ = client.Stop() }, nil
}

func (b *CopilotBackend) sdkClientOptions(mode sdk.ClientMode, suffix string) *sdk.ClientOptions {
	baseDir := b.homeDir
	if baseDir == "" {
		baseDir = "runtime-home/" + b.accountID
	}
	if suffix != "" {
		baseDir = filepath.Join(baseDir, suffix)
	}
	return &sdk.ClientOptions{
		BaseDirectory:             baseDir,
		GitHubToken:               b.sdkGitHubToken(),
		UseLoggedInUser:           sdk.Bool(b.sdkGitHubToken() == ""),
		Mode:                      mode,
		SessionIdleTimeoutSeconds: 60,
	}
}

func (b *CopilotBackend) listModelsSDK(ctx context.Context) ([]ModelSpec, error) {
	client, err := b.sdk(ctx)
	if err != nil {
		return nil, err
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	specs := make([]ModelSpec, 0, len(models))
	for _, model := range models {
		specs = append(specs, ModelSpec{
			ID:                 model.ID,
			SupportedEndpoints: copilotSDKModelEndpoints(model.ID),
			Capabilities: map[string]any{
				"name":                        model.Name,
				"supports_reasoning_effort":   model.Capabilities.Supports.ReasoningEffort,
				"supports_vision":             model.Capabilities.Supports.Vision,
				"supported_reasoning_efforts": model.SupportedReasoningEfforts,
				"default_reasoning_effort":    model.DefaultReasoningEffort,
				"max_context_window_tokens":   ptrValue(model.Capabilities.Limits.MaxContextWindowTokens),
				"max_prompt_tokens":           ptrValue(model.Capabilities.Limits.MaxPromptTokens),
			},
		})
	}
	return specs, nil
}

func copilotSDKModelEndpoints(model string) []string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(model, "claude"):
		return []string{endpointMessages, endpointChatCompletions}
	case strings.Contains(model, "embed"):
		return []string{endpointEmbeddings}
	case copilotPrefersResponses(model):
		return []string{endpointResponses, endpointChatCompletions}
	default:
		return []string{endpointChatCompletions, endpointResponses}
	}
}

func (b *CopilotBackend) canUseSDK(messages []NeutralMessage, params map[string]any) bool {
	if !b.sdkConfigured() {
		return false
	}
	if params["n"] != nil && intFromAny(params["n"], 1) != 1 {
		return false
	}
	return true
}

func (b *CopilotBackend) chatSDK(ctx context.Context, model string, messages []NeutralMessage, params map[string]any, stream bool) (ChatResult, error) {
	var lastErr error
	for attempt := 0; attempt < transientRetryAttempts(params); attempt++ {
		result, err := b.chatSDKOnce(ctx, model, messages, params, stream)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !isTransientOverloadError(err) || attempt == transientRetryAttempts(params)-1 {
			return ChatResult{}, err
		}
		if !sleepTransientRetry(ctx, attempt) {
			return ChatResult{}, ctx.Err()
		}
	}
	return ChatResult{}, lastErr
}

func (b *CopilotBackend) chatSDKOnce(ctx context.Context, model string, messages []NeutralMessage, params map[string]any, stream bool) (ChatResult, error) {
	client, cleanup, err := b.sdkForParams(ctx, params)
	if err != nil {
		return ChatResult{}, err
	}
	defer cleanup()
	session, err := client.CreateSession(ctx, b.sdkSessionConfig(model, params, stream))
	if err != nil {
		return ChatResult{}, err
	}
	defer session.Disconnect()
	prompt := b.sdkPrompt(messages, params)
	result, err := b.sendSDKAndCollect(ctx, session, model, prompt, params)
	if err != nil {
		return ChatResult{}, err
	}
	return applySDKOutputConstraints(result, params), nil
}

func transientRetryAttempts(params map[string]any) int {
	if requestHasWebSearchTool(params) {
		return 2
	}
	return 3
}

func sleepTransientRetry(ctx context.Context, attempt int) bool {
	delay := time.Duration(750*(attempt+1)) * time.Millisecond
	select {
	case <-time.After(delay):
		return true
	case <-ctx.Done():
		return false
	}
}

func (b *CopilotBackend) sendSDKAndCollect(ctx context.Context, session *sdk.Session, model, prompt string, params map[string]any) (ChatResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		timeout := 60 * time.Second
		if b.useSDKCLIWebSearch(params) {
			timeout = 180 * time.Second
		}
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	endpoint := sdkUsageEndpoint(params)
	result := ChatResult{
		Model:        model,
		FinishReason: "stop",
		Usage:        Usage{InputTokens: approxTokens(prompt), APIEndpoint: endpoint}.Normalized(),
	}
	var mu sync.Mutex
	idleCh := make(chan struct{}, 1)
	toolCh := make(chan struct{}, 1)
	errCh := make(chan error, 1)
	signal := func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	unsubscribe := session.On(func(event sdk.SessionEvent) {
		switch data := event.Data.(type) {
		case *sdk.AssistantMessageData:
			mu.Lock()
			result.Content = data.Content
			if data.Model != nil && *data.Model != "" {
				result.Model = *data.Model
			}
			if data.OutputTokens != nil {
				result.Usage.OutputTokens = int(*data.OutputTokens)
			}
			for _, request := range data.ToolRequests {
				if b.useSDKControlledCLIWebSearch(params) && isSDKWebSearchInternalTool(request.Name) {
					continue
				}
				if b.useSDKNativeCLIWebSearch(params) && isSDKNativeWebTool(request.Name) {
					continue
				}
				result.ToolCalls = appendToolCallUnique(result.ToolCalls, toolCallFromSDKRequest(request))
			}
			hasTools := len(result.ToolCalls) > 0
			mu.Unlock()
			if hasTools {
				signal(toolCh)
			}
		case *sdk.ExternalToolRequestedData:
			if b.useSDKControlledCLIWebSearch(params) && isSDKWebSearchInternalTool(data.ToolName) {
				return
			}
			if b.useSDKNativeCLIWebSearch(params) && isSDKNativeWebTool(data.ToolName) {
				return
			}
			mu.Lock()
			result.ToolCalls = appendToolCallUnique(result.ToolCalls, toolCallFromSDKExternalTool(data))
			mu.Unlock()
			signal(toolCh)
		case *sdk.AssistantUsageData:
			mu.Lock()
			result.Usage = mergeSDKUsage(result.Usage, sdkUsageFromEvent(data, endpoint))
			if data.Model != "" {
				result.Model = data.Model
			}
			mu.Unlock()
		case *sdk.SessionIdleData:
			signal(idleCh)
		case *sdk.SessionErrorData:
			select {
			case errCh <- fmt.Errorf("session error: %s", data.Message):
			default:
			}
		}
	})
	defer unsubscribe()

	if _, err := session.Send(ctx, sdk.MessageOptions{Prompt: prompt}); err != nil {
		return ChatResult{}, err
	}

	select {
	case <-toolCh:
		waitForSDKToolSettle(ctx, toolCh)
		abortCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = session.Abort(abortCtx)
		cancel()
		mu.Lock()
		out := finalizeSDKResult(result, prompt, endpoint)
		mu.Unlock()
		out.FinishReason = "tool_calls"
		return out, nil
	case <-idleCh:
		mu.Lock()
		out := finalizeSDKResult(result, prompt, endpoint)
		mu.Unlock()
		return out, nil
	case err := <-errCh:
		return ChatResult{}, err
	case <-ctx.Done():
		return ChatResult{}, fmt.Errorf("waiting for sdk session: %w", ctx.Err())
	}
}

func waitForSDKToolSettle(ctx context.Context, toolCh <-chan struct{}) {
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-toolCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(50 * time.Millisecond)
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

func finalizeSDKResult(result ChatResult, prompt, endpoint string) ChatResult {
	if result.Model == "" {
		result.Model = "unknown"
	}
	if result.FinishReason == "" {
		result.FinishReason = "stop"
	}
	if len(result.ToolCalls) > 0 {
		result.FinishReason = "tool_calls"
	}
	if result.Usage.APIEndpoint == "" {
		result.Usage.APIEndpoint = endpoint
	}
	if result.Usage.InputTokens == 0 {
		result.Usage.InputTokens = approxTokens(prompt)
	}
	if result.Usage.OutputTokens == 0 && result.Content != "" {
		result.Usage.OutputTokens = approxTokens(result.Content)
	}
	result.Usage = result.Usage.Normalized()
	return result
}

func appendToolCallUnique(calls []ToolCall, next ToolCall) []ToolCall {
	next = next.normalized()
	if next.Name == "" {
		return calls
	}
	for i := range calls {
		if calls[i].ID != "" && next.ID != "" && calls[i].ID == next.ID {
			if calls[i].Name == "" {
				calls[i].Name = next.Name
			}
			if calls[i].Arguments == "" || calls[i].Arguments == "{}" {
				calls[i].Arguments = next.Arguments
			}
			if calls[i].Kind == "" {
				calls[i].Kind = next.Kind
			}
			return calls
		}
	}
	return append(calls, next)
}

func toolCallFromSDKRequest(request sdk.AssistantMessageToolRequest) ToolCall {
	kind := "function"
	if request.Type != nil && string(*request.Type) != "" {
		kind = string(*request.Type)
	}
	return ToolCall{
		ID:        firstNonEmpty(request.ToolCallID, newID("call")),
		Name:      request.Name,
		Arguments: toolArgumentsString(request.Arguments),
		Kind:      kind,
	}
}

func toolCallFromSDKExternalTool(request *sdk.ExternalToolRequestedData) ToolCall {
	if request == nil {
		return ToolCall{}
	}
	return ToolCall{
		ID:        firstNonEmpty(request.ToolCallID, newID("call")),
		Name:      request.ToolName,
		Arguments: toolArgumentsString(request.Arguments),
		Kind:      "function",
	}
}

func toolArgumentsString(value any) string {
	switch v := value.(type) {
	case nil:
		return "{}"
	case string:
		if strings.TrimSpace(v) == "" {
			return "{}"
		}
		return v
	default:
		data, err := json.Marshal(v)
		if err != nil || len(data) == 0 {
			return "{}"
		}
		return string(data)
	}
}

func sdkUsageEndpoint(params map[string]any) string {
	return endpointFromParams(params, endpointChatCompletions)
}

func sdkUsageFromEvent(data *sdk.AssistantUsageData, fallbackEndpoint string) Usage {
	if data == nil {
		return Usage{APIEndpoint: fallbackEndpoint}
	}
	endpoint := fallbackEndpoint
	if data.APIEndpoint != nil && string(*data.APIEndpoint) != "" {
		endpoint = string(*data.APIEndpoint)
	}
	usage := Usage{
		APIEndpoint: endpoint,
	}
	if data.InputTokens != nil {
		usage.InputTokens = int(*data.InputTokens)
	}
	if data.OutputTokens != nil {
		usage.OutputTokens = int(*data.OutputTokens)
	}
	if data.CacheReadTokens != nil {
		usage.CachedTokens = int(*data.CacheReadTokens)
	}
	if data.Cost != nil {
		usage.Credits = *data.Cost
	}
	if data.Duration != nil {
		usage.DurationMS = int(*data.Duration)
	}
	if data.ProviderCallID != nil {
		usage.ProviderCallID = *data.ProviderCallID
	} else if data.APICallID != nil {
		usage.ProviderCallID = *data.APICallID
	}
	if len(data.QuotaSnapshots) > 0 {
		if raw, err := json.Marshal(data.QuotaSnapshots); err == nil {
			usage.QuotaSnapshots = raw
		}
	}
	return usage.Normalized()
}

func mergeSDKUsage(base, next Usage) Usage {
	if next.APIEndpoint != "" {
		base.APIEndpoint = next.APIEndpoint
	}
	if next.InputTokens != 0 {
		base.InputTokens = next.InputTokens
	}
	if next.OutputTokens != 0 {
		base.OutputTokens = next.OutputTokens
	}
	if next.TotalTokens != 0 {
		base.TotalTokens = next.TotalTokens
	}
	if next.CachedTokens != 0 {
		base.CachedTokens = next.CachedTokens
	}
	if next.Credits != 0 {
		base.Credits = next.Credits
	}
	if next.DurationMS != 0 {
		base.DurationMS = next.DurationMS
	}
	if next.ProviderCallID != "" {
		base.ProviderCallID = next.ProviderCallID
	}
	if len(next.QuotaSnapshots) > 0 {
		base.QuotaSnapshots = next.QuotaSnapshots
	}
	return base.Normalized()
}

func (b *CopilotBackend) chatStreamSDK(ctx context.Context, model string, messages []NeutralMessage, params map[string]any) (<-chan StreamItem, error) {
	result, err := b.chatSDK(ctx, model, messages, params, false)
	if err != nil {
		return nil, err
	}
	out := make(chan StreamItem)
	go func() {
		defer close(out)
		if result.Content != "" {
			select {
			case out <- StreamItem{Kind: "delta", Text: result.Content}:
			case <-ctx.Done():
				emitStreamItem(context.Background(), out, StreamItem{Kind: "error", Err: ctx.Err()})
				return
			}
		}
		for i, tc := range result.ToolCalls {
			if !emitStreamItem(ctx, out, StreamItem{Kind: "tool_call", ToolCall: tc, Index: i}) {
				return
			}
		}
		emitStreamItem(ctx, out, StreamItem{Kind: "done", Usage: result.Usage, FinishReason: result.FinishReason})
	}()
	return out, nil
}

func (b *CopilotBackend) sdkPrompt(messages []NeutralMessage, params map[string]any) string {
	prompt := ""
	if len(messages) == 1 && strings.EqualFold(messages[0].Role, "user") {
		prompt = messages[0].Content
	} else {
		prompt = renderPrompt(messages)
	}
	format := anyMap(params["response_format"])
	if typ := strings.ToLower(stringFromAny(format["type"])); strings.Contains(typ, "json") {
		prompt = strings.TrimSpace(prompt) + "\n\nRespond with valid JSON only."
	}
	if instruction := b.sdkToolChoiceInstruction(params); instruction != "" {
		prompt = strings.TrimSpace(prompt) + "\n\n" + instruction
	}
	return prompt
}

func (b *CopilotBackend) sdkToolChoiceInstruction(params map[string]any) string {
	if !hasSDKRequestTools(params) || toolChoiceIsNone(params["tool_choice"]) {
		return ""
	}
	if requestHasWebSearchTool(params) {
		if b.useSDKControlledCLIWebSearch(params) {
			return "Use the ghcp_web_search tool for web searches and ghcp_web_fetch for fetching URLs. Do not use built-in web_search or web_fetch tools."
		}
		if b.useSDKNativeCLIWebSearch(params) {
			return "Use Copilot CLI native web_search for web searches and native web_fetch for fetching URLs. Do not use ghcp_web_search or ghcp_web_fetch tools."
		}
		return "Use the web_search tool for web searches and web_fetch for fetching URLs if it is available."
	}
	if name := forcedToolName(params["tool_choice"]); name != "" {
		return "Use the " + name + " tool for this turn and return the tool request if more information is needed."
	}
	switch choice := strings.ToLower(stringFromAny(params["tool_choice"])); choice {
	case "required", "any":
		return "Use one of the available tools for this turn and return the tool request if more information is needed."
	default:
		return ""
	}
}

func applySDKOutputConstraints(result ChatResult, params map[string]any) ChatResult {
	if len(result.ToolCalls) > 0 {
		result.FinishReason = "tool_calls"
		result.Usage = result.Usage.Normalized()
		return result
	}
	content, stopped := applyStopSequences(result.Content, params["stop"])
	if stopped {
		result.Content = content
		result.FinishReason = "stop"
	}
	if maxTokens := sdkMaxOutputTokens(params); maxTokens > 0 {
		if truncated, ok := truncateApproxTokens(result.Content, maxTokens); ok {
			result.Content = truncated
			result.FinishReason = "length"
		}
	}
	result.Usage.OutputTokens = approxTokens(result.Content)
	result.Usage = result.Usage.Normalized()
	return result
}

func sdkMaxOutputTokens(params map[string]any) int {
	if n := intFromAny(params["max_tokens"], 0); n > 0 {
		return n
	}
	options := anyMap(params["response_options"])
	return intFromAny(firstAny(options["max_tokens"], options["max_output_tokens"]), 0)
}

func applyStopSequences(content string, value any) (string, bool) {
	stops := []string{}
	switch v := value.(type) {
	case nil:
	case string:
		stops = append(stops, v)
	case []string:
		stops = append(stops, v...)
	case []any:
		for _, item := range v {
			if stop := stringFromAny(item); stop != "" {
				stops = append(stops, stop)
			}
		}
	}
	cut := -1
	for _, stop := range stops {
		if stop == "" {
			continue
		}
		if idx := strings.Index(content, stop); idx >= 0 && (cut < 0 || idx < cut) {
			cut = idx
		}
	}
	if cut < 0 {
		return content, false
	}
	return content[:cut], true
}

func truncateApproxTokens(content string, maxTokens int) (string, bool) {
	if maxTokens <= 0 {
		return content, false
	}
	fields := strings.Fields(content)
	if len(fields) <= maxTokens {
		return content, false
	}
	return strings.Join(fields[:maxTokens], " "), true
}

func (b *CopilotBackend) sdkSessionConfig(model string, params map[string]any, stream bool) *sdk.SessionConfig {
	if b.useSDKControlledCLIWebSearch(params) {
		cfg := &sdk.SessionConfig{
			ClientName:     "ghcp-pool-go",
			Model:          model,
			Streaming:      sdk.Bool(stream),
			Tools:          sdkCLIWebSearchTools(),
			AvailableTools: b.sdkAvailableToolsForParams(params),
		}
		b.applySDKModelOptions(cfg, params)
		return cfg
	}
	if b.useSDKNativeCLIWebSearch(params) {
		cfg := &sdk.SessionConfig{
			ClientName:          "ghcp-pool-go",
			Model:               model,
			Streaming:           sdk.Bool(stream),
			Tools:               sdkCustomToolsFromParams(params),
			AvailableTools:      b.sdkAvailableToolsForParams(params),
			OnPermissionRequest: sdk.PermissionHandler.ApproveAll,
		}
		b.applySDKModelOptions(cfg, params)
		return cfg
	}
	cfg := &sdk.SessionConfig{
		ClientName:              "ghcp-pool-go",
		Model:                   model,
		Streaming:               sdk.Bool(stream),
		Tools:                   sdkCustomToolsFromParams(params),
		AvailableTools:          b.sdkAvailableToolsForParams(params),
		EnableConfigDiscovery:   sdk.Bool(false),
		SkipEmbeddingRetrieval:  sdk.Bool(true),
		EmbeddingCacheStorage:   sdk.String("in-memory"),
		EnableFileHooks:         sdk.Bool(false),
		EnableHostGitOperations: sdk.Bool(false),
		EnableSessionStore:      sdk.Bool(false),
		EnableSkills:            sdk.Bool(false),
	}
	b.applySDKModelOptions(cfg, params)
	return cfg
}

func (b *CopilotBackend) applySDKModelOptions(cfg *sdk.SessionConfig, params map[string]any) {
	if effort := stringParam(params, "reasoning_effort"); effort != "" && effort != "none" {
		if modelSupportsSDKReasoningEffort(cfg.Model) {
			cfg.ReasoningEffort = effort
		}
	}
	if tier := stringParam(params, "context_tier"); tier != "" && tier != "default" {
		cfg.ContextTier = sdk.ContextTier(tier)
	}
}

func modelSupportsSDKReasoningEffort(model string) bool {
	model = strings.ToLower(model)
	return !strings.HasPrefix(model, "claude-haiku-")
}

func (b *CopilotBackend) sdkAvailableTools() []string {
	tools := normalizeStringList(b.sdkTools)
	if b.sdkWebSearch && b.sdkWebSearchMode == "empty" && !containsString(tools, "web_search") && !containsString(tools, "builtin:web_search") {
		tools = append(tools, "web_search")
	}
	if tools == nil {
		return []string{}
	}
	return tools
}

func (b *CopilotBackend) sdkAvailableToolsForParams(params map[string]any) []string {
	if toolChoiceIsNone(params["tool_choice"]) {
		return []string{}
	}
	if b.useSDKNativeCLIWebSearch(params) {
		tools := []string{"builtin:web_search", "builtin:web_fetch"}
		for _, tool := range sdkCustomToolsFromParams(params) {
			name := "custom:" + tool.Name
			if tool.Name != "" && !containsString(tools, name) {
				tools = append(tools, name)
			}
		}
		return tools
	}
	if b.useSDKControlledCLIWebSearch(params) {
		tools := []string{"ghcp_web_search", "ghcp_web_fetch", "ghcp_report_intent"}
		for _, tool := range sdkCustomToolsFromParams(params) {
			if tool.Name != "" && !containsString(tools, tool.Name) && !containsString(tools, "custom:"+tool.Name) {
				tools = append(tools, tool.Name)
			}
		}
		return tools
	}
	tools := b.sdkAvailableTools()
	if b.sdkWebSearchMode == "empty" && requestHasWebSearchTool(params) && !containsString(tools, "web_search") && !containsString(tools, "builtin:web_search") {
		tools = append(tools, "web_search")
	}
	for _, tool := range sdkCustomToolsFromParams(params) {
		if tool.Name != "" && !containsString(tools, tool.Name) && !containsString(tools, "custom:"+tool.Name) {
			tools = append(tools, tool.Name)
		}
	}
	if tools == nil {
		return []string{}
	}
	tools = normalizeStringList(tools)
	if tools == nil {
		return []string{}
	}
	return tools
}

func (b *CopilotBackend) useSDKCLIWebSearch(params map[string]any) bool {
	return (b.sdkWebSearchMode == "cli" || b.sdkWebSearchMode == "native_cli") && requestHasWebSearchTool(params)
}

func (b *CopilotBackend) useSDKControlledCLIWebSearch(params map[string]any) bool {
	return b.sdkWebSearchMode == "cli" && requestHasWebSearchTool(params)
}

func (b *CopilotBackend) useSDKNativeCLIWebSearch(params map[string]any) bool {
	return b.sdkWebSearchMode == "native_cli" && requestHasWebSearchTool(params)
}

func sdkCLIWebSearchTools() []sdk.Tool {
	return []sdk.Tool{
		{
			Name:        "ghcp_web_search",
			Description: "Search the web for current information and return relevant results. Use this instead of built-in web_search.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":   map[string]any{"type": "string", "description": "Search query"},
					"queries": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Search queries"},
				},
			},
			OverridesBuiltInTool: true,
			SkipPermission:       true,
			Handler:              sdkWebSearchHandler,
		},
		{
			Name:                 "ghcp_report_intent",
			Description:          "Report intent before using web tools.",
			Parameters:           map[string]any{"type": "object", "properties": map[string]any{"intent": map[string]any{"type": "string"}}},
			OverridesBuiltInTool: true,
			SkipPermission:       true,
			Handler: func(sdk.ToolInvocation) (sdk.ToolResult, error) {
				return sdk.ToolResult{TextResultForLLM: "Intent acknowledged.", ResultType: "success"}, nil
			},
		},
		{
			Name:        "ghcp_web_fetch",
			Description: "Fetch a web page by URL and return text for the model. Use this instead of built-in web_fetch.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url":        map[string]any{"type": "string", "description": "URL to fetch"},
					"max_length": map[string]any{"type": "integer", "description": "Maximum number of characters to return"},
				},
				"required": []string{"url"},
			},
			OverridesBuiltInTool: true,
			SkipPermission:       true,
			Handler:              sdkWebFetchHandler,
		},
	}
}

func isSDKWebSearchInternalTool(name string) bool {
	switch name {
	case "ghcp_report_intent", "ghcp_web_fetch", "ghcp_web_search":
		return true
	default:
		return false
	}
}

func isSDKNativeWebTool(name string) bool {
	switch strings.TrimPrefix(name, "builtin:") {
	case "web_fetch", "web_search":
		return true
	default:
		return false
	}
}

func sdkWebSearchHandler(inv sdk.ToolInvocation) (sdk.ToolResult, error) {
	args := anyMap(inv.Arguments)
	queries := []string{}
	if query := stringFromAny(args["query"]); query != "" {
		queries = append(queries, query)
	}
	for _, item := range anySlice(args["queries"]) {
		if query := stringFromAny(item); query != "" {
			queries = append(queries, query)
		}
	}
	if len(queries) == 0 {
		return sdk.ToolResult{TextResultForLLM: "missing query", ResultType: "failure", Error: "missing query"}, nil
	}
	parts := []string{}
	for _, query := range queries {
		results, err := performSDKWebSearch(inv.TraceContext, query, 5)
		if err != nil {
			parts = append(parts, fmt.Sprintf("Query: %s\nError: %s", query, err.Error()))
			continue
		}
		if len(results) == 0 {
			parts = append(parts, "Query: "+query+"\nNo results found.")
			continue
		}
		lines := []string{"Query: " + query}
		for i, result := range results {
			line := fmt.Sprintf("%d. %s\n   URL: %s", i+1, result.Title, result.URL)
			if result.Snippet != "" {
				line += "\n   Snippet: " + result.Snippet
			}
			lines = append(lines, line)
		}
		parts = append(parts, strings.Join(lines, "\n"))
	}
	return sdk.ToolResult{TextResultForLLM: strings.Join(parts, "\n\n"), ResultType: "success"}, nil
}

type sdkSearchResult struct {
	Title   string
	URL     string
	Snippet string
}

func performSDKWebSearch(ctx context.Context, query string, limit int) ([]sdkSearchResult, error) {
	if limit <= 0 {
		limit = 5
	}
	if results, err := searchDuckDuckGo(ctx, query, limit); err == nil && len(results) > 0 {
		return results, nil
	}
	return searchBing(ctx, query, limit)
}

func searchDuckDuckGo(ctx context.Context, query string, limit int) ([]sdkSearchResult, error) {
	searchURL := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	body, err := fetchSearchPage(ctx, searchURL)
	if err != nil {
		return nil, err
	}
	matches := ddgResultRE.FindAllStringSubmatch(body, limit)
	snippets := ddgSnippetRE.FindAllStringSubmatch(body, limit)
	results := make([]sdkSearchResult, 0, len(matches))
	for i, match := range matches {
		resultURL := decodeDuckDuckGoURL(match[1])
		title := cleanHTMLText(match[2])
		if title == "" || resultURL == "" {
			continue
		}
		snippet := ""
		if i < len(snippets) {
			snippet = cleanHTMLText(snippets[i][1])
		}
		results = append(results, sdkSearchResult{Title: title, URL: resultURL, Snippet: snippet})
	}
	return results, nil
}

func searchBing(ctx context.Context, query string, limit int) ([]sdkSearchResult, error) {
	searchURL := "https://www.bing.com/search?q=" + url.QueryEscape(query)
	body, err := fetchSearchPage(ctx, searchURL)
	if err != nil {
		return nil, err
	}
	matches := bingResultRE.FindAllStringSubmatch(body, limit)
	results := make([]sdkSearchResult, 0, len(matches))
	for _, match := range matches {
		title := cleanHTMLText(match[2])
		resultURL := html.UnescapeString(match[1])
		if title == "" || resultURL == "" {
			continue
		}
		snippet := ""
		if len(match) > 3 {
			snippet = cleanHTMLText(match[3])
		}
		results = append(results, sdkSearchResult{Title: title, URL: resultURL, Snippet: snippet})
	}
	return results, nil
}

func fetchSearchPage(ctx context.Context, rawURL string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", copilotUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := sdkWebHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("search request failed with status %d", resp.StatusCode)
	}
	return string(data), nil
}

func decodeDuckDuckGoURL(raw string) string {
	raw = html.UnescapeString(raw)
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if strings.Contains(u.Host, "duckduckgo.com") {
		if uddg := u.Query().Get("uddg"); uddg != "" {
			if decoded, err := url.QueryUnescape(uddg); err == nil {
				return decoded
			}
			return uddg
		}
	}
	return raw
}

func cleanHTMLText(value string) string {
	value = htmlTagStripRE.ReplaceAllString(value, " ")
	value = html.UnescapeString(value)
	return strings.Join(strings.Fields(value), " ")
}

func sdkWebFetchHandler(inv sdk.ToolInvocation) (sdk.ToolResult, error) {
	args := anyMap(inv.Arguments)
	rawURL := stringFromAny(args["url"])
	if rawURL == "" {
		return sdk.ToolResult{TextResultForLLM: "missing url", ResultType: "failure", Error: "missing url"}, nil
	}
	rawURL = normalizeSDKWebFetchURL(rawURL)
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return sdk.ToolResult{TextResultForLLM: "invalid url", ResultType: "failure", Error: "invalid url"}, nil
	}
	maxLength := intFromAny(args["max_length"], 12000)
	if maxLength <= 0 {
		maxLength = 12000
	}
	if maxLength > 20000 {
		maxLength = 20000
	}
	ctx, cancel := context.WithTimeout(inv.TraceContext, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return sdk.ToolResult{TextResultForLLM: err.Error(), ResultType: "failure", Error: err.Error()}, nil
	}
	req.Header.Set("User-Agent", copilotUserAgent)
	resp, err := sdkWebHTTPClient.Do(req)
	if err != nil {
		return sdk.ToolResult{TextResultForLLM: err.Error(), ResultType: "failure", Error: err.Error()}, nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxLength)+1))
	if err != nil {
		return sdk.ToolResult{TextResultForLLM: err.Error(), ResultType: "failure", Error: err.Error()}, nil
	}
	text := string(data)
	if len(text) > maxLength {
		text = text[:maxLength] + "\n[truncated]"
	}
	return sdk.ToolResult{
		TextResultForLLM: fmt.Sprintf("Fetched %s (HTTP %d):\n%s", u.String(), resp.StatusCode, text),
		ResultType:       "success",
		ToolTelemetry: map[string]any{
			"url":         u.String(),
			"status_code": resp.StatusCode,
		},
	}, nil
}

func normalizeSDKWebFetchURL(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return rawURL
	}
	host := strings.ToLower(u.Host)
	if host != "github.com" && host != "gh.hereis.app" {
		return rawURL
	}
	parts := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if len(parts) < 5 {
		return rawURL
	}
	owner, repo, marker, branch := parts[0], parts[1], parts[2], parts[3]
	if marker != "blob" && marker != "raw" {
		return rawURL
	}
	rest := strings.Join(parts[4:], "/")
	if rest == "" {
		return rawURL
	}
	return "https://raw.githubusercontent.com/" + owner + "/" + repo + "/" + branch + "/" + rest
}

func sdkCustomToolsFromParams(params map[string]any) []sdk.Tool {
	if toolChoiceIsNone(params["tool_choice"]) {
		return nil
	}
	tools, _ := params["tools"].([]map[string]any)
	if len(tools) == 0 {
		return nil
	}
	out := make([]sdk.Tool, 0, len(tools))
	seen := map[string]bool{}
	for _, tool := range tools {
		if isWebSearchToolSpec(tool) {
			continue
		}
		kind := strings.ToLower(stringFromAny(tool["type"]))
		if kind != "" && kind != "function" && kind != "custom" && kind != "tool" {
			continue
		}
		name := stringFromAny(tool["name"])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, sdk.Tool{
			Name:        name,
			Description: stringFromAny(tool["description"]),
			Parameters:  sdkToolParameters(tool),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sdkToolParameters(tool map[string]any) map[string]any {
	for _, key := range []string{"parameters", "input_schema"} {
		if params := anyMap(tool[key]); params != nil {
			return params
		}
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func hasSDKRequestTools(params map[string]any) bool {
	tools, _ := params["tools"].([]map[string]any)
	return len(tools) > 0
}

func requestHasWebSearchTool(params map[string]any) bool {
	if toolChoiceIsNone(params["tool_choice"]) {
		return false
	}
	tools, _ := params["tools"].([]map[string]any)
	for _, tool := range tools {
		if isWebSearchToolSpec(tool) {
			return true
		}
	}
	return false
}

func toolChoiceIsNone(choice any) bool {
	switch v := choice.(type) {
	case nil:
		return false
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "none")
	case map[string]any:
		return strings.EqualFold(strings.TrimSpace(stringFromAny(v["type"])), "none")
	default:
		return false
	}
}

func (b *CopilotBackend) doCopilot(ctx context.Context, method, endpoint string, body any, stream bool) (*http.Response, []byte, error) {
	access, err := b.validAccessToken(ctx)
	if err != nil {
		return nil, nil, err
	}
	bodyBytes, err := requestBodyBytes(body)
	if err != nil {
		return nil, nil, err
	}
	attempts := 1
	if !stream {
		attempts = 3
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		reqCtx := ctx
		var cancel context.CancelFunc
		if !stream {
			reqCtx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		}
		req, err := http.NewRequestWithContext(reqCtx, method, access.BaseURL+endpoint, bytes.NewReader(bodyBytes))
		if err != nil {
			if cancel != nil {
				cancel()
			}
			return nil, nil, err
		}
		addCopilotHeaders(req, access.Token, endpoint, copilotRequestMetadata(body, endpoint))
		if stream {
			req.Header.Set("Accept", "text/event-stream")
		}
		resp, err := copilotHTTPClient.Do(req)
		if err != nil {
			if cancel != nil {
				cancel()
			}
			lastErr = fmt.Errorf("copilot request failed: %w", err)
			if attempt < attempts-1 && sleepTransientRetry(ctx, attempt) {
				continue
			}
			return nil, nil, lastErr
		}
		if resp.StatusCode >= 400 {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if cancel != nil {
				cancel()
			}
			lastErr = &CopilotUpstreamError{StatusCode: resp.StatusCode, Body: errBody}
			if attempt < attempts-1 && isTransientOverloadError(lastErr) && sleepTransientRetry(ctx, attempt) {
				continue
			}
			return nil, nil, lastErr
		}
		if stream {
			return resp, nil, nil
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
		resp.Body.Close()
		if cancel != nil {
			cancel()
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read copilot response: %w", err)
		}
		return nil, data, nil
	}
	return nil, nil, lastErr
}

func (b *CopilotBackend) validAccessToken(ctx context.Context) (copilotAccessToken, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.access.usable() {
		return b.access, nil
	}
	if b.githubToken == "" {
		if b.homeDir != "" {
			return copilotAccessToken{}, fmt.Errorf("account %s has only a credential home; the Go Copilot backend requires a GitHub token from admin login or GHCP_COPILOT_TOKEN", b.accountID)
		}
		return copilotAccessToken{}, fmt.Errorf("account %s has no GitHub token configured", b.accountID)
	}
	if looksLikeCopilotBearer(b.githubToken) {
		b.access = copilotAccessToken{
			Token:     b.githubToken,
			BaseURL:   firstNonEmpty(b.baseURL, copilotBaseURLFromBearer(b.githubToken)),
			ExpiresAt: copilotExpiryFromBearer(b.githubToken),
		}
		return b.access, nil
	}
	if b.authMode == copilotAuthModeOAuth {
		b.access = copilotAccessToken{
			Token:   b.githubToken,
			BaseURL: firstNonEmpty(b.baseURL, copilotPublicAPIBaseURL),
		}
		return b.access, nil
	}
	token, err := exchangeGitHubTokenForCopilot(ctx, b.githubToken)
	if err != nil {
		return copilotAccessToken{}, err
	}
	if b.baseURL != "" {
		token.BaseURL = b.baseURL
	}
	b.access = token
	return token, nil
}

func (t copilotAccessToken) usable() bool {
	if t.Token == "" || t.BaseURL == "" {
		return false
	}
	if t.ExpiresAt.IsZero() {
		return true
	}
	return time.Until(t.ExpiresAt) > 5*time.Minute
}

func exchangeGitHubTokenForCopilot(ctx context.Context, githubToken string) (copilotAccessToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, copilotTokenURL, nil)
	if err != nil {
		return copilotAccessToken{}, err
	}
	addGitHubCopilotTokenHeaders(req, githubToken)
	resp, err := copilotHTTPClient.Do(req)
	if err != nil {
		return copilotAccessToken{}, fmt.Errorf("exchange GitHub token for Copilot token: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return copilotAccessToken{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return copilotAccessToken{}, &copilotTokenExchangeError{StatusCode: resp.StatusCode, Body: data}
	}
	var payload struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
		Endpoints struct {
			API string `json:"api"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return copilotAccessToken{}, fmt.Errorf("decode Copilot token response: %w", err)
	}
	if payload.Token == "" {
		return copilotAccessToken{}, fmt.Errorf("Copilot token response did not include a token")
	}
	return copilotAccessToken{
		Token:     payload.Token,
		BaseURL:   normalizeBaseURL(firstNonEmpty(payload.Endpoints.API, copilotBaseURLFromBearer(payload.Token))),
		ExpiresAt: unixTokenTime(payload.ExpiresAt),
	}, nil
}

func requestBody(body any) (io.Reader, error) {
	data, err := requestBodyBytes(body)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	return bytes.NewReader(data), nil
}

func requestBodyBytes(body any) ([]byte, error) {
	switch v := body.(type) {
	case nil:
		return nil, nil
	case []byte:
		return v, nil
	case io.Reader:
		return io.ReadAll(v)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
}

type copilotRequestInfo struct {
	Initiator string
	Vision    bool
}

func addCopilotHeaders(req *http.Request, token, endpoint string, info copilotRequestInfo) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", copilotUserAgent)
	req.Header.Set("Editor-Version", copilotEditorVersion)
	req.Header.Set("Editor-Plugin-Version", copilotPluginVersion)
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")
	req.Header.Set("Openai-Intent", copilotIntent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Github-Api-Version", copilotAPIVersion)
	req.Header.Set("X-Request-Id", newID("req"))
	if info.Initiator == "" {
		info.Initiator = "user"
	}
	req.Header.Set("x-initiator", info.Initiator)
	if info.Vision {
		req.Header.Set("Copilot-Vision-Request", "true")
	}
	if endpointMatches(endpoint, endpointMessages) {
		req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	}
}

func addCopilotModelHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", copilotUserAgent)
	req.Header.Set("Editor-Version", copilotEditorVersion)
	req.Header.Set("Editor-Plugin-Version", copilotPluginVersion)
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Github-Api-Version", copilotAPIVersion)
	req.Header.Set("X-Request-Id", newID("req"))
}

func addGitHubCopilotTokenHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("User-Agent", copilotUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Editor-Version", copilotEditorVersion)
	req.Header.Set("Editor-Plugin-Version", copilotPluginVersion)
	req.Header.Set("X-Github-Api-Version", copilotAPIVersion)
}

func copilotRequestMetadata(body any, endpoint string) copilotRequestInfo {
	info := copilotRequestInfo{Initiator: "user", Vision: containsCopilotVision(body)}
	switch {
	case endpointMatches(endpoint, endpointChatCompletions):
		if lastRoleFromBody(body, "messages") != "user" {
			info.Initiator = "agent"
		}
	case endpointMatches(endpoint, endpointResponses):
		if lastRoleFromBody(body, "input") != "user" {
			info.Initiator = "agent"
		}
	case endpointMatches(endpoint, endpointMessages):
		if !lastAnthropicMessageIsUserPrompt(body) {
			info.Initiator = "agent"
		}
	}
	return info
}

func lastRoleFromBody(body any, field string) string {
	m, ok := body.(map[string]any)
	if !ok {
		return ""
	}
	raw := m[field]
	if text, ok := raw.(string); ok && text != "" {
		return "user"
	}
	items := anySlice(raw)
	if len(items) == 0 {
		return ""
	}
	item, ok := items[len(items)-1].(map[string]any)
	if !ok {
		return ""
	}
	return strings.ToLower(stringFromAny(item["role"]))
}

func lastAnthropicMessageIsUserPrompt(body any) bool {
	m, ok := body.(map[string]any)
	if !ok {
		return false
	}
	items := anySlice(m["messages"])
	for i := len(items) - 1; i >= 0; i-- {
		item, ok := items[i].(map[string]any)
		if !ok {
			continue
		}
		if strings.ToLower(stringFromAny(item["role"])) != "user" {
			return false
		}
		content := item["content"]
		for _, part := range anySlice(content) {
			block, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if stringFromAny(block["type"]) != "tool_result" {
				return true
			}
		}
		return len(anySlice(content)) == 0 && coerceText(content) != ""
	}
	return false
}

func containsCopilotVision(value any) bool {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			if containsCopilotVision(item) {
				return true
			}
		}
	case []map[string]any:
		for _, item := range v {
			if containsCopilotVision(item) {
				return true
			}
		}
	case map[string]any:
		typ := strings.ToLower(stringFromAny(v["type"]))
		if typ == "image" || typ == "input_image" || typ == "image_url" {
			return true
		}
		if _, ok := v["image_url"]; ok {
			return true
		}
		if source, ok := v["source"].(map[string]any); ok && strings.HasPrefix(strings.ToLower(stringFromAny(source["media_type"])), "image/") {
			return true
		}
		for _, item := range v {
			if containsCopilotVision(item) {
				return true
			}
		}
	}
	return false
}

func looksLikeCopilotBearer(token string) bool {
	return strings.Contains(token, "proxy-ep=") || strings.HasPrefix(token, "tid=")
}

func copilotBaseURLFromBearer(token string) string {
	for _, part := range strings.Split(token, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "proxy-ep=") {
			host := strings.TrimPrefix(part, "proxy-ep=")
			if strings.HasPrefix(host, "proxy.") {
				host = "api." + strings.TrimPrefix(host, "proxy.")
			}
			return normalizeBaseURL("https://" + host)
		}
	}
	return defaultCopilotAPIBaseURL
}

func copilotExpiryFromBearer(token string) time.Time {
	for _, part := range strings.Split(token, ";") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "exp=") {
			continue
		}
		raw := strings.TrimPrefix(part, "exp=")
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return unixTokenTime(n)
		}
	}
	return time.Now().Add(30 * time.Minute)
}

func unixTokenTime(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	if value > 1e10 {
		return time.Unix(0, value*int64(time.Millisecond))
	}
	return time.Unix(value, 0)
}

func normalizeBaseURL(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return defaultCopilotAPIBaseURL
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	return strings.TrimRight(base, "/")
}

func normalizeBaseURLOrEmpty(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	return normalizeBaseURL(base)
}

func normalizeDomain(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return strings.TrimRight(u.Host, "/")
	}
	return strings.Trim(strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://"), "/")
}

func copilotEnterpriseAPIBaseURL(enterpriseURL string) string {
	domain := normalizeDomain(enterpriseURL)
	if domain == "" {
		return ""
	}
	return "https://copilot-api." + domain
}

func normalizeCopilotAuthMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return copilotAuthModeExchange
	}
	return mode
}

func normalizeCopilotBackendMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return copilotBackendModeSDK
	}
	return mode
}

func defaultCopilotAuthMode(mode, authMode string) string {
	if strings.TrimSpace(authMode) != "" {
		return authMode
	}
	if normalizeCopilotBackendMode(mode) == copilotBackendModeOpencode {
		return copilotAuthModeOAuth
	}
	return copilotAuthModeExchange
}

func usableCopilotModel(id, policyState string, capabilities map[string]any) bool {
	if id == "" || strings.EqualFold(policyState, "disabled") {
		return false
	}
	limits := anyMap(capabilities["limits"])
	supports := anyMap(capabilities["supports"])
	if intFromAny(limits["max_output_tokens"], 0) <= 0 || intFromAny(limits["max_prompt_tokens"], 0) <= 0 {
		return false
	}
	_, hasToolCalls := supports["tool_calls"].(bool)
	return hasToolCalls
}

func chatPayload(model string, messages []NeutralMessage, params map[string]any, stream bool) map[string]any {
	body := map[string]any{"model": model, "messages": openAIMessages(messages), "stream": stream}
	copyParam(body, params, "temperature")
	copyParam(body, params, "top_p")
	copyParamAs(body, params, "max_tokens", "max_tokens")
	copyParam(body, params, "stop")
	copyParam(body, params, "n")
	copyParam(body, params, "presence_penalty")
	copyParam(body, params, "frequency_penalty")
	copyParam(body, params, "response_format")
	copyParam(body, params, "reasoning_effort")
	copyParam(body, params, "context_tier")
	if choice := chatToolChoice(params["tool_choice"]); choice != nil {
		body["tool_choice"] = choice
	}
	copyParam(body, params, "parallel_tool_calls")
	if tools := openAITools(params["tools"]); len(tools) > 0 {
		body["tools"] = tools
	} else if needsNoopTool(messages) {
		body["tools"] = []map[string]any{noopChatTool()}
	}
	if stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	return compactMap(body)
}

func responsesPayload(model string, messages []NeutralMessage, params map[string]any, stream bool) map[string]any {
	if raw := rawResponsesPayload(params); raw != nil {
		return normalizeNativeResponsesPayload(raw, model, stream, params)
	}
	options, _ := params["response_options"].(map[string]any)
	instructions := []string{}
	input := []any{}
	for _, message := range messages {
		if message.Role == "system" {
			if message.Content != "" {
				instructions = append(instructions, message.Content)
			}
			continue
		}
		input = append(input, responseInputItems(message)...)
	}
	body := map[string]any{
		"model":  model,
		"input":  input,
		"stream": stream,
		"store":  optionValue(options, "store", false),
	}
	if len(instructions) > 0 {
		body["instructions"] = strings.Join(instructions, "\n")
	}
	copyParam(body, params, "temperature")
	copyParam(body, params, "top_p")
	copyParamAs(body, params, "max_tokens", "max_output_tokens")
	copyOption(body, options, "background")
	copyOption(body, options, "context_tier")
	copyOption(body, options, "include")
	copyOption(body, options, "max_tool_calls")
	copyOption(body, options, "metadata")
	copyOption(body, options, "parallel_tool_calls")
	copyOption(body, options, "previous_response_id")
	copyOption(body, options, "prompt_cache_key")
	copyOption(body, options, "prompt_cache_retention")
	copyOption(body, options, "safety_identifier")
	copyOption(body, options, "service_tier")
	copyOption(body, options, "stream_options")
	copyOption(body, options, "truncation")
	copyOption(body, options, "user")
	if choice := responsesToolChoice(params["tool_choice"]); choice != nil {
		body["tool_choice"] = choice
	}
	copyParam(body, params, "parallel_tool_calls")
	if tools, _ := params["tools"].([]map[string]any); len(tools) > 0 {
		body["tools"] = tools
	} else if needsNoopTool(messages) {
		body["tools"] = []map[string]any{noopResponsesTool()}
	}
	if reasoning := anyMap(optionValue(options, "reasoning", nil)); reasoning != nil {
		body["reasoning"] = reasoning
	}
	if effort := stringParam(params, "reasoning_effort"); effort != "" && effort != "none" {
		reasoning := anyMap(body["reasoning"])
		if reasoning == nil {
			reasoning = map[string]any{}
		}
		reasoning["effort"] = effort
		if reasoning["summary"] == nil {
			reasoning["summary"] = "auto"
		}
		body["reasoning"] = reasoning
		if body["include"] == nil {
			body["include"] = []string{"reasoning.encrypted_content"}
		}
	}
	if responseFormat, ok := params["response_format"].(map[string]any); ok && len(responseFormat) > 0 {
		body["text"] = map[string]any{"format": responseFormat}
	} else if text := anyMap(optionValue(options, "text", nil)); text != nil {
		body["text"] = text
	}
	return compactMap(body)
}

func rawResponsesPayload(params map[string]any) map[string]any {
	raw, ok := params[internalResponsesRawParam].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	return cloneMap(raw)
}

func normalizeNativeResponsesPayload(raw map[string]any, model string, stream bool, params map[string]any) map[string]any {
	sanitized := sanitizeNativeResponsesRaw(raw)
	sanitized["model"] = model
	sanitized["stream"] = stream
	if _, ok := sanitized["store"]; !ok {
		sanitized["store"] = false
	}
	if sanitized["context_tier"] == nil {
		copyParam(sanitized, params, "context_tier")
	}
	if sanitized["tools"] == nil && rawResponsesNeedsNoopTool(sanitized) {
		sanitized["tools"] = []map[string]any{noopResponsesTool()}
	}
	effort := firstNonEmpty(stringParam(params, "reasoning_effort"), stringFromAny(sanitized["reasoning_effort"]))
	delete(sanitized, "reasoning_effort")
	if effort != "" && effort != "none" {
		reasoning := anyMap(sanitized["reasoning"])
		if reasoning == nil {
			reasoning = map[string]any{}
		}
		reasoning["effort"] = effort
		if reasoning["summary"] == nil {
			reasoning["summary"] = "auto"
		}
		sanitized["reasoning"] = reasoning
		if sanitized["include"] == nil {
			sanitized["include"] = []string{"reasoning.encrypted_content"}
		}
	}
	return compactMap(sanitized)
}

func sanitizeNativeResponsesRaw(raw map[string]any) map[string]any {
	sanitized := cloneMap(raw)
	delete(sanitized, "cache")
	delete(sanitized, "model")
	delete(sanitized, "stream")
	if sanitized["max_output_tokens"] == nil && sanitized["max_tokens"] != nil {
		sanitized["max_output_tokens"] = sanitized["max_tokens"]
	}
	delete(sanitized, "max_tokens")
	for key := range sanitized {
		if strings.HasPrefix(key, "__ghcp_") {
			delete(sanitized, key)
		}
	}
	return sanitized
}

func anthropicPayload(model string, messages []NeutralMessage, params map[string]any, stream bool) map[string]any {
	if raw := rawAnthropicPayload(params); raw != nil {
		return normalizeNativeAnthropicPayload(raw, model, stream, params)
	}
	system := []string{}
	bodyMessages := []any{}
	for _, message := range messages {
		if message.Role == "system" {
			if message.Content != "" {
				system = append(system, message.Content)
			}
			continue
		}
		bodyMessages = append(bodyMessages, anthropicMessage(message)...)
	}
	options, _ := params["response_options"].(map[string]any)
	body := map[string]any{
		"model":      model,
		"messages":   bodyMessages,
		"stream":     stream,
		"max_tokens": optionValue(options, "max_tokens", firstAny(params["max_tokens"], 4096)),
	}
	if len(system) > 0 {
		body["system"] = strings.Join(system, "\n")
	}
	copyParam(body, params, "temperature")
	copyParam(body, params, "top_p")
	if stops := optionValue(options, "stop_sequences", nil); stops != nil {
		body["stop_sequences"] = stops
	}
	if metadata := optionValue(options, "metadata", nil); metadata != nil {
		body["metadata"] = metadata
	}
	if contextTier := optionValue(options, "context_tier", nil); contextTier != nil {
		body["context_tier"] = contextTier
	}
	if serviceTier := optionValue(options, "service_tier", nil); serviceTier != nil {
		body["service_tier"] = serviceTier
	}
	if tools := anthropicTools(params["tools"]); len(tools) > 0 {
		body["tools"] = tools
	} else if needsNoopTool(messages) {
		body["tools"] = []map[string]any{noopAnthropicTool()}
	}
	if choice := anthropicToolChoiceForBackend(firstAny(optionValue(options, "tool_choice", nil), params["tool_choice"])); choice != nil {
		body["tool_choice"] = choice
	}
	if outputConfig := optionValue(options, "output_config", nil); outputConfig != nil {
		body["output_config"] = outputConfig
	}
	if thinking, outputConfig := copilotNativeThinking(optionValue(options, "thinking", nil), body["output_config"], model); thinking != nil || optionValue(options, "thinking", nil) != nil {
		body["thinking"] = thinking
		if outputConfig != nil {
			body["output_config"] = outputConfig
		} else {
			delete(body, "output_config")
		}
	}
	return compactMap(body)
}

func rawAnthropicPayload(params map[string]any) map[string]any {
	raw, ok := params[internalAnthropicRawParam].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	return cloneMap(raw)
}

func normalizeNativeAnthropicPayload(raw map[string]any, model string, stream bool, params map[string]any) map[string]any {
	sanitized := sanitizeNativeAnthropicRaw(raw)
	sanitized["model"] = model
	sanitized["stream"] = stream
	if sanitized["context_tier"] == nil {
		copyParam(sanitized, params, "context_tier")
	}
	hoistSystemMessages(sanitized)
	if thinking, outputConfig := copilotNativeThinking(sanitized["thinking"], sanitized["output_config"], model); thinking != nil || sanitized["thinking"] != nil {
		sanitized["thinking"] = thinking
		if outputConfig != nil {
			sanitized["output_config"] = outputConfig
		} else {
			delete(sanitized, "output_config")
		}
	}
	if sanitized["tools"] == nil && rawAnthropicNeedsNoopTool(sanitized) {
		sanitized["tools"] = []map[string]any{noopAnthropicTool()}
	}
	return compactMap(sanitized)
}

func sanitizeNativeAnthropicRaw(raw map[string]any) map[string]any {
	sanitized, _ := sanitizeAnthropicContent(raw).(map[string]any)
	if sanitized == nil {
		sanitized = cloneMap(raw)
	}
	delete(sanitized, "context_management")
	delete(sanitized, "cache")
	return sanitized
}

func hoistSystemMessages(payload map[string]any) {
	rawMessages, ok := payload["messages"].([]any)
	if !ok {
		return
	}
	remaining := make([]any, 0, len(rawMessages))
	systems := []any{}
	if existing := payload["system"]; existing != nil {
		switch v := existing.(type) {
		case []any:
			systems = append(systems, v...)
		default:
			systems = append(systems, v)
		}
	}
	for _, raw := range rawMessages {
		msg, ok := raw.(map[string]any)
		if !ok || stringFromAny(msg["role"]) != "system" {
			remaining = append(remaining, raw)
			continue
		}
		if content := msg["content"]; content != nil {
			systems = append(systems, content)
		}
	}
	if len(systems) > 0 {
		payload["system"] = mergeAnthropicSystem(systems)
	}
	payload["messages"] = remaining
}

func mergeAnthropicSystem(systems []any) any {
	blocks := []any{}
	texts := []string{}
	for _, system := range systems {
		switch v := system.(type) {
		case string:
			if v != "" {
				texts = append(texts, v)
			}
		case []any:
			blocks = append(blocks, v...)
		default:
			blocks = append(blocks, v)
		}
	}
	if len(blocks) == 0 {
		return strings.Join(texts, "\n")
	}
	if len(texts) > 0 {
		blocks = append([]any{map[string]any{"type": "text", "text": strings.Join(texts, "\n")}}, blocks...)
	}
	return blocks
}

func sanitizeAnthropicContent(value any) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, sanitizeAnthropicContent(item))
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for key, item := range v {
			if key == "cache_control" {
				if cc, ok := item.(map[string]any); ok {
					clean := map[string]any{}
					for k, cv := range cc {
						if k != "scope" {
							clean[k] = cv
						}
					}
					out[key] = clean
					continue
				}
			}
			out[key] = sanitizeAnthropicContent(item)
		}
		return out
	default:
		return value
	}
}

func copilotNativeThinking(thinking any, outputConfig any, model string) (any, any) {
	m, ok := thinking.(map[string]any)
	if !ok || len(m) == 0 {
		return thinking, outputConfig
	}
	out := map[string]any{}
	for key, value := range m {
		out[key] = value
	}
	if stringFromAny(out["type"]) == "enabled" {
		out["type"] = "adaptive"
	}
	if out["type"] == nil {
		out["type"] = "adaptive"
	}
	cfg := anyMap(outputConfig)
	if cfg == nil {
		cfg = map[string]any{}
	}
	if !modelSupportsOutputEffort(model) {
		delete(cfg, "effort")
		if len(cfg) == 0 {
			return nil, nil
		}
		return nil, cfg
	} else if cfg["effort"] == nil {
		if budget := intFromAny(out["budget_tokens"], 0); budget > 0 {
			cfg["effort"] = effortFromThinkingBudget(budget)
		}
	}
	delete(out, "budget_tokens")
	if len(cfg) == 0 {
		return out, nil
	}
	return out, cfg
}

func modelSupportsOutputEffort(model string) bool {
	model = strings.ToLower(model)
	return strings.HasPrefix(model, "claude-sonnet-") || strings.HasPrefix(model, "claude-opus-")
}

func effortFromThinkingBudget(budget int) string {
	if budget >= 16000 {
		return "high"
	}
	if budget >= 8000 {
		return "medium"
	}
	return "low"
}

func anyMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	if m, ok := value.(map[string]any); ok {
		out := map[string]any{}
		for key, v := range m {
			out[key] = v
		}
		return out
	}
	return nil
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for key, value := range m {
		out[key] = value
	}
	return out
}

func openAIMessages(messages []NeutralMessage) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		item := map[string]any{"role": firstNonEmpty(message.Role, "user"), "content": message.Content}
		if message.ToolCallID != "" {
			item["tool_call_id"] = message.ToolCallID
		}
		if len(message.ToolCalls) > 0 {
			item["tool_calls"] = normalizeOpenAIToolCalls(message.ToolCalls)
		}
		out = append(out, compactMap(item))
	}
	return out
}

func responseInputItems(message NeutralMessage) []any {
	switch message.Role {
	case "tool":
		return []any{map[string]any{"type": "function_call_output", "call_id": message.ToolCallID, "output": message.Content}}
	case "assistant":
		items := []any{map[string]any{"type": "message", "role": "assistant", "content": message.Content}}
		for _, tc := range normalizeOpenAIToolCalls(message.ToolCalls) {
			fn, _ := tc["function"].(map[string]any)
			items = append(items, map[string]any{"type": "function_call", "call_id": tc["id"], "name": fn["name"], "arguments": fn["arguments"], "status": "completed"})
		}
		return items
	default:
		return []any{map[string]any{"type": "message", "role": firstNonEmpty(message.Role, "user"), "content": message.Content}}
	}
}

func anthropicMessage(message NeutralMessage) []any {
	switch message.Role {
	case "tool":
		return []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": message.ToolCallID, "content": message.Content}}}}
	case "assistant":
		content := []any{}
		if message.Content != "" {
			content = append(content, map[string]any{"type": "text", "text": message.Content})
		}

		for _, tc := range normalizeOpenAIToolCalls(message.ToolCalls) {
			fn, _ := tc["function"].(map[string]any)
			content = append(content, map[string]any{"type": "tool_use", "id": stringFromAny(tc["id"]), "name": stringFromAny(fn["name"]), "input": argsToObject(stringFromAny(fn["arguments"]))})
		}
		if len(content) == 0 {
			content = append(content, map[string]any{"type": "text", "text": ""})
		}
		return []any{map[string]any{"role": "assistant", "content": content}}
	default:
		return []any{map[string]any{"role": "user", "content": message.Content}}
	}
}

func normalizeOpenAIToolCalls(toolCalls []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(toolCalls))
	for _, tc := range toolCalls {
		if fn, ok := tc["function"].(map[string]any); ok {
			out = append(out, compactMap(map[string]any{"id": tc["id"], "type": firstNonEmpty(stringFromAny(tc["type"]), "function"), "function": fn}))
			continue
		}
		out = append(out, map[string]any{
			"id":   firstNonEmpty(stringFromAny(tc["id"]), newID("call")),
			"type": firstNonEmpty(stringFromAny(tc["type"]), "function"),
			"function": map[string]any{
				"name":      tc["name"],
				"arguments": firstNonEmpty(stringFromAny(tc["arguments"]), "{}"),
			},
		})
	}
	return out
}

func openAITools(value any) []map[string]any {
	tools, _ := value.([]map[string]any)
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if isWebSearchToolSpec(tool) {
			out = append(out, compactMap(cloneMap(tool)))
			continue
		}
		if stringFromAny(tool["name"]) == "" {
			continue
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": compactMap(map[string]any{
				"name":        tool["name"],
				"description": tool["description"],
				"parameters":  tool["parameters"],
			}),
		})
	}
	return out
}

func chatToolChoice(choice any) any {
	switch v := choice.(type) {
	case nil:
		return nil
	case string:
		return v
	case map[string]any:
		if fn, ok := v["function"].(map[string]any); ok {
			if name := stringFromAny(fn["name"]); name != "" {
				return map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
		}
		if name := stringFromAny(v["name"]); name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
		if typ := stringFromAny(v["type"]); typ != "" {
			return typ
		}
	}
	return nil
}

func responsesToolChoice(choice any) any {
	switch v := choice.(type) {
	case nil:
		return nil
	case string:
		return v
	case map[string]any:
		if fn, ok := v["function"].(map[string]any); ok {
			if name := stringFromAny(fn["name"]); name != "" {
				return map[string]any{"type": "function", "name": name}
			}
		}
		typ := stringFromAny(v["type"])
		if typ == "function" {
			if name := stringFromAny(v["name"]); name != "" {
				return map[string]any{"type": "function", "name": name}
			}
		}
		if typ != "" {
			return v
		}
		if name := stringFromAny(v["name"]); name != "" {
			return map[string]any{"type": "function", "name": name}
		}
	}
	return nil
}

func anthropicToolChoiceForBackend(choice any) any {
	switch v := choice.(type) {
	case nil:
		return nil
	case string:
		switch v {
		case "required":
			return map[string]any{"type": "any"}
		case "none":
			return map[string]any{"type": "none"}
		case "auto":
			return map[string]any{"type": "auto"}
		default:
			return nil
		}
	case map[string]any:
		if fn, ok := v["function"].(map[string]any); ok {
			if name := stringFromAny(fn["name"]); name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
		if name := stringFromAny(v["name"]); name != "" {
			return map[string]any{"type": "tool", "name": name}
		}
		if typ := stringFromAny(v["type"]); typ == "auto" || typ == "any" || typ == "none" {
			return map[string]any{"type": typ}
		}
	}
	return nil
}

func anthropicTools(value any) []map[string]any {
	tools, _ := value.([]map[string]any)
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if isWebSearchToolSpec(tool) {
			out = append(out, compactMap(cloneMap(tool)))
			continue
		}
		name := stringFromAny(tool["name"])
		if name == "" {
			continue
		}
		out = append(out, compactMap(map[string]any{
			"name":         name,
			"description":  tool["description"],
			"input_schema": firstAny(tool["input_schema"], tool["parameters"], map[string]any{"type": "object"}),
		}))
	}
	return out
}

func needsNoopTool(messages []NeutralMessage) bool {
	for _, message := range messages {
		if message.ToolCallID != "" || len(message.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

func rawResponsesNeedsNoopTool(payload map[string]any) bool {
	for _, item := range anySlice(payload["input"]) {
		if m, ok := item.(map[string]any); ok {
			switch stringFromAny(m["type"]) {
			case "function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output":
				return true
			}
		}
	}
	return false
}

func rawAnthropicNeedsNoopTool(payload map[string]any) bool {
	for _, item := range anySlice(payload["messages"]) {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for _, part := range anySlice(msg["content"]) {
			block, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch stringFromAny(block["type"]) {
			case "tool_use", "tool_result":
				return true
			}
		}
	}
	return false
}

func noopToolSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"reason": map[string]any{"type": "string", "description": "Unused"},
		},
	}
}

func noopChatTool() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "_noop",
			"description": "Do not call this tool. It exists only for API compatibility and must never be invoked.",
			"parameters":  noopToolSchema(),
		},
	}
}

func noopResponsesTool() map[string]any {
	return map[string]any{
		"type":        "function",
		"name":        "_noop",
		"description": "Do not call this tool. It exists only for API compatibility and must never be invoked.",
		"parameters":  noopToolSchema(),
	}
}

func noopAnthropicTool() map[string]any {
	return map[string]any{
		"name":         "_noop",
		"description":  "Do not call this tool. It exists only for API compatibility and must never be invoked.",
		"input_schema": noopToolSchema(),
	}
}

func copyParam(dest map[string]any, params map[string]any, key string) {
	copyParamAs(dest, params, key, key)
}

func copyParamAs(dest map[string]any, params map[string]any, src, dst string) {
	if value, ok := params[src]; ok && value != nil {
		dest[dst] = value
	}
}

func copyOption(dest map[string]any, options map[string]any, key string) {
	if value := optionValue(options, key, nil); value != nil {
		dest[key] = value
	}
}

type openAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails any `json:"prompt_tokens_details"`
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	InputTokensDetails  any `json:"input_tokens_details"`
	OutputTokensDetails any `json:"output_tokens_details"`
}

func (u openAIUsage) toUsage(endpoint string) Usage {
	input := firstInt("", u.PromptTokens, u.InputTokens)
	output := firstInt("", u.CompletionTokens, u.OutputTokens)
	total := firstInt("", u.TotalTokens, input+output)
	return Usage{InputTokens: input, OutputTokens: output, TotalTokens: total, CachedTokens: u.CachedTokens(), APIEndpoint: endpoint}.Normalized()
}

func (u openAIUsage) CachedTokens() int {
	for _, details := range []any{u.PromptTokensDetails, u.InputTokensDetails} {
		if m, ok := details.(map[string]any); ok {
			if n := intFromAny(m["cached_tokens"], 0); n > 0 {
				return n
			}
		}
	}
	return 0
}

func parseChatResult(data []byte, model string) (ChatResult, error) {
	var payload struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   any              `json:"content"`
				ToolCalls []openAIToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage openAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return ChatResult{}, fmt.Errorf("decode chat response: %w", err)
	}
	result := ChatResult{Model: firstNonEmpty(payload.Model, model), FinishReason: "stop", Usage: payload.Usage.toUsage(endpointChatCompletions)}
	if len(payload.Choices) > 0 {
		choice := payload.Choices[0]
		result.Content = coerceText(choice.Message.Content)
		result.FinishReason = firstNonEmpty(choice.FinishReason, result.FinishReason)
		result.ToolCalls = toolCallsFromOpenAI(choice.Message.ToolCalls)
	}
	return result, nil
}

func parseResponsesResult(data []byte, model string) (ChatResult, error) {
	var payload struct {
		ID         string           `json:"id"`
		Model      string           `json:"model"`
		Status     string           `json:"status"`
		OutputText string           `json:"output_text"`
		Output     []map[string]any `json:"output"`
		Usage      openAIUsage      `json:"usage"`
		Error      *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return ChatResult{}, fmt.Errorf("decode responses response: %w", err)
	}
	if payload.Status == "failed" || payload.Error != nil {
		msg := "responses request failed"
		if payload.Error != nil && payload.Error.Message != "" {
			msg = payload.Error.Message
		}
		return ChatResult{}, fmt.Errorf("%s", msg)
	}
	content := payload.OutputText
	if content == "" {
		content = responseTextFromOutput(payload.Output)
	}
	return ChatResult{
		ID:              payload.ID,
		Status:          firstNonEmpty(payload.Status, "completed"),
		Model:           firstNonEmpty(payload.Model, model),
		Content:         content,
		Usage:           payload.Usage.toUsage(endpointResponses),
		FinishReason:    "stop",
		ToolCalls:       toolCallsFromResponsesOutput(payload.Output),
		ResponsesOutput: payload.Output,
	}, nil
}

func parseAnthropicResult(data []byte, model string) (ChatResult, error) {
	var payload struct {
		ID         string           `json:"id"`
		Model      string           `json:"model"`
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return ChatResult{}, fmt.Errorf("decode messages response: %w", err)
	}
	text := []string{}
	toolCalls := []ToolCall{}
	for _, block := range payload.Content {
		switch stringFromAny(block["type"]) {
		case "tool_use":
			args, _ := json.Marshal(firstAny(block["input"], map[string]any{}))
			toolCalls = append(toolCalls, ToolCall{ID: stringFromAny(block["id"]), Name: stringFromAny(block["name"]), Arguments: string(args), Kind: "function"})
		default:
			if value := coerceText(block); value != "" {
				text = append(text, value)
			}
		}
	}
	finish := "stop"
	if payload.StopReason == "tool_use" {
		finish = "tool_calls"
	} else if payload.StopReason == "max_tokens" {
		finish = "length"
	}
	return ChatResult{
		ID:               payload.ID,
		Model:            firstNonEmpty(payload.Model, model),
		Content:          strings.Join(text, "\n\n"),
		Usage:            Usage{InputTokens: payload.Usage.InputTokens, OutputTokens: payload.Usage.OutputTokens, APIEndpoint: endpointMessages}.Normalized(),
		FinishReason:     finish,
		ToolCalls:        toolCalls,
		AnthropicContent: payload.Content,
	}, nil
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func toolCallsFromOpenAI(calls []openAIToolCall) []ToolCall {
	out := make([]ToolCall, 0, len(calls))
	for _, call := range calls {
		out = append(out, ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: firstNonEmpty(call.Function.Arguments, "{}"), Kind: firstNonEmpty(call.Type, "function")})
	}
	return out
}

func responseTextFromOutput(output []map[string]any) string {
	parts := []string{}
	for _, item := range output {
		for _, content := range anySlice(item["content"]) {
			block, ok := content.(map[string]any)
			if !ok {
				continue
			}
			switch stringFromAny(block["type"]) {
			case "output_text", "summary_text", "text":
				if text := stringFromAny(firstAny(block["text"], block["summary"])); text != "" {
					parts = append(parts, text)
				}
			case "refusal":
				if refusal := stringFromAny(block["refusal"]); refusal != "" {
					parts = append(parts, refusal)
				}
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func toolCallsFromResponsesOutput(output []map[string]any) []ToolCall {
	toolCalls := []ToolCall{}
	for _, item := range output {
		switch stringFromAny(item["type"]) {
		case "function_call":
			toolCalls = append(toolCalls, ToolCall{ID: firstNonEmpty(stringFromAny(item["call_id"]), stringFromAny(item["id"])), Name: stringFromAny(item["name"]), Arguments: firstNonEmpty(stringFromAny(item["arguments"]), "{}"), Kind: "function"})
		case "custom_tool_call":
			toolCalls = append(toolCalls, ToolCall{ID: firstNonEmpty(stringFromAny(item["call_id"]), stringFromAny(item["id"])), Name: stringFromAny(item["name"]), Arguments: stringFromAny(item["input"]), Kind: "custom"})
		}
	}
	return toolCalls
}

func scanSSE(r io.Reader, handle func(event, data string) bool) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	event := ""
	dataLines := []string{}
	flush := func() bool {
		if len(dataLines) == 0 {
			event = ""
			return true
		}
		keepGoing := handle(event, strings.Join(dataLines, "\n"))
		event = ""
		dataLines = nil
		return keepGoing
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if !flush() {
				return scanner.Err()
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(dataLines) > 0 {
		flush()
	}
	return scanner.Err()
}

func streamChatSSE(ctx context.Context, r io.Reader, out chan<- StreamItem) {
	state := newStreamToolState()
	usage := Usage{}
	finish := "stop"
	completed := false
	err := scanSSE(r, func(_, data string) bool {
		if data == "[DONE]" {
			for i, tc := range state.toolCalls() {
				if !emitStreamItem(ctx, out, StreamItem{Kind: "tool_call", ToolCall: tc, Index: i}) {
					return false
				}
			}
			if !emitStreamItem(ctx, out, StreamItem{Kind: "done", Usage: usage, FinishReason: finish}) {
				return false
			}
			completed = true
			return false
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   any                   `json:"content"`
					ToolCalls []openAIToolCallDelta `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage openAIUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return true
		}
		if chunk.Usage.TotalTokens != 0 || chunk.Usage.PromptTokens != 0 || chunk.Usage.InputTokens != 0 {
			usage = chunk.Usage.toUsage(endpointChatCompletions)
		}
		for _, choice := range chunk.Choices {
			if text := coerceText(choice.Delta.Content); text != "" {
				if !emitStreamItem(ctx, out, StreamItem{Kind: "delta", Text: text}) {
					return false
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				state.add(tc)
			}
			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}
		}
		return true
	})
	if err != nil {
		emitStreamItem(ctx, out, StreamItem{Kind: "error", Err: err})
		return
	}
	if !completed {
		emitStreamItem(ctx, out, StreamItem{Kind: "error", Err: fmt.Errorf("chat stream ended without terminal [DONE]")})
	}
}

func streamResponsesSSE(ctx context.Context, r io.Reader, out chan<- StreamItem) {
	state := newStreamToolState()
	usage := Usage{}
	finish := "stop"
	completed := false
	err := scanSSE(r, func(event, data string) bool {
		var item map[string]any
		if err := json.Unmarshal([]byte(data), &item); err != nil {
			return true
		}
		etype := firstNonEmpty(stringFromAny(item["type"]), event)
		switch etype {
		case "response.output_text.delta":
			if text := firstNonEmpty(stringFromAny(item["delta"]), stringFromAny(item["text"])); text != "" {
				if !emitStreamItem(ctx, out, StreamItem{Kind: "delta", Text: text}) {
					return false
				}
			}
		case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
			state.addResponseDelta(item)
		case "response.output_item.done":
			if obj, ok := item["item"].(map[string]any); ok {
				state.addResponseItem(obj, intFromAny(item["output_index"], -1))
			}
		case "response.completed":
			if resp, ok := item["response"].(map[string]any); ok {
				usage = usageFromAny(resp["usage"], endpointResponses)
				if output, ok := resp["output"].([]any); ok {
					for i, raw := range output {
						if obj, ok := raw.(map[string]any); ok {
							state.addResponseItem(obj, i)
						}
					}
				}
			}
			for i, tc := range state.toolCalls() {
				if !emitStreamItem(ctx, out, StreamItem{Kind: "tool_call", ToolCall: tc, Index: i}) {
					return false
				}
			}
			if !emitStreamItem(ctx, out, StreamItem{Kind: "done", Usage: usage, FinishReason: finish}) {
				return false
			}
			completed = true
			return false
		case "response.failed", "response.incomplete", "error":
			msg := responsesStreamErrorMessage(item)
			emitStreamItem(ctx, out, StreamItem{Kind: "error", Err: fmt.Errorf("responses stream failed: %s", msg)})
			completed = true
			return false
		}
		return true
	})
	if err != nil {
		emitStreamItem(ctx, out, StreamItem{Kind: "error", Err: err})
		return
	}
	if !completed {
		emitStreamItem(ctx, out, StreamItem{Kind: "error", Err: fmt.Errorf("responses stream ended without terminal event")})
	}
}

func responsesStreamErrorMessage(item map[string]any) string {
	if msg := stringFromAny(item["message"]); msg != "" {
		return msg
	}
	for _, value := range []any{item["error"], nestedMapValue(item, "response", "error")} {
		errObj := anyMap(value)
		if errObj == nil {
			if msg := stringFromAny(value); msg != "" {
				return msg
			}
			continue
		}
		if msg := stringFromAny(errObj["message"]); msg != "" {
			return msg
		}
		if typ := stringFromAny(errObj["type"]); typ != "" {
			return typ
		}
	}
	return "unknown upstream error"
}

func nestedMapValue(item map[string]any, parent, key string) any {
	obj, ok := item[parent].(map[string]any)
	if !ok {
		return nil
	}
	return obj[key]
}

func streamAnthropicSSE(ctx context.Context, r io.Reader, out chan<- StreamItem) {
	state := newStreamToolState()
	usage := Usage{}
	finish := "stop"
	completed := false
	err := scanSSE(r, func(event, data string) bool {
		var item map[string]any
		if err := json.Unmarshal([]byte(data), &item); err != nil {
			return true
		}
		etype := firstNonEmpty(stringFromAny(item["type"]), event)
		switch etype {
		case "content_block_start":
			if block, ok := item["content_block"].(map[string]any); ok && stringFromAny(block["type"]) == "tool_use" {
				state.addAnthropicTool(item, block)
			}
		case "content_block_delta":
			if delta, ok := item["delta"].(map[string]any); ok {
				switch stringFromAny(delta["type"]) {
				case "text_delta":
					if text := stringFromAny(delta["text"]); text != "" {
						if !emitStreamItem(ctx, out, StreamItem{Kind: "delta", Text: text}) {
							return false
						}
					}
				case "input_json_delta":
					state.addAnthropicArgs(item, stringFromAny(delta["partial_json"]))
				}
			}
		case "message_delta":
			if delta, ok := item["delta"].(map[string]any); ok {
				if reason := stringFromAny(delta["stop_reason"]); reason == "tool_use" {
					finish = "tool_calls"
				} else if reason == "max_tokens" {
					finish = "length"
				}
			}
			usage = usageFromAny(item["usage"], endpointMessages)
		case "message_stop":
			for i, tc := range state.toolCalls() {
				if !emitStreamItem(ctx, out, StreamItem{Kind: "tool_call", ToolCall: tc, Index: i}) {
					return false
				}
			}
			if !emitStreamItem(ctx, out, StreamItem{Kind: "done", Usage: usage, FinishReason: finish}) {
				return false
			}
			completed = true
			return false
		case "error":
			emitStreamItem(ctx, out, StreamItem{Kind: "error", Err: fmt.Errorf("messages stream failed: %s", stringFromAny(item["error"]))})
			completed = true
			return false
		}
		return true
	})
	if err != nil {
		emitStreamItem(ctx, out, StreamItem{Kind: "error", Err: err})
		return
	}
	if !completed {
		emitStreamItem(ctx, out, StreamItem{Kind: "error", Err: fmt.Errorf("messages stream ended without terminal event")})
	}
}

func emitStreamItem(ctx context.Context, out chan<- StreamItem, item StreamItem) bool {
	select {
	case out <- item:
		return true
	case <-ctx.Done():
		return false
	}
}

type openAIToolCallDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type streamToolState struct {
	order []int
	items map[int]*ToolCall
}

func newStreamToolState() *streamToolState {
	return &streamToolState{items: map[int]*ToolCall{}}
}

func (s *streamToolState) add(delta openAIToolCallDelta) {
	index := len(s.order)
	if delta.Index != nil {
		index = *delta.Index
	}
	item := s.items[index]
	if item == nil {
		item = &ToolCall{Kind: "function", Arguments: ""}
		s.items[index] = item
		s.order = append(s.order, index)
	}
	if delta.ID != "" {
		item.ID = delta.ID
	}
	if delta.Type != "" {
		item.Kind = delta.Type
	}
	if delta.Function.Name != "" {
		item.Name = delta.Function.Name
	}
	item.Arguments += delta.Function.Arguments
}

func (s *streamToolState) addResponseDelta(item map[string]any) {
	index := intFromAny(item["output_index"], len(s.order))
	tc := s.items[index]
	if tc == nil {
		tc = &ToolCall{Kind: "function", Arguments: ""}
		s.items[index] = tc
		s.order = append(s.order, index)
	}
	tc.Arguments += firstNonEmpty(stringFromAny(item["delta"]), stringFromAny(item["input"]))
}

func (s *streamToolState) addResponseItem(item map[string]any, outputIndex int) {
	kind := stringFromAny(item["type"])
	if kind != "function_call" && kind != "custom_tool_call" {
		return
	}
	index := outputIndex
	if index < 0 {
		index = len(s.order)
	}
	tc := s.items[index]
	if tc == nil {
		tc = &ToolCall{}
		s.items[index] = tc
		s.order = append(s.order, index)
	}
	tc.ID = firstNonEmpty(stringFromAny(item["call_id"]), stringFromAny(item["id"]), tc.ID)
	tc.Name = firstNonEmpty(stringFromAny(item["name"]), tc.Name)
	tc.Kind = "function"
	if kind == "custom_tool_call" {
		tc.Kind = "custom"
	}
	tc.Arguments = firstNonEmpty(stringFromAny(item["arguments"]), stringFromAny(item["input"]), tc.Arguments, "{}")
}

func (s *streamToolState) addAnthropicTool(event, block map[string]any) {
	index := intFromAny(event["index"], len(s.order))
	tc := s.items[index]
	if tc == nil {
		tc = &ToolCall{Kind: "function", Arguments: ""}
		s.items[index] = tc
		s.order = append(s.order, index)
	}
	tc.ID = firstNonEmpty(stringFromAny(block["id"]), tc.ID)
	tc.Name = firstNonEmpty(stringFromAny(block["name"]), tc.Name)
}

func (s *streamToolState) addAnthropicArgs(event map[string]any, delta string) {
	index := intFromAny(event["index"], len(s.order))
	tc := s.items[index]
	if tc == nil {
		tc = &ToolCall{Kind: "function", Arguments: ""}
		s.items[index] = tc
		s.order = append(s.order, index)
	}
	tc.Arguments += delta
}

func (s *streamToolState) toolCalls() []ToolCall {
	out := []ToolCall{}
	for _, index := range s.order {
		tc := s.items[index]
		if tc == nil || tc.Name == "" {
			continue
		}
		if tc.ID == "" {
			tc.ID = newID("call")
		}
		if tc.Arguments == "" {
			tc.Arguments = "{}"
		}
		out = append(out, tc.normalized())
	}
	return out
}

func usageFromAny(value any, endpoint string) Usage {
	m, ok := value.(map[string]any)
	if !ok {
		return Usage{APIEndpoint: endpoint}
	}
	return Usage{
		InputTokens:  intFromAny(firstAny(m["prompt_tokens"], m["input_tokens"]), 0),
		OutputTokens: intFromAny(firstAny(m["completion_tokens"], m["output_tokens"]), 0),
		TotalTokens:  intFromAny(m["total_tokens"], 0),
		APIEndpoint:  endpoint,
	}.Normalized()
}

func anySlice(value any) []any {
	switch v := value.(type) {
	case []any:
		return v
	case []map[string]any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, item)
		}
		return out
	default:
		return nil
	}
}
