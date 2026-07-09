package gateway

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdk "github.com/github/copilot-sdk/go"
)

var (
	copilotCLIClientForParams = func(b *CopilotCLIBackend, ctx context.Context, params map[string]any) (*sdk.Client, func(), error) {
		return b.clientForParams(ctx, params)
	}
	copilotCLICreateSession = func(client *sdk.Client, ctx context.Context, cfg *sdk.SessionConfig) (*sdk.Session, error) {
		return client.CreateSession(ctx, cfg)
	}
	copilotCLIDisconnectSession = func(session *sdk.Session) {
		if session != nil {
			session.Disconnect()
		}
	}
	copilotCLIStreamSession = streamSDKSession
)

// CopilotCLIBackend is a backend that uses Copilot SDK in CLI mode exclusively.
// Unlike CopilotBackend which supports both SDK and OpenCode modes, this backend
// always uses ModeCopilotCli for maximum compatibility with Copilot CLI features.
type CopilotCLIBackend struct {
	accountID     string
	githubToken   string
	homeDir       string
	webSearchMode string
	customTools   []string

	mu        sync.Mutex
	sdkClient *sdk.Client
	sdkReady  bool
	sdkErr    error
}

// CopilotCLIBackendOptions configures the CLI backend behavior
type CopilotCLIBackendOptions struct {
	WebSearchMode string   // "off", "empty", "cli", "native_cli"
	CustomTools   []string // Additional custom tool names to expose
}

// NewCopilotCLIBackend creates a new Copilot CLI backend with default options
func NewCopilotCLIBackend(accountID, token, homeDir string) *CopilotCLIBackend {
	return NewCopilotCLIBackendWithOptions(accountID, token, homeDir, CopilotCLIBackendOptions{})
}

// NewCopilotCLIBackendWithOptions creates a new Copilot CLI backend with custom options
func NewCopilotCLIBackendWithOptions(accountID, token, homeDir string, options CopilotCLIBackendOptions) *CopilotCLIBackend {
	return &CopilotCLIBackend{
		accountID:     accountID,
		githubToken:   strings.TrimSpace(token),
		homeDir:       homeDir,
		webSearchMode: normalizeCopilotSDKWebSearchMode(options.WebSearchMode, options.WebSearchMode != ""),
		customTools:   normalizeStringList(options.CustomTools),
	}
}

