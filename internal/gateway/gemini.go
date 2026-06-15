package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type GeminiGenerateContentRequest struct {
	Contents          []GeminiContent         `json:"contents"`
	SystemInstruction *GeminiContent          `json:"systemInstruction,omitempty"`
	Tools             []GeminiTool            `json:"tools,omitempty"`
	ToolConfig        *GeminiToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *GeminiGenerationConfig `json:"generationConfig,omitempty"`
	Config            *GeminiGenerationConfig `json:"config,omitempty"`
}

type GeminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []GeminiPart `json:"parts,omitempty"`
}

type GeminiPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *GeminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *GeminiFunctionResponse `json:"functionResponse,omitempty"`
}

type GeminiFunctionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name,omitempty"`
	Args map[string]any `json:"args,omitempty"`
}

type GeminiFunctionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name,omitempty"`
	Response map[string]any `json:"response,omitempty"`
}

type GeminiTool struct {
	FunctionDeclarations []GeminiFunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type GeminiFunctionDeclaration struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type GeminiToolConfig struct {
	FunctionCallingConfig *GeminiFunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

type GeminiFunctionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type GeminiGenerationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	TopK            *int     `json:"topK,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
}

type GeminiGenerateContentResponse struct {
	Candidates    []GeminiCandidate    `json:"candidates,omitempty"`
	UsageMetadata *GeminiUsageMetadata `json:"usageMetadata,omitempty"`
	ModelVersion  string               `json:"modelVersion,omitempty"`
}

type GeminiCandidate struct {
	Content      GeminiContent `json:"content,omitempty"`
	FinishReason string        `json:"finishReason,omitempty"`
	Index        int           `json:"index,omitempty"`
}

type GeminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount int `json:"candidatesTokenCount,omitempty"`
	TotalTokenCount      int `json:"totalTokenCount,omitempty"`
}

type GeminiCountTokensResponse struct {
	TotalTokens  int `json:"totalTokens"`
	PromptTokens int `json:"promptTokens,omitempty"`
}

func (s *server) geminiModels(w http.ResponseWriter, r *http.Request) {
	p, ok := s.require(w, r, "inference")
	if !ok {
		return
	}
	models := []map[string]any{}
	for _, model := range s.gw.Registry.VisibleModels() {
		if !p.MayUseModel(model) {
			continue
		}
		for _, displayID := range s.gw.Settings.DisplayIDsForModel(model) {
			models = append(models, map[string]any{
				"name":                       "models/" + displayID,
				"version":                    displayID,
				"displayName":                displayID,
				"description":                "GitHub Copilot model exposed by ghcp-pool-go",
				"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent", "countTokens"},
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models}, nil)
}

func (s *server) geminiModelAction(w http.ResponseWriter, r *http.Request) {
	actionPath := r.PathValue("model_action")
	model, action, ok := parseGeminiModelAction(actionPath)
	if !ok {
		writeGeminiError(w, http.StatusNotFound, "NOT_FOUND", "Endpoint not found")
		return
	}
	switch action {
	case "generateContent":
		s.geminiGenerateContent(w, r, model, false)
	case "streamGenerateContent":
		s.geminiGenerateContent(w, r, model, true)
	case "countTokens":
		s.geminiCountTokens(w, r, model)
	default:
		writeGeminiError(w, http.StatusNotFound, "NOT_FOUND", "Endpoint not found")
	}
}

func (s *server) geminiGenerateContent(w http.ResponseWriter, r *http.Request, model string, stream bool) {
	p, ok := s.require(w, r, "inference")
	if !ok {
		return
	}
	var body GeminiGenerateContentRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	chat, err := geminiToChatRequest(model, body, stream)
	if err != nil {
		writeGeminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	if !s.modelAllowed(w, chat.Model, p) {
		writeGeminiError(w, http.StatusForbidden, "PERMISSION_DENIED", "model not allowed: "+chat.Model)
		return
	}
	plan, err := s.gw.PrepareWithWait(r.Context(), chat, p, firstNonEmpty(r.Header.Get("x-ghcp-cache"), chat.Cache))
	if err != nil {
		s.writeInferenceError(w, err)
		return
	}
	if stream {
		ctx, cancel := context.WithCancel(r.Context())
		writeStream(w, plan.Headers(), geminiStreamFromChat(ctx, s.gw.StreamChat(ctx, plan)), cancel)
		return
	}
	result, _, err := s.gw.CompleteResult(r.Context(), plan)
	if err != nil {
		writeGeminiError(w, http.StatusBadGateway, "INTERNAL", "backend error: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, geminiFromChatResult(result), plan.Headers())
}

func (s *server) geminiCountTokens(w http.ResponseWriter, r *http.Request, model string) {
	p, ok := s.require(w, r, "inference")
	if !ok {
		return
	}
	var body GeminiGenerateContentRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	chat, err := geminiToChatRequest(model, body, false)
	if err != nil {
		writeGeminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	if !s.modelAllowed(w, chat.Model, p) {
		writeGeminiError(w, http.StatusForbidden, "PERMISSION_DENIED", "model not allowed: "+chat.Model)
		return
	}
	total, _ := s.gw.EstimateInputTokens(chat)
	writeJSON(w, http.StatusOK, GeminiCountTokensResponse{TotalTokens: total, PromptTokens: total}, nil)
}

func parseGeminiModelAction(pathValue string) (string, string, bool) {
	parts := strings.SplitN(pathValue, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	model := strings.TrimPrefix(parts[0], "models/")
	if model == "" {
		return "", "", false
	}
	return model, parts[1], true
}

func geminiToChatRequest(model string, req GeminiGenerateContentRequest, stream bool) (ChatCompletionRequest, error) {
	chat := ChatCompletionRequest{
		Model:             model,
		Stream:            stream,
		PreferredEndpoint: endpointChatCompletions,
		FallbackEndpoints: []string{endpointResponses, endpointMessages},
	}
	if stream {
		chat.StreamOptions = map[string]any{"include_usage": true}
	}
	config := req.GenerationConfig
	if config == nil {
		config = req.Config
	}
	if config != nil {
		chat.Temperature = config.Temperature
		chat.TopP = config.TopP
		chat.MaxTokens = config.MaxOutputTokens
	}
	if req.SystemInstruction != nil {
		if text := geminiText(req.SystemInstruction.Parts); text != "" {
			chat.Messages = append(chat.Messages, ChatMessage{Role: "system", Content: text, Raw: map[string]any{"role": "system", "content": text}})
		}
	}
	for _, content := range req.Contents {
		messages, err := geminiContentToChatMessages(content)
		if err != nil {
			return chat, err
		}
		chat.Messages = append(chat.Messages, messages...)
	}
	for _, tool := range req.Tools {
		for _, decl := range tool.FunctionDeclarations {
			chat.Tools = append(chat.Tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        decl.Name,
					"description": decl.Description,
					"parameters":  firstAny(decl.Parameters, map[string]any{"type": "object"}),
				},
			})
		}
	}
	if req.ToolConfig != nil && req.ToolConfig.FunctionCallingConfig != nil {
		cfg := req.ToolConfig.FunctionCallingConfig
		if len(cfg.AllowedFunctionNames) > 0 {
			chat.Tools = filterChatToolsByName(chat.Tools, cfg.AllowedFunctionNames)
		}
		switch strings.ToUpper(cfg.Mode) {
		case "NONE":
			chat.ToolChoice = "none"
		case "ANY":
			names := cfg.AllowedFunctionNames
			if len(names) == 1 {
				chat.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": names[0]}}
			} else {
				chat.ToolChoice = "required"
			}
		default:
			chat.ToolChoice = "auto"
		}
	}
	if len(chat.Messages) == 0 {
		return chat, fmt.Errorf("contents or systemInstruction is required")
	}
	return chat, nil
}

func filterChatToolsByName(tools []map[string]any, allowed []string) []map[string]any {
	if len(allowed) == 0 {
		return tools
	}
	allowedSet := map[string]bool{}
	for _, name := range allowed {
		allowedSet[name] = true
	}
	filtered := []map[string]any{}
	for _, tool := range tools {
		fn, _ := tool["function"].(map[string]any)
		name := firstNonEmpty(stringFromAny(fn["name"]), stringFromAny(tool["name"]))
		if allowedSet[name] {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

func geminiContentToChatMessages(content GeminiContent) ([]ChatMessage, error) {
	role := content.Role
	if role == "" {
		role = "user"
	}
	switch role {
	case "user":
		return geminiUserContentToChat(content)
	case "model":
		return geminiModelContentToChat(content)
	default:
		return nil, fmt.Errorf("unsupported Gemini role: %s", role)
	}
}

func geminiUserContentToChat(content GeminiContent) ([]ChatMessage, error) {
	messages := []ChatMessage{}
	textParts := []string{}
	flush := func() {
		if len(textParts) == 0 {
			return
		}
		text := strings.Join(textParts, "\n\n")
		messages = append(messages, ChatMessage{Role: "user", Content: text, Raw: map[string]any{"role": "user", "content": text}})
		textParts = nil
	}
	for _, part := range content.Parts {
		if part.Text != "" {
			textParts = append(textParts, part.Text)
		}
		if part.FunctionResponse != nil {
			flush()
			payload := firstAny(part.FunctionResponse.Response, map[string]any{})
			data, err := json.Marshal(payload)
			if err != nil {
				return nil, fmt.Errorf("marshal functionResponse: %w", err)
			}
			callID := firstNonEmpty(part.FunctionResponse.ID, part.FunctionResponse.Name)
			messages = append(messages, ChatMessage{Role: "tool", Content: string(data), Raw: map[string]any{"role": "tool", "content": string(data), "tool_call_id": callID}})
		}
	}
	flush()
	return messages, nil
}

func geminiModelContentToChat(content GeminiContent) ([]ChatMessage, error) {
	textParts := []string{}
	toolCalls := []any{}
	for _, part := range content.Parts {
		if part.Text != "" {
			textParts = append(textParts, part.Text)
		}
		if part.FunctionCall != nil {
			args, err := json.Marshal(firstAny(part.FunctionCall.Args, map[string]any{}))
			if err != nil {
				return nil, fmt.Errorf("marshal functionCall args: %w", err)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   firstNonEmpty(part.FunctionCall.ID, part.FunctionCall.Name),
				"type": "function",
				"function": map[string]any{
					"name":      part.FunctionCall.Name,
					"arguments": string(args),
				},
			})
		}
	}
	text := strings.Join(textParts, "\n\n")
	raw := map[string]any{"role": "assistant", "content": text}
	if len(toolCalls) > 0 {
		raw["tool_calls"] = toolCalls
	}
	return []ChatMessage{{Role: "assistant", Content: text, Raw: raw}}, nil
}

func geminiText(parts []GeminiPart) string {
	text := []string{}
	for _, part := range parts {
		if part.Text != "" {
			text = append(text, part.Text)
		}
	}
	return strings.Join(text, "\n\n")
}

func geminiFromChatResult(result ChatResult) GeminiGenerateContentResponse {
	candidate := GeminiCandidate{Index: 0, FinishReason: geminiFinishReason(result.FinishReason), Content: GeminiContent{Role: "model"}}
	if result.Content != "" {
		candidate.Content.Parts = append(candidate.Content.Parts, GeminiPart{Text: result.Content})
	}
	for _, tc := range result.ToolCalls {
		candidate.Content.Parts = append(candidate.Content.Parts, GeminiPart{FunctionCall: &GeminiFunctionCall{ID: tc.ID, Name: tc.Name, Args: argsToObject(tc.Arguments)}})
	}
	if len(candidate.Content.Parts) == 0 {
		candidate.Content.Parts = append(candidate.Content.Parts, GeminiPart{Text: ""})
	}
	return GeminiGenerateContentResponse{
		Candidates:    []GeminiCandidate{candidate},
		UsageMetadata: geminiUsage(result.Usage),
		ModelVersion:  result.Model,
	}
}

func geminiUsage(usage Usage) *GeminiUsageMetadata {
	usage = usage.Normalized()
	return &GeminiUsageMetadata{PromptTokenCount: usage.InputTokens, CandidatesTokenCount: usage.OutputTokens, TotalTokenCount: usage.TotalTokens}
}

func geminiFinishReason(reason string) string {
	switch reason {
	case "", "stop", "tool_calls":
		return "STOP"
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	default:
		return strings.ToUpper(reason)
	}
}

func geminiStreamFromChat(ctx context.Context, chunks <-chan string) <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		for chunk := range chunks {
			for _, data := range sseDataLines(chunk) {
				if data == "[DONE]" {
					continue
				}
				resp := geminiResponseFromChatChunk([]byte(data))
				if resp != nil {
					if !emitSSE(ctx, out, dataSSE(resp)) {
						return
					}
				}
			}
		}
	}()
	return out
}

func sseDataLines(chunk string) []string {
	data := []string{}
	for _, line := range strings.Split(chunk, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return data
}

func geminiResponseFromChatChunk(data []byte) any {
	var chunk struct {
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content   any              `json:"content"`
				ToolCalls []map[string]any `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
		Error any            `json:"error"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil
	}
	if chunk.Error != nil {
		return map[string]any{"error": chunk.Error}
	}
	if len(chunk.Choices) == 0 && chunk.Usage != nil {
		return GeminiGenerateContentResponse{UsageMetadata: geminiUsage(usageFromAny(chunk.Usage, endpointChatCompletions)), ModelVersion: chunk.Model}
	}
	resp := GeminiGenerateContentResponse{ModelVersion: chunk.Model}
	for _, choice := range chunk.Choices {
		finish := ""
		if choice.FinishReason != "" {
			finish = geminiFinishReason(choice.FinishReason)
		}
		candidate := GeminiCandidate{Index: 0, FinishReason: finish, Content: GeminiContent{Role: "model"}}
		if text := coerceText(choice.Delta.Content); text != "" {
			candidate.Content.Parts = append(candidate.Content.Parts, GeminiPart{Text: text})
		}
		for _, raw := range choice.Delta.ToolCalls {
			fn, _ := raw["function"].(map[string]any)
			candidate.Content.Parts = append(candidate.Content.Parts, GeminiPart{FunctionCall: &GeminiFunctionCall{ID: stringFromAny(raw["id"]), Name: stringFromAny(fn["name"]), Args: argsToObject(stringFromAny(fn["arguments"]))}})
		}
		if len(candidate.Content.Parts) > 0 || choice.FinishReason != "" {
			resp.Candidates = append(resp.Candidates, candidate)
		}
	}
	if len(resp.Candidates) == 0 {
		return nil
	}
	return resp
}

func writeGeminiError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": status, "message": message, "status": code}}, nil)
}
