package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// UsageSink is a durable, append-only destination for usage events that
// survives container lifecycle events. SQLite remains the fast local read
// cache for admin queries; the sink is the durable cross-replica record.
//
// Implementations must be safe for concurrent use and must never block the
// caller: Emit drops events under backpressure rather than slowing request
// handling.
type UsageSink interface {
	Emit(rec UsageRecord)
	Close() error

	// isUsageSink seals the interface so the variant set stays closed.
	isUsageSink()
}

// UsageRecord is one usage event headed for the durable sink. It mirrors the
// columns persisted to SQLite so the two stores stay aligned.
type UsageRecord struct {
	TS           float64
	AccountID    string
	Model        string
	APIEndpoint  string
	InputTokens  int
	OutputTokens int
	CachedTokens int
	TotalTokens  int
	Credits      float64
	DurationMS   int
	CacheResult  string
	Success      bool
	ErrorType    string
}

// noopSink discards every event. It is the sink used when no durable
// destination is configured, so the gateway runs unchanged for local/dev.
type noopSink struct{}

func (noopSink) Emit(UsageRecord) {}
func (noopSink) Close() error     { return nil }
func (noopSink) isUsageSink()     {}

// newUsageSink builds the durable sink from settings. It returns noopSink when
// Azure Monitor is not configured. A non-nil error means the configuration was
// present but invalid (bad endpoint URL, credential bootstrap failure).
func newUsageSink(cfg AzureMonitorConfig) (UsageSink, error) {
	if !cfg.configured() {
		return noopSink{}, nil
	}
	provider, err := defaultMonitorTokenProvider()
	if err != nil {
		return nil, fmt.Errorf("azure monitor sink credential: %w", err)
	}
	return newAzureMonitorSink(cfg, provider, http.DefaultClient)
}

const (
	usageSinkBufferSize    = 2048
	usageSinkBatchSize     = 100
	usageSinkFlushInterval = 5 * time.Second
)

// azureMonitorSink streams usage events to a Log Analytics custom table via the
// Azure Monitor Logs Ingestion API. Events are buffered on a bounded channel
// and flushed in batches by a single worker goroutine, so request handlers
// never block on network I/O. When the buffer is full, events are dropped and
// counted rather than applying backpressure.
type azureMonitorSink struct {
	ingestor httpIngestor

	events  chan UsageRecord
	done    chan struct{}
	wg      sync.WaitGroup
	dropped atomic.Uint64
}

func newAzureMonitorSink(cfg AzureMonitorConfig, token tokenProvider, client *http.Client) (*azureMonitorSink, error) {
	uploadURL, err := buildIngestionURL(cfg)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	s := &azureMonitorSink{
		ingestor: httpIngestor{client: client, uploadURL: uploadURL, token: token},
		events:   make(chan UsageRecord, usageSinkBufferSize),
		done:     make(chan struct{}),
	}
	s.wg.Add(1)
	go s.run()
	return s, nil
}

func (s *azureMonitorSink) isUsageSink() {}

// Emit queues a record for delivery. It never blocks: a full buffer drops the
// record and increments the dropped counter.
func (s *azureMonitorSink) Emit(rec UsageRecord) {
	select {
	case s.events <- rec:
	default:
		s.dropped.Add(1)
	}
}

// Close stops accepting events, flushes what is buffered, and waits for the
// worker to drain. It is safe to call once.
func (s *azureMonitorSink) Close() error {
	close(s.done)
	s.wg.Wait()
	if dropped := s.dropped.Load(); dropped > 0 {
		slog.Warn("azure monitor usage sink dropped events under backpressure",
			slog.Uint64("dropped", dropped))
	}
	return nil
}

func (s *azureMonitorSink) run() {
	defer s.wg.Done()
	ticker := time.NewTicker(usageSinkFlushInterval)
	defer ticker.Stop()

	batch := make([]UsageRecord, 0, usageSinkBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.upload(batch)
		batch = batch[:0]
	}

	for {
		select {
		case rec := <-s.events:
			batch = append(batch, rec)
			if len(batch) >= usageSinkBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-s.done:
			s.drainInto(&batch)
			flush()
			return
		}
	}
}

// drainInto moves any remaining buffered events into batch without blocking.
func (s *azureMonitorSink) drainInto(batch *[]UsageRecord) {
	for {
		select {
		case rec := <-s.events:
			*batch = append(*batch, rec)
		default:
			return
		}
	}
}

func (s *azureMonitorSink) upload(batch []UsageRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), usageSinkHTTPTimeout)
	defer cancel()
	if err := s.ingestor.post(ctx, batch); err != nil {
		slog.Warn("azure monitor usage ingestion failed",
			slog.Int("events", len(batch)),
			slog.Any("err", err))
	}
}
