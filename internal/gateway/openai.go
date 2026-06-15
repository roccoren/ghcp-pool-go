package gateway

import (
	"encoding/json"
	"fmt"
	"time"
)

type ResponsesRequest struct {
	Model                string           `json:"model"`
	Input                any              `json:"input"`
	Instructions         string           `json:"instructions,omitempty"`
	Stream               bool             `json:"stream"`
	Temperature          *float64         `json:"temperature,omitempty"`
	TopP                 *float64         `json:"top_p,omitempty"`
	MaxOutputTokens      *int             `json:"max_output_tokens,omitempty"`
	MaxToolCalls         *int             `json:"max_tool_calls,omitempty"`
	Stop                 any              `json:"stop,omitempty"`
	User                 string           `json:"user,omitempty"`
	Background           *bool            `json:"background,omitempty"`
	Include              []string         `json:"include,omitempty"`
	Metadata             map[string]any   `json:"metadata,omitempty"`
	PreviousResponseID   string           `json:"previous_response_id,omitempty"`
	PromptCacheKey       string           `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention string           `json:"prompt_cache_retention,omitempty"`
	Reasoning            map[string]any   `json:"reasoning,omitempty"`
	SafetyIdentifier     string           `json:"safety_identifier,omitempty"`
	ServiceTier          string           `json:"service_tier,omitempty"`
	Store                *bool            `json:"store,omitempty"`
	StreamOptions        map[string]any   `json:"stream_options,omitempty"`
	Text                 map[string]any   `json:"text,omitempty"`
	TopLogprobs          *int             `json:"top_logprobs,omitempty"`
	Truncation           string           `json:"truncation,omitempty"`
	Cache                string           `json:"cache,omitempty"`
	Tools                []map[string]any `json:"tools,omitempty"`
	ToolChoice           any              `json:"tool_choice,omitempty"`
	ParallelToolCalls    *bool            `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort      string           `json:"reasoning_effort,omitempty"`
}

func (r ResponsesRequest) EffectiveReasoningEffort() string {
	if r.ReasoningEffort != "" {
		return r.ReasoningEffort
	}
	if r.Reasoning != nil {
		if effort, ok := r.Reasoning["effort"].(string); ok {
			return effort
		}
	}
	return ""
}

func (r ResponsesRequest) ResponseOptions() map[string]any {
	parallel := any(true)
	if r.ParallelToolCalls != nil {
		parallel = *r.ParallelToolCalls
	}
	store := any(false)
	if r.Store != nil {
		store = *r.Store
	}
	toolChoice := firstAny(r.ToolChoice, "auto")
	truncation := firstNonEmpty(r.Truncation, "disabled")
	return map[string]any{
		"background":             ptrValue(r.Background),
		"include":                r.Include,
		"instructions":           emptyToNilString(r.Instructions),
		"max_output_tokens":      ptrValue(r.MaxOutputTokens),
		"max_tool_calls":         ptrValue(r.MaxToolCalls),
		"metadata":               r.Metadata,
		"parallel_tool_calls":    parallel,
		"previous_response_id":   emptyToNilString(r.PreviousResponseID),
		"prompt_cache_key":       emptyToNilString(r.PromptCacheKey),
		"prompt_cache_retention": emptyToNilString(r.PromptCacheRetention),
		"reasoning":              nilIfEmptyMap(r.Reasoning),
		"safety_identifier":      emptyToNilString(r.SafetyIdentifier),
		"service_tier":           emptyToNilString(r.ServiceTier),
		"store":                  store,
		"stream_options":         nilIfEmptyMap(r.StreamOptions),
		"temperature":            ptrValue(r.Temperature),
		"text":                   nilIfEmptyMap(r.Text),
		"tool_choice":            toolChoice,
		"tools":                  nonNilTools(r.Tools),
		"top_logprobs":           ptrValue(r.TopLogprobs),
		"top_p":                  ptrValue(r.TopP),
		"truncation":             truncation,
		"user":                   emptyToNilString(r.User),
	}
}

