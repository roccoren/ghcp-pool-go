package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func staticToken(token string) tokenProvider {
	return func(context.Context) (string, error) { return token, nil }
}

func TestNewUsageSinkUnconfiguredReturnsNoop(t *testing.T) {
	cases := []struct {
		name string
		cfg  AzureMonitorConfig
	}{
		{"all empty", AzureMonitorConfig{}},
		{"missing rule", AzureMonitorConfig{Endpoint: "https://x.ingest.monitor.azure.com", Stream: "Custom-UsageEvent"}},
		{"missing stream", AzureMonitorConfig{Endpoint: "https://x.ingest.monitor.azure.com", RuleID: "dcr-1"}},
		{"whitespace only", AzureMonitorConfig{Endpoint: "  ", RuleID: "  ", Stream: "  "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink, err := newUsageSink(tc.cfg)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, ok := sink.(noopSink); !ok {
				t.Fatalf("expected noopSink, got %T", sink)
			}
		})
	}
}

func TestBuildIngestionURL(t *testing.T) {
	cfg := AzureMonitorConfig{
		Endpoint: "https://dce-x.eastus-1.ingest.monitor.azure.com/",
		RuleID:   "dcr-abc123",
		Stream:   "Custom-UsageEvent",
	}
	got, err := buildIngestionURL(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://dce-x.eastus-1.ingest.monitor.azure.com/dataCollectionRules/dcr-abc123/streams/Custom-UsageEvent?api-version=2023-01-01"
	if got != want {
		t.Fatalf("url=\n got %q\nwant %q", got, want)
	}
}

func TestBuildIngestionURLRejectsNonHTTPS(t *testing.T) {
	_, err := buildIngestionURL(AzureMonitorConfig{Endpoint: "http://insecure", RuleID: "dcr-1", Stream: "Custom-X"})
	if err == nil {
		t.Fatal("expected error for non-https endpoint")
	}
}

func TestAzureMonitorSinkPostsBatch(t *testing.T) {
	type captured struct {
		path       string
		query      string
		auth       string
		contentTyp string
		rows       []ingestionRow
	}
	recvCh := make(chan captured, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rows []ingestionRow
		_ = json.Unmarshal(body, &rows)
		recvCh <- captured{
			path:       r.URL.Path,
			query:      r.URL.RawQuery,
			auth:       r.Header.Get("Authorization"),
			contentTyp: r.Header.Get("Content-Type"),
			rows:       rows,
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	cfg := AzureMonitorConfig{Endpoint: server.URL, RuleID: "dcr-test", Stream: "Custom-UsageEvent"}
	sink, err := newAzureMonitorSink(cfg, staticToken("tok-123"), server.Client())
	if err != nil {
		t.Fatal(err)
	}

	acct := "acct-a"
	sink.Emit(usageRecordFrom(&acct, "gpt-4.1", Usage{
		InputTokens:  10,
		OutputTokens: 5,
		TotalTokens:  15,
		Credits:      0.25,
		APIEndpoint:  "chat.completions",
		DurationMS:   42,
	}, "miss", true, ""))

	closed := make(chan error, 1)
	go func() { closed <- sink.Close() }()

	select {
	case got := <-recvCh:
		if got.path != "/dataCollectionRules/dcr-test/streams/Custom-UsageEvent" {
			t.Fatalf("path=%q", got.path)
		}
		if got.query != "api-version=2023-01-01" {
			t.Fatalf("query=%q", got.query)
		}
		if got.auth != "Bearer tok-123" {
			t.Fatalf("auth=%q", got.auth)
		}
		if got.contentTyp != "application/json" {
			t.Fatalf("content-type=%q", got.contentTyp)
		}
		if len(got.rows) != 1 {
			t.Fatalf("rows=%d, want 1", len(got.rows))
		}
		row := got.rows[0]
		if row.AccountID != "acct-a" || row.Model != "gpt-4.1" || row.TotalTokens != 15 || row.InputTokens != 10 {
			t.Fatalf("row mismatch: %+v", row)
		}
		if row.TimeGenerated == "" {
			t.Fatal("TimeGenerated must be set")
		}
		if _, perr := time.Parse(time.RFC3339Nano, row.TimeGenerated); perr != nil {
			t.Fatalf("TimeGenerated not RFC3339: %q (%v)", row.TimeGenerated, perr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ingestion POST")
	}
	if cerr := <-closed; cerr != nil {
		t.Fatalf("close err: %v", cerr)
	}
}

func TestAzureMonitorSinkEmitNeverBlocks(t *testing.T) {
	var hits atomic.Int64
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	cfg := AzureMonitorConfig{Endpoint: server.URL, RuleID: "dcr-test", Stream: "Custom-UsageEvent"}
	sink, err := newAzureMonitorSink(cfg, staticToken("tok"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		unblock()
		_ = sink.Close()
	})

	done := make(chan struct{})
	go func() {
		for i := 0; i < usageSinkBufferSize*4; i++ {
			sink.Emit(UsageRecord{Model: "m", TotalTokens: 1})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Emit blocked under backpressure")
	}
}

func TestFloatUnixToRFC3339(t *testing.T) {
	got := floatUnixToRFC3339(1700000000.5)
	want := time.Unix(1700000000, int64(0.5*float64(time.Second))).UTC().Format(time.RFC3339Nano)
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if floatUnixToRFC3339(0) == "" {
		t.Fatal("zero timestamp must fall back to now, not empty")
	}
}

func TestNoopSinkIsUsageSink(t *testing.T) {
	var _ UsageSink = noopSink{}
	var sink UsageSink = noopSink{}
	sink.Emit(UsageRecord{})
	if err := sink.Close(); err != nil {
		t.Fatalf("noop close err: %v", err)
	}
}

func TestMeterTeesToSink(t *testing.T) {
	store, err := NewUsageStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fake := &fakeSink{}
	meter := NewMeter(store, &Metrics{}, fake)
	acct := "acct-z"
	meter.Observe(&acct, "claude-opus-4.8", Usage{InputTokens: 3, OutputTokens: 7}, "miss", true, "")
	got := fake.snapshot()
	if len(got) != 1 {
		t.Fatalf("sink received %d records, want 1", len(got))
	}
	if got[0].Model != "claude-opus-4.8" || got[0].AccountID != "acct-z" || got[0].TotalTokens != 10 {
		t.Fatalf("record mismatch: %+v", got[0])
	}
}

type fakeSink struct {
	mu      sync.Mutex
	records []UsageRecord
}

func (f *fakeSink) Emit(rec UsageRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, rec)
}

func (f *fakeSink) Close() error { return nil }
func (f *fakeSink) isUsageSink() {}

func (f *fakeSink) snapshot() []UsageRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]UsageRecord, len(f.records))
	copy(out, f.records)
	return out
}
