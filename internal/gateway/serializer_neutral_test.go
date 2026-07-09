package gateway

import (
	"net/http"
	"testing"
)

func TestAnthropicResponseFromNeutralChatResult(t *testing.T) {
	t.Run("renders text block and end_turn stop reason when finish is stop", func(t *testing.T) {
		// Given
		result := ChatResult{
			ID:           "msg_text",
			Model:        "claude-3.5",
			Content:      "hi",
			FinishReason: "stop",
		}

		// When
		response := anthropicResponse(result, nil)

		// Then
		assertStringValue(t, response, "type", "message")
		assertStringValue(t, response, "role", "assistant")
		assertStringValue(t, response, "stop_reason", "end_turn")
		content := mustAnySlice(t, response["content"], "content")
		if len(content) != 1 {
			t.Fatalf("content=%v", content)
		}
		block := mustAnyMap(t, content[0], "content[0]")
		assertStringValue(t, block, "type", "text")
		assertStringValue(t, block, "text", "hi")
	})

	t.Run("renders tool_use block from neutral tool calls", func(t *testing.T) {
		// Given
		result := ChatResult{
			ID:           "msg_tool",
			Model:        "claude-3.5",
			FinishReason: "tool_calls",
			ToolCalls: []ToolCall{{
				ID:        "t1",
				Name:      "get_weather",
				Arguments: `{"city":"Tokyo"}`,
			}},
		}

		// When
		response := anthropicResponse(result, nil)

		// Then
		assertStringValue(t, response, "stop_reason", "tool_use")
		content := mustAnySlice(t, response["content"], "content")
		if len(content) != 1 {
			t.Fatalf("content=%v", content)
		}
		block := mustAnyMap(t, content[0], "content[0]")
		assertStringValue(t, block, "type", "tool_use")
		assertStringValue(t, block, "id", "t1")
		assertStringValue(t, block, "name", "get_weather")
		input := mustAnyMap(t, block["input"], "content[0].input")
		assertStringValue(t, input, "city", "Tokyo")
	})

	t.Run("maps length finish reason to max_tokens", func(t *testing.T) {
		// Given
		result := ChatResult{Content: "trimmed", FinishReason: "length"}

		// When
		response := anthropicResponse(result, nil)

		// Then
		assertStringValue(t, response, "stop_reason", "max_tokens")
	})

	t.Run("maps content_filter finish reason to refusal", func(t *testing.T) {
		// Given
		result := ChatResult{Content: "filtered", FinishReason: "content_filter"}

		// When
		response := anthropicResponse(result, nil)

		// Then
		assertStringValue(t, response, "stop_reason", "refusal")
	})
}

