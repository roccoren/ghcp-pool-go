package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type sseFrame struct {
	event   string
	data    string
	payload map[string]any
}

func TestCharacterizationChatCompletions(t *testing.T) {
	_, h := testServer(t)
	rr := request(t, h, http.MethodPost, "/v1/chat/completions", chatBody("gpt-4.1", "characterize chat output"), userHeaders)
	if rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}

	body := decodeBody(t, rr)
	assertStringValue(t, body, "object", "chat.completion")
	assertNonEmptyStringField(t, body, "id")
	assertStringValue(t, body, "model", "gpt-4.1")
	choices := mustAnySlice(t, body["choices"], "choices")
	if len(choices) != 1 {
		t.Fatalf("choices=%v", choices)
	}
	choice := mustAnyMap(t, choices[0], "choices[0]")
	assertStringValue(t, choice, "finish_reason", "stop")
	message := mustAnyMap(t, choice["message"], "choices[0].message")
	assertStringValue(t, message, "role", "assistant")
	assertNonEmptyStringField(t, message, "content")
	assertKeysPresent(t, mustAnyMap(t, body["usage"], "usage"), "usage", "prompt_tokens", "completion_tokens", "total_tokens")
}

func TestCharacterizationResponses(t *testing.T) {
	_, h := testServer(t)
	rr := request(t, h, http.MethodPost, "/v1/responses", map[string]any{
		"model":             "gpt-4.1",
		"input":             "characterize responses output",
		"max_output_tokens": 64,
	}, userHeaders)
	if rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}

	body := decodeBody(t, rr)
	assertStringValue(t, body, "object", "response")
	assertStringValue(t, body, "status", "completed")
	assertStringValue(t, body, "stop_reason", "stop")
	if body["end_turn"] != true {
		t.Fatalf("end_turn=%v", body["end_turn"])
	}
	assertNonEmptyStringField(t, body, "output_text")
	output := mustAnySlice(t, body["output"], "output")
	if len(output) == 0 {
		t.Fatalf("output=%v", output)
	}
	item := mustAnyMap(t, output[0], "output[0]")
	assertStringValue(t, item, "type", "message")
	assertStringValue(t, item, "status", "completed")
	assertStringValue(t, item, "role", "assistant")
	content := mustAnySlice(t, item["content"], "output[0].content")
	if len(content) != 1 {
		t.Fatalf("content=%v", content)
	}
	part := mustAnyMap(t, content[0], "output[0].content[0]")
	assertStringValue(t, part, "type", "output_text")
	assertNonEmptyStringField(t, part, "text")
	assertKeysPresent(t, mustAnyMap(t, body["usage"], "usage"), "usage", "input_tokens", "output_tokens", "total_tokens")
	if _, ok := part["annotations"].([]any); !ok {
		t.Fatalf("annotations=%T %v", part["annotations"], part["annotations"])
	}
}

func TestCharacterizationAnthropicMessages(t *testing.T) {
	_, h := testServer(t)
	rr := request(t, h, http.MethodPost, "/v1/messages", map[string]any{
		"model":      "claude-3.5",
		"max_tokens": 64,
		"messages":   []map[string]any{{"role": "user", "content": "characterize anthropic output"}},
	}, userHeaders)
	if rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}

	body := decodeBody(t, rr)
	assertStringValue(t, body, "type", "message")
	assertStringValue(t, body, "role", "assistant")
	assertStringValue(t, body, "stop_reason", "end_turn")
	content := mustAnySlice(t, body["content"], "content")
	if len(content) != 1 {
		t.Fatalf("content=%v", content)
	}
	block := mustAnyMap(t, content[0], "content[0]")
	assertStringValue(t, block, "type", "text")
	assertNonEmptyStringField(t, block, "text")
	assertKeysPresent(t, mustAnyMap(t, body["usage"], "usage"), "usage", "input_tokens", "output_tokens", "output_tokens_details")
}

func TestCharacterizationChatStreamSSE(t *testing.T) {
	_, h := testServer(t)
	body := chatBody("gpt-4.1", "characterize chat stream output")
	body["stream"] = true
	body["stream_options"] = map[string]any{"include_usage": true}
	rr := request(t, h, http.MethodPost, "/v1/chat/completions", body, userHeaders)
	if rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content-type=%q", rr.Header().Get("Content-Type"))
	}

	frames := collectSSEFrames(t, rr.Body.String())
	if len(frames) < 5 {
		t.Fatalf("frames=%v", frames)
	}
	assertUnnamedSSEFrames(t, frames)
	if frames[len(frames)-1].data != "[DONE]" {
		t.Fatalf("terminal frame=%+v", frames[len(frames)-1])
	}
	first := frames[0].payload
	assertStringValue(t, first, "object", "chat.completion.chunk")
	firstChoice := firstChoiceMap(t, first)
	firstDelta := mustAnyMap(t, firstChoice["delta"], "first delta")
	assertStringValue(t, firstDelta, "role", "assistant")

	contentFrames := frames[1 : len(frames)-3]
	if len(contentFrames) == 0 {
		t.Fatalf("missing content frames: %v", frames)
	}
	for _, frame := range contentFrames {
		choice := firstChoiceMap(t, frame.payload)
		delta := mustAnyMap(t, choice["delta"], "content delta")
		assertNonEmptyStringField(t, delta, "content")
	}

	finishChoice := firstChoiceMap(t, frames[len(frames)-3].payload)
	assertStringValue(t, finishChoice, "finish_reason", "stop")
	assertKeysPresent(t, mustAnyMap(t, frames[len(frames)-2].payload["usage"], "usage"), "usage", "prompt_tokens", "completion_tokens", "total_tokens")
}

