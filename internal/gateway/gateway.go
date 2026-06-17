package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Gateway struct {
	Settings      Settings
	Pool          *PoolManager
	Registry      *ModelRegistry
	Router        *Router
	Cache         *CacheLayer
	UsageStore    *UsageStore
	Metrics       *Metrics
	Meter         *Meter
	LoginManager  *LoginManager
	Authenticator *Authenticator
	Responses     *ResponseStore

	refreshCancel context.CancelFunc
}

func NewGateway(settings Settings) (*Gateway, error) {
	pool, err := NewPoolManager(settings)
	if err != nil {
		return nil, err
	}
	authenticator, err := NewAuthenticator(context.Background(), settings)
	if err != nil {
		return nil, err
	}
	registry := NewModelRegistry(pool)
	store, err := NewUsageStore(settings.Usage.SQLitePath)
	if err != nil {
		return nil, err
	}
	metrics := &Metrics{}
	gw := &Gateway{
		Settings:      settings,
		Pool:          pool,
		Registry:      registry,
		Router:        NewRouter(pool, registry, settings.Routes),
		Cache:         NewCacheLayer(settings.Cache),
		UsageStore:    store,
		Metrics:       metrics,
		Meter:         NewMeter(store, metrics),
		Authenticator: authenticator,
		Responses:     NewResponseStore(),
	}
	gw.LoginManager = NewLoginManager(settings, pool)
	return gw, nil
}

func (g *Gateway) Startup(ctx context.Context) error {
	if err := g.Pool.Start(ctx); err != nil {
		return err
	}
	g.Registry.Refresh(ctx)
	refreshCtx, cancel := context.WithCancel(context.Background())
	g.refreshCancel = cancel
	go g.Registry.RefreshLoop(refreshCtx, g.Settings.ModelRefreshSeconds)
	return nil
}

func (g *Gateway) Shutdown() {
	if g.refreshCancel != nil {
		g.refreshCancel()
	}
	g.Pool.Close()
	_ = g.UsageStore.Close()
}

func (g *Gateway) SetModelAliases(aliases map[string]string) error {
	clean := sanitizeAliases(aliases)
	if err := SaveModelAliases(g.Settings.ModelMapPath, clean); err != nil {
		return err
	}
	g.Settings.ModelAliases = clean
	return nil
}

func (g *Gateway) Prepare(req ChatCompletionRequest, principal Principal, control string) (Plan, error) {
	requestedModel := req.Model
	model := g.Settings.ResolveModelAlias(requestedModel)
	messages := req.NeutralMessages()
	params := req.SamplingParams()
	endpoint := g.Router.PickEndpoint(model, req.EndpointPreferences())
	params[internalEndpointParam] = endpoint
	effort := stringParam(params, "reasoning_effort")
	if effort == "" {
		effort = resolveReasoningEffort(model, g.Settings.ReasoningEfforts)
	}
	if effort != "" {
		if !ValidReasoningEfforts[effort] {
			return Plan{}, fmt.Errorf("invalid reasoning_effort %q; valid: none, low, medium, high, xhigh, max", effort)
		}
		params["reasoning_effort"] = effort
	} else {
		params["reasoning_effort"] = nil
	}
	contextTier := stringParam(params, "context_tier")
	if contextTier == "" {
		contextTier = resolveContextTier(model, g.Settings.ContextTiers)
	}
	if contextTier != "" {
		if !ValidContextTiers[contextTier] {
			return Plan{}, fmt.Errorf("invalid context_tier %q; valid: default, long_context", contextTier)
		}
		params["context_tier"] = contextTier
	} else {
		params["context_tier"] = nil
	}
	includeUsage := false
	if v, ok := req.StreamOptions["include_usage"].(bool); ok {
		includeUsage = v
	}
	namespace := principal.CacheNamespace()
	key := g.Cache.MakeKey(namespace, model, messages, params)
	if rec := g.Cache.Lookup(key, control, model); rec != nil {
		return Plan{Model: model, ResponseModel: requestedModel, Endpoint: endpoint, Messages: messages, Params: params, CacheKey: key, Control: control, Namespace: namespace, CacheHit: rec, IncludeUsage: includeUsage}, nil
	}
	account, err := g.Router.Select(model, endpoint)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Model: model, ResponseModel: requestedModel, Endpoint: endpoint, Messages: messages, Params: params, CacheKey: key, Control: control, Namespace: namespace, Account: account, IncludeUsage: includeUsage}, nil
}

