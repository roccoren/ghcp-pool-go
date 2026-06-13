package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func NewServer(gw *Gateway) http.Handler {
	mux := http.NewServeMux()
	s := &server{gw: gw}

	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /v1/models", s.listModels)
	mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("POST /v1/responses", s.responses)
	mux.HandleFunc("POST /v1/messages", s.anthropicMessages)
	mux.HandleFunc("GET /admin/accounts", s.adminAccounts)
	mux.HandleFunc("POST /admin/accounts/{account_id}/enable", s.adminAccountEnable)
	mux.HandleFunc("POST /admin/accounts/{account_id}/disable", s.adminAccountDisable)
	mux.HandleFunc("GET /admin/users", s.adminUsers)
	mux.HandleFunc("POST /admin/users", s.adminCreateUser)
	mux.HandleFunc("GET /admin/users/{account_id}", s.adminUser)
	mux.HandleFunc("DELETE /admin/users/{account_id}", s.adminDeleteUser)
	mux.HandleFunc("POST /admin/users/{account_id}/login", s.adminUserLogin)
	mux.HandleFunc("GET /admin/users/{account_id}/login", s.adminUserLoginStatus)
	mux.HandleFunc("POST /admin/users/{account_id}/login/poll", s.adminUserLoginPoll)
	mux.HandleFunc("POST /admin/users/{account_id}/token", s.adminUserSetToken)
	mux.HandleFunc("POST /admin/users/{account_id}/logout", s.adminUserLogout)
	mux.HandleFunc("GET /admin/users/{account_id}/usage", s.adminUserUsage)
	mux.HandleFunc("GET /admin/users/{account_id}/models", s.adminUserModels)
	mux.HandleFunc("GET /admin/models", s.adminModels)
	mux.HandleFunc("POST /admin/models/refresh", s.adminModelsRefresh)
	mux.HandleFunc("POST /admin/models/refresh/{account_id}", s.adminModelsRefreshAccount)
	mux.HandleFunc("GET /admin/routes", s.adminRoutes)
	mux.HandleFunc("PUT /admin/routes", s.adminSetRoutes)
	mux.HandleFunc("GET /admin/routes/resolve", s.adminRoutesResolve)
	mux.HandleFunc("GET /admin/reasoning-efforts", s.adminReasoningEfforts)
	mux.HandleFunc("PUT /admin/reasoning-efforts", s.adminSetReasoningEfforts)
	mux.HandleFunc("GET /admin/usage", s.adminUsage)
	mux.HandleFunc("GET /admin/usage/aggregate", s.adminUsageAggregate)
	mux.HandleFunc("GET /admin/dashboard/data", s.adminDashboardData)
	mux.HandleFunc("GET /dashboard", s.dashboard)
	mux.HandleFunc("GET /admin/cache/stats", s.adminCacheStats)
	mux.HandleFunc("DELETE /admin/cache", s.adminCacheClear)
	mux.HandleFunc("GET /admin/debug", s.adminDebug)
	mux.HandleFunc("POST /admin/debug/enable", s.adminDebugEnable)
	mux.HandleFunc("POST /admin/debug/disable", s.adminDebugDisable)
	mux.HandleFunc("GET /admin/debug/log", s.adminDebugLog)
	mux.HandleFunc("DELETE /admin/debug/log", s.adminDebugClear)

	rec := NewDebugRecorder(gw.Settings.Debug)
	s.debug = rec
	return rec.Middleware(mux)
}

type server struct {
	gw    *Gateway
	debug *DebugRecorder
}

func (s *server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"}, nil)
}

func (s *server) readyz(w http.ResponseWriter, _ *http.Request) {
	if len(s.gw.Pool.EnabledAccounts()) == 0 || s.gw.Registry.LastRefresh == 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not-ready"}, nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"}, nil)
}

func (s *server) metrics(w http.ResponseWriter, _ *http.Request) {
	body, ctype := s.gw.Metrics.Render()
	w.Header().Set("Content-Type", ctype)
	_, _ = w.Write(body)
}

