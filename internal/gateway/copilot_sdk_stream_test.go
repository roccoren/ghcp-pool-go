package gateway

import (
	"context"
	"testing"

	sdk "github.com/github/copilot-sdk/go"
)

func TestReduceSDKStreamEventsEmitsDeltasThenDone(t *testing.T) {
	t.Parallel()

	// Given
	out := make(chan StreamItem, 8)
	model := "claude-sonnet-4.6"
	events := []sdk.SessionEvent{
		{Data: &sdk.AssistantMessageDeltaData{DeltaContent: "Hel"}},
		{Data: &sdk.AssistantMessageDeltaData{DeltaContent: "lo"}},
		{Data: &sdk.AssistantMessageData{Content: "Hello", Model: &model}},
		{Data: &sdk.AssistantUsageData{Model: model}},
		{Data: &sdk.SessionIdleData{}},
	}

	// When
	reduceSDKStreamEvents(events, out)
	close(out)

	// Then
	items := make([]StreamItem, 0, len(events))
	for item := range out {
		items = append(items, item)
	}
	if len(items) != 3 {
		t.Fatalf("item count=%d items=%+v", len(items), items)
	}
	if got := items[0]; got.Kind != "delta" || got.Text != "Hel" {
		t.Fatalf("first item=%+v", got)
	}
	if got := items[1]; got.Kind != "delta" || got.Text != "lo" {
		t.Fatalf("second item=%+v", got)
	}
	if got := items[2]; got.Kind != "done" {
		t.Fatalf("last item=%+v", got)
	}
	if got := items[0].Text + items[1].Text; got != "Hello" {
		t.Fatalf("delta text=%q", got)
	}
}

func TestReduceSDKStreamEventsStopsAfterSessionError(t *testing.T) {
	t.Parallel()

	// Given
	out := make(chan StreamItem, 8)
	events := []sdk.SessionEvent{
		{Data: &sdk.AssistantMessageDeltaData{DeltaContent: "a"}},
		{Data: &sdk.SessionErrorData{Message: "boom", ErrorType: "query"}},
		{Data: &sdk.AssistantMessageDeltaData{DeltaContent: "b"}},
		{Data: &sdk.SessionIdleData{}},
	}

	// When
	reduceSDKStreamEvents(events, out)
	close(out)

	// Then
	items := make([]StreamItem, 0, len(events))
	for item := range out {
		items = append(items, item)
	}
	if len(items) != 2 {
		t.Fatalf("item count=%d items=%+v", len(items), items)
	}
	if got := items[0]; got.Kind != "delta" || got.Text != "a" {
		t.Fatalf("first item=%+v", got)
	}
	if got := items[1]; got.Kind != "error" || got.Err == nil || got.Err.Error() != "sdk session error: boom" {
		t.Fatalf("second item=%+v", got)
	}
}

