package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const (
	monitorIngestionScope = "https://monitor.azure.com/.default"
	monitorAPIVersion     = "2023-01-01"
	usageSinkHTTPTimeout  = 30 * time.Second
)

// AzureMonitorConfig configures the Logs Ingestion API sink. All three fields
// are required for the sink to activate; otherwise the gateway uses noopSink.
type AzureMonitorConfig struct {
	// Endpoint is the Data Collection Endpoint logs-ingestion URI, e.g.
	// https://dce-name.eastus-1.ingest.monitor.azure.com
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	// RuleID is the Data Collection Rule immutable ID, e.g.
	// dcr-000a00a000a00000a000000aa000a0aa
	RuleID string `yaml:"rule_id" json:"rule_id"`
	// Stream is the DCR stream name, e.g. Custom-UsageEvent
	Stream string `yaml:"stream" json:"stream"`
}

func (c AzureMonitorConfig) configured() bool {
	return strings.TrimSpace(c.Endpoint) != "" &&
		strings.TrimSpace(c.RuleID) != "" &&
		strings.TrimSpace(c.Stream) != ""
}

// tokenProvider returns a bearer token for the monitor ingestion scope. It is
// a seam so tests inject a fake instead of reaching Azure AD.
type tokenProvider func(ctx context.Context) (string, error)

func buildIngestionURL(cfg AzureMonitorConfig) (string, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("azure monitor endpoint %q: %w", cfg.Endpoint, err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("azure monitor endpoint must be an https URL, got %q", cfg.Endpoint)
	}
	uploadURL := fmt.Sprintf(
		"%s/dataCollectionRules/%s/streams/%s?api-version=%s",
		endpoint,
		url.PathEscape(strings.TrimSpace(cfg.RuleID)),
		url.PathEscape(strings.TrimSpace(cfg.Stream)),
		monitorAPIVersion,
	)
	return uploadURL, nil
}

type httpIngestor struct {
	client    *http.Client
	uploadURL string
	token     tokenProvider
}

func (h httpIngestor) post(ctx context.Context, batch []UsageRecord) error {
	bearer, err := h.token(ctx)
	if err != nil {
		return fmt.Errorf("acquire monitor token: %w", err)
	}
	body, err := json.Marshal(toIngestionRows(batch))
	if err != nil {
		return fmt.Errorf("marshal usage batch: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.uploadURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build ingestion request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("post usage batch: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("usage ingestion HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

// ingestionRow is the JSON shape posted to the DCR stream. Field names are the
// column names declared in the DCR streamDeclaration. TimeGenerated is the
// required Log Analytics timestamp column.
type ingestionRow struct {
	TimeGenerated string  `json:"TimeGenerated"`
	AccountID     string  `json:"AccountId,omitempty"`
	Model         string  `json:"Model,omitempty"`
	APIEndpoint   string  `json:"ApiEndpoint,omitempty"`
	InputTokens   int     `json:"InputTokens"`
	OutputTokens  int     `json:"OutputTokens"`
	CachedTokens  int     `json:"CachedTokens"`
	TotalTokens   int     `json:"TotalTokens"`
	Credits       float64 `json:"Credits"`
	DurationMS    int     `json:"DurationMs"`
	CacheResult   string  `json:"CacheResult,omitempty"`
	Success       bool    `json:"Success"`
	ErrorType     string  `json:"ErrorType,omitempty"`
}

func toIngestionRows(batch []UsageRecord) []ingestionRow {
	rows := make([]ingestionRow, 0, len(batch))
	for _, rec := range batch {
		rows = append(rows, ingestionRow{
			TimeGenerated: floatUnixToRFC3339(rec.TS),
			AccountID:     rec.AccountID,
			Model:         rec.Model,
			APIEndpoint:   rec.APIEndpoint,
			InputTokens:   rec.InputTokens,
			OutputTokens:  rec.OutputTokens,
			CachedTokens:  rec.CachedTokens,
			TotalTokens:   rec.TotalTokens,
			Credits:       rec.Credits,
			DurationMS:    rec.DurationMS,
			CacheResult:   rec.CacheResult,
			Success:       rec.Success,
			ErrorType:     rec.ErrorType,
		})
	}
	return rows
}

func floatUnixToRFC3339(ts float64) string {
	if ts <= 0 {
		return time.Now().UTC().Format(time.RFC3339Nano)
	}
	sec := int64(ts)
	nsec := int64((ts - float64(sec)) * float64(time.Second))
	return time.Unix(sec, nsec).UTC().Format(time.RFC3339Nano)
}

func defaultMonitorTokenProvider() (tokenProvider, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (string, error) {
		tk, err := cred.GetToken(ctx, policy.TokenRequestOptions{
			Scopes: []string{monitorIngestionScope},
		})
		if err != nil {
			return "", err
		}
		return tk.Token, nil
	}, nil
}