func (r ResponsesRequest) ToChatRequest() ChatCompletionRequest {
	messages := []ChatMessage{}
	if r.Instructions != "" {
		messages = append(messages, ChatMessage{Role: "system", Content: r.Instructions, Raw: map[string]any{"role": "system", "content": r.Instructions}})
	}
	switch input := r.Input.(type) {
	case string:
		messages = append(messages, ChatMessage{Role: "user", Content: input, Raw: map[string]any{"role": "user", "content": input}})
	case []any:
		for _, item := range input {
			if obj, ok := item.(map[string]any); ok {
				if msg := responseInputItemToMessage(obj); msg != nil {
					messages = append(messages, *msg)
				}
			}
		}
	}
	return ChatCompletionRequest{
		Model:             r.Model,
		Messages:          messages,
		Stream:            r.Stream,
		Temperature:       r.Temperature,
		TopP:              r.TopP,
		MaxTokens:         r.MaxOutputTokens,
		Stop:              r.Stop,
		User:              r.User,
		Cache:             r.Cache,
		Tools:             r.Tools,
		ToolChoice:        r.ToolChoice,
		ParallelToolCalls: r.ParallelToolCalls,
		ResponseOptions:   r.ResponseOptions(),
		ReasoningEffort:   r.EffectiveReasoningEffort(),
		StreamOptions:     r.StreamOptions,
	}
}

func responseInputItemToMessage(item map[string]any) *ChatMessage {
	itype, _ := item["type"].(string)
	switch itype {
	case "function_call":
		callID := firstNonEmpty(stringFromAny(item["call_id"]), stringFromAny(item["id"]))
		name := stringFromAny(item["name"])
		args := stringFromAny(item["arguments"])
		raw := map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": callID, "name": name, "arguments": args}}}
		return &ChatMessage{Role: "assistant", Content: "", Raw: raw}
	case "custom_tool_call":
		callID := firstNonEmpty(stringFromAny(item["call_id"]), stringFromAny(item["id"]))
		raw := map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
			"id": callID, "name": stringFromAny(item["name"]), "arguments": stringFromAny(item["input"]), "type": "custom",
		}}}
		return &ChatMessage{Role: "assistant", Content: "", Raw: raw}
	case "function_call_output", "custom_tool_call_output":
		callID := firstNonEmpty(stringFromAny(item["call_id"]), stringFromAny(item["id"]))
		out := coerceText(item["output"])
		return &ChatMessage{Role: "tool", Content: out, Raw: map[string]any{"role": "tool", "content": out, "tool_call_id": callID}}
	case "reasoning":
		return nil
	}
	if role := stringFromAny(item["role"]); role != "" {
		return &ChatMessage{Role: role, Content: item["content"], Raw: map[string]any{"role": role, "content": item["content"]}}
	}
	if item["content"] != nil {
		return &ChatMessage{Role: "user", Content: item["content"], Raw: map[string]any{"role": "user", "content": item["content"]}}
	}
	return nil
}

type AnthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type AnthropicMessagesRequest struct {
	Model             string             `json:"model"`
	Messages          []AnthropicMessage `json:"messages"`
	System            any                `json:"system,omitempty"`
	Stream            bool               `json:"stream"`
	MaxTokens         *int               `json:"max_tokens,omitempty"`
	CacheControl      map[string]any     `json:"cache_control,omitempty"`
	Container         string             `json:"container,omitempty"`
	ContextManagement []map[string]any   `json:"context_management,omitempty"`
	InferenceGeo      string             `json:"inference_geo,omitempty"`
	Metadata          map[string]any     `json:"metadata,omitempty"`
	OutputConfig      map[string]any     `json:"output_config,omitempty"`
	ServiceTier       string             `json:"service_tier,omitempty"`
	Temperature       *float64           `json:"temperature,omitempty"`
	Thinking          map[string]any     `json:"thinking,omitempty"`
	TopK              *int               `json:"top_k,omitempty"`
	TopP              *float64           `json:"top_p,omitempty"`
	StopSequences     []string           `json:"stop_sequences,omitempty"`
	Cache             string             `json:"cache,omitempty"`
	Tools             []map[string]any   `json:"tools,omitempty"`
	ToolChoice        any                `json:"tool_choice,omitempty"`
	ReasoningEffort   string             `json:"reasoning_effort,omitempty"`
}

