# Fix: SDK Session Timeout Hang (>10 min)

## Problem

Users reported API calls hanging for >10 minutes when using Claude with web_search tools. The streaming response would emit tool call events but never complete.

## Root Cause

The `sendSDKAndCollect` function had a conditional timeout that only applied when the parent context had NO deadline:

```go
if _, ok := ctx.Deadline(); !ok {
    ctx, cancel = context.WithTimeout(ctx, timeout)
    defer cancel()
}
```

This created several issues:

1. **Inconsistent timeout enforcement**: If the parent context already had a deadline (even an expired one), no new timeout was applied
2. **SDK event loop could hang indefinitely**: If the Copilot SDK never sent `SessionIdleData` after tool completion, the select statement would wait forever
3. **Tool settle logic had no fallback**: The `waitForSDKToolSettle` debounce timer could wait indefinitely if the SDK stopped sending events

## Solution

1. **Always enforce timeout**: Apply a timeout unconditionally, using the minimum of the configured timeout and any remaining parent deadline
2. **Add fallback timeout for tool settle**: Wrap `waitForSDKToolSettle` with a 5-second context timeout
3. **Fix context cancellation leak**: Ensure all cancel functions are called with proper variable names

## Changes

### File: `internal/gateway/copilot_http.go`

**Before:**
```go
func (b *CopilotBackend) sendSDKAndCollect(ctx context.Context, session *sdk.Session, model, prompt string, params map[string]any) (ChatResult, error) {
    if _, ok := ctx.Deadline(); !ok {
        var cancel context.CancelFunc
        timeout := 60 * time.Second
        if b.useSDKCLIWebSearch(params) {
            timeout = 180 * time.Second
        }
        ctx, cancel = context.WithTimeout(ctx, timeout)
        defer cancel()
    }
    // ... rest of function
    
    select {
    case <-toolCh:
        waitForSDKToolSettle(ctx, toolCh)  // No timeout here!
        // ...
    }
}
```

**After:**
```go
func (b *CopilotBackend) sendSDKAndCollect(ctx context.Context, session *sdk.Session, model, prompt string, params map[string]any) (ChatResult, error) {
    // Always enforce maximum timeout, even if parent context has a deadline
    timeout := 60 * time.Second
    if b.useSDKCLIWebSearch(params) {
        timeout = 180 * time.Second
    }
    if deadline, ok := ctx.Deadline(); ok {
        remaining := time.Until(deadline)
        if remaining > 0 && remaining < timeout {
            timeout = remaining
        }
    }
    ctx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()
    // ... rest of function
    
    select {
    case <-toolCh:
        // Add fallback timeout for tool settle to prevent infinite waits
        settleCtx, settleCancel := context.WithTimeout(ctx, 5*time.Second)
        waitForSDKToolSettle(settleCtx, toolCh)
        settleCancel()
        // ...
    }
}
```

## Verification

Build test passed:
```bash
docker build --target build -t ghcp-test-build .
# Build succeeded with new timeout logic
```

## Impact

- **Prevents indefinite hangs**: All SDK requests now have a guaranteed maximum timeout (60s or 180s for web_search)
- **Faster failure detection**: Tool settle phase has a 5s fallback timeout
- **Better resource cleanup**: Context cancellation functions are properly called
- **Maintains compatibility**: Respects parent context deadlines when they're shorter than the configured timeout

## Testing Recommendations

1. **Smoke test with web_search**: Verify that web_search tool calls complete within 180s
2. **Test tool call timeout**: Verify that tool calls abort after 5s settle timeout if SDK hangs
3. **Test context propagation**: Verify that parent context cancellation still works correctly
4. **Load test**: Verify no resource leaks under concurrent requests

## Related

- Issue: Claude API calls hang for >10 minutes with web_search
- Component: Copilot SDK backend streaming (`chatStreamSDK`)
- Timeout config: 60s default, 180s for web_search requests
