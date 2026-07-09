package gateway

import (
	"context"
	"fmt"
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
	idleCh := make(chan struct{}, 1)
	toolCh := make(chan struct{}, 1)
	errCh := make(chan error, 1)
	signal := func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}

	unsubscribe := session.On(func(event sdk.SessionEvent) {
		items, terminal := reducer.reduce(event)
		for _, item := range items {
			if item.Err != nil {
				select {
				case errCh <- item.Err:
				default:
				}
				return
			}
			if item.Kind == "tool_call" {
				signal(toolCh)
			}
			if !emitStreamItem(ctx, out, item) {
				return
			}
		}
		if terminal {
			signal(idleCh)
		}
	})
	defer unsubscribe()

	if _, err := session.Send(ctx, sdk.MessageOptions{Prompt: prompt}); err != nil {
		emitStreamItem(context.Background(), out, StreamItem{Kind: "error", Err: fmt.Errorf("send sdk session prompt: %w", err)})
		return
	}

	select {
	case <-idleCh:
		return
	case <-toolCh:
		settleCtx, settleCancel := context.WithTimeout(ctx, 5*time.Second)
		waitForSDKToolSettle(settleCtx, toolCh)
		settleCancel()
		abortCtx, abortCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = session.Abort(abortCtx)
		abortCancel()
		select {
		case <-idleCh:
			return
		case err := <-errCh:
			emitStreamItem(ctx, out, StreamItem{Kind: "error", Err: err})
			return
		case <-ctx.Done():
			emitStreamItem(context.Background(), out, StreamItem{Kind: "error", Err: fmt.Errorf("stream sdk session: %w", ctx.Err())})
			return
		}
	case err := <-errCh:
		emitStreamItem(ctx, out, StreamItem{Kind: "error", Err: err})
		return
	case <-ctx.Done():
		emitStreamItem(context.Background(), out, StreamItem{Kind: "error", Err: fmt.Errorf("stream sdk session: %w", ctx.Err())})
		return
	}
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
		return []StreamItem{{Kind: "error", Err: fmt.Errorf("sdk session error: %s", data.Message)}}, false
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