func (g *Gateway) PrepareWithWait(ctx context.Context, req ChatCompletionRequest, principal Principal, control string) (Plan, error) {
	deadline := time.Now().Add(time.Duration(g.Settings.RouteBusyWaitSeconds * float64(time.Second)))
	for {
		plan, err := g.Prepare(req, principal, control)
		if err == nil {
			return plan, nil
		}
		var noAccount NoAccountForModel
		if errors.As(err, &noAccount) {
			return Plan{}, err
		}
		var rateLimit RateLimitExceeded
		if errors.As(err, &rateLimit) {
			return Plan{}, err
		}
		if time.Now().After(deadline) {
			return Plan{}, err
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return Plan{}, ctx.Err()
		}
	}
}

func (g *Gateway) CompleteResult(ctx context.Context, plan Plan) (ChatResult, string, error) {
	if plan.CacheHit != nil {
		rec := plan.CacheHit
		g.Meter.Observe(nil, rec.Model, rec.Usage, "hit", true, "")
		return ChatResult{Content: rec.Content, Model: plan.ResponseModel, Usage: rec.Usage, FinishReason: rec.FinishReason, ToolCalls: rec.ToolCalls, ResponsesOutput: rec.ResponsesOutput, AnthropicContent: rec.AnthropicContent}, "cache", nil
	}
	exclude := map[string]bool{}
	current := plan.Account
	var lastErr error
	for current != nil {
		result, err := current.Backend.Chat(ctx, plan.Model, plan.Messages, plan.Params)
		if err != nil {
			current.RecordFailure(err.Error())
			current.Release()
			if isNonRetryableBackendError(err) {
				return ChatResult{}, "", err
			}
			exclude[current.ID()] = true
			lastErr = err
			current = g.Router.SelectExcluding(plan.Model, exclude, plan.Endpoint)
			continue
		}
		current.RecordSuccess()
		current.Release()
		result.Usage = result.Usage.Normalized()
		g.Cache.Store(plan.CacheKey, CacheRecord{
			Content:          result.Content,
			Model:            result.Model,
			FinishReason:     result.FinishReason,
			Usage:            result.Usage,
			ToolCalls:        result.ToolCalls,
			ResponsesOutput:  result.ResponsesOutput,
			AnthropicContent: result.AnthropicContent,
		}, plan.Control)
		id := current.ID()
		g.Meter.Observe(&id, plan.Model, result.Usage, "miss", true, "")
		result.Model = plan.ResponseModel
		return result, id, nil
	}
	if lastErr != nil {
		return ChatResult{}, "", lastErr
	}
	return ChatResult{}, "", RoutingError{message: fmt.Sprintf("no account could serve model %q", plan.Model)}
}

func (g *Gateway) EstimateInputTokens(req ChatCompletionRequest) (int, string) {
	model := g.Settings.ResolveModelAlias(req.Model)
	messages := req.NeutralMessages()
	params := req.SamplingParams()
	parts := []string{renderPrompt(messages)}
	if tools := params["tools"]; tools != nil {
		data, _ := json.Marshal(tools)
		parts = append(parts, string(data))
	}
	return max(1, approxTokens(strings.Join(parts, "\n"))), model
}