func (s *server) listModels(w http.ResponseWriter, r *http.Request) {
	p, ok := s.require(w, r, "inference")
	if !ok {
		return
	}
	data := []any{}
	for _, model := range s.gw.Registry.VisibleModels() {
		if p.MayUseModel(model) {
			data = append(data, map[string]any{"id": model, "object": "model", "created": nowUnixInt(), "owned_by": "github-copilot"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data}, nil)
}

func (s *server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	p, ok := s.require(w, r, "inference")
	if !ok {
		return
	}
	var body ChatCompletionRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if !p.MayUseModel(body.Model) {
		writeError(w, http.StatusForbidden, "model not allowed: "+body.Model)
		return
	}
	plan, err := s.gw.PrepareWithWait(r.Context(), body, p, firstNonEmpty(r.Header.Get("x-ghcp-cache"), body.Cache))
	if err != nil {
		s.writeInferenceError(w, err)
		return
	}
	if body.Stream {
		writeStream(w, plan.Headers(), s.gw.StreamChat(r.Context(), plan))
		return
	}
	result, accountID, err := s.gw.CompleteResult(r.Context(), plan)
	if err != nil {
		writeError(w, http.StatusBadGateway, "backend error: "+err.Error())
		return
	}
	headers := plan.Headers()
	headers["x-ghcp-account"] = accountID
	writeJSON(w, http.StatusOK, completionResponse(result.Model, result.Content, result.FinishReason, result.Usage, result.ToolCalls), headers)
}

func (s *server) responses(w http.ResponseWriter, r *http.Request) {
	p, ok := s.require(w, r, "inference")
	if !ok {
		return
	}
	var body ResponsesRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if !strings.HasPrefix(strings.ToLower(body.Model), "gpt") {
		writeError(w, http.StatusBadRequest, "/v1/responses is only available for gpt* models")
		return
	}
	chat := body.ToChatRequest()
	if !p.MayUseModel(chat.Model) {
		writeError(w, http.StatusForbidden, "model not allowed: "+chat.Model)
		return
	}
	plan, err := s.gw.PrepareWithWait(r.Context(), chat, p, firstNonEmpty(r.Header.Get("x-ghcp-cache"), chat.Cache))
	if err != nil {
		s.writeInferenceError(w, err)
		return
	}
	if body.Stream {
		writeStream(w, plan.Headers(), s.gw.StreamResponses(r.Context(), plan))
		return
	}
	result, accountID, err := s.gw.CompleteResult(r.Context(), plan)
	if err != nil {
		writeError(w, http.StatusBadGateway, "backend error: "+err.Error())
		return
	}
	headers := plan.Headers()
	headers["x-ghcp-account"] = accountID
	writeJSON(w, http.StatusOK, responseResponse(result.Model, result.Content, result.FinishReason, result.Usage, result.ToolCalls, chat.ResponseOptions), headers)
}

func (s *server) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	p, ok := s.require(w, r, "inference")
	if !ok {
		return
	}
	var body AnthropicMessagesRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if !strings.HasPrefix(strings.ToLower(body.Model), "claude") {
		writeError(w, http.StatusBadRequest, "/v1/messages is only available for claude* models")
		return
	}
	if body.MaxTokens == nil {
		writeError(w, http.StatusBadRequest, "/v1/messages requires max_tokens")
		return
	}
	chat := body.ToChatRequest()
	if !p.MayUseModel(chat.Model) {
		writeError(w, http.StatusForbidden, "model not allowed: "+chat.Model)
		return
	}
	plan, err := s.gw.PrepareWithWait(r.Context(), chat, p, firstNonEmpty(r.Header.Get("x-ghcp-cache"), chat.Cache))
	if err != nil {
		s.writeInferenceError(w, err)
		return
	}
	if body.Stream {
		writeStream(w, plan.Headers(), s.gw.StreamAnthropic(r.Context(), plan))
		return
	}
	result, accountID, err := s.gw.CompleteResult(r.Context(), plan)
	if err != nil {
		writeError(w, http.StatusBadGateway, "backend error: "+err.Error())
		return
	}
	headers := plan.Headers()
	headers["x-ghcp-account"] = accountID
	writeJSON(w, http.StatusOK, anthropicResponse(result.Model, result.Content, result.FinishReason, result.Usage, result.ToolCalls, chat.ResponseOptions), headers)
}

func (s *server) adminAccounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	accounts := []any{}
	for _, account := range s.gw.Pool.Snapshot() {
		accounts = append(accounts, account.Status())
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts}, nil)
}

