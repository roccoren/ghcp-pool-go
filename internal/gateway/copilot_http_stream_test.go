package gateway

import (
	"context"
	"os"
	"testing"
	"time"

	sdk "github.com/github/copilot-sdk/go"
)

func TestSDKBackendChatStreamUsesStreamingSessionAndConstraints(t *testing.T) {
	// Given
	backend := NewCopilotBackendWithOptions("acct", "gh-token", "", CopilotBackendOptions{})
	messages := []NeutralMessage{{Role: "user", Content: "stream me"}}
	params := map[string]any{
		"response_format": map[string]any{"type": "json_object"},
		"stop":            "STOP",
		"tool_choice":     "required",
		"tools":           []map[string]any{{"name": "calculator"}},
	}
	wantPrompt := backend.sdkPrompt(messages, params)
	wantEndpoint := sdkUsageEndpoint(params)
	cleanupDone := make(chan struct{}, 1)
	disconnectDone := make(chan struct{}, 1)
	streamCalled := false

	originalClientForParams := copilotSDKClientForParams
	originalCreateSession := copilotSDKCreateSession
	originalDisconnectSession := copilotSDKDisconnectSession
	originalStreamSession := copilotSDKStreamSession
	t.Cleanup(func() {
		copilotSDKClientForParams = originalClientForParams
		copilotSDKCreateSession = originalCreateSession
		copilotSDKDisconnectSession = originalDisconnectSession
		copilotSDKStreamSession = originalStreamSession
	})

	copilotSDKClientForParams = func(_ *CopilotBackend, _ context.Context, gotParams map[string]any) (*sdk.Client, func(), error) {
		if gotParams["tool_choice"] != params["tool_choice"] {
			t.Fatalf("clientForParams tool_choice = %#v, want %#v", gotParams["tool_choice"], params["tool_choice"])
		}
		return nil, func() { cleanupDone <- struct{}{} }, nil
	}
	copilotSDKCreateSession = func(_ *sdk.Client, _ context.Context, cfg *sdk.SessionConfig) (*sdk.Session, error) {
		if cfg == nil {
			t.Fatal("CreateSession config is nil")
		}
		if cfg.Streaming == nil || !*cfg.Streaming {
			t.Fatalf("Streaming = %v, want true", cfg.Streaming)
		}
		return nil, nil
	}
	copilotSDKDisconnectSession = func(_ *sdk.Session) {
		disconnectDone <- struct{}{}
	}
	copilotSDKStreamSession = func(_ context.Context, _ *sdk.Session, prompt, endpoint string, out chan<- StreamItem, _ ...sdk.Attachment) {
		streamCalled = true
		if prompt != wantPrompt {
			t.Fatalf("prompt = %q, want %q", prompt, wantPrompt)
		}
		if endpoint != wantEndpoint {
			t.Fatalf("endpoint = %q, want %q", endpoint, wantEndpoint)
		}
		out <- StreamItem{Kind: "delta", Text: "alpha "}
		out <- StreamItem{Kind: "delta", Text: "beta STOP tail"}
		out <- StreamItem{Kind: "done", FinishReason: "stop"}
	}

	// When
	stream, err := backend.chatStreamSDK(context.Background(), "gpt-4.1", messages, params)
	if err != nil {
		t.Fatalf("chatStreamSDK() error = %v", err)
	}
	items := collectStreamItems(stream)

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
	if got := items[0]; got.Kind != "delta" || got.Text != "alpha " {
		t.Fatalf("first item = %+v", got)
	}
	if got := items[1]; got.Kind != "delta" || got.Text != "beta " {
		t.Fatalf("second item = %+v", got)
	}
	if got := items[2]; got.Kind != "done" || got.FinishReason != "stop" {
		t.Fatalf("done item = %+v", got)
	}
	if got := items[2].Usage.OutputTokens; got != approxTokens("alpha beta ") {
		t.Fatalf("output tokens = %d, want %d", got, approxTokens("alpha beta "))
	}
}

func TestSDKBackendChatStreamEmitsIncrementalDeltas(t *testing.T) {
	t.Parallel()

	// Given
	if os.Getenv("GHCP_BACKEND") != "copilot" {
		t.Skip("requires GHCP_BACKEND=copilot")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	backend := NewCopilotBackendWithOptions("stream-test", os.Getenv("GHCP_COPILOT_TOKEN"), "", CopilotBackendOptions{Mode: copilotBackendModeSDK})
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