func (g *Gateway) CreateEmbeddings(ctx context.Context, requestedModel string, inputs []string, params map[string]any) (EmbeddingResult, string, map[string]string, error) {
	model := g.Settings.ResolveModelAlias(requestedModel)
	account, err := g.Router.Select(model, endpointEmbeddings)
	if err != nil {
		return EmbeddingResult{}, "", nil, err
	}
	defer account.Release()
	start := time.Now()
	result, err := account.Backend.Embeddings(ctx, model, inputs, params)
	if err != nil {
		if errors.Is(err, ErrEmbeddingsUnsupported) {
			return EmbeddingResult{}, "", nil, err
		}
		account.RecordFailure(err.Error())
		return EmbeddingResult{}, "", nil, err
	}
	account.RecordSuccess()
	result.Usage = result.Usage.Normalized()
	id := account.ID()
	g.Meter.Observe(&id, model, result.Usage, "miss", true, "")
	result.Model = requestedModel
	headers := map[string]string{
		"x-ghcp-account":    id,
		"x-ghcp-cache":      "bypass",
		"x-ghcp-model":      requestedModel,
		"openai-model":      requestedModel,
		"x-openai-model":    requestedModel,
		"request-id":        newID("req"),
		"x-ghcp-latency-ms": fmt.Sprintf("%d", time.Since(start).Milliseconds()),
	}
	if requestedModel != model {
		headers["x-ghcp-upstream-model"] = model
	}
	return result, id, headers, nil
}

func (g *Gateway) StreamChat(ctx context.Context, plan Plan) <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		completionID := newID("chatcmpl")
		if plan.CacheHit != nil {
			rec := plan.CacheHit
			if !emitSSE(ctx, out, dataSSE(chunkResponse(completionID, plan.ResponseModel, map[string]any{"role": "assistant"}, nil))) {
				return
			}
			for _, piece := range charChunks(rec.Content, 24) {
				if !emitSSE(ctx, out, dataSSE(chunkResponse(completionID, plan.ResponseModel, map[string]any{"content": piece}, nil))) {
					return
				}
			}
			for i, tc := range rec.ToolCalls {
				if !emitSSE(ctx, out, dataSSE(chunkResponse(completionID, plan.ResponseModel, map[string]any{"tool_calls": []any{chatToolCallDelta(i, tc)}}, nil))) {
					return
				}
			}
			if !emitSSE(ctx, out, dataSSE(chunkResponse(completionID, plan.ResponseModel, map[string]any{}, rec.FinishReason))) {
				return
			}
			if plan.IncludeUsage {
				if !emitSSE(ctx, out, dataSSE(usageChunkResponse(completionID, plan.ResponseModel, rec.Usage))) {
					return
				}
			}
			if !emitSSE(ctx, out, "data: [DONE]\n\n") {
				return
			}
			g.Meter.Observe(nil, rec.Model, rec.Usage, "hit", true, "")
			return
		}
		account := plan.Account
		if account == nil {
			emitSSE(ctx, out, dataSSE(map[string]any{"error": map[string]any{"message": "no account selected", "type": "routing_error"}}))
			return
		}
		defer account.Release()
		content := ""
		toolCalls := []ToolCall{}
		usage := Usage{}
		finish := "stop"
		if !emitSSE(ctx, out, dataSSE(chunkResponse(completionID, plan.ResponseModel, map[string]any{"role": "assistant"}, nil))) {
			return
		}
		stream, err := account.Backend.ChatStream(ctx, plan.Model, plan.Messages, plan.Params)
		if err != nil {
			account.RecordFailure(err.Error())
			emitSSE(ctx, out, dataSSE(map[string]any{"error": map[string]any{"message": err.Error(), "type": "backend_error"}}))
			id := account.ID()
			g.Meter.Observe(&id, plan.Model, usage, "miss", false, "backend_error")
			return
		}
		for item := range stream {
			if item.Err != nil {
				account.RecordFailure(item.Err.Error())
				emitSSE(ctx, out, dataSSE(map[string]any{"error": map[string]any{"message": item.Err.Error(), "type": "backend_error"}}))
				id := account.ID()
				g.Meter.Observe(&id, plan.Model, usage, "miss", false, "backend_error")
				return
			}
			switch item.Kind {
			case "delta":
				content += item.Text
				if !emitSSE(ctx, out, dataSSE(chunkResponse(completionID, plan.ResponseModel, map[string]any{"content": item.Text}, nil))) {
					return
				}
			case "tool_call":
				tc := item.ToolCall.normalized()
				toolCalls = append(toolCalls, tc)
				if !emitSSE(ctx, out, dataSSE(chunkResponse(completionID, plan.ResponseModel, map[string]any{"tool_calls": []any{chatToolCallDelta(item.Index, tc)}}, nil))) {
					return
				}
			case "done":
				usage = item.Usage.Normalized()
				finish = firstNonEmpty(item.FinishReason, finish)
			}
		}
		if len(toolCalls) > 0 {
			finish = "tool_calls"
		}
		if !emitSSE(ctx, out, dataSSE(chunkResponse(completionID, plan.ResponseModel, map[string]any{}, finish))) {
			return
		}
		if plan.IncludeUsage {
			if !emitSSE(ctx, out, dataSSE(usageChunkResponse(completionID, plan.ResponseModel, usage))) {
				return
			}
		}
		if !emitSSE(ctx, out, "data: [DONE]\n\n") {
			return
		}
		account.RecordSuccess()
		g.Cache.Store(plan.CacheKey, CacheRecord{Content: content, Model: plan.Model, FinishReason: finish, Usage: usage, ToolCalls: toolCalls}, plan.Control)
		id := account.ID()
		g.Meter.Observe(&id, plan.Model, usage, "miss", true, "")
	}()
	return out
}