func (s *server) adminAccountEnable(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	id := r.PathValue("account_id")
	if !s.gw.Pool.SetEnabled(id, true) {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	_ = s.gw.Pool.StartAccount(r.Context(), id)
	s.gw.Registry.RefreshAccount(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{"account": id, "enabled": true}, nil)
}

func (s *server) adminAccountDisable(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	id := r.PathValue("account_id")
	if !s.gw.Pool.SetEnabled(id, false) {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	s.gw.Registry.RefreshAccount(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{"account": id, "enabled": false}, nil)
}

func (s *server) adminUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	users := []any{}
	for _, account := range s.gw.Pool.Snapshot() {
		view := account.Status()
		view["login"] = s.gw.LoginManager.Status(account.ID())
		users = append(users, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users}, nil)
}

func (s *server) adminUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	view, ok := s.userView(r.PathValue("account_id"))
	if !ok {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, view, nil)
}

func (s *server) adminCreateUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	var payload map[string]any
	if !decodeJSON(w, r, &payload) {
		return
	}
	id := stringFromAny(payload["id"])
	if id == "" || id == "null" {
		id = "user_" + strings.ReplaceAll(newID(""), "-", "")[:12]
	}
	baseDir := stringFromAny(payload["base_directory"])
	if s.gw.Settings.Backend == "copilot" {
		resolved, err := s.gw.Pool.Homes.Resolve(id, baseDir)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		baseDir = resolved
	}
	cfg := AccountConfig{
		ID:             id,
		Label:          firstNonEmpty(stringFromAny(payload["label"]), id),
		BaseDirectory:  baseDir,
		MaxConcurrency: intFromPayload(payload["max_concurrency"], 32),
		Weight:         intFromPayload(payload["weight"], 1),
		Allow:          stringSliceFromAny(firstAny(payload["allow"], []any{"*"})),
		Deny:           stringSliceFromAny(payload["deny"]),
		Models:         stringSliceFromAny(payload["models"]),
	}
	enabled := true
	if v, ok := payload["enabled"].(bool); ok {
		enabled = v
	}
	cfg.Enabled = &enabled
	if token := stringsTrim(stringFromAny(payload["token"])); token != "" && token != "null" {
		cfg.RuntimeToken = token
		cfg.RuntimeTokenSource = "api_token"
	}
	_, err := s.gw.Pool.AddAccount(r.Context(), cfg)
	if err != nil {
		var conflict *conflictError
		if errors.As(err, &conflict) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.gw.Registry.RefreshAccount(r.Context(), id)
	view, _ := s.userView(id)
	writeJSON(w, http.StatusOK, view, nil)
}

func (s *server) adminDeleteUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	id := r.PathValue("account_id")
	_ = s.gw.LoginManager.Logout(r.Context(), id)
	if !s.gw.Pool.RemoveAccount(id) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	s.gw.Pool.Homes.Forget(id)
	s.gw.Registry.RefreshAccount(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{"user": id, "removed": true}, nil)
}

func (s *server) adminUserLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	result, err := s.gw.LoginManager.Start(r.Context(), r.PathValue("account_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result, nil)
}

func (s *server) adminUserLoginStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	state := s.gw.LoginManager.Status(r.PathValue("account_id"))
	if state == nil {
		writeError(w, http.StatusNotFound, "no pending login")
		return
	}
	writeJSON(w, http.StatusOK, state, nil)
}

func (s *server) adminUserLoginPoll(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	result, err := s.gw.LoginManager.Poll(r.Context(), r.PathValue("account_id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if result["status"] == "authorized" {
		s.gw.Registry.RefreshAccount(r.Context(), r.PathValue("account_id"))
	}
	writeJSON(w, http.StatusOK, result, nil)
}

func (s *server) adminUserSetToken(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	id := r.PathValue("account_id")
	if s.gw.Pool.Get(id) == nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	var payload map[string]any
	if !decodeJSON(w, r, &payload) {
		return
	}
	token := stringsTrim(stringFromAny(payload["token"]))
	if token == "" || token == "null" {
		writeError(w, http.StatusBadRequest, "missing 'token'")
		return
	}
	if err := s.gw.LoginManager.SetToken(r.Context(), id, token); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.gw.Registry.RefreshAccount(r.Context(), id)
	view, _ := s.userView(id)
	writeJSON(w, http.StatusOK, view, nil)
}

func (s *server) adminUserLogout(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	id := r.PathValue("account_id")
	if s.gw.Pool.Get(id) == nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	_ = s.gw.LoginManager.Logout(r.Context(), id)
	s.gw.Registry.RefreshAccount(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{"user": id, "logged_out": true}, nil)
}

func (s *server) adminUserUsage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	id := r.PathValue("account_id")
	if s.gw.Pool.Get(id) == nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	q := usageQueryFromRequest(r)
	q.Account = id
	result, err := s.gw.UsageStore.Query(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result, nil)
}