func TestApplyStreamOutputConstraints(t *testing.T) {
	t.Parallel()

	t.Run("stop", func(t *testing.T) {
		t.Parallel()

		// Given
		ctx := context.Background()
		in := make(chan StreamItem, 4)
		in <- StreamItem{Kind: "delta", Text: "Hello "}
		in <- StreamItem{Kind: "delta", Text: "world STOP tail"}
		in <- StreamItem{Kind: "done", FinishReason: "stop"}
		close(in)

		// When
		out := applyStreamOutputConstraints(ctx, in, map[string]any{"stop": "STOP"})
		items := collectStreamItems(out)

		// Then
		if len(items) != 3 {
			t.Fatalf("item count=%d items=%+v", len(items), items)
		}
		if got := items[0]; got.Kind != "delta" || got.Text != "Hello " {
			t.Fatalf("first item=%+v", got)
		}
		if got := items[1]; got.Kind != "delta" || got.Text != "world " {
			t.Fatalf("second item=%+v", got)
		}
		if got := items[2]; got.Kind != "done" || got.FinishReason != "stop" {
			t.Fatalf("done item=%+v", got)
		}
		if got := items[2].Usage.OutputTokens; got != approxTokens("Hello world ") {
			t.Fatalf("output tokens=%d want=%d", got, approxTokens("Hello world "))
		}
	})

	t.Run("max_tokens", func(t *testing.T) {
		t.Parallel()

		// Given
		ctx := context.Background()
		in := make(chan StreamItem, 5)
		in <- StreamItem{Kind: "delta", Text: "alpha "}
		in <- StreamItem{Kind: "delta", Text: "beta gamma "}
		in <- StreamItem{Kind: "delta", Text: "delta epsilon"}
		in <- StreamItem{Kind: "done", FinishReason: "stop"}
		close(in)

		// When
		out := applyStreamOutputConstraints(ctx, in, map[string]any{"max_tokens": 3})
		items := collectStreamItems(out)

		// Then
		if len(items) != 3 {
			t.Fatalf("item count=%d items=%+v", len(items), items)
		}
		if got := items[0].Text + items[1].Text; got != "alpha beta gamma " {
			t.Fatalf("content=%q", got)
		}
		if got := items[2]; got.Kind != "done" || got.FinishReason != "length" {
			t.Fatalf("done item=%+v", got)
		}
		if got := items[2].Usage.OutputTokens; got != approxTokens("alpha beta gamma") {
			t.Fatalf("output tokens=%d want=%d", got, approxTokens("alpha beta gamma"))
		}
	})

	t.Run("passthrough", func(t *testing.T) {
		t.Parallel()

		// Given
		ctx := context.Background()
		in := make(chan StreamItem, 4)
		in <- StreamItem{Kind: "delta", Text: "alpha "}
		in <- StreamItem{Kind: "delta", Text: "beta"}
		in <- StreamItem{Kind: "done", FinishReason: "stop", Usage: Usage{InputTokens: 2, OutputTokens: 99}.Normalized()}
		close(in)

		// When
		out := applyStreamOutputConstraints(ctx, in, nil)
		items := collectStreamItems(out)

		// Then
		if len(items) != 3 {
			t.Fatalf("item count=%d items=%+v", len(items), items)
		}
		if got := items[0]; got.Kind != "delta" || got.Text != "alpha " {
			t.Fatalf("first item=%+v", got)
		}
		if got := items[1]; got.Kind != "delta" || got.Text != "beta" {
			t.Fatalf("second item=%+v", got)
		}
		if got := items[2]; got.Kind != "done" || got.FinishReason != "stop" {
			t.Fatalf("done item=%+v", got)
		}
		if got := items[2].Usage.OutputTokens; got != approxTokens("alpha beta") {
			t.Fatalf("output tokens=%d want=%d", got, approxTokens("alpha beta"))
		}
	})

	t.Run("tool_call passthrough", func(t *testing.T) {
		t.Parallel()

		// Given
		ctx := context.Background()
		tool := ToolCall{ID: "call_1", Name: "search", Arguments: `{"q":"hi"}`, Kind: "function"}
		in := make(chan StreamItem, 4)
		in <- StreamItem{Kind: "tool_call", ToolCall: tool, Index: 0}
		in <- StreamItem{Kind: "delta", Text: "should remain"}
		in <- StreamItem{Kind: "done", FinishReason: "tool_calls", Usage: Usage{OutputTokens: 7}.Normalized()}
		close(in)

		// When
		out := applyStreamOutputConstraints(ctx, in, map[string]any{"stop": "remain", "max_tokens": 1})
		items := collectStreamItems(out)

		// Then
		if len(items) != 3 {
			t.Fatalf("item count=%d items=%+v", len(items), items)
		}
		if got := items[0]; got.Kind != "tool_call" || got.ToolCall != tool || got.Index != 0 {
			t.Fatalf("tool item=%+v", got)
		}
		if got := items[1]; got.Kind != "delta" || got.Text != "should remain" {
			t.Fatalf("delta item=%+v", got)
		}
		if got := items[2]; got.Kind != "done" || got.FinishReason != "tool_calls" || got.Usage.OutputTokens != 7 {
			t.Fatalf("done item=%+v", got)
		}
	})
}

func collectStreamItems(in <-chan StreamItem) []StreamItem {
	items := make([]StreamItem, 0, 8)
	for item := range in {
		items = append(items, item)
	}
	return items
}