// Start initializes the SDK client in CLI mode
func (b *CopilotCLIBackend) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.sdkReady {
		b.mu.Unlock()
		return nil
	}
	if b.sdkClient == nil {
		b.sdkClient = sdk.NewClient(b.cliClientOptions(""))
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

// ListModels discovers available models via SDK
func (b *CopilotCLIBackend) ListModels(ctx context.Context) ([]ModelSpec, error) {
	client, err := b.getSDKClient(ctx)
	if err != nil {
		return nil, err
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("cli backend: sdk model discovery returned no models")
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

// Chat performs a non-streaming chat completion
func (b *CopilotCLIBackend) Chat(ctx context.Context, model string, messages []NeutralMessage, params map[string]any) (ChatResult, error) {
	return b.chatCLI(ctx, model, messages, cleanBackendParams(params), false)
}

// ChatStream performs a streaming chat completion
func (b *CopilotCLIBackend) ChatStream(ctx context.Context, model string, messages []NeutralMessage, params map[string]any) (<-chan StreamItem, error) {
	clean := cleanBackendParams(params)
	prompt := b.buildPrompt(messages, clean)
	endpoint := sdkUsageEndpoint(clean)
	out := make(chan StreamItem)
	go func() {
		defer close(out)

		client, cleanup, err := copilotCLIClientForParams(b, ctx, clean)
		if err != nil {
			emitStreamItem(ctx, out, StreamItem{Kind: "error", Err: fmt.Errorf("cli backend: create sdk client: %w", err)})
			return
		}
		defer cleanup()

		session, err := copilotCLICreateSession(client, ctx, b.sessionConfig(model, clean, true))
		if err != nil {
			emitStreamItem(ctx, out, StreamItem{Kind: "error", Err: fmt.Errorf("cli backend: create sdk session: %w", err)})
			return
		}
		defer copilotCLIDisconnectSession(session)

		copilotCLIStreamSession(ctx, session, prompt, endpoint, out)
	}()
	return out, nil
}

// Embeddings returns an error as CLI backend does not support embeddings
func (b *CopilotCLIBackend) Embeddings(ctx context.Context, model string, inputs []string, params map[string]any) (EmbeddingResult, error) {
	return EmbeddingResult{}, ErrEmbeddingsUnsupported
}

// Close stops the SDK client
func (b *CopilotCLIBackend) Close() error {
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

// chatCLI performs a CLI-mode chat with retry logic for transient errors
func (b *CopilotCLIBackend) chatCLI(ctx context.Context, model string, messages []NeutralMessage, params map[string]any, stream bool) (ChatResult, error) {
	var lastErr error
	attempts := transientRetryAttempts(params)
	for attempt := 0; attempt < attempts; attempt++ {
		result, err := b.chatCLIOnce(ctx, model, messages, params, stream)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !isTransientOverloadError(err) || attempt == attempts-1 {
			return ChatResult{}, err
		}
		if !sleepTransientRetry(ctx, attempt) {
			return ChatResult{}, ctx.Err()
		}
	}
	return ChatResult{}, lastErr
}

// chatCLIOnce performs a single CLI-mode chat attempt
func (b *CopilotCLIBackend) chatCLIOnce(ctx context.Context, model string, messages []NeutralMessage, params map[string]any, stream bool) (ChatResult, error) {
	client, cleanup, err := copilotCLIClientForParams(b, ctx, params)
	if err != nil {
		return ChatResult{}, err
	}
	defer cleanup()
	session, err := copilotCLICreateSession(client, ctx, b.sessionConfig(model, params, stream))
	if err != nil {
		return ChatResult{}, err
	}
	defer copilotCLIDisconnectSession(session)
	prompt := b.buildPrompt(messages, params)
	result, err := b.sendAndCollect(ctx, session, model, prompt, params)
	if err != nil {
		return ChatResult{}, err
	}
	return applySDKOutputConstraints(result, params), nil
}

// getSDKClient returns the initialized SDK client
func (b *CopilotCLIBackend) getSDKClient(ctx context.Context) (*sdk.Client, error) {
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
		return nil, fmt.Errorf("cli backend: sdk client not available")
	}
	return nil, fmt.Errorf("cli backend: sdk client not connected")
}

// clientForParams returns an SDK client configured for the request
// For web search requests, creates a temporary CLI session
func (b *CopilotCLIBackend) clientForParams(ctx context.Context, params map[string]any) (*sdk.Client, func(), error) {
	if !b.needsCLISession(params) {
		client, err := b.getSDKClient(ctx)
		return client, func() {}, err
	}
	client := sdk.NewClient(b.cliClientOptions("cli-web-search"))
	if err := client.Start(ctx); err != nil {
		return nil, func() {}, err
	}
	return client, func() { _ = client.Stop() }, nil
}

// cliClientOptions creates SDK client options for CLI mode
func (b *CopilotCLIBackend) cliClientOptions(suffix string) *sdk.ClientOptions {
	baseDir := b.homeDir
	if baseDir == "" {
		baseDir = "runtime-home/" + b.accountID
	}
	if suffix != "" {
		baseDir = filepath.Join(baseDir, suffix)
	}
	return &sdk.ClientOptions{
		BaseDirectory:             baseDir,
		GitHubToken:               b.gitHubToken(),
		UseLoggedInUser:           sdk.Bool(b.gitHubToken() == ""),
		Mode:                      sdk.ModeCopilotCli,
		SessionIdleTimeoutSeconds: 60,
	}
}

// sessionConfig creates SDK session configuration
func (b *CopilotCLIBackend) sessionConfig(model string, params map[string]any, stream bool) *sdk.SessionConfig {
	if b.useCLIWebSearch(params) {
		cfg := &sdk.SessionConfig{
			ClientName:     "ghcp-pool-go-cli",
			Model:          model,
			Streaming:      sdk.Bool(stream),
			Tools:          sdkCLIWebSearchTools(),
			AvailableTools: b.availableToolsForParams(params),
		}
		b.applyModelOptions(cfg, params)
		return cfg
	}
	if b.useNativeCLIWebSearch(params) {
		cfg := &sdk.SessionConfig{
			ClientName:          "ghcp-pool-go-cli",
			Model:               model,
			Streaming:           sdk.Bool(stream),
			Tools:               sdkNativeCLIWebSearchTools(params),
			AvailableTools:      b.availableToolsForParams(params),
			OnPermissionRequest: sdk.PermissionHandler.ApproveAll,
		}
		b.applyModelOptions(cfg, params)
		return cfg
	}
	cfg := &sdk.SessionConfig{
		ClientName:              "ghcp-pool-go-cli",
		Model:                   model,
		Streaming:               sdk.Bool(stream),
		Tools:                   sdkCustomToolsFromParams(params),
		AvailableTools:          b.availableToolsForParams(params),
		EnableConfigDiscovery:   sdk.Bool(false),
		SkipEmbeddingRetrieval:  sdk.Bool(true),
		EmbeddingCacheStorage:   sdk.String("in-memory"),
		EnableFileHooks:         sdk.Bool(false),
		EnableHostGitOperations: sdk.Bool(false),
		EnableSessionStore:      sdk.Bool(false),
		EnableSkills:            sdk.Bool(false),
	}
	b.applyModelOptions(cfg, params)
	return cfg
}

// applyModelOptions applies model-specific parameters to session config
func (b *CopilotCLIBackend) applyModelOptions(cfg *sdk.SessionConfig, params map[string]any) {
	if effort := stringParam(params, "reasoning_effort"); effort != "" && effort != "none" {
		if modelSupportsSDKReasoningEffort(cfg.Model) {
			cfg.ReasoningEffort = effort
		}
	}
	if tier := stringParam(params, "context_tier"); tier != "" && tier != "default" {
		cfg.ContextTier = sdk.ContextTier(tier)
	}
}

// buildPrompt constructs the prompt from messages and parameters
func (b *CopilotCLIBackend) buildPrompt(messages []NeutralMessage, params map[string]any) string {
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
	if instruction := b.toolChoiceInstruction(params); instruction != "" {
		prompt = strings.TrimSpace(prompt) + "\n\n" + instruction
	}
	return prompt
}

// toolChoiceInstruction generates instruction text for tool choice
func (b *CopilotCLIBackend) toolChoiceInstruction(params map[string]any) string {
	if !hasSDKRequestTools(params) || toolChoiceIsNone(params["tool_choice"]) {
		return ""
	}
	if requestHasWebSearchTool(params) {
		if b.useCLIWebSearch(params) {
			return "Use the ghcp_web_search tool for web searches and ghcp_web_fetch for fetching URLs. Do not use built-in web_search or web_fetch tools."
		}
		if b.useNativeCLIWebSearch(params) {
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

// sendAndCollect sends the prompt to the SDK session and collects the response
func (b *CopilotCLIBackend) sendAndCollect(ctx context.Context, session *sdk.Session, model, prompt string, params map[string]any) (ChatResult, error) {
	timeout := 60 * time.Second
	if b.needsCLISession(params) {
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
				if b.useCLIWebSearch(params) && isSDKWebSearchInternalTool(request.Name) {
					continue
				}
				if b.useNativeCLIWebSearch(params) && isSDKNativeWebTool(request.Name) {
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
			if b.useCLIWebSearch(params) && isSDKWebSearchInternalTool(data.ToolName) {
				return
			}
			if b.useNativeCLIWebSearch(params) && isSDKNativeWebTool(data.ToolName) {
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
			case errCh <- fmt.Errorf("cli backend session error: %s", data.Message):
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
		return ChatResult{}, fmt.Errorf("cli backend: waiting for sdk session: %w", ctx.Err())
	}
}

// availableToolsForParams returns available tools for the request
func (b *CopilotCLIBackend) availableToolsForParams(params map[string]any) []string {
	if toolChoiceIsNone(params["tool_choice"]) {
		return []string{}
	}
	if b.useNativeCLIWebSearch(params) {
		tools := []string{"builtin:web_search", "web_fetch"}
		for _, tool := range sdkCustomToolsFromParams(params) {
			if tool.Name != "" && !containsString(tools, tool.Name) && !containsString(tools, "custom:"+tool.Name) {
				tools = append(tools, tool.Name)
			}
		}
		return tools
	}
	if b.useCLIWebSearch(params) {
		tools := []string{"ghcp_web_search", "ghcp_web_fetch", "ghcp_report_intent"}
		for _, tool := range sdkCustomToolsFromParams(params) {
			if tool.Name != "" && !containsString(tools, tool.Name) && !containsString(tools, "custom:"+tool.Name) {
				tools = append(tools, tool.Name)
			}
		}
		return tools
	}
	tools := b.baseAvailableTools()
	if b.webSearchMode == "empty" && requestHasWebSearchTool(params) && !containsString(tools, "web_search") && !containsString(tools, "builtin:web_search") {
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

// baseAvailableTools returns the base set of available tools
func (b *CopilotCLIBackend) baseAvailableTools() []string {
	tools := normalizeStringList(b.customTools)
	if b.webSearchMode == "empty" && !containsString(tools, "web_search") && !containsString(tools, "builtin:web_search") {
		tools = append(tools, "web_search")
	}
	if tools == nil {
		return []string{}
	}
	return tools
}

// needsCLISession returns true if the request requires a CLI session
func (b *CopilotCLIBackend) needsCLISession(params map[string]any) bool {
	return (b.webSearchMode == "cli" || b.webSearchMode == "native_cli") && requestHasWebSearchTool(params)
}

// useCLIWebSearch returns true if using gateway-controlled CLI web search
func (b *CopilotCLIBackend) useCLIWebSearch(params map[string]any) bool {
	return b.webSearchMode == "cli" && requestHasWebSearchTool(params)
}

// useNativeCLIWebSearch returns true if using native CLI web search
func (b *CopilotCLIBackend) useNativeCLIWebSearch(params map[string]any) bool {
	return b.webSearchMode == "native_cli" && requestHasWebSearchTool(params)
}

// gitHubToken returns the GitHub token or empty string if using bearer token
func (b *CopilotCLIBackend) gitHubToken() string {
	if looksLikeCopilotBearer(b.githubToken) {
		return ""
	}
	return b.githubToken
}