func (s *server) adminUserModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	id := r.PathValue("account_id")
	account := s.gw.Pool.Get(id)
	if account == nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": id, "models": account.Models}, nil)
}

func (s *server) adminModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"last_refresh": s.gw.Registry.LastRefresh, "refresh_interval_seconds": s.gw.Settings.ModelRefreshSeconds, "models_count": len(s.gw.Registry.AllModels()), "models": s.gw.Registry.ModelsIndex()}, nil)
}

func (s *server) adminModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	s.gw.Registry.Refresh(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"refreshed": true, "last_refresh": s.gw.Registry.LastRefresh, "models_count": len(s.gw.Registry.AllModels())}, nil)
}

func (s *server) adminModelsRefreshAccount(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	id := r.PathValue("account_id")
	account := s.gw.Pool.Get(id)
	if account == nil {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	s.gw.Registry.RefreshAccount(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{"account": id, "refreshed": true, "models": account.Models}, nil)
}

func (s *server) adminRoutes(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": s.gw.Router.Routes}, nil)
}

func (s *server) adminSetRoutes(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	var routes []RouteConfig
	if !decodeJSON(w, r, &routes) {
		return
	}
	for i := range routes {
		if routes[i].Strategy == "" {
			routes[i].Strategy = "least_busy"
		}
		if !ValidStrategies[routes[i].Strategy] {
			writeError(w, http.StatusBadRequest, "invalid strategy '"+routes[i].Strategy+"'")
			return
		}
		if routes[i].Model == "" {
			routes[i].Model = "*"
		}
	}
	s.gw.Router.SetRoutes(routes)
	writeJSON(w, http.StatusOK, map[string]any{"updated": len(routes)}, nil)
}

func (s *server) adminRoutesResolve(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.gw.Router.Explain(r.URL.Query().Get("model")), nil)
}

func (s *server) adminReasoningEfforts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reasoning_efforts": s.gw.Settings.ReasoningEfforts}, nil)
}

func (s *server) adminSetReasoningEfforts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	var payload map[string]string
	if !decodeJSON(w, r, &payload) {
		return
	}
	for pattern, effort := range payload {
		effort = strings.ToLower(effort)
		if !ValidReasoningEfforts[effort] {
			writeError(w, http.StatusBadRequest, "invalid reasoning effort '"+effort+"' for '"+pattern+"'")
			return
		}
		payload[pattern] = effort
	}
	s.gw.Settings.ReasoningEfforts = payload
	writeJSON(w, http.StatusOK, map[string]any{"updated": len(payload), "reasoning_efforts": payload}, nil)
}

func (s *server) adminUsage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	result, err := s.gw.UsageStore.Query(usageQueryFromRequest(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result, nil)
}

func (s *server) adminUsageAggregate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	totals, _ := s.gw.UsageStore.Query(UsageQuery{ExcludeCache: true})
	perAccount, _ := s.gw.UsageStore.Query(UsageQuery{GroupBy: "account", ExcludeCache: true})
	perModel, _ := s.gw.UsageStore.Query(UsageQuery{GroupBy: "model", ExcludeCache: true})
	writeJSON(w, http.StatusOK, map[string]any{"totals": totals, "per_account": perAccount["groups"], "per_model": perModel["groups"], "cache": s.gw.Cache.Stats()}, nil)
}