type EmbeddingsRequest struct {
	Model          string `json:"model"`
	Input          any    `json:"input"`
	EncodingFormat string `json:"encoding_format,omitempty"`
	Dimensions     *int   `json:"dimensions,omitempty"`
	User           string `json:"user,omitempty"`
}

func (r EmbeddingsRequest) InputTexts() []string {
	switch v := r.Input.(type) {
	case nil:
		return nil
	case string:
		return []string{v}
	case []any:
		if len(v) == 0 {
			return nil
		}
		if allNumbers(v) {
			return []string{numbersToText(v)}
		}
		out := make([]string, 0, len(v))
		for _, item := range v {
			if tokens, ok := item.([]any); ok && allNumbers(tokens) {
				out = append(out, numbersToText(tokens))
			} else {
				out = append(out, coerceText(item))
			}
		}
		return out
	default:
		return []string{coerceText(v)}
	}
}

func (r EmbeddingsRequest) Params() map[string]any {
	return map[string]any{
		"encoding_format": emptyToNilString(r.EncodingFormat),
		"dimensions":      ptrValue(r.Dimensions),
		"user":            emptyToNilString(r.User),
	}
}

func (r AnthropicMessagesRequest) AnthropicOptions() map[string]any {
	return map[string]any{
		"cache_control":      nilIfEmptyMap(r.CacheControl),
		"container":          emptyToNilString(r.Container),
		"context_management": r.ContextManagement,
		"inference_geo":      emptyToNilString(r.InferenceGeo),
		"max_tokens":         ptrValue(r.MaxTokens),
		"metadata":           r.Metadata,
		"output_config":      nilIfEmptyMap(r.OutputConfig),
		"service_tier":       emptyToNilString(r.ServiceTier),
		"stop_sequences":     r.StopSequences,
		"system":             r.System,
		"temperature":        ptrValue(r.Temperature),
		"thinking":           nilIfEmptyMap(r.Thinking),
		"tool_choice":        r.ToolChoice,
		"tools":              nonNilTools(r.Tools),
		"top_k":              ptrValue(r.TopK),
		"top_p":              ptrValue(r.TopP),
	}
}

func (r AnthropicMessagesRequest) ToChatRequest() ChatCompletionRequest {
	messages := []ChatMessage{}
	if r.System != nil {
		messages = append(messages, ChatMessage{Role: "system", Content: r.System, Raw: map[string]any{"role": "system", "content": r.System}})
	}
	for _, message := range r.Messages {
		messages = append(messages, anthropicMessageToChatMessages(message)...)
	}
	return ChatCompletionRequest{
		Model:           r.Model,
		Messages:        messages,
		Stream:          r.Stream,
		Temperature:     r.Temperature,
		TopP:            r.TopP,
		MaxTokens:       r.MaxTokens,
		Stop:            r.StopSequences,
		Cache:           r.Cache,
		Tools:           r.Tools,
		ToolChoice:      anthropicToolChoice(r.ToolChoice),
		ResponseOptions: r.AnthropicOptions(),
		ReasoningEffort: anthropicReasoningEffort(r.ReasoningEffort, r.OutputConfig),
	}
}

