package gateway

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

type UsageStore struct {
	db *sql.DB
	mu sync.Mutex
}

func NewUsageStore(sqlitePath string) (*UsageStore, error) {
	db, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &UsageStore{db: db}
	if err := store.initSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *UsageStore) initSchema() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS usage_event (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts REAL NOT NULL,
  account_id TEXT,
  model TEXT,
  api_endpoint TEXT,
  provider_call_id TEXT,
  input_tokens INTEGER DEFAULT 0,
  output_tokens INTEGER DEFAULT 0,
  cached_tokens INTEGER DEFAULT 0,
  total_tokens INTEGER DEFAULT 0,
  credits REAL DEFAULT 0,
  duration_ms INTEGER,
  cache_result TEXT,
  success INTEGER DEFAULT 1,
  error_type TEXT,
  quota_snapshots TEXT
);
CREATE INDEX IF NOT EXISTS idx_usage_account ON usage_event(account_id);
CREATE INDEX IF NOT EXISTS idx_usage_model ON usage_event(model);
`)
	return err
}

func (s *UsageStore) Record(accountID *string, model string, usage Usage, cacheResult string, success bool, errorType string) error {
	usage = usage.Normalized()
	quota := sql.NullString{}
	if len(usage.QuotaSnapshots) > 0 {
		quota.Valid = true
		quota.String = string(usage.QuotaSnapshots)
	}
	account := sql.NullString{}
	if accountID != nil {
		account.Valid = true
		account.String = *accountID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
INSERT INTO usage_event (
  ts, account_id, model, api_endpoint, provider_call_id,
  input_tokens, output_tokens, cached_tokens, total_tokens,
  credits, duration_ms, cache_result, success, error_type, quota_snapshots
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		nowUnix(),
		account,
		model,
		nullString(usage.APIEndpoint),
		nullString(usage.ProviderCallID),
		usage.InputTokens,
		usage.OutputTokens,
		usage.CachedTokens,
		usage.TotalTokens,
		usage.Credits,
		nullInt(usage.DurationMS),
		cacheResult,
		boolInt(success),
		nullString(errorType),
		quota,
	)
	return err
}

type UsageQuery struct {
	Account      string
	Model        string
	Since        *float64
	Until        *float64
	GroupBy      string
	ExcludeCache bool
}

var groupColumns = map[string]string{
	"account":  "account_id",
	"model":    "model",
	"endpoint": "api_endpoint",
}

const aggCols = `COUNT(*) AS calls,
COALESCE(SUM(input_tokens),0) AS input_tokens,
COALESCE(SUM(output_tokens),0) AS output_tokens,
COALESCE(SUM(cached_tokens),0) AS cached_tokens,
COALESCE(SUM(total_tokens),0) AS total_tokens,
COALESCE(SUM(credits),0) AS credits,
COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END),0) AS errors`

func (s *UsageStore) Query(q UsageQuery) (map[string]any, error) {
	clause, args := usageWhere(q)
	groupCol := ""
	if q.GroupBy != "" {
		var ok bool
		groupCol, ok = groupColumns[q.GroupBy]
		if !ok {
			return nil, fmt.Errorf("invalid group_by %q; valid: account, endpoint, model", q.GroupBy)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if groupCol != "" {
		rows, err := s.db.Query(fmt.Sprintf(`SELECT %s AS key, %s FROM usage_event%s GROUP BY %s ORDER BY total_tokens DESC`, groupCol, aggCols, clause, groupCol), args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		groups := []map[string]any{}
		for rows.Next() {
			row, err := scanGroup(rows)
			if err != nil {
				return nil, err
			}
			groups = append(groups, row)
		}
		return map[string]any{"group_by": q.GroupBy, "groups": groups}, rows.Err()
	}
	row := s.db.QueryRow(fmt.Sprintf(`SELECT %s FROM usage_event%s`, aggCols, clause), args...)
	return scanAggregate(row)
}

func (s *UsageStore) Matrix(q UsageQuery) ([]map[string]any, error) {
	q.ExcludeCache = true
	clause, args := usageWhere(q)
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(fmt.Sprintf(`SELECT account_id, model, %s FROM usage_event%s GROUP BY account_id, model ORDER BY account_id, total_tokens DESC`, aggCols, clause), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var account sql.NullString
		var model sql.NullString
		agg, err := scanAggregatePrefix(rows, &account, &model)
		if err != nil {
			return nil, err
		}
		agg["account_id"] = nullableString(account)
		agg["model"] = nullableString(model)
		out = append(out, agg)
	}
	return out, rows.Err()
}

func (s *UsageStore) Close() error {
	return s.db.Close()
}

type Metrics struct {
	mu       sync.Mutex
	requests int
	errors   int
}

func (m *Metrics) Observe(success bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests++
	if !success {
		m.errors++
	}
}

func (m *Metrics) Render() ([]byte, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	body := fmt.Sprintf(`# TYPE ghcp_requests_total counter
ghcp_requests_total %d
# TYPE ghcp_errors_total counter
ghcp_errors_total %d
`, m.requests, m.errors)
	return []byte(body), "text/plain; version=0.0.4"
}

type Meter struct {
	store   *UsageStore
	metrics *Metrics
}

func NewMeter(store *UsageStore, metrics *Metrics) *Meter {
	return &Meter{store: store, metrics: metrics}
}

func (m *Meter) Observe(accountID *string, model string, usage Usage, cacheResult string, success bool, errorType string) {
	if cacheResult == "hit" && usage.Credits != 0 {
		usage.Credits = 0
	}
	_ = m.store.Record(accountID, model, usage, cacheResult, success, errorType)
	m.metrics.Observe(success)
}

func usageWhere(q UsageQuery) (string, []any) {
	where := []string{}
	args := []any{}
	if q.Account != "" {
		where = append(where, "account_id = ?")
		args = append(args, q.Account)
	}
	if q.Model != "" {
		where = append(where, "model = ?")
		args = append(args, q.Model)
	}
	if q.Since != nil {
		where = append(where, "ts >= ?")
		args = append(args, *q.Since)
	}
	if q.Until != nil {
		where = append(where, "ts <= ?")
		args = append(args, *q.Until)
	}
	if q.ExcludeCache {
		where = append(where, "cache_result != 'hit'")
	}
	if len(where) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

type scanner interface {
	Scan(dest ...any) error
}

func scanGroup(rows *sql.Rows) (map[string]any, error) {
	var key sql.NullString
	agg, err := scanAggregatePrefix(rows, &key)
	if err != nil {
		return nil, err
	}
	agg["key"] = nullableString(key)
	return agg, nil
}

func scanAggregate(row scanner) (map[string]any, error) {
	return scanAggregatePrefix(row)
}

func scanAggregatePrefix(row scanner, prefix ...any) (map[string]any, error) {
	var calls, input, output, cached, total, errors int
	var credits float64
	dest := append(prefix, &calls, &input, &output, &cached, &total, &credits, &errors)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	return map[string]any{
		"calls":         calls,
		"input_tokens":  input,
		"output_tokens": output,
		"cached_tokens": cached,
		"total_tokens":  total,
		"credits":       credits,
		"errors":        errors,
	}, nil
}

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func nullInt(value int) sql.NullInt64 {
	return sql.NullInt64{Int64: int64(value), Valid: value != 0}
}

func nullableString(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func usageJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}
