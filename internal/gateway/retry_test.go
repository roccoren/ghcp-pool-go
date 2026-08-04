package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

func withFastRetryBackoff(t *testing.T) {
	t.Helper()
	original := transientRetryDelay
	transientRetryDelay = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { transientRetryDelay = original })
}

func overloadErr() error {
	return &CopilotUpstreamError{StatusCode: 529, Body: []byte(`{"error":"Overloaded"}`)}
}

func TestRetryTransientChat(t *testing.T) {
	withFastRetryBackoff(t)
	webSearchParams := map[string]any{"tools": []map[string]any{{"type": "web_search_preview"}}}

	tests := []struct {
		name         string
		params       map[string]any
		errs         []error
		wantAttempts int
		wantContent  string
		wantErr      bool
	}{
		{
			name:         "succeeds without retrying",
			errs:         []error{nil},
			wantAttempts: 1,
			wantContent:  "ok",
		},
		{
			name:         "retries past a transient overload",
			errs:         []error{overloadErr(), nil},
			wantAttempts: 2,
			wantContent:  "ok",
		},
		{
			name:         "gives up after exhausting attempts",
			errs:         []error{overloadErr(), overloadErr(), overloadErr()},
			wantAttempts: 3,
			wantErr:      true,
		},
		{
			name:         "does not retry a non-transient error",
			errs:         []error{errors.New("invalid_request_error: bad model"), nil},
			wantAttempts: 1,
			wantErr:      true,
		},
		{
			name:         "web search requests get a smaller budget",
			params:       webSearchParams,
			errs:         []error{overloadErr(), overloadErr(), nil},
			wantAttempts: 2,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempts := 0
			result, err := retryTransientChat(context.Background(), tt.params, func() (ChatResult, error) {
				i := attempts
				attempts++
				if i < len(tt.errs) && tt.errs[i] != nil {
					return ChatResult{}, tt.errs[i]
				}
				return ChatResult{Content: "ok"}, nil
			})
			if attempts != tt.wantAttempts {
				t.Errorf("attempts = %d, want %d", attempts, tt.wantAttempts)
			}
			if tt.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if err == nil && result.Content != tt.wantContent {
				t.Errorf("content = %q, want %q", result.Content, tt.wantContent)
			}
		})
	}
}

func TestRetryTransientChatStopsOnContextCancel(t *testing.T) {
	original := transientRetryDelay
	transientRetryDelay = func(int) time.Duration { return time.Hour }
	t.Cleanup(func() { transientRetryDelay = original })

	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()

	_, err := retryTransientChat(ctx, nil, func() (ChatResult, error) {
		attempts++
		return ChatResult{}, overloadErr()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1: cancellation must abort the backoff", attempts)
	}
}

func TestTransientRetryAttemptsBudget(t *testing.T) {
	if got := transientRetryAttempts(nil); got != 3 {
		t.Errorf("plain request budget = %d, want 3", got)
	}
	webSearch := map[string]any{"tools": []map[string]any{{"type": "web_search_preview"}}}
	if got := transientRetryAttempts(webSearch); got != 2 {
		t.Errorf("web search budget = %d, want 2", got)
	}
}
