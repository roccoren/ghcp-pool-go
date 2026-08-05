package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var ErrEmbeddingsUnsupported = errors.New("embeddings are not available through the configured backend")

type Backend interface {
	Start(context.Context) error
	ListModels(context.Context) ([]ModelSpec, error)
	Chat(context.Context, string, []NeutralMessage, map[string]any) (ChatResult, error)
	Embeddings(context.Context, string, []string, map[string]any) (EmbeddingResult, error)
	ChatStream(context.Context, string, []NeutralMessage, map[string]any) (<-chan StreamItem, error)
	Close() error
}

type FakeBackend struct {
	accountID string
	models    []string
}

func NewFakeBackend(accountID string, models []string) *FakeBackend {
	if len(models) == 0 {
		models = []string{"gpt-4.1"}
	}
	return &FakeBackend{accountID: accountID, models: models}
}

func (b *FakeBackend) ListModels(context.Context) ([]ModelSpec, error) {
	out := make([]ModelSpec, 0, len(b.models))
	for _, model := range b.models {
		out = append(out, ModelSpec{ID: model, SupportedEndpoints: fakeSupportedEndpoints(model), Capabilities: map[string]any{"streaming": true}})
	}
	return out, nil
}

func (b *FakeBackend) Start(context.Context) error { return nil }

func (b *FakeBackend) Chat(_ context.Context, model string, messages []NeutralMessage, params map[string]any) (ChatResult, error) {
	if tc := b.decideToolCall(messages, params); tc != nil {
		_, usage := b.render(model, messages, stringParam(params, "reasoning_effort"))
		return ChatResult{
			Content:      "",
			Model:        model,
			Usage:        usage,
			FinishReason: "tool_calls",
			ToolCalls:    []ToolCall{*tc},
		}, nil
	}
	content, usage := b.render(model, messages, stringParam(params, "reasoning_effort"))
	return ChatResult{Content: content, Model: model, Usage: usage, FinishReason: "stop"}, nil
}

func (b *FakeBackend) Embeddings(_ context.Context, model string, inputs []string, params map[string]any) (EmbeddingResult, error) {
	dimensions := intFromAny(params["dimensions"], 16)
	if dimensions <= 0 {
		dimensions = 16
	}
	vectors := make([][]float64, 0, len(inputs))
	inputTokens := 0
	for _, input := range inputs {
		vectors = append(vectors, fakeEmbedding(model, input, dimensions))
		inputTokens += approxTokens(input)
	}
	return EmbeddingResult{
		Model:      model,
		Embeddings: vectors,
		Usage: Usage{
			InputTokens: inputTokens,
			Credits:     0.01,
			APIEndpoint: "/embeddings",
		}.Normalized(),
	}, nil
}

func (b *FakeBackend) ChatStream(ctx context.Context, model string, messages []NeutralMessage, params map[string]any) (<-chan StreamItem, error) {
	ch := make(chan StreamItem)
	go func() {
		defer close(ch)
		if tc := b.decideToolCall(messages, params); tc != nil {
			_, usage := b.render(model, messages, stringParam(params, "reasoning_effort"))
			select {
			case ch <- StreamItem{Kind: "tool_call", ToolCall: *tc, Index: 0}:
			case <-ctx.Done():
				return
			}
			ch <- StreamItem{Kind: "done", Usage: usage, FinishReason: "tool_calls"}
			return
		}
		content, usage := b.render(model, messages, stringParam(params, "reasoning_effort"))
		for _, word := range strings.Split(content, " ") {
			select {
			case ch <- StreamItem{Kind: "delta", Text: word + " "}:
			case <-ctx.Done():
				return
			}
		}
		ch <- StreamItem{Kind: "done", Usage: usage, FinishReason: "stop"}
	}()
	return ch, nil
}

func (b *FakeBackend) Close() error { return nil }

func fakeEmbedding(model, text string, dimensions int) []float64 {
	out := make([]float64, 0, dimensions)
	for counter := 0; len(out) < dimensions; counter++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", model, text, counter)))
		for _, b := range sum {
			out = append(out, float64(int(b)-128)/128.0)
			if len(out) == dimensions {
				break
			}
		}
	}
	return out
}

func (b *FakeBackend) decideToolCall(messages []NeutralMessage, params map[string]any) *ToolCall {
	tools, ok := params["tools"].([]map[string]any)
	if !ok || len(tools) == 0 {
		return nil
	}
	if choice, ok := params["tool_choice"].(string); ok && choice == "none" {
		return nil
	}
	for _, message := range messages {
		if message.Role == "tool" {
			return nil
		}
	}
	target := tools[0]
	if forced := forcedToolName(params["tool_choice"]); forced != "" {
		for _, tool := range tools {
			if tool["name"] == forced {
				target = tool
				break
			}
		}
	}
	lastUser := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			lastUser = messages[i].Content
			break
		}
	}
	name, _ := target["name"].(string)
	if name == "" {
		name = "tool"
	}
	kind, _ := target["type"].(string)
	if kind == "" {
		kind = "function"
	}
	sum := sha256.Sum256([]byte(name + lastUser))
	args, _ := json.Marshal(map[string]string{"query": strings.TrimSpace(lastUser)})
	return &ToolCall{
		ID:        "call_" + hex.EncodeToString(sum[:])[:8],
		Name:      name,
		Arguments: string(args),
		Kind:      kind,
	}
}

