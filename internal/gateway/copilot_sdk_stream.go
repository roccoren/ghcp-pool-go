package gateway

import (
	"context"
	"fmt"
	"sync"
	"time"

	sdk "github.com/github/copilot-sdk/go"
)

func reduceSDKStreamEvents(events []sdk.SessionEvent, out chan<- StreamItem) {
	reducer := newSDKStreamReducer("", "")
	for _, event := range events {
		items, terminal := reducer.reduce(event)
		for _, item := range items {
			out <- item
		}
		if terminal {
			return
		}
	}
}

func streamSDKSession(ctx context.Context, session *sdk.Session, prompt, endpoint string, out chan<- StreamItem) {
	reducer := newSDKStreamReducer(prompt, endpoint)
	var mu sync.Mutex
	terminated := false
	toolCh := make(chan struct{}, 1)
	doneCh := make(chan struct{})
	var doneOnce sync.Once
	signalDone := func() {
		doneOnce.Do(func() { close(doneCh) })
	}
	signal := func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}

	handleEvent := func(event sdk.SessionEvent) {
		mu.Lock()
		if terminated {
			mu.Unlock()
			return
		}
		items, terminal := reducer.reduce(event)
		if terminal {
			terminated = true
		}
		mu.Unlock()

		hasToolCall := false
		for _, item := range items {
			if item.Kind == "tool_call" {
				hasToolCall = true
			}
			if !emitStreamItem(ctx, out, item) {
				return
			}
		}
		if hasToolCall {
			signal(toolCh)
		}
		if terminal {
			signalDone()
		}
	}

	go func() {
		var timer *time.Timer
		var timerCh <-chan time.Time
		stopTimer := func() {
			if timer == nil {
				return
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		defer stopTimer()

		for {
			select {
			case <-toolCh:
				if timer == nil {
					timer = time.NewTimer(50 * time.Millisecond)
					timerCh = timer.C
					continue
				}
				stopTimer()
				timer.Reset(50 * time.Millisecond)
			case <-timerCh:
				abortCtx, abortCancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = session.Abort(abortCtx)
				abortCancel()
				return
			case <-doneCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	unsubscribe := session.On(handleEvent)
	defer unsubscribe()

	if _, err := session.Send(ctx, sdk.MessageOptions{Prompt: prompt}); err != nil {
		handleEvent(sdk.SessionEvent{Data: &sdk.SessionErrorData{Message: fmt.Sprintf("send sdk session prompt: %v", err), ErrorType: "query"}})
		return
	}

	select {
	case <-doneCh:
		return
	case <-ctx.Done():
		return
	}
}

func applyStreamOutputConstraints(ctx context.Context, in <-chan StreamItem, params map[string]any) <-chan StreamItem {
	out := make(chan StreamItem)
	go func() {
		defer close(out)

		accumulated := ""
		emitted := ""
		toolSeen := false
		// Carries the input side and any upstream counts, so a locally triggered
		// terminal frame does not report output tokens alone.
		seen := Usage{}

		absorb := func(u Usage) {
			if u.InputTokens != 0 || u.TotalTokens != 0 {
				seen = mergeSDKUsage(seen, u)
			}
		}

		// Draining also collects the upstream usage, which arrives on the final
		// event. A locally triggered terminal fires before that event, so without
		// this the input side would be missing from exactly those turns.
		drain := func() {
			for item := range in {
				absorb(item.Usage)
			}
		}

		emitTerminal := func(finish string) bool {
			usage := seen
			usage.OutputTokens = approxTokens(emitted)
			return emitStreamItem(ctx, out, StreamItem{Kind: "done", FinishReason: finish, Usage: usage.Normalized()})
		}

		for item := range in {
			absorb(item.Usage)
			switch item.Kind {
			case "delta":
				if toolSeen {
					if !emitStreamItem(ctx, out, item) {
						drain()
						return
					}
					continue
				}

				accumulated += item.Text
				constrained, stopped := applyStopSequences(accumulated, params["stop"])
				finish := ""
				if stopped {
					finish = "stop"
				}
				if maxTokens := sdkMaxOutputTokens(params); maxTokens > 0 {
					if truncated, ok := truncateApproxTokens(constrained, maxTokens); ok {
						constrained = truncated
						finish = "length"
					}
				}

				newText := ""
				if len(constrained) >= len(emitted) {
					newText = constrained[len(emitted):]
				}
				if newText != "" {
					if !emitStreamItem(ctx, out, StreamItem{Kind: "delta", Text: newText}) {
						drain()
						return
					}
					emitted = constrained
				}
				if finish != "" {
					drain()
					emitTerminal(finish)
					return
				}
			case "tool_call":
				toolSeen = true
				if !emitStreamItem(ctx, out, item) {
					drain()
					return
				}
			case "error":
				_ = emitStreamItem(ctx, out, item)
				return
			case "done":
				if !toolSeen {
					// Only approximate when the upstream count is missing or when the
					// content was trimmed here and no longer matches it.
					if item.Usage.OutputTokens == 0 || emitted != accumulated {
						item.Usage.OutputTokens = approxTokens(emitted)
					}
					item.Usage = item.Usage.Normalized()
				}
				_ = emitStreamItem(ctx, out, item)
				return
			default:
				if !emitStreamItem(ctx, out, item) {
					drain()
					return
				}
			}
		}
	}()
	return out
}

type sdkStreamReducer struct {
	endpoint          string
	usage             Usage
	toolCalls         []ToolCall
	sawAssistantDelta bool
	finishReason      string
}

func newSDKStreamReducer(prompt, endpoint string) *sdkStreamReducer {
	return &sdkStreamReducer{
		endpoint:     endpoint,
		usage:        Usage{InputTokens: approxTokens(prompt), APIEndpoint: endpoint}.Normalized(),
		finishReason: "stop",
	}
}

func (r *sdkStreamReducer) reduce(event sdk.SessionEvent) ([]StreamItem, bool) {
	switch data := event.Data.(type) {
	case *sdk.AssistantMessageDeltaData:
		if data.DeltaContent == "" {
			return nil, false
		}
		r.sawAssistantDelta = true
		return []StreamItem{{Kind: "delta", Text: data.DeltaContent}}, false
	case *sdk.AssistantMessageData:
		return r.reduceAssistantMessage(data), false
	case *sdk.ExternalToolRequestedData:
		item, ok := r.reduceExternalTool(data)
		if !ok {
			return nil, false
		}
		return []StreamItem{item}, false
	case *sdk.AssistantUsageData:
		r.usage = mergeSDKUsage(r.usage, sdkUsageFromEvent(data, r.endpoint))
		return nil, false
	case *sdk.SessionIdleData:
		return []StreamItem{{Kind: "done", Usage: r.usage.Normalized(), FinishReason: r.doneReason()}}, true
	case *sdk.SessionErrorData:
		return []StreamItem{{Kind: "error", Err: fmt.Errorf("sdk session error: %s", data.Message)}}, true
	default:
		return nil, false
	}
}

func (r *sdkStreamReducer) reduceAssistantMessage(data *sdk.AssistantMessageData) []StreamItem {
	if data == nil {
		return nil
	}
	items := make([]StreamItem, 0, len(data.ToolRequests)+1)
	if data.OutputTokens != nil {
		r.usage.OutputTokens = int(*data.OutputTokens)
		r.usage = r.usage.Normalized()
	}
	if !r.sawAssistantDelta && data.Content != "" {
		items = append(items, StreamItem{Kind: "delta", Text: data.Content})
		r.sawAssistantDelta = true
	}
	for _, request := range data.ToolRequests {
		if isSDKWebSearchInternalTool(request.Name) || isSDKNativeWebTool(request.Name) {
			continue
		}
		before := len(r.toolCalls)
		r.toolCalls = appendToolCallUnique(r.toolCalls, toolCallFromSDKRequest(request))
		if len(r.toolCalls) == before {
			continue
		}
		r.finishReason = "tool_calls"
		index := len(r.toolCalls) - 1
		items = append(items, StreamItem{Kind: "tool_call", ToolCall: r.toolCalls[index], Index: index})
	}
	return items
}

func (r *sdkStreamReducer) reduceExternalTool(data *sdk.ExternalToolRequestedData) (StreamItem, bool) {
	if data == nil || isSDKWebSearchInternalTool(data.ToolName) || isSDKNativeWebTool(data.ToolName) {
		return StreamItem{}, false
	}
	before := len(r.toolCalls)
	r.toolCalls = appendToolCallUnique(r.toolCalls, toolCallFromSDKExternalTool(data))
	if len(r.toolCalls) == before {
		return StreamItem{}, false
	}
	r.finishReason = "tool_calls"
	index := len(r.toolCalls) - 1
	return StreamItem{Kind: "tool_call", ToolCall: r.toolCalls[index], Index: index}, true
}

func (r *sdkStreamReducer) doneReason() string {
	if len(r.toolCalls) > 0 {
		return "tool_calls"
	}
	if r.finishReason == "" {
		return "stop"
	}
	return r.finishReason
}