func (g *Gateway) StreamResponses(ctx context.Context, plan Plan) <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		responseID := newID("resp")
		seq := 0
		next := func() int { seq++; return seq }
		options, _ := plan.Params["response_options"].(map[string]any)
		if !emitSSE(ctx, out, namedSSE(responseCreated(responseID, plan.ResponseModel, next(), options))) {
			return
		}
		if !emitSSE(ctx, out, namedSSE(responseInProgress(responseID, plan.ResponseModel, next(), options))) {
			return
		}
		webSearch := responseOptionsHaveWebSearchTool(options)
		if plan.CacheHit != nil {
			rec := plan.CacheHit
			events, output := responseReplayEvents(rec.Content, rec.ToolCalls, webSearch && len(rec.ToolCalls) == 0, &seq)
			for _, event := range events {
				if !emitSSE(ctx, out, namedSSE(event)) {
					return
				}
			}
			finish := rec.FinishReason
			if len(rec.ToolCalls) > 0 {
				finish = "tool_calls"
			}
			completed := responseCompleted(responseID, plan.ResponseModel, output, rec.Content, finish, rec.Usage, next(), options, len(rec.ToolCalls) == 0)
			if !emitSSE(ctx, out, namedSSE(completed)) {
				return
			}
			if response, ok := completed["response"].(map[string]any); ok {
				g.Responses.Store(response, responseInputItemsFromRaw(rawResponsesPayload(plan.Params)))
			}
			g.Meter.Observe(nil, rec.Model, rec.Usage, "hit", true, "")
			return
		}
		account := plan.Account
		if account == nil {
			emitSSE(ctx, out, namedSSE(map[string]any{"type": "response.failed", "sequence_number": next(), "response": responseObject(responseID, plan.ResponseModel, "failed", []any{}, "", "", nil, options, nil), "error": map[string]any{"message": "no account selected"}}))
			return
		}
		defer account.Release()
		content, toolCalls, usage, finish, err := consumeStream(ctx, account, plan)
		if err != nil {
			account.RecordFailure(err.Error())
			emitSSE(ctx, out, namedSSE(map[string]any{"type": "response.failed", "sequence_number": next(), "response": responseObject(responseID, plan.ResponseModel, "failed", []any{}, "", "", nil, options, nil), "error": map[string]any{"message": err.Error()}}))
			id := account.ID()
			g.Meter.Observe(&id, plan.Model, usage, "miss", false, "backend_error")
			return
		}
		events, output := responseReplayEvents(content, toolCalls, webSearch && len(toolCalls) == 0, &seq)
		for _, event := range events {
			if !emitSSE(ctx, out, namedSSE(event)) {
				return
			}
		}
		if len(toolCalls) > 0 {
			finish = "tool_calls"
		}
		completed := responseCompleted(responseID, plan.ResponseModel, output, content, finish, usage, next(), options, len(toolCalls) == 0)
		if !emitSSE(ctx, out, namedSSE(completed)) {
			return
		}
		if response, ok := completed["response"].(map[string]any); ok {
			g.Responses.Store(response, responseInputItemsFromRaw(rawResponsesPayload(plan.Params)))
		}
		account.RecordSuccess()
		g.Cache.Store(plan.CacheKey, CacheRecord{Content: content, Model: plan.Model, FinishReason: finish, Usage: usage, ToolCalls: toolCalls}, plan.Control)
		id := account.ID()
		g.Meter.Observe(&id, plan.Model, usage, "miss", true, "")
	}()
	return out
}