func (b *FakeBackend) render(model string, messages []NeutralMessage, effort string) (string, Usage) {
	lastUser := ""
	toolResult := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if lastUser == "" && messages[i].Role == "user" {
			lastUser = messages[i].Content
		}
		if toolResult == "" && messages[i].Role == "tool" {
			toolResult = messages[i].Content
		}
	}
	prompt := renderPrompt(messages)
	sum := sha256.Sum256([]byte(prompt))
	digest := hex.EncodeToString(sum[:])[:8]
	effortTag := ""
	if effort != "" {
		effortTag = " effort=" + effort
	}
	body := strings.TrimSpace(lastUser)
	if toolResult != "" {
		body = "used tool result: " + strings.TrimSpace(toolResult)
	}
	content := strings.TrimSpace(fmt.Sprintf("[fake:%s/%s%s] echo: %s (ctx=%s)", b.accountID, model, effortTag, body, digest))
	usage := Usage{
		InputTokens:    approxTokens(prompt),
		OutputTokens:   approxTokens(content),
		Credits:        0.01,
		APIEndpoint:    "/chat/completions",
		ProviderCallID: "fake-" + digest,
		DurationMS:     1,
	}.Normalized()
	return content, usage
}

// approxTokens estimates a token count without pulling in a tokenizer.
//
// Splitting on whitespace collapses any script that does not delimit words with
// spaces — Chinese, Japanese, Korean, Thai — to a single token regardless of
// length, which silently zeroed out usage accounting and max_tokens for those
// languages. Count wide scripts per rune and everything else at the usual
// four-characters-per-token rule of thumb.
func approxTokens(text string) int {
	if strings.TrimSpace(text) == "" {
		return 0
	}
	wide, narrow := 0, 0
	for _, r := range text {
		if isWideScriptRune(r) {
			wide++
		} else {
			narrow++
		}
	}
	return max(1, wide+(narrow+3)/4)
}

// isWideScriptRune reports whether r belongs to a script whose characters are
// roughly one token each rather than part of a space-delimited word.
func isWideScriptRune(r rune) bool {
	switch {
	case r >= 0x3040 && r <= 0x30FF: // hiragana, katakana
		return true
	case r >= 0x3400 && r <= 0x4DBF: // CJK extension A
		return true
	case r >= 0x4E00 && r <= 0x9FFF: // CJK unified ideographs
		return true
	case r >= 0xAC00 && r <= 0xD7AF: // hangul syllables
		return true
	case r >= 0xF900 && r <= 0xFAFF: // CJK compatibility ideographs
		return true
	case r >= 0x0E00 && r <= 0x0E7F: // thai
		return true
	case r >= 0xFF00 && r <= 0xFF65: // fullwidth forms
		return true
	case r >= 0x3000 && r <= 0x303F: // CJK punctuation
		return true
	}
	return false
}

func renderPrompt(messages []NeutralMessage) string {
	systems := []string{}
	convo := []NeutralMessage{}
	for _, message := range messages {
		if message.Role == "system" {
			systems = append(systems, message.Content)
		} else {
			convo = append(convo, message)
		}
	}
	lines := []string{}
	if len(systems) > 0 {
		lines = append(lines, "[system]\n"+strings.Join(systems, "\n"))
	}
	for _, message := range convo {
		if message.Role == "tool" {
			lines = append(lines, fmt.Sprintf("[tool result for %s]\n%s", message.ToolCallID, message.Content))
			continue
		}
		block := fmt.Sprintf("[%s]\n%s", firstNonEmpty(message.Role, "user"), message.Content)
		for _, tc := range message.ToolCalls {
			fn := tc
			if nested, ok := tc["function"].(map[string]any); ok {
				fn = nested
			}
			block += fmt.Sprintf("\n(called tool %v [%v] with arguments %v)", fn["name"], tc["id"], firstAny(fn["arguments"], tc["arguments"]))
		}
		lines = append(lines, block)
	}
	lines = append(lines, "[assistant]")
	return strings.Join(lines, "\n\n")
}

func stringParam(params map[string]any, key string) string {
	if v, ok := params[key].(string); ok {
		return v
	}
	return ""
}

func forcedToolName(choice any) string {
	obj, ok := choice.(map[string]any)
	if !ok {
		return ""
	}
	if name, ok := obj["name"].(string); ok {
		return name
	}
	if fn, ok := obj["function"].(map[string]any); ok {
		if name, ok := fn["name"].(string); ok {
			return name
		}
	}
	return ""
}
