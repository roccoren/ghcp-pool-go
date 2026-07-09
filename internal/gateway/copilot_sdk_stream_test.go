package gateway

import (
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
