package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	sdk "github.com/github/copilot-sdk/go"
)

const (
	defaultCopilotOAuthClientID = "Ov23li8tweQw6odWQebz"
	copilotBackendModeSDK       = "sdk"
	copilotUserAgent            = "GitHubCopilotChat/0.39.0"
)

var ValidCopilotBackendModes = map[string]bool{
	copilotBackendModeSDK: true,
}

var sdkWebHTTPClient = &http.Client{Timeout: 20 * time.Second}

var (
	ddgResultRE               = regexp.MustCompile(`(?is)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippetRE              = regexp.MustCompile(`(?is)<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	bingResultRE              = regexp.MustCompile(`(?is)<li class="b_algo".*?<a href="([^"]+)"[^>]*>(.*?)</a>.*?(?:<p>(.*?)</p>)?`)
	htmlTagStripRE            = regexp.MustCompile(`(?is)<[^>]+>`)
	copilotSDKClientForParams = func(b *CopilotBackend, ctx context.Context, params map[string]any) (*sdk.Client, func(), error) {
		return b.sdkForParams(ctx, params)
	}
	copilotSDKCreateSession = func(client *sdk.Client, ctx context.Context, cfg *sdk.SessionConfig) (*sdk.Session, error) {
		return client.CreateSession(ctx, cfg)
	}
	copilotSDKDisconnectSession = func(session *sdk.Session) {
		if session != nil {
			session.Disconnect()
		}
	}
	copilotSDKStreamSession = streamSDKSession
)

type CopilotBackend struct {
	accountID        string
	githubToken      string
	homeDir          string
	mode             string
	cliMode          bool
	sdkWebSearch     bool
	sdkWebSearchMode string
	sdkTools         []string

	mu        sync.Mutex
	sdkClient *sdk.Client
	sdkReady  bool
	sdkErr    error
}

type CopilotBackendOptions struct {
	// CLIMode runs the SDK client in Copilot CLI mode instead of the
	// multi-tenant-safe empty mode.
	CLIMode          bool
	Mode             string
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
		cliMode:          options.CLIMode,
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
		b.sdkClient = sdk.NewClient(b.sdkClientOptions(b.defaultSDKClientMode(), ""))
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
	specs, err := b.listModelsSDK(ctx)
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("sdk model discovery returned no models")
	}
	return specs, nil
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

func (b *CopilotBackend) Chat(ctx context.Context, model string, messages []NeutralMessage, params map[string]any) (ChatResult, error) {
	endpoint := endpointFromParams(params, endpointChatCompletions)
	clean := cleanBackendParams(params)
	if b.canUseSDK(messages, clean) {
		return b.chatSDKStructured(ctx, model, messages, clean, false)
	}
	return ChatResult{}, unsupportedSDKModeRequest(endpoint)
}

func (b *CopilotBackend) Embeddings(ctx context.Context, model string, inputs []string, params map[string]any) (EmbeddingResult, error) {
	return EmbeddingResult{}, ErrEmbeddingsUnsupported
}

func (b *CopilotBackend) ChatStream(ctx context.Context, model string, messages []NeutralMessage, params map[string]any) (<-chan StreamItem, error) {
	endpoint := endpointFromParams(params, endpointChatCompletions)
	clean := cleanBackendParams(params)
	if !b.canUseSDK(messages, clean) {
		return nil, unsupportedSDKModeRequest(endpoint)
	}
	if parseResponseFormat(clean).wantsJSON() {
		return b.chatStreamStructured(ctx, model, messages, clean)
	}
	return b.chatStreamSDK(ctx, model, messages, clean)
}

// chatStreamStructured serves a response_format request by completing the turn
// first and only then emitting it.
//
// Structured output is validated, and a non-conforming answer is repaired or
// rejected. None of that is expressible once tokens have already reached the
// client, so these requests trade incremental delivery for the guarantee. The
// buffered result has already been through applySDKOutputConstraints, so the
// streaming constraint filter is deliberately not applied on top.
func (b *CopilotBackend) chatStreamStructured(ctx context.Context, model string, messages []NeutralMessage, params map[string]any) (<-chan StreamItem, error) {
	result, err := b.chatSDKStructured(ctx, model, messages, params, false)
	if err != nil {
		return nil, err
	}
	out := make(chan StreamItem)
	go func() {
		defer close(out)
		if result.Content != "" {
			if !emitStreamItem(ctx, out, StreamItem{Kind: "delta", Text: result.Content}) {
				return
			}
		}
		for i, call := range result.ToolCalls {
			if !emitStreamItem(ctx, out, StreamItem{Kind: "tool_call", ToolCall: call, Index: i}) {
				return
			}
		}
		emitStreamItem(ctx, out, StreamItem{Kind: "done", Usage: result.Usage, FinishReason: result.FinishReason})
	}()
	return out, nil
}

func unsupportedSDKModeRequest(endpoint string) error {
	return fmt.Errorf("copilot sdk mode does not support this request shape on endpoint %q; use an sdk-compatible request shape", endpoint)
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
	// CLI mode always has a usable client: with no token the SDK falls back to
	// the logged-in user, which is how the CLI backend was always started.
	return b.cliMode || b.sdkGitHubToken() != "" || b.homeDir != ""
}

// defaultSDKClientMode is the client mode for this backend's long-lived client.
// CLI mode maximizes Copilot CLI feature compatibility; empty mode exposes no
// built-in tools and is the multi-tenant-safe default.
func (b *CopilotBackend) defaultSDKClientMode() sdk.ClientMode {
	if b.cliMode {
		return sdk.ModeCopilotCli
	}
	return sdk.ModeEmpty
}

func (b *CopilotBackend) sdkClientName() string {
	if b.cliMode {
		return "ghcp-pool-go-cli"
	}
	return "ghcp-pool-go"
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

func (b *CopilotBackend) chatSDKStructured(ctx context.Context, model string, messages []NeutralMessage, params map[string]any, stream bool) (ChatResult, error) {
	if err := structuredOutputToolConflict(params); err != nil {
		return ChatResult{}, err
	}
	spec := parseResponseFormat(params)
	result, err := b.chatSDK(ctx, model, messages, params, stream)
	if err != nil || !spec.wantsJSON() || len(result.ToolCalls) > 0 {
		return result, err
	}
	cause := validateStructuredOutput(result.Content, spec)
	if cause == nil {
		return result, nil
	}
	repaired, repairErr := b.chatSDK(ctx, model, structuredOutputRepairMessages(messages, result.Content, cause), params, stream)
	if repairErr != nil {
		return ChatResult{}, repairErr
	}
	repaired.Usage = accumulateUsage(result.Usage, repaired.Usage)
	if len(repaired.ToolCalls) > 0 {
		return repaired, nil
	}
	if err := validateStructuredOutput(repaired.Content, spec); err != nil {
		return ChatResult{}, err
	}
	return repaired, nil
}

func (b *CopilotBackend) chatSDK(ctx context.Context, model string, messages []NeutralMessage, params map[string]any, stream bool) (ChatResult, error) {
	return retryTransientChat(ctx, params, func() (ChatResult, error) {
		return b.chatSDKOnce(ctx, model, messages, params, stream)
	})
}

// retryTransientChat retries attempt while it reports upstream overload.
//
// Kept separate from the SDK call so the retry policy is exercisable without a
// live session; the direct-HTTP test that used to cover it went away with the
// transport it mocked.
func retryTransientChat(ctx context.Context, params map[string]any, attempt func() (ChatResult, error)) (ChatResult, error) {
	var lastErr error
	attempts := transientRetryAttempts(params)
	for i := 0; i < attempts; i++ {
		result, err := attempt()
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !isTransientOverloadError(err) || i == attempts-1 {
			return ChatResult{}, err
		}
		if !sleepTransientRetry(ctx, i) {
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

// transientRetryDelay is a variable so tests can collapse the backoff.
var transientRetryDelay = func(attempt int) time.Duration {
	return time.Duration(750*(attempt+1)) * time.Millisecond
}

func sleepTransientRetry(ctx context.Context, attempt int) bool {
	delay := transientRetryDelay(attempt)
	select {
	case <-time.After(delay):
		return true
	case <-ctx.Done():
		return false
	}
}

func (b *CopilotBackend) sendSDKAndCollect(ctx context.Context, session *sdk.Session, model, prompt string, params map[string]any) (ChatResult, error) {
	// Always enforce maximum timeout, even if parent context has a deadline
	timeout := 60 * time.Second
	if b.useSDKCLIWebSearch(params) {
		timeout = 180 * time.Second
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
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
		// Add fallback timeout for tool settle to prevent infinite waits
		settleCtx, settleCancel := context.WithTimeout(ctx, 5*time.Second)
		waitForSDKToolSettle(settleCtx, toolCh)
		settleCancel()
		abortCtx, abortCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = session.Abort(abortCtx)
		abortCancel()
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
	prompt := b.sdkPrompt(messages, params)
	endpoint := sdkUsageEndpoint(params)
	raw := make(chan StreamItem)
	go func() {
		defer close(raw)

		client, cleanup, err := copilotSDKClientForParams(b, ctx, params)
		if err != nil {
			emitStreamItem(ctx, raw, StreamItem{Kind: "error", Err: fmt.Errorf("sdk backend: create sdk client: %w", err)})
			return
		}
		defer cleanup()

		session, err := copilotSDKCreateSession(client, ctx, b.sdkSessionConfig(model, params, true))
		if err != nil {
			emitStreamItem(ctx, raw, StreamItem{Kind: "error", Err: fmt.Errorf("sdk backend: create sdk session: %w", err)})
			return
		}
		defer copilotSDKDisconnectSession(session)

		copilotSDKStreamSession(ctx, session, prompt, endpoint, raw)
	}()
	return applyStreamOutputConstraints(ctx, raw, params), nil
}

func (b *CopilotBackend) sdkPrompt(messages []NeutralMessage, params map[string]any) string {
	prompt := ""
	if len(messages) == 1 && strings.EqualFold(messages[0].Role, "user") {
		prompt = messages[0].Content
	} else {
		prompt = renderPrompt(messages)
	}
	spec := parseResponseFormat(params)
	_, toolRegistered := structuredOutputTool(spec)
	toolRegistered = toolRegistered && !toolChoiceIsNone(params["tool_choice"])
	if instruction := structuredOutputInstruction(spec, toolRegistered); instruction != "" {
		prompt = strings.TrimSpace(prompt) + "\n\n" + instruction
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
			return "Use Copilot CLI native web_search for web searches and web_fetch for fetching URLs. The web_fetch tool is gateway-provided and can fetch GitHub raw/blob URLs."
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
	result = applyStructuredOutput(result, parseResponseFormat(params))
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
	trimmed := stopped
	if maxTokens := sdkMaxOutputTokens(params); maxTokens > 0 {
		if truncated, ok := truncateApproxTokens(result.Content, maxTokens); ok {
			result.Content = truncated
			result.FinishReason = "length"
			trimmed = true
		}
	}
	// The SDK's own count is authoritative. Approximate only when it is absent,
	// or when the content was trimmed here so the upstream count no longer
	// describes what the client receives. In that case the upstream total
	// describes different content too, so drop it and let Normalized recompute:
	// a usage block whose total disagrees with its own parts is unparseable.
	if result.Usage.OutputTokens == 0 || trimmed {
		result.Usage.OutputTokens = approxTokens(result.Content)
	}
	if trimmed {
		result.Usage.TotalTokens = 0
	}
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

// truncateApproxTokens trims content to roughly maxTokens, counted the same way
// approxTokens counts, and cuts on a rune boundary so scripts without spaces are
// not left unbounded or rejoined with spaces they never had.
func truncateApproxTokens(content string, maxTokens int) (string, bool) {
	if maxTokens <= 0 || approxTokens(content) <= maxTokens {
		return content, false
	}
	runes := []rune(content)
	wide, narrow, cut := 0, 0, len(runes)
	for i, r := range runes {
		if isWideScriptRune(r) {
			wide++
		} else {
			narrow++
		}
		if wide+(narrow+3)/4 > maxTokens {
			cut = i
			break
		}
	}
	head := string(runes[:cut])
	// Prefer a word boundary so space-delimited text is not cut mid-word. Scripts
	// without spaces have no boundary to find and are cut on the rune.
	if idx := strings.LastIndexAny(head, " \t\n"); idx > 0 {
		head = head[:idx]
	}
	return strings.TrimRight(head, " \t\n"), true
}

func (b *CopilotBackend) sdkSessionConfig(model string, params map[string]any, stream bool) *sdk.SessionConfig {
	if b.useSDKControlledCLIWebSearch(params) {
		cfg := &sdk.SessionConfig{
			ClientName:     b.sdkClientName(),
			Model:          model,
			Streaming:      sdk.Bool(stream),
			Tools:          withStructuredOutputTool(sdkCLIWebSearchTools(), params),
			AvailableTools: b.sdkAvailableToolsForParams(params),
		}
		b.applySDKModelOptions(cfg, params)
		return cfg
	}
	if b.useSDKNativeCLIWebSearch(params) {
		cfg := &sdk.SessionConfig{
			ClientName:          b.sdkClientName(),
			Model:               model,
			Streaming:           sdk.Bool(stream),
			Tools:               withStructuredOutputTool(sdkNativeCLIWebSearchTools(params), params),
			AvailableTools:      b.sdkAvailableToolsForParams(params),
			OnPermissionRequest: sdk.PermissionHandler.ApproveAll,
		}
		b.applySDKModelOptions(cfg, params)
		return cfg
	}
	cfg := &sdk.SessionConfig{
		ClientName:              b.sdkClientName(),
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
		tools := []string{"builtin:web_search", "web_fetch"}
		for _, tool := range sdkCustomToolsFromParams(params) {
			if tool.Name != "" && !containsString(tools, tool.Name) && !containsString(tools, "custom:"+tool.Name) {
				tools = append(tools, tool.Name)
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

func sdkNativeCLIWebSearchTools(params map[string]any) []sdk.Tool {
	tools := []sdk.Tool{
		{
			Name:        "web_fetch",
			Description: "Fetch a web page by URL and return text for the model. GitHub blob/raw URLs are normalized to raw.githubusercontent.com before fetching.",
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
	return append(tools, sdkCustomToolsFromParams(params)...)
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
	out := make([]sdk.Tool, 0, len(tools)+1)
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
	// Declared here rather than in SessionConfig because this function also feeds
	// the AvailableTools allowlist, which would otherwise filter the tool out.
	out = withStructuredOutputTool(out, params)
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

func looksLikeCopilotBearer(token string) bool {
	return strings.Contains(token, "proxy-ep=") || strings.HasPrefix(token, "tid=")
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

func normalizeCopilotBackendMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return copilotBackendModeSDK
	}
	return mode
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

func rawResponsesPayload(params map[string]any) map[string]any {
	raw, ok := params[internalResponsesRawParam].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	return cloneMap(raw)
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

func noopAnthropicTool() map[string]any {
	return map[string]any{
		"name":        "_noop",
		"description": "Do not call this tool. It exists only for API compatibility and must never be invoked.",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"reason": map[string]any{"type": "string", "description": "Unused"},
			},
		},
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

func emitStreamItem(ctx context.Context, out chan<- StreamItem, item StreamItem) bool {
	select {
	case out <- item:
		return true
	case <-ctx.Done():
		return false
	}
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
