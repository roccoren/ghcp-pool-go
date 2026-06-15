package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/github/copilot-sdk/go"
)

const (
	copilotTokenURL          = "https://api.github.com/copilot_internal/v2/token"
	defaultCopilotAPIBaseURL = "https://api.individual.githubcopilot.com"
	copilotUserAgent         = "GitHubCopilotChat/0.39.0"
	copilotEditorVersion     = "vscode/1.111.0"
	copilotPluginVersion     = "copilot-chat/0.39.0"
)

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

type CopilotBackend struct {
	accountID   string
	githubToken string
	homeDir     string

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

func NewCopilotBackend(accountID, token, homeDir string) *CopilotBackend {
	return &CopilotBackend{accountID: accountID, githubToken: strings.TrimSpace(token), homeDir: homeDir}
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
		options := &sdk.ClientOptions{
			BaseDirectory:             b.homeDir,
			GitHubToken:               b.sdkGitHubToken(),
			UseLoggedInUser:           sdk.Bool(b.sdkGitHubToken() == ""),
			Mode:                      sdk.ModeEmpty,
			SessionIdleTimeoutSeconds: 60,
		}
		if options.BaseDirectory == "" {
			options.BaseDirectory = "runtime-home/" + b.accountID
		}
		b.sdkClient = sdk.NewClient(options)
	}
	client := b.sdkClient
	b.mu.Unlock()

	if err := client.Start(ctx); err != nil {
		b.mu.Lock()
		b.sdkErr = err
		b.mu.Unlock()
		return err
	}
	b.mu.Lock()
	b.sdkReady = true
	b.sdkErr = nil
	b.mu.Unlock()
	return nil
}