func (g *Gateway) StreamAnthropic(ctx context.Context, plan Plan) <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		messageID := newID("msg")
		options, _ := plan.Params["response_options"].(map[string]any)
		if plan.CacheHit != nil {
			rec := plan.CacheHit
			if !emitSSE(ctx, out, namedSSE(anthropicMessageStart(messageID, plan.ResponseModel, rec.Usage, options))) {
				return
			}
			for _, event := range anthropicReplayEvents(rec.Content, rec.ToolCalls) {
				if !emitSSE(ctx, out, namedSSE(event)) {
					return
				}
			}
			finish := rec.FinishReason
			if len(rec.ToolCalls) > 0 {
				finish = "tool_calls"
			}
			if !emitSSE(ctx, out, namedSSE(anthropicMessageDelta(finish, rec.Usage, options))) {
				return
			}
			if !emitSSE(ctx, out, namedSSE(map[string]any{"type": "message_stop"})) {
				return
			}
			g.Meter.Observe(nil, rec.Model, rec.Usage, "hit", true, "")
			return
		}
		account := plan.Account
		if account == nil {
			emitSSE(ctx, out, namedSSE(map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "no account selected"}}))
			return
		}
		defer account.Release()
		if !emitSSE(ctx, out, namedSSE(anthropicMessageStart(messageID, plan.ResponseModel, Usage{}, options))) {
			return
		}
		content, toolCalls, usage, finish, err := consumeStream(ctx, account, plan)
		if err != nil {
			account.RecordFailure(err.Error())
			emitSSE(ctx, out, namedSSE(map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": err.Error()}}))
			id := account.ID()
			g.Meter.Observe(&id, plan.Model, usage, "miss", false, "backend_error")
			return
		}
		for _, event := range anthropicReplayEvents(content, toolCalls) {
			if !emitSSE(ctx, out, namedSSE(event)) {
				return
			}
		}
		if len(toolCalls) > 0 {
			finish = "tool_calls"
		}
		if !emitSSE(ctx, out, namedSSE(anthropicMessageDelta(finish, usage, options))) {
			return
		}
		if !emitSSE(ctx, out, namedSSE(map[string]any{"type": "message_stop"})) {
			return
		}
		account.RecordSuccess()
		g.Cache.Store(plan.CacheKey, CacheRecord{Content: content, Model: plan.Model, FinishReason: finish, Usage: usage, ToolCalls: toolCalls}, plan.Control)
		id := account.ID()
		g.Meter.Observe(&id, plan.Model, usage, "miss", true, "")
	}()
	return out
}