func (s *server) adminDashboardData(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	q := usageQueryFromRequest(r)
	q.ExcludeCache = true
	totals, _ := s.gw.UsageStore.Query(q)
	q.GroupBy = "account"
	perAccount, _ := s.gw.UsageStore.Query(q)
	q.GroupBy = "model"
	perModel, _ := s.gw.UsageStore.Query(q)
	q.GroupBy = ""
	matrix, _ := s.gw.UsageStore.Matrix(q)
	byAccount := map[string][]map[string]any{}
	for _, row := range matrix {
		if id, ok := row["account_id"].(string); ok {
			entry := map[string]any{}
			for k, v := range row {
				if k != "account_id" {
					entry[k] = v
				}
			}
			byAccount[id] = append(byAccount[id], entry)
		}
	}
	accounts := []any{}
	for _, account := range s.gw.Pool.Snapshot() {
		accounts = append(accounts, account.Status())
	}
	writeJSON(w, http.StatusOK, map[string]any{"totals": totals, "per_account": perAccount["groups"], "per_model": perModel["groups"], "matrix": matrix, "models_by_account": byAccount, "accounts": accounts, "cache": s.gw.Cache.Stats()}, nil)
}

func (s *server) dashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<!doctype html><html><body><h1>ghcp-pool</h1><p>Go rewrite dashboard</p></body></html>"))
}

func (s *server) adminCacheStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.gw.Cache.Stats(), nil)
}

func (s *server) adminCacheClear(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": s.gw.Cache.Clear()}, nil)
}

func (s *server) adminDebug(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.debug.Status(), nil)
}

func (s *server) adminDebugEnable(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	s.debug.SetEnabled(true)
	writeJSON(w, http.StatusOK, s.debug.Status(), nil)
}

func (s *server) adminDebugDisable(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	s.debug.SetEnabled(false)
	writeJSON(w, http.StatusOK, s.debug.Status(), nil)
}

func (s *server) adminDebugLog(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	path := r.URL.Query().Get("path")
	entries := s.debug.Entries(limit, path)
	writeJSON(w, http.StatusOK, map[string]any{"enabled": s.debug.Enabled(), "count": len(entries), "entries": entries}, nil)
}

func (s *server) adminDebugClear(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": s.debug.Clear()}, nil)
}

func (s *server) require(w http.ResponseWriter, r *http.Request, scope string) (Principal, bool) {
	key := firstNonEmpty(r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
	principal, ok := s.gw.Authenticator.Authenticate(key)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid or missing API key")
		return Principal{}, false
	}
	if !principal.HasScope(scope) {
		writeError(w, http.StatusForbidden, "missing scope: "+scope)
		return Principal{}, false
	}
	return principal, true
}

func (s *server) writeInferenceError(w http.ResponseWriter, err error) {
	var noAccount NoAccountForModel
	var routeErr RoutingError
	switch {
	case errors.As(err, &noAccount):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.As(err, &routeErr):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func (s *server) userView(id string) (map[string]any, bool) {
	account := s.gw.Pool.Get(id)
	if account == nil {
		return nil, false
	}
	view := account.Status()
	view["login"] = s.gw.LoginManager.Status(id)
	return view, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dest any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(dest); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any, headers map[string]string) {
	for key, value := range headers {
		w.Header().Set(key, value)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]any{"detail": detail}, nil)
}

func writeStream(w http.ResponseWriter, headers map[string]string, stream <-chan string) {
	for key, value := range headers {
		w.Header().Set(key, value)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for chunk := range stream {
		_, _ = w.Write([]byte(chunk))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func usageQueryFromRequest(r *http.Request) UsageQuery {
	q := r.URL.Query()
	query := UsageQuery{Account: q.Get("account"), Model: q.Get("model"), GroupBy: q.Get("group_by")}
	if since := q.Get("since"); since != "" {
		if v, err := strconv.ParseFloat(since, 64); err == nil {
			query.Since = &v
		}
	}
	if until := q.Get("until"); until != "" {
		if v, err := strconv.ParseFloat(until, 64); err == nil {
			query.Until = &v
		}
	}
	return query
}

func nowUnixInt() int64 { return timeNowUnix() }

func timeNowUnix() int64 { return time.Now().Unix() }

func intFromPayload(value any, fallback int) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return fallback
	}
}

func stringSliceFromAny(value any) []string {
	switch v := value.(type) {
	case nil:
		return nil
	case []string:
		return v
	case []any:
		out := []string{}
		for _, item := range v {
			out = append(out, stringFromAny(item))
		}
		return out
	case string:
		return []string{v}
	default:
		return nil
	}
}

func withTimeout(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 5*time.Minute)
}