func (b *CopilotBackend) ListModels(ctx context.Context) ([]ModelSpec, error) {
	_, data, err := b.doCopilot(ctx, http.MethodGet, "/models", nil, false)
	if err != nil {
		if b.sdkGitHubToken() != "" {
			if specs, sdkErr := b.listModelsSDK(ctx); sdkErr == nil {
				return specs, nil
			}
		}
		return nil, err
	}
	var payload struct {
		Data []struct {
			ID                 string         `json:"id"`
			SupportedEndpoints []string       `json:"supported_endpoints"`
			Capabilities       map[string]any `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}
	specs := make([]ModelSpec, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.ID == "" {
			continue
		}
		specs = append(specs, ModelSpec{ID: item.ID, SupportedEndpoints: item.SupportedEndpoints, Capabilities: item.Capabilities})
	}
	return specs, nil
}

func (b *CopilotBackend) Chat(ctx context.Context, model string, messages []NeutralMessage, params map[string]any) (ChatResult, error) {
	endpoint := endpointFromParams(params, endpointChatCompletions)
	clean := cleanBackendParams(params)
	if endpointMatches(endpoint, endpointChatCompletions) && b.canUseSDK(messages, clean) {
		if result, err := b.chatSDK(ctx, model, messages, clean, false); err == nil {
			return result, nil
		}
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
	if b.sdkClient == nil {
		return nil, fmt.Errorf("sdk client not available")
	}
	return b.sdkClient, nil
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
			SupportedEndpoints: []string{endpointChatCompletions},
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

func (b *CopilotBackend) canUseSDK(messages []NeutralMessage, params map[string]any) bool {
	if !b.sdkConfigured() {
		return false
	}
	if len(messages) != 1 || messages[0].Role != "user" || messages[0].ToolCallID != "" || len(messages[0].ToolCalls) > 0 {
		return false
	}
	if tools, _ := params["tools"].([]map[string]any); len(tools) > 0 {
		return false
	}
	if choice := params["tool_choice"]; choice != nil {
		if s, ok := choice.(string); !ok || (s != "" && s != "auto" && s != "none") {
			return false
		}
	}
	for _, key := range []string{
		"temperature", "top_p", "max_tokens", "stop", "n", "presence_penalty",
		"frequency_penalty", "response_format", "parallel_tool_calls",
	} {
		if params[key] != nil {
			return false
		}
	}
	return true
}

func (b *CopilotBackend) chatSDK(ctx context.Context, model string, messages []NeutralMessage, params map[string]any, stream bool) (ChatResult, error) {
	client, err := b.sdk(ctx)
	if err != nil {
		return ChatResult{}, err
	}
	session, err := client.CreateSession(ctx, b.sdkSessionConfig(model, params, stream))
	if err != nil {
		return ChatResult{}, err
	}
	defer session.Disconnect()
	prompt := messages[0].Content
	event, err := session.SendAndWait(ctx, sdk.MessageOptions{Prompt: prompt})
	if err != nil {
		return ChatResult{}, err
	}
	content := ""
	outputTokens := 0
	if event != nil {
		if data, ok := event.Data.(*sdk.AssistantMessageData); ok {
			content = data.Content
			if data.OutputTokens != nil {
				outputTokens = int(*data.OutputTokens)
			}
			if data.Model != nil {
				model = *data.Model
			}
		}
	}
	usage := Usage{InputTokens: approxTokens(prompt), OutputTokens: outputTokens, APIEndpoint: endpointChatCompletions}.Normalized()
	if usage.OutputTokens == 0 {
		usage.OutputTokens = approxTokens(content)
		usage = usage.Normalized()
	}
	return ChatResult{Content: content, Model: model, Usage: usage, FinishReason: "stop"}, nil
}

func (b *CopilotBackend) chatStreamSDK(ctx context.Context, model string, messages []NeutralMessage, params map[string]any) (<-chan StreamItem, error) {
	client, err := b.sdk(ctx)
	if err != nil {
		return nil, err
	}
	session, err := client.CreateSession(ctx, b.sdkSessionConfig(model, params, true))
	if err != nil {
		return nil, err
	}
	prompt := messages[0].Content
	out := make(chan StreamItem)
	go func() {
		defer close(out)
		defer session.Disconnect()
		done := make(chan struct{})
		errCh := make(chan error, 1)
		var once sync.Once
		var finalContent string
		outputTokens := 0
		markDone := func() { once.Do(func() { close(done) }) }
		unsubscribe := session.On(func(event sdk.SessionEvent) {
			switch data := event.Data.(type) {
			case *sdk.AssistantMessageDeltaData:
				emitStreamItem(ctx, out, StreamItem{Kind: "delta", Text: data.DeltaContent})
			case *sdk.AssistantMessageData:
				finalContent = data.Content
				if data.OutputTokens != nil {
					outputTokens = int(*data.OutputTokens)
				}
			case *sdk.SessionErrorData:
				msg := data.Message
				if data.StatusCode != nil {
					msg = fmt.Sprintf("sdk session error %d: %s", *data.StatusCode, msg)
				} else if data.ErrorType != "" {
					msg = fmt.Sprintf("sdk session error %s: %s", data.ErrorType, msg)
				}
				select {
				case errCh <- fmt.Errorf("%s", msg):
				default:
				}
				markDone()
			case *sdk.SessionIdleData:
				markDone()
			}
		})
		defer unsubscribe()
		if _, err := session.Send(ctx, sdk.MessageOptions{Prompt: prompt}); err != nil {
			emitStreamItem(ctx, out, StreamItem{Kind: "error", Err: err})
			return
		}
		select {
		case <-done:
			select {
			case err := <-errCh:
				emitStreamItem(ctx, out, StreamItem{Kind: "error", Err: err})
				return
			default:
			}
			usage := Usage{InputTokens: approxTokens(prompt), OutputTokens: outputTokens, APIEndpoint: endpointChatCompletions}.Normalized()
			if usage.OutputTokens == 0 {
				usage.OutputTokens = approxTokens(finalContent)
				usage = usage.Normalized()
			}
			emitStreamItem(ctx, out, StreamItem{Kind: "done", Usage: usage, FinishReason: "stop"})
		case <-ctx.Done():
			_ = session.Abort(context.Background())
			emitStreamItem(context.Background(), out, StreamItem{Kind: "error", Err: ctx.Err()})
		}
	}()
	return out, nil
}

func (b *CopilotBackend) sdkSessionConfig(model string, params map[string]any, stream bool) *sdk.SessionConfig {
	cfg := &sdk.SessionConfig{
		ClientName:              "ghcp-pool-go",
		Model:                   model,
		Streaming:               sdk.Bool(stream),
		AvailableTools:          []string{},
		EnableConfigDiscovery:   sdk.Bool(false),
		SkipEmbeddingRetrieval:  sdk.Bool(true),
		EmbeddingCacheStorage:   sdk.String("in-memory"),
		EnableFileHooks:         sdk.Bool(false),
		EnableHostGitOperations: sdk.Bool(false),
		EnableSessionStore:      sdk.Bool(false),
		EnableSkills:            sdk.Bool(false),
	}
	if effort := stringParam(params, "reasoning_effort"); effort != "" && effort != "none" {
		cfg.ReasoningEffort = effort
	}
	return cfg
}

func (b *CopilotBackend) doCopilot(ctx context.Context, method, endpoint string, body any, stream bool) (*http.Response, []byte, error) {
	access, err := b.validAccessToken(ctx)
	if err != nil {
		return nil, nil, err
	}
	reader, err := requestBody(body)
	if err != nil {
		return nil, nil, err
	}
	reqCtx := ctx
	var cancel context.CancelFunc
	if !stream {
		reqCtx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, method, access.BaseURL+endpoint, reader)
	if err != nil {
		return nil, nil, err
	}
	addCopilotHeaders(req, access.Token)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := copilotHTTPClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("copilot request failed: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, nil, fmt.Errorf("copilot upstream error %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	if stream {
		return resp, nil, nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("read copilot response: %w", err)
	}
	return nil, data, nil
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
			BaseURL:   copilotBaseURLFromBearer(b.githubToken),
			ExpiresAt: copilotExpiryFromBearer(b.githubToken),
		}
		return b.access, nil
	}
	token, err := exchangeGitHubTokenForCopilot(ctx, b.githubToken)
	if err != nil {
		return copilotAccessToken{}, err
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
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("User-Agent", copilotUserAgent)
	req.Header.Set("Accept", "application/json")
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
		return copilotAccessToken{}, fmt.Errorf("Copilot token request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
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
	switch v := body.(type) {
	case nil:
		return nil, nil
	case []byte:
		return bytes.NewReader(v), nil
	case io.Reader:
		return v, nil
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(data), nil
	}
}

func addCopilotHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", copilotUserAgent)
	req.Header.Set("Editor-Version", copilotEditorVersion)
	req.Header.Set("Editor-Plugin-Version", copilotPluginVersion)
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")
	req.Header.Set("Openai-Intent", "conversation-agent")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Github-Api-Version", "2025-04-01")
	req.Header.Set("X-Request-Id", newID("req"))
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
	if choice := chatToolChoice(params["tool_choice"]); choice != nil {
		body["tool_choice"] = choice
	}
	copyParam(body, params, "parallel_tool_calls")
	if tools := openAITools(params["tools"]); len(tools) > 0 {
		body["tools"] = tools
	}
	if stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	return compactMap(body)
}

func responsesPayload(model string, messages []NeutralMessage, params map[string]any, stream bool) map[string]any {
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
		"store":  false,
	}
	if len(instructions) > 0 {
		body["instructions"] = strings.Join(instructions, "\n")
	}
	copyParam(body, params, "temperature")
	copyParam(body, params, "top_p")
	copyParamAs(body, params, "max_tokens", "max_output_tokens")
	if choice := responsesToolChoice(params["tool_choice"]); choice != nil {
		body["tool_choice"] = choice
	}
	copyParam(body, params, "parallel_tool_calls")
	if tools, _ := params["tools"].([]map[string]any); len(tools) > 0 {
		body["tools"] = tools
	}
	if effort := stringParam(params, "reasoning_effort"); effort != "" && effort != "none" {
		body["reasoning"] = map[string]any{"effort": effort, "summary": "detailed"}
	}
	if responseFormat, ok := params["response_format"].(map[string]any); ok && len(responseFormat) > 0 {
		body["text"] = map[string]any{"format": responseFormat}
	}
	return compactMap(body)
}

func anthropicPayload(model string, messages []NeutralMessage, params map[string]any, stream bool) map[string]any {
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
	if tools := anthropicTools(params["tools"]); len(tools) > 0 {
		body["tools"] = tools
	}
	if choice := anthropicToolChoiceForBackend(firstAny(optionValue(options, "tool_choice", nil), params["tool_choice"])); choice != nil {
		body["tool_choice"] = choice
	}
	if thinking := optionValue(options, "thinking", nil); thinking != nil {
		body["thinking"] = thinking
	}
	return compactMap(body)
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

func copyParam(dest map[string]any, params map[string]any, key string) {
	copyParamAs(dest, params, key, key)
}

func copyParamAs(dest map[string]any, params map[string]any, src, dst string) {
	if value, ok := params[src]; ok && value != nil {
		dest[dst] = value
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
		Model:        firstNonEmpty(payload.Model, model),
		Content:      content,
		Usage:        payload.Usage.toUsage(endpointResponses),
		FinishReason: "stop",
		ToolCalls:    toolCallsFromResponsesOutput(payload.Output),
	}, nil
}

func parseAnthropicResult(data []byte, model string) (ChatResult, error) {
	var payload struct {
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
		case "text":
			if value := stringFromAny(block["text"]); value != "" {
				text = append(text, value)
			}
		case "tool_use":
			args, _ := json.Marshal(firstAny(block["input"], map[string]any{}))
			toolCalls = append(toolCalls, ToolCall{ID: stringFromAny(block["id"]), Name: stringFromAny(block["name"]), Arguments: string(args), Kind: "function"})
		}
	}
	finish := "stop"
	if payload.StopReason == "tool_use" {
		finish = "tool_calls"
	} else if payload.StopReason == "max_tokens" {
		finish = "length"
	}
	return ChatResult{
		Model:        firstNonEmpty(payload.Model, model),
		Content:      strings.Join(text, "\n\n"),
		Usage:        Usage{InputTokens: payload.Usage.InputTokens, OutputTokens: payload.Usage.OutputTokens, APIEndpoint: endpointMessages}.Normalized(),
		FinishReason: finish,
		ToolCalls:    toolCalls,
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
			if block, ok := content.(map[string]any); ok && stringFromAny(block["type"]) == "output_text" {
				if text := stringFromAny(block["text"]); text != "" {
					parts = append(parts, text)
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
			msg := firstNonEmpty(stringFromAny(item["message"]), stringFromAny(item["error"]))
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
	default:
		return nil
	}
}