func anthropicMessageToChatMessages(item AnthropicMessage) []ChatMessage {
	parts, ok := item.Content.([]any)
	if !ok {
		return []ChatMessage{{Role: item.Role, Content: item.Content, Raw: map[string]any{"role": item.Role, "content": item.Content}}}
	}
	textParts := []string{}
	toolCalls := []any{}
	toolResults := []ChatMessage{}
	for _, part := range parts {
		block, ok := part.(map[string]any)
		if !ok {
			if s, ok := part.(string); ok {
				textParts = append(textParts, s)
			}
			continue
		}
		switch stringFromAny(block["type"]) {
		case "text", "input_text", "output_text":
			if text := stringFromAny(block["text"]); text != "" {
				textParts = append(textParts, text)
			}
		case "thinking":
			if text := stringFromAny(block["thinking"]); text != "" {
				textParts = append(textParts, text)
			}
		case "tool_use":
			args, _ := json.Marshal(firstAny(block["input"], map[string]any{}))
			toolCalls = append(toolCalls, map[string]any{"id": stringFromAny(block["id"]), "name": stringFromAny(block["name"]), "arguments": string(args)})
		case "tool_result":
			callID := stringFromAny(block["tool_use_id"])
			content := coerceText(block["content"])
			toolResults = append(toolResults, ChatMessage{Role: "tool", Content: content, Raw: map[string]any{"role": "tool", "content": content, "tool_call_id": callID}})
		case "redacted_thinking":
		default:
			if text := coerceText(block); text != "" {
				textParts = append(textParts, text)
			}
		}
	}
	out := []ChatMessage{}
	if len(textParts) > 0 || len(toolCalls) > 0 {
		content := stringsJoin(textParts, "\n")
		raw := map[string]any{"role": item.Role, "content": content}
		if len(toolCalls) > 0 {
			raw["tool_calls"] = toolCalls
		}
		out = append(out, ChatMessage{Role: item.Role, Content: content, Raw: raw})
	}
	out = append(out, toolResults...)
	return out
}

func usageToOpenAI(usage Usage) map[string]any {
	usage = usage.Normalized()
	block := map[string]any{
		"prompt_tokens":     usage.InputTokens,
		"completion_tokens": usage.OutputTokens,
		"total_tokens":      usage.TotalTokens,
	}
	if usage.CachedTokens != 0 {
		block["prompt_tokens_details"] = map[string]any{"cached_tokens": usage.CachedTokens}
	}
	return block
}

func embeddingResponse(model string, embeddings [][]float64, usage Usage) map[string]any {
	data := make([]any, 0, len(embeddings))
	for i, vector := range embeddings {
		data = append(data, map[string]any{
			"object":    "embedding",
			"index":     i,
			"embedding": vector,
		})
	}
	usage = usage.Normalized()
	return map[string]any{
		"object": "list",
		"data":   data,
		"model":  model,
		"usage": map[string]any{
			"prompt_tokens": usage.InputTokens,
			"total_tokens":  usage.TotalTokens,
		},
	}
}

func usageToResponses(usage Usage) map[string]any {
	usage = usage.Normalized()
	return map[string]any{
		"input_tokens":          usage.InputTokens,
		"input_tokens_details":  map[string]any{"cached_tokens": usage.CachedTokens},
		"output_tokens":         usage.OutputTokens,
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		"total_tokens":          usage.TotalTokens,
	}
}

func completionResponse(model, content, finish string, usage Usage, toolCalls []ToolCall) map[string]any {
	message := map[string]any{"role": "assistant", "content": nil}
	if content != "" {
		message["content"] = content
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCallsToOpenAI(toolCalls)
		finish = "tool_calls"
	}
	return map[string]any{
		"id":      newID("chatcmpl"),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": usageToOpenAI(usage),
	}
}

func toolCallsToOpenAI(toolCalls []ToolCall) []map[string]any {
	out := make([]map[string]any, 0, len(toolCalls))
	for _, tc := range toolCalls {
		out = append(out, map[string]any{
			"id":       tc.ID,
			"type":     "function",
			"function": map[string]any{"name": tc.Name, "arguments": tc.Arguments},
		})
	}
	return out
}

func responseResponse(model, content, finish string, usage Usage, toolCalls []ToolCall, options map[string]any) map[string]any {
	output := []any{}
	if content != "" {
		output = append(output, responseMessageItem(newID("msg"), content))
	}
	for _, tc := range toolCalls {
		if tc.Kind == "custom" {
			output = append(output, map[string]any{"id": newID("ctc"), "type": "custom_tool_call", "status": "completed", "call_id": tc.ID, "name": tc.Name, "input": tc.Arguments})
		} else {
			output = append(output, map[string]any{"id": newID("fc"), "type": "function_call", "status": "completed", "call_id": tc.ID, "name": tc.Name, "arguments": tc.Arguments})
		}
	}
	if len(output) == 0 {
		output = append(output, responseMessageItem(newID("msg"), content))
	}
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	return responseObject(newID("resp"), model, "completed", output, content, finish, &usage, options, ptrBool(len(toolCalls) == 0))
}

