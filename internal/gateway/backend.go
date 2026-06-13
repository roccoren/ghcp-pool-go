package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Backend interface {
	ListModels(context.Context) ([]ModelSpec, error)
	Chat(context.Context, string, []NeutralMessage, map[string]any) (ChatResult, error)
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
		out = append(out, ModelSpec{ID: model, Capabilities: map[string]any{"streaming": true}})
	}
	return out, nil
}

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

type CopilotBackend struct {
	accountID string
	token     string
	homeDir   string
	python    string
	worker    string
}

func NewCopilotBackend(accountID, token, homeDir string) *CopilotBackend {
	return &CopilotBackend{
		accountID: accountID,
		token:     token,
		homeDir:   homeDir,
		python:    firstNonEmpty(os.Getenv("GHCP_PYTHON"), "python3"),
		worker:    copilotWorkerPath(),
	}
}

func (b *CopilotBackend) ListModels(ctx context.Context) ([]ModelSpec, error) {
	var out struct {
		Models []string `json:"models"`
	}
	if err := b.runWorker(ctx, "models", nil, &out); err != nil {
		return nil, err
	}
	specs := make([]ModelSpec, 0, len(out.Models))
	for _, model := range out.Models {
		specs = append(specs, ModelSpec{ID: model, Capabilities: map[string]any{"streaming": true}})
	}
	return specs, nil
}

func (b *CopilotBackend) Chat(ctx context.Context, model string, messages []NeutralMessage, params map[string]any) (ChatResult, error) {
	var out ChatResult
	payload := map[string]any{
		"model":    model,
		"messages": messages,
		"params":   params,
	}
	if err := b.runWorker(ctx, "chat", payload, &out); err != nil {
		return ChatResult{}, err
	}
	out.Usage = out.Usage.Normalized()
	if out.Model == "" {
		out.Model = model
	}
	if out.FinishReason == "" {
		out.FinishReason = "stop"
	}
	return out, nil
}

func (b *CopilotBackend) ChatStream(ctx context.Context, model string, messages []NeutralMessage, params map[string]any) (<-chan StreamItem, error) {
	ch := make(chan StreamItem)
	go func() {
		defer close(ch)
		result, err := b.Chat(ctx, model, messages, params)
		if err != nil {
			ch <- StreamItem{Kind: "error", Err: err}
			return
		}
		for _, tc := range result.ToolCalls {
			ch <- StreamItem{Kind: "tool_call", ToolCall: tc}
		}
		for _, piece := range charChunks(result.Content, 24) {
			ch <- StreamItem{Kind: "delta", Text: piece}
		}
		ch <- StreamItem{Kind: "done", Usage: result.Usage, FinishReason: result.FinishReason}
	}()
	return ch, nil
}

func (b *CopilotBackend) Close() error { return nil }

func (b *CopilotBackend) runWorker(ctx context.Context, op string, payload any, dest any) error {
	req := map[string]any{
		"op":             op,
		"account_id":     b.accountID,
		"github_token":   b.token,
		"base_directory": b.homeDir,
		"payload":        payload,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, b.python, b.worker)
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("copilot worker failed: %s", msg)
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Error  string          `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		return fmt.Errorf("decode copilot worker response: %w: %s", err, strings.TrimSpace(stdout.String()))
	}
	if !envelope.OK {
		return fmt.Errorf("copilot worker error: %s", envelope.Error)
	}
	if dest == nil {
		return nil
	}
	return json.Unmarshal(envelope.Result, dest)
}

func copilotWorkerPath() string {
	if path := os.Getenv("GHCP_COPILOT_WORKER"); path != "" {
		return path
	}
	candidates := []string{
		"/app/copilot_worker.py",
		filepath.Join("internal", "gateway", "copilot_worker.py"),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return candidates[0]
}

func approxTokens(text string) int {
	if strings.TrimSpace(text) == "" {
		return 0
	}
	return max(1, len(strings.Fields(text)))
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
