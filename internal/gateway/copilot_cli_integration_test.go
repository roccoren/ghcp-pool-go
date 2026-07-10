package gateway

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestCLIBackendChatStreamEmitsIncrementalDeltas(t *testing.T) {
	t.Parallel()

	// Given
	if os.Getenv("GHCP_BACKEND") != "copilot" {
		t.Skip("requires GHCP_BACKEND=copilot")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	backend := NewCopilotCLIBackend("stream-test", os.Getenv("GHCP_COPILOT_TOKEN"), "")
	defer func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()
	stream, err := backend.ChatStream(ctx, "gpt-4.1", []NeutralMessage{{Role: "user", Content: "Count from one to six with each number as a separate word."}}, nil)
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}

	// When
	items := make([]StreamItem, 0, 8)
	for item := range stream {
		items = append(items, item)
		if item.Kind == "done" || item.Kind == "error" {
			break
		}
	}

	// Then
	deltaCount := 0
	terminalCount := 0
	sawDone := false
	for _, item := range items {
		switch item.Kind {
		case "delta":
			deltaCount++
		case "done":
			terminalCount++
			sawDone = true
		case "error":
			terminalCount++
			t.Fatalf("stream returned error item: %v", item.Err)
		}
	}
	if deltaCount < 2 {
		t.Fatalf("delta count = %d, want at least 2; items=%+v", deltaCount, items)
	}
	if !sawDone {
		t.Fatalf("expected terminal done item; items=%+v", items)
	}
	if terminalCount != 1 {
		t.Fatalf("terminal item count = %d, want 1; items=%+v", terminalCount, items)
	}
	if last := items[len(items)-1]; last.Kind != "done" {
		t.Fatalf("last item = %+v, want done", last)
	}
}

// TestCopilotCLIBackend_GatewayIntegration verifies the CLI backend integrates with the Gateway
func TestCopilotCLIBackend_GatewayIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create a CLI backend
	backend := NewCopilotCLIBackendWithOptions("test-cli", "gho_testtoken", "", CopilotCLIBackendOptions{
		WebSearchMode: "cli",
		CustomTools:   []string{"view", "edit"},
	})
	defer backend.Close()

	// Verify it implements Backend interface
	var _ Backend = backend

	// Verify backend configuration
	if backend.accountID != "test-cli" {
		t.Errorf("accountID = %q, want %q", backend.accountID, "test-cli")
	}
	if backend.webSearchMode != "cli" {
		t.Errorf("webSearchMode = %q, want %q", backend.webSearchMode, "cli")
	}
	if len(backend.customTools) != 2 {
		t.Errorf("len(customTools) = %d, want 2", len(backend.customTools))
	}

	// Verify embeddings are unsupported
	_, err := backend.Embeddings(ctx, "test", []string{"test"}, nil)
	if err != ErrEmbeddingsUnsupported {
		t.Errorf("Embeddings error = %v, want %v", err, ErrEmbeddingsUnsupported)
	}
}

// TestCopilotCLIBackend_PoolIntegration verifies CLI backend works with PoolManager
func TestCopilotCLIBackend_PoolIntegration(t *testing.T) {
	settings := Settings{
		Backend: "copilot-cli",
		Copilot: CopilotConfig{
			SDKWebSearchMode: "cli",
			SDKTools:         []string{"view"},
		},
		Accounts: []AccountConfig{
			{
				ID:                      "cli-test-1",
				Label:                   "CLI Test 1",
				Token:                   "gho_test1",
				Enabled:                 boolPtr(true),
				MaxConcurrency:          5,
				CopilotSDKWebSearchMode: "cli",
			},
			{
				ID:                      "cli-test-2",
				Label:                   "CLI Test 2",
				Token:                   "gho_test2",
				Enabled:                 boolPtr(true),
				MaxConcurrency:          10,
				CopilotSDKWebSearchMode: "native_cli",
			},
		},
	}

	pool, err := NewPoolManager(settings)
	if err != nil {
		t.Fatalf("NewPoolManager error: %v", err)
	}
	defer pool.Close()

	snapshot := pool.Snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("pool accounts = %d, want 2", len(snapshot))
	}

	// Verify first account
	acc1 := snapshot[0]
	if acc1.ID() != "cli-test-1" {
		t.Errorf("account[0].ID = %q, want %q", acc1.ID(), "cli-test-1")
	}
	if acc1.Config.MaxConcurrency != 5 {
		t.Errorf("account[0].Config.MaxConcurrency = %d, want 5", acc1.Config.MaxConcurrency)
	}

	// Verify second account
	acc2 := snapshot[1]
	if acc2.ID() != "cli-test-2" {
		t.Errorf("account[1].ID = %q, want %q", acc2.ID(), "cli-test-2")
	}
	if acc2.Config.MaxConcurrency != 10 {
		t.Errorf("account[1].Config.MaxConcurrency = %d, want 10", acc2.Config.MaxConcurrency)
	}

	// Verify both backends are CopilotCLIBackend type
	for i, acc := range snapshot {
		backend, ok := acc.Backend.(*CopilotCLIBackend)
		if !ok {
			t.Errorf("account[%d] backend is not CopilotCLIBackend, got %T", i, acc.Backend)
			continue
		}
		if backend.accountID != acc.ID() {
			t.Errorf("account[%d] backend accountID = %q, want %q", i, backend.accountID, acc.ID())
		}
	}
}