func consumeStream(ctx context.Context, account *Account, plan Plan) (string, []ToolCall, Usage, string, error) {
	stream, err := account.Backend.ChatStream(ctx, plan.Model, plan.Messages, plan.Params)
	if err != nil {
		return "", nil, Usage{}, "stop", err
	}
	content := ""
	toolCalls := []ToolCall{}
	usage := Usage{}
	finish := "stop"
	for item := range stream {
		if item.Err != nil {
			return content, toolCalls, usage, finish, item.Err
		}
		switch item.Kind {
		case "delta":
			content += item.Text
		case "tool_call":
			toolCalls = append(toolCalls, item.ToolCall.normalized())
		case "done":
			usage = item.Usage.Normalized()
			finish = firstNonEmpty(item.FinishReason, finish)
		}
	}
	return content, toolCalls, usage, finish, nil
}

func dataSSE(obj any) string {
	data, _ := json.Marshal(obj)
	return "data: " + string(data) + "\n\n"
}

func namedSSE(obj map[string]any) string {
	data, _ := json.Marshal(obj)
	return "event: " + stringFromAny(obj["type"]) + "\ndata: " + string(data) + "\n\n"
}

func emitSSE(ctx context.Context, out chan<- string, chunk string) bool {
	select {
	case out <- chunk:
		return true
	case <-ctx.Done():
		return false
	}
}

func chatToolCallDelta(index int, tc ToolCall) map[string]any {
	return map[string]any{"index": index, "id": tc.ID, "type": "function", "function": map[string]any{"name": tc.Name, "arguments": tc.Arguments}}
}