func responseObject(id, model, status string, output []any, outputText, finish string, usage *Usage, options map[string]any, endTurn *bool) map[string]any {
	created := time.Now().Unix()
	obj := map[string]any{
		"id":          id,
		"object":      "response",
		"created_at":  created,
		"status":      status,
		"model":       model,
		"output":      output,
		"output_text": outputText,
	}
	for k, v := range responseObjectDefaults(options) {
		obj[k] = v
	}
	if status == "completed" {
		obj["completed_at"] = created
	}
	if usage != nil {
		obj["stop_reason"] = finish
		obj["usage"] = usageToResponses(*usage)
	}
	if endTurn != nil {
		obj["end_turn"] = *endTurn
	}
	return obj
}

func responseObjectDefaults(options map[string]any) map[string]any {
	return map[string]any{
		"error":                  nil,
		"incomplete_details":     nil,
		"instructions":           optionValue(options, "instructions", nil),
		"metadata":               optionValue(options, "metadata", map[string]any{}),
		"parallel_tool_calls":    optionValue(options, "parallel_tool_calls", true),
		"temperature":            optionValue(options, "temperature", nil),
		"tool_choice":            optionValue(options, "tool_choice", "auto"),
		"tools":                  optionValue(options, "tools", []map[string]any{}),
		"top_p":                  optionValue(options, "top_p", nil),
		"background":             optionValue(options, "background", nil),
		"max_output_tokens":      optionValue(options, "max_output_tokens", nil),
		"max_tool_calls":         optionValue(options, "max_tool_calls", nil),
		"previous_response_id":   optionValue(options, "previous_response_id", nil),
		"prompt_cache_key":       optionValue(options, "prompt_cache_key", nil),
		"prompt_cache_retention": optionValue(options, "prompt_cache_retention", nil),
		"reasoning":              optionValue(options, "reasoning", nil),
		"safety_identifier":      optionValue(options, "safety_identifier", nil),
		"service_tier":           optionValue(options, "service_tier", nil),
		"store":                  optionValue(options, "store", false),
		"text":                   optionValue(options, "text", map[string]any{"format": map[string]any{"type": "text"}}),
		"top_logprobs":           optionValue(options, "top_logprobs", nil),
		"truncation":             optionValue(options, "truncation", "disabled"),
		"user":                   optionValue(options, "user", nil),
	}
}

func responseMessageItem(id, content string) map[string]any {
	return map[string]any{
		"id":      id,
		"type":    "message",
		"status":  "completed",
		"role":    "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": content, "annotations": []any{}}},
		"phase":   "final_answer",
	}
}

func anthropicResponse(model, content, finish string, usage Usage, toolCalls []ToolCall, options map[string]any) map[string]any {
	blocks := []any{}
	if content != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": content})
	}
	for _, tc := range toolCalls {
		blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": argsToObject(tc.Arguments)})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": content})
	}
	stop := anthropicStopReason(finish)
	if len(toolCalls) > 0 {
		stop = "tool_use"
	}
	msg := map[string]any{
		"id":            newID("msg"),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   stop,
		"stop_sequence": nil,
		"stop_details":  nil,
		"usage":         anthropicUsage(usage, options),
	}
	if container := anthropicContainer(options); container != nil {
		msg["container"] = container
	}
	return msg
}

