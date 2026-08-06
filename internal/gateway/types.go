package gateway

import (
	"encoding/json"
	"time"
)

type Usage struct {
	InputTokens    int             `json:"input_tokens" yaml:"input_tokens"`
	OutputTokens   int             `json:"output_tokens" yaml:"output_tokens"`
	CachedTokens   int             `json:"cached_tokens" yaml:"cached_tokens"`
	TotalTokens    int             `json:"total_tokens" yaml:"total_tokens"`
	Credits        float64         `json:"credits" yaml:"credits"`
	APIEndpoint    string          `json:"api_endpoint,omitempty" yaml:"api_endpoint"`
	ProviderCallID string          `json:"provider_call_id,omitempty" yaml:"provider_call_id"`
	DurationMS     int             `json:"duration_ms,omitempty" yaml:"duration_ms"`
	QuotaSnapshots json.RawMessage `json:"quota_snapshots,omitempty" yaml:"quota_snapshots"`
}

// Normalized fills in a missing or stale total.
//
// Usage is seeded before the output count is known, so a total set at that point
// equals the input count. Recomputing only when the total falls below the parts
// corrects that without discarding a larger upstream total, which can legitimately
// include tokens that appear in neither input nor output.
func (u Usage) Normalized() Usage {
	if sum := u.InputTokens + u.OutputTokens; u.TotalTokens < sum {
		u.TotalTokens = sum
	}
	return u
}

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Kind      string `json:"kind"`
}

func (tc ToolCall) normalized() ToolCall {
	if tc.Kind == "" {
		tc.Kind = "function"
	}
	return tc
}

type ChatResult struct {
	ID               string           `json:"id,omitempty"`
	Status           string           `json:"status,omitempty"`
	Content          string           `json:"content"`
	Model            string           `json:"model"`
	Usage            Usage            `json:"usage"`
	FinishReason     string           `json:"finish_reason"`
	ToolCalls        []ToolCall       `json:"tool_calls,omitempty"`
	ResponsesOutput  []map[string]any `json:"responses_output,omitempty"`
	AnthropicContent []map[string]any `json:"anthropic_content,omitempty"`
}

type EmbeddingResult struct {
	Model      string      `json:"model"`
	Embeddings [][]float64 `json:"embeddings"`
	Usage      Usage       `json:"usage"`
}

type StreamItem struct {
	Kind         string
	Text         string
	ToolCall     ToolCall
	Index        int
	Usage        Usage
	FinishReason string
	Err          error
}

type ModelSpec struct {
	ID                 string         `json:"id"`
	Name               string         `json:"name,omitempty"`
	Version            string         `json:"version,omitempty"`
	ModelPickerEnabled *bool          `json:"model_picker_enabled,omitempty"`
	SupportedEndpoints []string       `json:"supported_endpoints,omitempty"`
	Capabilities       map[string]any `json:"capabilities,omitempty"`
}

type CacheRecord struct {
	Content          string           `json:"content"`
	Model            string           `json:"model"`
	FinishReason     string           `json:"finish_reason"`
	Usage            Usage            `json:"usage"`
	ToolCalls        []ToolCall       `json:"tool_calls,omitempty"`
	ResponsesOutput  []map[string]any `json:"responses_output,omitempty"`
	AnthropicContent []map[string]any `json:"anthropic_content,omitempty"`
}

type NeutralMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []map[string]any `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	// Attachments carries images, which the SDK takes as message attachments
	// rather than as content blocks.
	Attachments []MessageAttachment `json:"-"`
	// DeclaredImages counts image blocks seen on the wire, including ones that
	// did not convert, so a silent drop can be turned into an error.
	DeclaredImages int `json:"-"`
}

type ChatMessage struct {
	Role    string         `json:"role"`
	Content any            `json:"content"`
	Name    string         `json:"name,omitempty"`
	Raw     map[string]any `json:"-"`
}

func (m *ChatMessage) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Raw = raw
	if v, ok := raw["role"].(string); ok {
		m.Role = v
	}
	m.Content = raw["content"]
	if v, ok := raw["name"].(string); ok {
		m.Name = v
	}
	return nil
}

func (m ChatMessage) MarshalJSON() ([]byte, error) {
	raw := map[string]any{}
	for k, v := range m.Raw {
		raw[k] = v
	}
	raw["role"] = m.Role
	raw["content"] = m.Content
	if m.Name != "" {
		raw["name"] = m.Name
	}
	return json.Marshal(raw)
}

func (m ChatMessage) Text() string {
	return coerceText(m.Content)
}

type ChatCompletionRequest struct {
	Model             string           `json:"model"`
	Messages          []ChatMessage    `json:"messages"`
	Stream            bool             `json:"stream"`
	Temperature       *float64         `json:"temperature,omitempty"`
	TopP              *float64         `json:"top_p,omitempty"`
	MaxTokens         *int             `json:"max_tokens,omitempty"`
	Stop              any              `json:"stop,omitempty"`
	N                 *int             `json:"n,omitempty"`
	PresencePenalty   *float64         `json:"presence_penalty,omitempty"`
	FrequencyPenalty  *float64         `json:"frequency_penalty,omitempty"`
	ResponseFormat    map[string]any   `json:"response_format,omitempty"`
	User              string           `json:"user,omitempty"`
	StreamOptions     map[string]any   `json:"stream_options,omitempty"`
	Tools             []map[string]any `json:"tools,omitempty"`
	ToolChoice        any              `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls,omitempty"`
	ResponseOptions   map[string]any   `json:"response_options,omitempty"`
	Cache             string           `json:"cache,omitempty"`
	ReasoningEffort   string           `json:"reasoning_effort,omitempty"`
	ContextTier       string           `json:"context_tier,omitempty"`

	PreferredEndpoint string         `json:"-"`
	FallbackEndpoints []string       `json:"-"`
	AnthropicRaw      map[string]any `json:"-"`
	ResponsesRaw      map[string]any `json:"-"`
}

