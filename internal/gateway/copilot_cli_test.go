package gateway

import (
	"context"
	"testing"
	"time"

	sdk "github.com/github/copilot-sdk/go"
)

func TestCopilotCLIBackend_Implementation(t *testing.T) {
	backend := NewCopilotCLIBackend("test-account", "gho_testtoken", "")
	if backend == nil {
		t.Fatal("NewCopilotCLIBackend returned nil")
	}
	if backend.accountID != "test-account" {
		t.Errorf("accountID = %q, want %q", backend.accountID, "test-account")
	}
	var _ Backend = backend
}

func TestCopilotCLIBackend_WithOptions(t *testing.T) {
	tests := []struct {
		name    string
		options CopilotCLIBackendOptions
		wantWeb string
	}{
		{
			name:    "default options",
			options: CopilotCLIBackendOptions{},
			wantWeb: "off",
		},
		{
			name:    "cli mode",
			options: CopilotCLIBackendOptions{WebSearchMode: "cli"},
			wantWeb: "cli",
		},
		{
			name:    "native_cli mode",
			options: CopilotCLIBackendOptions{WebSearchMode: "native_cli"},
			wantWeb: "native_cli",
		},
		{
			name:    "empty mode",
			options: CopilotCLIBackendOptions{WebSearchMode: "empty"},
			wantWeb: "empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := NewCopilotCLIBackendWithOptions("test-account", "gho_testtoken", "", tt.options)
			if backend.webSearchMode != tt.wantWeb {
				t.Errorf("webSearchMode = %q, want %q", backend.webSearchMode, tt.wantWeb)
			}
		})
	}
}

func TestCopilotCLIBackend_Close(t *testing.T) {
	backend := NewCopilotCLIBackend("test-account", "gho_testtoken", "")
	if err := backend.Close(); err != nil {
		t.Errorf("Close() returned unexpected error: %v", err)
	}
}

func TestCopilotCLIBackend_Embeddings(t *testing.T) {
	backend := NewCopilotCLIBackend("test-account", "gho_testtoken", "")
	ctx := context.Background()
	_, err := backend.Embeddings(ctx, "model", []string{"test"}, nil)
	if err != ErrEmbeddingsUnsupported {
		t.Errorf("Embeddings() error = %v, want %v", err, ErrEmbeddingsUnsupported)
	}
}

func TestCopilotCLIBackend_ClientOptions(t *testing.T) {
	tests := []struct {
		name      string
		accountID string
		homeDir   string
		suffix    string
		wantBase  string
	}{
		{
			name:      "no home dir",
			accountID: "acc1",
			homeDir:   "",
			suffix:    "",
			wantBase:  "runtime-home/acc1",
		},
		{
			name:      "with home dir",
			accountID: "acc1",
			homeDir:   "/tmp/home",
			suffix:    "",
			wantBase:  "/tmp/home",
		},
		{
			name:      "with suffix",
			accountID: "acc1",
			homeDir:   "",
			suffix:    "cli-web-search",
			wantBase:  "runtime-home/acc1/cli-web-search",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := NewCopilotCLIBackend(tt.accountID, "gho_testtoken", tt.homeDir)
			opts := backend.cliClientOptions(tt.suffix)
			if opts.BaseDirectory != tt.wantBase {
				t.Errorf("BaseDirectory = %q, want %q", opts.BaseDirectory, tt.wantBase)
			}
			if string(opts.Mode) != "copilot-cli" {
				t.Errorf("Mode = %q, want %q", opts.Mode, "copilot-cli")
			}
		})
	}
}