func responseReplayEvents(content string, toolCalls []ToolCall, webSearch bool, seq *int) ([]map[string]any, []any) {
	next := func() int { *seq++; return *seq }
	events := []map[string]any{}
	output := []any{}
	outputIndex := 0
	if webSearch {
		itemID := newID("ws")
		searchItem := map[string]any{"id": itemID, "type": "web_search_call", "status": "completed"}
		events = append(events,
			map[string]any{"type": "response.output_item.added", "sequence_number": next(), "output_index": outputIndex, "item": map[string]any{"id": itemID, "type": "web_search_call", "status": "in_progress"}},
			map[string]any{"type": "response.web_search_call.in_progress", "sequence_number": next(), "output_index": outputIndex, "item_id": itemID},
			map[string]any{"type": "response.web_search_call.searching", "sequence_number": next(), "output_index": outputIndex, "item_id": itemID},
			map[string]any{"type": "response.web_search_call.completed", "sequence_number": next(), "output_index": outputIndex, "item_id": itemID},
			map[string]any{"type": "response.output_item.done", "sequence_number": next(), "output_index": outputIndex, "item": searchItem},
		)
		output = append(output, searchItem)
		outputIndex++
	}
	if content != "" {
		itemID := newID("msg")
		events = append(events,
			map[string]any{"type": "response.output_item.added", "sequence_number": next(), "output_index": outputIndex, "item": map[string]any{"id": itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}, "phase": "final_answer"}},
			map[string]any{"type": "response.content_part.added", "sequence_number": next(), "item_id": itemID, "output_index": outputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
		)
		for _, piece := range charChunks(content, 24) {
			events = append(events, map[string]any{"type": "response.output_text.delta", "sequence_number": next(), "item_id": itemID, "output_index": outputIndex, "content_index": 0, "delta": piece})
		}
		events = append(events,
			map[string]any{"type": "response.output_text.done", "sequence_number": next(), "item_id": itemID, "output_index": outputIndex, "content_index": 0, "text": content},
			map[string]any{"type": "response.content_part.done", "sequence_number": next(), "item_id": itemID, "output_index": outputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": content, "annotations": []any{}}},
			map[string]any{"type": "response.output_item.done", "sequence_number": next(), "output_index": outputIndex, "item": responseMessageItem(itemID, content)},
		)
		output = append(output, responseMessageItem(itemID, content))
		outputIndex++
	}
	for _, tc := range toolCalls {
		itemID := newID("msg")
		if tc.Kind == "custom" {
			events = append(events,
				map[string]any{"type": "response.output_item.added", "sequence_number": next(), "output_index": outputIndex, "item": map[string]any{"id": itemID, "type": "custom_tool_call", "status": "in_progress", "call_id": tc.ID, "name": tc.Name, "input": ""}},
				map[string]any{"type": "response.custom_tool_call_input.delta", "sequence_number": next(), "item_id": itemID, "call_id": tc.ID, "output_index": outputIndex, "delta": tc.Arguments},
				map[string]any{"type": "response.custom_tool_call_input.done", "sequence_number": next(), "item_id": itemID, "call_id": tc.ID, "output_index": outputIndex, "input": tc.Arguments},
				map[string]any{"type": "response.output_item.done", "sequence_number": next(), "output_index": outputIndex, "item": map[string]any{"id": itemID, "type": "custom_tool_call", "status": "completed", "call_id": tc.ID, "name": tc.Name, "input": tc.Arguments}},
			)
			output = append(output, map[string]any{"id": itemID, "type": "custom_tool_call", "status": "completed", "call_id": tc.ID, "name": tc.Name, "input": tc.Arguments})
		} else {
			events = append(events,
				map[string]any{"type": "response.output_item.added", "sequence_number": next(), "output_index": outputIndex, "item": map[string]any{"id": itemID, "type": "function_call", "status": "in_progress", "call_id": tc.ID, "name": tc.Name, "arguments": ""}},
				map[string]any{"type": "response.function_call_arguments.delta", "sequence_number": next(), "item_id": itemID, "output_index": outputIndex, "delta": tc.Arguments},
				map[string]any{"type": "response.function_call_arguments.done", "sequence_number": next(), "item_id": itemID, "output_index": outputIndex, "arguments": tc.Arguments},
				map[string]any{"type": "response.output_item.done", "sequence_number": next(), "output_index": outputIndex, "item": map[string]any{"id": itemID, "type": "function_call", "status": "completed", "call_id": tc.ID, "name": tc.Name, "arguments": tc.Arguments}},
			)
			output = append(output, map[string]any{"id": itemID, "type": "function_call", "status": "completed", "call_id": tc.ID, "name": tc.Name, "arguments": tc.Arguments})
		}
		outputIndex++
	}
	if len(output) == 0 {
		itemID := newID("msg")
		events = append(events,
			map[string]any{"type": "response.output_item.added", "sequence_number": next(), "output_index": 0, "item": map[string]any{"id": itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}, "phase": "final_answer"}},
			map[string]any{"type": "response.content_part.added", "sequence_number": next(), "item_id": itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
			map[string]any{"type": "response.output_text.done", "sequence_number": next(), "item_id": itemID, "output_index": 0, "content_index": 0, "text": ""},
			map[string]any{"type": "response.content_part.done", "sequence_number": next(), "item_id": itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
			map[string]any{"type": "response.output_item.done", "sequence_number": next(), "output_index": 0, "item": responseMessageItem(itemID, "")},
		)
		output = append(output, responseMessageItem(itemID, ""))
	}
	return events, output
}

func anthropicReplayEvents(content string, toolCalls []ToolCall) []map[string]any {
	events := []map[string]any{}
	index := 0
	if content != "" {
		events = append(events, map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "text", "text": ""}})
		for _, piece := range charChunks(content, 24) {
			events = append(events, map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": piece}})
		}
		events = append(events, map[string]any{"type": "content_block_stop", "index": index})
		index++
	}
	for _, tc := range toolCalls {
		events = append(events,
			map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": map[string]any{}}},
			map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Arguments}},
			map[string]any{"type": "content_block_stop", "index": index},
		)
		index++
	}
	if index == 0 {
		events = append(events, map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}, map[string]any{"type": "content_block_stop", "index": 0})
	}
	return events
}