func (r ChatCompletionRequest) SamplingParams() map[string]any {
	params := map[string]any{
		"temperature":             ptrValue(r.Temperature),
		"top_p":                   ptrValue(r.TopP),
		"max_tokens":              ptrValue(r.MaxTokens),
		"stop":                    r.Stop,
		"n":                       ptrValue(r.N),
		"presence_penalty":        ptrValue(r.PresencePenalty),
		"frequency_penalty":       ptrValue(r.FrequencyPenalty),
		"response_format":         nilIfEmptyMap(r.ResponseFormat),
		"reasoning_effort":        emptyToNil(r.ReasoningEffort),
		"context_tier":            emptyToNilString(r.ContextTier),
		"tools":                   normalizeTools(r.Tools),
		"tool_choice":             r.ToolChoice,
		"parallel_tool_calls":     ptrValue(r.ParallelToolCalls),
		"response_options":        nilIfEmptyMap(r.ResponseOptions),
		internalAnthropicRawParam: nilIfEmptyMap(r.AnthropicRaw),
		internalResponsesRawParam: nilIfEmptyMap(r.ResponsesRaw),
	}
	return params
}

func (r ChatCompletionRequest) NeutralMessages() []NeutralMessage {
	out := make([]NeutralMessage, 0, len(r.Messages))
	for _, m := range r.Messages {
		entry := NeutralMessage{
			Role:           m.Role,
			Content:        m.Text(),
			Attachments:    attachmentsFromContent(m.Content),
			DeclaredImages: countImageBlocks(m.Content),
		}
		if raw := m.Raw; raw != nil {
			if tcs, ok := raw["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					if obj, ok := tc.(map[string]any); ok {
						entry.ToolCalls = append(entry.ToolCalls, obj)
					}
				}
			}
			if id, ok := raw["tool_call_id"].(string); ok {
				entry.ToolCallID = id
			}
		}
		out = append(out, entry)
	}
	return out
}

func (r ChatCompletionRequest) EndpointPreferences() []string {
	preferred := r.PreferredEndpoint
	if preferred == "" {
		preferred = endpointChatCompletions
	}
	out := []string{preferred}
	for _, endpoint := range r.FallbackEndpoints {
		if endpoint == "" {
			continue
		}
		seen := false
		for _, existing := range out {
			if endpointMatches(existing, endpoint) {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, endpoint)
		}
	}
	return out
}

type Plan struct {
	Model         string
	ResponseModel string
	Endpoint      string
	Messages      []NeutralMessage
	Params        map[string]any
	CacheKey      string
	Control       string
	Namespace     string
	CacheHit      *CacheRecord
	Account       *Account
	IncludeUsage  bool
}

func (p Plan) Headers() map[string]string {
	responseModel := firstNonEmpty(p.ResponseModel, p.Model)
	headers := map[string]string{
		"x-ghcp-model":              responseModel,
		"openai-model":              responseModel,
		"x-openai-model":            responseModel,
		"request-id":                newID("req"),
		"anthropic-organization-id": "ghcp-pool",
	}
	if responseModel != p.Model {
		headers["x-ghcp-upstream-model"] = p.Model
	}
	if p.CacheHit != nil {
		headers["x-ghcp-account"] = "cache"
		headers["x-ghcp-cache"] = "hit"
		return headers
	}
	account := "none"
	if p.Account != nil {
		account = p.Account.ID()
	}
	headers["x-ghcp-account"] = account
	headers["x-ghcp-cache"] = "miss"
	return headers
}

type UsageEvent struct {
	TS          float64
	AccountID   *string
	Model       string
	Usage       Usage
	CacheResult string
	Success     bool
	ErrorType   string
}

func nowUnix() float64 {
	return float64(time.Now().UnixNano()) / float64(time.Second)
}