func TestCharacterizationAnthropicStreamSSE(t *testing.T) {
	_, h := testServer(t)
	rr := request(t, h, http.MethodPost, "/v1/messages", map[string]any{
		"model":      "claude-3.5",
		"max_tokens": 64,
		"stream":     true,
		"messages":   []map[string]any{{"role": "user", "content": "characterize anthropic stream output"}},
	}, userHeaders)
	if rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content-type=%q", rr.Header().Get("Content-Type"))
	}

	frames := collectSSEFrames(t, rr.Body.String())
	required := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	indexes := map[string]int{}
	for _, name := range required {
		indexes[name] = firstFrameIndex(t, frames, name)
	}
	if !(indexes["message_start"] < indexes["content_block_delta"] && indexes["content_block_delta"] < indexes["message_stop"]) {
		t.Fatalf("event order=%v", indexes)
	}

	start := mustAnyMap(t, frames[indexes["message_start"]].payload["message"], "message_start.message")
	assertStringValue(t, start, "type", "message")
	assertStringValue(t, start, "role", "assistant")
	delta := mustAnyMap(t, frames[indexes["content_block_delta"]].payload["delta"], "content_block_delta.delta")
	assertStringValue(t, delta, "type", "text_delta")
	assertNonEmptyStringField(t, delta, "text")
	messageDelta := mustAnyMap(t, frames[indexes["message_delta"]].payload["delta"], "message_delta.delta")
	assertStringValue(t, messageDelta, "stop_reason", "end_turn")
	assertKeysPresent(t, mustAnyMap(t, frames[indexes["message_delta"]].payload["usage"], "message_delta.usage"), "usage", "input_tokens", "output_tokens")
	if frames[indexes["message_stop"]].payload["type"] != "message_stop" {
		t.Fatalf("message_stop=%v", frames[indexes["message_stop"]].payload)
	}
}

func collectSSEFrames(t *testing.T, body string) []sseFrame {
	t.Helper()
	frames := []sseFrame{}
	err := scanSSE(strings.NewReader(body), func(event, data string) bool {
		frame := sseFrame{event: event, data: data}
		if data != "[DONE]" {
			if err := json.Unmarshal([]byte(data), &frame.payload); err != nil {
				t.Fatalf("decode SSE payload %q: %v", data, err)
			}
		}
		frames = append(frames, frame)
		return true
	})
	if err != nil {
		t.Fatalf("scan SSE: %v", err)
	}
	return frames
}

func firstChoiceMap(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	choices := mustAnySlice(t, payload["choices"], "choices")
	if len(choices) != 1 {
		t.Fatalf("choices=%v", choices)
	}
	return mustAnyMap(t, choices[0], "choices[0]")
}

func firstFrameIndex(t *testing.T, frames []sseFrame, want string) int {
	t.Helper()
	for i, frame := range frames {
		if frame.event == want {
			return i
		}
	}
	t.Fatalf("event %q not found in %+v", want, frames)
	return -1
}

func assertKeysPresent(t *testing.T, obj map[string]any, label string, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := obj[key]; !ok {
			t.Fatalf("%s missing key %q: %v", label, key, obj)
		}
	}
}

func assertUnnamedSSEFrames(t *testing.T, frames []sseFrame) {
	t.Helper()
	for i, frame := range frames {
		if frame.event != "" {
			t.Fatalf("frame[%d] event: want empty string, got %q (data=%q)", i, frame.event, frame.data)
		}
	}
}

func assertNonEmptyStringField(t *testing.T, obj map[string]any, key string) string {
	t.Helper()
	value, ok := obj[key].(string)
	if !ok || value == "" {
		t.Fatalf("%s: want non-empty string, got %T %v", key, obj[key], obj[key])
	}
	return value
}

func assertStringValue(t *testing.T, obj map[string]any, key, want string) {
	t.Helper()
	got, ok := obj[key].(string)
	if !ok || got != want {
		t.Fatalf("%s: want string %q, got %T %v", key, want, obj[key], obj[key])
	}
}

func mustAnyMap(t *testing.T, value any, label string) map[string]any {
	t.Helper()
	obj, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s type=%T value=%v", label, value, value)
	}
	return obj
}

func mustAnySlice(t *testing.T, value any, label string) []any {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("%s type=%T value=%v", label, value, value)
	}
	return items
}