func TestCopilotCLIBackend_PromptBuilding(t *testing.T) {
	backend := NewCopilotCLIBackend("test-account", "gho_testtoken", "")
	tests := []struct {
		name     string
		messages []NeutralMessage
		params   map[string]any
		want     string
	}{
		{
			name: "single user message",
			messages: []NeutralMessage{
				{Role: "user", Content: "Hello"},
			},
			params: nil,
			want:   "Hello",
		},
		{
			name: "multiple messages",
			messages: []NeutralMessage{
				{Role: "system", Content: "You are helpful."},
				{Role: "user", Content: "Hello"},
			},
			params: nil,
			want:   "[system]\nYou are helpful.\n\n[user]\nHello\n\n[assistant]",
		},
		{
			name: "json response format",
			messages: []NeutralMessage{
				{Role: "user", Content: "Return data"},
			},
			params: map[string]any{
				"response_format": map[string]any{"type": "json_object"},
			},
			want: "Return data\n\nRespond with valid JSON only.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := backend.buildPrompt(tt.messages, tt.params)
			if got != tt.want {
				t.Errorf("buildPrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCopilotCLIBackend_ToolChoiceInstruction(t *testing.T) {
	tests := []struct {
		name          string
		webSearchMode string
		params        map[string]any
		want          string
	}{
		{
			name:          "no tools",
			webSearchMode: "off",
			params:        nil,
			want:          "",
		},
		{
			name:          "tool_choice none",
			webSearchMode: "cli",
			params: map[string]any{
				"tools":       []map[string]any{{"name": "test"}},
				"tool_choice": "none",
			},
			want: "",
		},
		{
			name:          "cli web search",
			webSearchMode: "cli",
			params: map[string]any{
				"tools": []map[string]any{{"name": "web_search"}},
			},
			want: "Use the ghcp_web_search tool for web searches and ghcp_web_fetch for fetching URLs. Do not use built-in web_search or web_fetch tools.",
		},
		{
			name:          "native_cli web search",
			webSearchMode: "native_cli",
			params: map[string]any{
				"tools": []map[string]any{{"name": "web_search"}},
			},
			want: "Use Copilot CLI native web_search for web searches and web_fetch for fetching URLs. The web_fetch tool is gateway-provided and can fetch GitHub raw/blob URLs.",
		},
		{
			name:          "forced tool",
			webSearchMode: "off",
			params: map[string]any{
				"tools":       []map[string]any{{"name": "calculator"}},
				"tool_choice": map[string]any{"name": "calculator"},
			},
			want: "Use the calculator tool for this turn and return the tool request if more information is needed.",
		},
		{
			name:          "required tool choice",
			webSearchMode: "off",
			params: map[string]any{
				"tools":       []map[string]any{{"name": "test"}},
				"tool_choice": "required",
			},
			want: "Use one of the available tools for this turn and return the tool request if more information is needed.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := NewCopilotCLIBackendWithOptions("test-account", "gho_testtoken", "", CopilotCLIBackendOptions{
				WebSearchMode: tt.webSearchMode,
			})
			got := backend.toolChoiceInstruction(tt.params)
			if got != tt.want {
				t.Errorf("toolChoiceInstruction() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCopilotCLIBackend_AvailableTools(t *testing.T) {
	tests := []struct {
		name          string
		webSearchMode string
		customTools   []string
		params        map[string]any
		want          []string
	}{
		{
			name:          "no tools, tool_choice none",
			webSearchMode: "off",
			params:        map[string]any{"tool_choice": "none"},
			want:          []string{},
		},
		{
			name:          "cli web search mode",
			webSearchMode: "cli",
			params: map[string]any{
				"tools": []map[string]any{{"name": "web_search"}},
			},
			want: []string{"ghcp_web_search", "ghcp_web_fetch", "ghcp_report_intent"},
		},
		{
			name:          "native_cli web search mode",
			webSearchMode: "native_cli",
			params: map[string]any{
				"tools": []map[string]any{{"name": "web_search"}},
			},
			want: []string{"builtin:web_search", "web_fetch"},
		},
		{
			name:          "empty mode with web_search",
			webSearchMode: "empty",
			params: map[string]any{
				"tools": []map[string]any{{"name": "web_search"}},
			},
			want: []string{"web_search"},
		},
		{
			name:          "custom tools",
			webSearchMode: "off",
			customTools:   []string{"custom1", "custom2"},
			params:        map[string]any{},
			want:          []string{"custom1", "custom2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := NewCopilotCLIBackendWithOptions("test-account", "gho_testtoken", "", CopilotCLIBackendOptions{
				WebSearchMode: tt.webSearchMode,
				CustomTools:   tt.customTools,
			})
			got := backend.availableToolsForParams(tt.params)
			if len(got) != len(tt.want) {
				t.Errorf("availableToolsForParams() length = %d, want %d", len(got), len(tt.want))
				return
			}
			for i, want := range tt.want {
				if i >= len(got) || got[i] != want {
					t.Errorf("availableToolsForParams()[%d] = %q, want %q", i, got[i], want)
				}
			}
		})
	}
}

func TestCopilotCLIBackend_SessionConfig(t *testing.T) {
	tests := []struct {
		name          string
		webSearchMode string
		model         string
		params        map[string]any
		wantClient    string
		checkTools    func(*testing.T, []string)
	}{
		{
			name:          "basic config",
			webSearchMode: "off",
			model:         "gpt-5.5",
			params:        nil,
			wantClient:    "ghcp-pool-go-cli",
			checkTools: func(t *testing.T, tools []string) {
				if len(tools) != 0 {
					t.Errorf("expected no tools for nil params, got %v", tools)
				}
			},
		},
		{
			name:          "cli web search",
			webSearchMode: "cli",
			model:         "gpt-5.5",
			params: map[string]any{
				"tools": []map[string]any{{"name": "web_search"}},
			},
			wantClient: "ghcp-pool-go-cli",
			checkTools: func(t *testing.T, tools []string) {
				if len(tools) < 3 {
					t.Errorf("expected at least 3 CLI web search tools, got %v", tools)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := NewCopilotCLIBackendWithOptions("test-account", "gho_testtoken", "", CopilotCLIBackendOptions{
				WebSearchMode: tt.webSearchMode,
			})
			cfg := backend.sessionConfig(tt.model, tt.params, false)
			if cfg.ClientName != tt.wantClient {
				t.Errorf("ClientName = %q, want %q", cfg.ClientName, tt.wantClient)
			}
			if cfg.Model != tt.model {
				t.Errorf("Model = %q, want %q", cfg.Model, tt.model)
			}
			if tt.checkTools != nil && cfg.Tools != nil {
				var toolNames []string
				for _, tool := range cfg.Tools {
					toolNames = append(toolNames, tool.Name)
				}
				tt.checkTools(t, toolNames)
			}
		})
	}
}

func TestCopilotCLIBackend_ModelOptions(t *testing.T) {
	backend := NewCopilotCLIBackend("test-account", "gho_testtoken", "")
	tests := []struct {
		name       string
		model      string
		params     map[string]any
		wantEffort string
		wantTier   string
	}{
		{
			name:       "no options",
			model:      "gpt-5.5",
			params:     nil,
			wantEffort: "",
			wantTier:   "",
		},
		{
			name:  "reasoning effort supported",
			model: "claude-opus-4.8",
			params: map[string]any{
				"reasoning_effort": "high",
			},
			wantEffort: "high",
			wantTier:   "",
		},
		{
			name:  "reasoning effort haiku (stripped)",
			model: "claude-haiku-4.5",
			params: map[string]any{
				"reasoning_effort": "high",
			},
			wantEffort: "",
			wantTier:   "",
		},
		{
			name:  "context tier",
			model: "claude-opus-4.8",
			params: map[string]any{
				"context_tier": "long_context",
			},
			wantEffort: "",
			wantTier:   "long_context",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := backend.sessionConfig(tt.model, tt.params, false)
			if cfg.ReasoningEffort != tt.wantEffort {
				t.Errorf("ReasoningEffort = %q, want %q", cfg.ReasoningEffort, tt.wantEffort)
			}
			if string(cfg.ContextTier) != tt.wantTier {
				t.Errorf("ContextTier = %q, want %q", cfg.ContextTier, tt.wantTier)
			}
		})
	}
}

func TestCopilotCLIBackend_Timeout(t *testing.T) {
	backend := NewCopilotCLIBackend("test-account", "gho_testtoken", "")
	tests := []struct {
		name          string
		webSearchMode string
		params        map[string]any
		deadline      time.Duration
		wantMin       time.Duration
		wantMax       time.Duration
	}{
		{
			name:          "default timeout",
			webSearchMode: "off",
			params:        nil,
			wantMin:       59 * time.Second,
			wantMax:       61 * time.Second,
		},
		{
			name:          "web search timeout",
			webSearchMode: "cli",
			params: map[string]any{
				"tools": []map[string]any{{"name": "web_search"}},
			},
			wantMin: 179 * time.Second,
			wantMax: 181 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend.webSearchMode = tt.webSearchMode
			ctx := context.Background()
			if tt.deadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.deadline)
				defer cancel()
			}
			start := time.Now()
			timeout := 60 * time.Second
			if backend.needsCLISession(tt.params) {
				timeout = 180 * time.Second
			}
			if deadline, ok := ctx.Deadline(); ok {
				remaining := time.Until(deadline)
				if remaining > 0 && remaining < timeout {
					timeout = remaining
				}
			}
			elapsed := time.Since(start)
			if elapsed > 100*time.Millisecond {
				t.Errorf("timeout calculation took too long: %v", elapsed)
			}
			if timeout < tt.wantMin || timeout > tt.wantMax {
				t.Errorf("timeout = %v, want between %v and %v", timeout, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestCopilotCLIBackend_ChatStreamUsesStreamingSession(t *testing.T) {
	// Given
	backend := NewCopilotCLIBackendWithOptions("test-account", "gho_testtoken", "", CopilotCLIBackendOptions{WebSearchMode: "off"})
	messages := []NeutralMessage{{Role: "system", Content: "You are terse."}, {Role: "user", Content: "List alpha beta gamma."}}
	params := map[string]any{
		"response_format": map[string]any{"type": "json_object"},
		"tool_choice":     "required",
		"tools":           []map[string]any{{"name": "calculator"}},
	}
	wantPrompt := backend.buildPrompt(messages, params)
	wantEndpoint := sdkUsageEndpoint(params)
	cleanupDone := make(chan struct{}, 1)
	disconnectDone := make(chan struct{}, 1)
	var streamCalled bool
	originalClientForParams := copilotCLIClientForParams
	originalCreateSession := copilotCLICreateSession
	originalDisconnectSession := copilotCLIDisconnectSession
	originalStreamSession := copilotCLIStreamSession
	t.Cleanup(func() {
		copilotCLIClientForParams = originalClientForParams
		copilotCLICreateSession = originalCreateSession
		copilotCLIDisconnectSession = originalDisconnectSession
		copilotCLIStreamSession = originalStreamSession
	})
	copilotCLIClientForParams = func(_ *CopilotCLIBackend, _ context.Context, gotParams map[string]any) (*sdk.Client, func(), error) {
		if gotParams["tool_choice"] != params["tool_choice"] {
			t.Fatalf("clientForParams tool_choice = %#v, want %#v", gotParams["tool_choice"], params["tool_choice"])
		}
		return nil, func() { cleanupDone <- struct{}{} }, nil
	}
	copilotCLICreateSession = func(_ *sdk.Client, _ context.Context, cfg *sdk.SessionConfig) (*sdk.Session, error) {
		if cfg == nil {
			t.Fatal("CreateSession config is nil")
		}
		if cfg.Streaming == nil || !*cfg.Streaming {
			t.Fatalf("Streaming = %v, want true", cfg.Streaming)
		}
		return nil, nil
	}
	copilotCLIDisconnectSession = func(_ *sdk.Session) {
		disconnectDone <- struct{}{}
	}
	copilotCLIStreamSession = func(_ context.Context, _ *sdk.Session, prompt, endpoint string, out chan<- StreamItem) {
		streamCalled = true
		if prompt != wantPrompt {
			t.Fatalf("prompt = %q, want %q", prompt, wantPrompt)
		}
		if endpoint != wantEndpoint {
			t.Fatalf("endpoint = %q, want %q", endpoint, wantEndpoint)
		}
		out <- StreamItem{Kind: "delta", Text: "alpha "}
		out <- StreamItem{Kind: "delta", Text: "beta "}
		out <- StreamItem{Kind: "done", FinishReason: "stop"}
	}

	// When
	stream, err := backend.ChatStream(context.Background(), "gpt-4.1", messages, params)
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	items := make([]StreamItem, 0, 3)
	for item := range stream {
		items = append(items, item)
	}

	// Then
	if !streamCalled {
		t.Fatal("expected streamSDKSession to be called")
	}
	select {
	case <-cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("expected client cleanup to be called")
	}
	select {
	case <-disconnectDone:
	case <-time.After(time.Second):
		t.Fatal("expected session disconnect to be called")
	}
	if len(items) != 3 {
		t.Fatalf("item count = %d, want 3; items=%+v", len(items), items)
	}
	if items[0].Kind != "delta" || items[1].Kind != "delta" || items[2].Kind != "done" {
		t.Fatalf("unexpected stream items: %+v", items)
	}
}
