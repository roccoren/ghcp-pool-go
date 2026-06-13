package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const redacted = "***redacted***"

type DebugRecorder struct {
	enabled      bool
	maxEntries   int
	maxBodyChars int
	entries      []map[string]any
	seq          int
	mu           sync.Mutex
}

func NewDebugRecorder(config DebugConfig) *DebugRecorder {
	maxEntries := config.MaxEntries
	if maxEntries <= 0 {
		maxEntries = 200
	}
	maxBodyChars := config.MaxBodyChars
	if maxBodyChars <= 0 {
		maxBodyChars = 20000
	}
	return &DebugRecorder{enabled: config.Enabled, maxEntries: maxEntries, maxBodyChars: maxBodyChars}
}

func (d *DebugRecorder) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !d.Enabled() || !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		requestBody, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(requestBody))

		capture := &captureWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(capture, r)

		d.record(map[string]any{
			"method":             r.Method,
			"path":               r.URL.Path,
			"query":              r.URL.RawQuery,
			"status":             capture.status,
			"duration_ms":        int(time.Since(start).Milliseconds()),
			"stream":             strings.Contains(capture.Header().Get("Content-Type"), "text/event-stream"),
			"request_headers":    redactHeaders(r.Header),
			"request_body":       decodeDebugBody(requestBody, d.maxBodyChars),
			"request_truncated":  len(string(requestBody)) > d.maxBodyChars,
			"response_headers":   redactHeaders(capture.Header()),
			"response_body":      decodeDebugBody(capture.body.Bytes(), d.maxBodyChars),
			"response_truncated": capture.body.Len() > d.maxBodyChars,
		})
	})
}

func (d *DebugRecorder) Enabled() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.enabled
}

func (d *DebugRecorder) SetEnabled(enabled bool) {
	d.mu.Lock()
	d.enabled = enabled
	d.mu.Unlock()
}

func (d *DebugRecorder) Status() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return map[string]any{
		"enabled":          d.enabled,
		"count":            len(d.entries),
		"max_entries":      d.maxEntries,
		"max_body_chars":   d.maxBodyChars,
		"capture_prefixes": []string{"/v1/"},
	}
}

func (d *DebugRecorder) Entries(limit int, path string) []map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	filtered := []map[string]any{}
	for _, entry := range d.entries {
		if path == "" || entry["path"] == path {
			filtered = append(filtered, entry)
		}
	}
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	out := make([]map[string]any, len(filtered))
	copy(out, filtered)
	return out
}

func (d *DebugRecorder) Clear() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(d.entries)
	d.entries = nil
	return n
}

func (d *DebugRecorder) record(entry map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq++
	entry["seq"] = d.seq
	entry["ts"] = nowUnix()
	d.entries = append(d.entries, entry)
	if len(d.entries) > d.maxEntries {
		d.entries = d.entries[len(d.entries)-d.maxEntries:]
	}
}

type captureWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *captureWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *captureWriter) Write(data []byte) (int, error) {
	w.body.Write(data)
	return w.ResponseWriter.Write(data)
}

func (w *captureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func redactHeaders(headers http.Header) map[string]string {
	out := map[string]string{}
	for key, values := range headers {
		lower := strings.ToLower(key)
		value := strings.Join(values, ", ")
		switch lower {
		case "authorization", "x-api-key", "api-key", "cookie", "set-cookie":
			value = redacted
		}
		out[key] = value
	}
	return out
}

func decodeDebugBody(raw []byte, maxChars int) any {
	if len(raw) == 0 {
		return ""
	}
	text := string(raw)
	if len(text) > maxChars {
		if maxChars <= 0 {
			return ""
		}
		if maxChars <= 2000 {
			return text[:maxChars]
		}
		half := maxChars / 2
		return text[:half] + "\n\n...[truncated]...\n\n" + text[len(text)-half:]
	}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var value any
		if err := json.Unmarshal(raw, &value); err == nil {
			return value
		}
	}
	return text
}