func TestResponsesOutputFromNeutralChatResult(t *testing.T) {
	t.Run("renders message output item from neutral content", func(t *testing.T) {
		// Given
		result := ChatResult{
			ID:           "resp_text",
			Model:        "gpt-4.1",
			Content:      "hi",
			FinishReason: "stop",
			Usage:        Usage{InputTokens: 2, OutputTokens: 1}.Normalized(),
		}

		// When
		response := responseResponse(result, nil)

		// Then
		assertStringValue(t, response, "object", "response")
		assertStringValue(t, response, "status", "completed")
		assertStringValue(t, response, "stop_reason", "stop")
		assertStringValue(t, response, "output_text", "hi")
		if response["end_turn"] != true {
			t.Fatalf("end_turn=%v", response["end_turn"])
		}
		output := mustAnySlice(t, response["output"], "output")
		if len(output) != 1 {
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
		assertStringValue(t, part, "text", "hi")
	})

	t.Run("renders function call items from neutral tool calls", func(t *testing.T) {
		// Given
		result := ChatResult{
			ID:           "resp_tool",
			Model:        "gpt-4.1",
			FinishReason: "tool_calls",
			Usage:        Usage{InputTokens: 2, OutputTokens: 1}.Normalized(),
			ToolCalls: []ToolCall{{
				ID:        "call_1",
				Name:      "get_weather",
				Arguments: `{"city":"Tokyo"}`,
			}},
		}

		// When
		response := responseResponse(result, nil)

		// Then
		assertStringValue(t, response, "stop_reason", "tool_calls")
		if response["end_turn"] != false {
			t.Fatalf("end_turn=%v", response["end_turn"])
		}
		output := mustAnySlice(t, response["output"], "output")
		if len(output) != 1 {
			t.Fatalf("output=%v", output)
		}
		item := mustAnyMap(t, output[0], "output[0]")
		assertStringValue(t, item, "type", "function_call")
		assertStringValue(t, item, "status", "completed")
		assertStringValue(t, item, "call_id", "call_1")
		assertStringValue(t, item, "name", "get_weather")
		assertStringValue(t, item, "arguments", `{"city":"Tokyo"}`)
	})
}

func TestStreamAnthropicFromNeutralItems(t *testing.T) {
	// Given
	_, h := testServer(t)
	rr := request(t, h, http.MethodPost, "/v1/messages", map[string]any{
		"model":      "claude-3.5",
		"max_tokens": 64,
		"stream":     true,
		"messages":   []map[string]any{{"role": "user", "content": "neutral anthropic stream"}},
	}, userHeaders)
	if rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}

	// When
	frames := collectSSEFrames(t, rr.Body.String())

	// Then
	required := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	indexes := frameIndexesByEvent(t, frames, required)
	assertIncreasingEventOrder(t, indexes, required)
	start := mustAnyMap(t, frames[indexes["message_start"]].payload["message"], "message_start.message")
	assertStringValue(t, start, "type", "message")
	delta := mustAnyMap(t, frames[indexes["content_block_delta"]].payload["delta"], "content_block_delta.delta")
	assertStringValue(t, delta, "type", "text_delta")
	messageDelta := mustAnyMap(t, frames[indexes["message_delta"]].payload["delta"], "message_delta.delta")
	assertStringValue(t, messageDelta, "stop_reason", "end_turn")
}

func TestStreamResponsesFromNeutralItems(t *testing.T) {
	// Given
	_, h := testServer(t)
	rr := request(t, h, http.MethodPost, "/v1/responses", map[string]any{
		"model":             "gpt-4.1",
		"input":             "neutral responses stream",
		"max_output_tokens": 64,
		"stream":            true,
	}, userHeaders)
	if rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}

	// When
	frames := collectSSEFrames(t, rr.Body.String())

	// Then
	required := []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed"}
	indexes := frameIndexesByEvent(t, frames, required)
	assertIncreasingEventOrder(t, indexes, required)
	created := mustAnyMap(t, frames[indexes["response.created"]].payload["response"], "response.created.response")
	assertStringValue(t, created, "status", "in_progress")
	outputItem := mustAnyMap(t, frames[indexes["response.output_item.done"]].payload["item"], "response.output_item.done.item")
	assertStringValue(t, outputItem, "type", "message")
	completed := mustAnyMap(t, frames[indexes["response.completed"]].payload["response"], "response.completed.response")
	assertStringValue(t, completed, "status", "completed")
	assertStringValue(t, completed, "stop_reason", "stop")
	output := mustAnySlice(t, completed["output"], "response.completed.output")
	if len(output) == 0 {
		t.Fatalf("output=%v", output)
	}
}

func frameIndexesByEvent(t *testing.T, frames []sseFrame, names []string) map[string]int {
	t.Helper()
	indexes := make(map[string]int, len(names))
	for _, name := range names {
		indexes[name] = firstFrameIndex(t, frames, name)
	}
	return indexes
}

func assertIncreasingEventOrder(t *testing.T, indexes map[string]int, names []string) {
	t.Helper()
	for i := 1; i < len(names); i++ {
		if indexes[names[i-1]] >= indexes[names[i]] {
			t.Fatalf("event order=%v", indexes)
		}
	}
}