func anthropicUsage(usage Usage, options map[string]any) map[string]any {
	usage = usage.Normalized()
	block := map[string]any{
		"input_tokens":                usage.InputTokens,
		"output_tokens":               usage.OutputTokens,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     usage.CachedTokens,
		"output_tokens_details":       map[string]any{"thinking_tokens": 0},
	}
	if optionValue(options, "service_tier", nil) != nil {
		block["service_tier"] = "standard"
	}
	if geo := optionValue(options, "inference_geo", nil); geo != nil {
		block["inference_geo"] = geo
	}
	return block
}

func anthropicStopReason(finish string) string {
	switch finish {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return finish
	}
}

func anthropicContainer(options map[string]any) map[string]any {
	container := optionValue(options, "container", nil)
	if container == nil || container == "" {
		return nil
	}
	return map[string]any{
		"id":         stringFromAny(container),
		"expires_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	}
}

func chunkResponse(id, model string, delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
}

func usageChunkResponse(id, model string, usage Usage) map[string]any {
	return map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{}, "usage": usageToOpenAI(usage)}
}

func responseCreated(responseID, model string, seq int, options map[string]any) map[string]any {
	return map[string]any{"type": "response.created", "sequence_number": seq, "response": responseObject(responseID, model, "in_progress", []any{}, "", "", nil, options, nil)}
}

func responseInProgress(responseID, model string, seq int, options map[string]any) map[string]any {
	return map[string]any{"type": "response.in_progress", "sequence_number": seq, "response": responseObject(responseID, model, "in_progress", []any{}, "", "", nil, options, nil)}
}

func anthropicMessageStart(messageID, model string, usage Usage, options map[string]any) map[string]any {
	msg := map[string]any{"id": messageID, "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "stop_details": nil, "usage": anthropicUsage(usage, options)}
	if container := anthropicContainer(options); container != nil {
		msg["container"] = container
	}
	return map[string]any{"type": "message_start", "message": msg}
}

func anthropicMessageDelta(finish string, usage Usage, options map[string]any) map[string]any {
	delta := map[string]any{"stop_reason": anthropicStopReason(finish), "stop_sequence": nil, "stop_details": nil}
	if container := anthropicContainer(options); container != nil {
		delta["container"] = container
	}
	return map[string]any{"type": "message_delta", "delta": delta, "usage": anthropicUsage(usage, options)}
}

func responseCompleted(responseID, model string, output []any, outputText, finish string, usage Usage, seq int, options map[string]any, endTurn bool) map[string]any {
	return map[string]any{"type": "response.completed", "sequence_number": seq, "response": responseObject(responseID, model, "completed", output, outputText, finish, &usage, options, &endTurn)}
}

func optionValue(options map[string]any, key string, fallback any) any {
	if options == nil {
		return fallback
	}
	if value, ok := options[key]; ok && value != nil {
		return value
	}
	return fallback
}

func emptyToNilString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nonNilTools(tools []map[string]any) []map[string]any {
	if tools == nil {
		return []map[string]any{}
	}
	return tools
}

func stringFromAny(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return toJSONString(value)
}

func ptrBool(v bool) *bool { return &v }

func argsToObject(arguments string) map[string]any {
	if arguments == "" {
		return map[string]any{}
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(arguments), &obj); err != nil {
		return map[string]any{}
	}
	return obj
}

func anthropicToolChoice(choice any) any {
	switch v := choice.(type) {
	case nil:
		return nil
	case string:
		return v
	case map[string]any:
		switch v["type"] {
		case "auto":
			return "auto"
		case "any":
			return "required"
		case "none":
			return "none"
		case "tool":
			if name := stringFromAny(v["name"]); name != "" {
				return map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
		}
	}
	return nil
}

func anthropicReasoningEffort(effort string, outputConfig map[string]any) string {
	if effort != "" {
		return effort
	}
	if outputConfig != nil {
		return stringFromAny(outputConfig["effort"])
	}
	return ""
}

func allNumbers(values []any) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		switch value.(type) {
		case float64, int, int64, json.Number:
		default:
			return false
		}
	}
	return true
}

func numbersToText(values []any) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, fmt.Sprint(value))
	}
	return stringsJoin(parts, " ")
}

func stringsJoin(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, part := range parts[1:] {
		out += sep + part
	}
	return out
}