// TestCopilotCLIBackend_ConfigurationOptions verifies various CLI backend configuration options
func TestCopilotCLIBackend_ConfigurationOptions(t *testing.T) {
	tests := []struct {
		name          string
		config        AccountConfig
		copilotConfig CopilotConfig
		wantWebSearch string
		wantTools     int
	}{
		{
			name: "cli mode from defaults",
			config: AccountConfig{
				ID:    "test1",
				Token: "gho_test",
			},
			copilotConfig: CopilotConfig{
				SDKWebSearchMode: "cli",
				SDKTools:         []string{"view", "edit"},
			},
			wantWebSearch: "cli",
			wantTools:     2,
		},
		{
			name: "native_cli mode from account override",
			config: AccountConfig{
				ID:                      "test2",
				Token:                   "gho_test",
				CopilotSDKWebSearchMode: "native_cli",
			},
			copilotConfig: CopilotConfig{
				SDKWebSearchMode: "cli",
				SDKTools:         []string{"view"},
			},
			wantWebSearch: "native_cli",
			wantTools:     1,
		},
		{
			name: "custom tools from account",
			config: AccountConfig{
				ID:              "test3",
				Token:           "gho_test",
				CopilotSDKTools: []string{"custom1", "custom2", "custom3"},
			},
			copilotConfig: CopilotConfig{
				SDKWebSearchMode: "off",
				SDKTools:         []string{"view"},
			},
			wantWebSearch: "off",
			wantTools:     3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := copilotCLIBackendOptions(tt.copilotConfig, &tt.config)
			if options.WebSearchMode != tt.wantWebSearch {
				t.Errorf("WebSearchMode = %q, want %q", options.WebSearchMode, tt.wantWebSearch)
			}
			if len(options.CustomTools) != tt.wantTools {
				t.Errorf("len(CustomTools) = %d, want %d", len(options.CustomTools), tt.wantTools)
			}
		})
	}
}

// TestBuildBackend_CLIBackendCreation verifies buildBackend creates CLI backend correctly
func TestBuildBackend_CLIBackendCreation(t *testing.T) {
	ctx := context.Background()

	cfg := &AccountConfig{
		ID:                      "cli-acc",
		Token:                   "gho_clitest",
		CopilotSDKWebSearchMode: "cli",
		CopilotSDKTools:         []string{"view"},
	}

	settings := Settings{
		Backend: "copilot-cli",
		Copilot: CopilotConfig{
			SDKWebSearchMode: "off",
			SDKTools:         []string{"edit"},
		},
	}

	backend, err := buildBackend(ctx, cfg, settings, "")
	if err != nil {
		t.Fatalf("buildBackend error: %v", err)
	}
	defer backend.Close()

	cliBE, ok := backend.(*CopilotCLIBackend)
	if !ok {
		t.Fatalf("backend is not CopilotCLIBackend, got %T", backend)
	}

	if cliBE.accountID != "cli-acc" {
		t.Errorf("accountID = %q, want %q", cliBE.accountID, "cli-acc")
	}
	if cliBE.webSearchMode != "cli" {
		t.Errorf("webSearchMode = %q, want %q", cliBE.webSearchMode, "cli")
	}
	if len(cliBE.customTools) != 1 || cliBE.customTools[0] != "view" {
		t.Errorf("customTools = %v, want [view]", cliBE.customTools)
	}
}

func boolPtr(b bool) *bool {
	return &b
}
