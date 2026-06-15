package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	mux.HandleFunc("POST /v1/messages/count_tokens", s.anthropicCountTokens)
	mux.HandleFunc("POST /v1/embeddings", s.embeddings)
	mux.HandleFunc("GET /v1beta/models", s.geminiModels)
	mux.HandleFunc("POST /v1beta/models/{model_action...}", s.geminiModelAction)
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
	mux.HandleFunc("GET /admin/model-aliases", s.adminModelAliases)
	mux.HandleFunc("PUT /admin/model-aliases", s.adminSetModelAliases)
	mux.HandleFunc("GET /admin/rate-limits", s.adminRateLimits)
	mux.HandleFunc("PUT /admin/rate-limits", s.adminSetRateLimits)
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
			for _, displayID := range s.gw.Settings.DisplayIDsForModel(model) {
				data = append(data, map[string]any{"id": displayID, "object": "model", "created": nowUnixInt(), "owned_by": "github-copilot"})
			}
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
	body.PreferredEndpoint = endpointChatCompletions
	body.FallbackEndpoints = []string{endpointResponses}
	if !s.modelAllowed(w, body.Model, p) {
		writeError(w, http.StatusForbidden, "model not allowed: "+body.Model)
		return
	}
	plan, err := s.gw.PrepareWithWait(r.Context(), body, p, firstNonEmpty(r.Header.Get("x-ghcp-cache"), body.Cache))
	if err != nil {
		s.writeInferenceError(w, err)
		return
	}
	if body.Stream {
		ctx, cancel := context.WithCancel(r.Context())
		writeStream(w, plan.Headers(), s.gw.StreamChat(ctx, plan), cancel)
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
	chat := body.ToChatRequest()
	chat.PreferredEndpoint = endpointResponses
	chat.FallbackEndpoints = []string{endpointChatCompletions, endpointMessages}
	if !s.modelAllowed(w, chat.Model, p) {
		writeError(w, http.StatusForbidden, "model not allowed: "+chat.Model)
		return
	}
	plan, err := s.gw.PrepareWithWait(r.Context(), chat, p, firstNonEmpty(r.Header.Get("x-ghcp-cache"), chat.Cache))
	if err != nil {
		s.writeInferenceError(w, err)
		return
	}
	if body.Stream {
		ctx, cancel := context.WithCancel(r.Context())
		writeStream(w, plan.Headers(), s.gw.StreamResponses(ctx, plan), cancel)
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
	p, ok := s.requireAnthropic(w, r, "inference")
	if !ok {
		return
	}
	rawBody, body, ok := decodeAnthropicMessagesRaw(w, r)
	if !ok {
		return
	}
	body.Model = normalizeAnthropicRequestedModel(body.Model, r.Header.Get("anthropic-beta"))
	rawBody["model"] = body.Model
	rawBody = sanitizeNativeAnthropicRaw(rawBody)
	resolvedModel := s.gw.Settings.ResolveModelAlias(body.Model)
	if !s.modelSupportsAnthropicMessages(resolvedModel) {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "/v1/messages is only available for models that support /v1/messages")
		return
	}
	if body.MaxTokens == nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "/v1/messages requires max_tokens")
		return
	}
	chat := body.ToChatRequest()
	chat.AnthropicRaw = rawBody
	chat.PreferredEndpoint = endpointMessages
	chat.FallbackEndpoints = []string{endpointResponses, endpointChatCompletions}
	if !s.modelAllowed(w, chat.Model, p) {
		writeAnthropicError(w, http.StatusForbidden, "permission_error", "model not allowed: "+chat.Model)
		return
	}
	plan, err := s.gw.PrepareWithWait(r.Context(), chat, p, firstNonEmpty(r.Header.Get("x-ghcp-cache"), chat.Cache))
	if err != nil {
		s.writeAnthropicInferenceError(w, err)
		return
	}
	if body.Stream {
		ctx, cancel := context.WithCancel(r.Context())
		writeStream(w, plan.Headers(), s.gw.StreamAnthropic(ctx, plan), cancel)
		return
	}
	result, accountID, err := s.gw.CompleteResult(r.Context(), plan)
	if err != nil {
		status, errorType, message := anthropicErrorFromBackend(err)
		writeAnthropicError(w, status, errorType, message)
		return
	}
	headers := plan.Headers()
	headers["x-ghcp-account"] = accountID
	writeJSON(w, http.StatusOK, anthropicResponse(result.Model, result.Content, result.FinishReason, result.Usage, result.ToolCalls, chat.ResponseOptions), headers)
}

func (s *server) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireAnthropic(w, r, "inference")
	if !ok {
		return
	}
	var body AnthropicMessagesRequest
	if !decodeJSONAnthropic(w, r, &body) {
		return
	}
	body.Model = normalizeAnthropicRequestedModel(body.Model, r.Header.Get("anthropic-beta"))
	resolvedModel := s.gw.Settings.ResolveModelAlias(body.Model)
	if !p.MayUseModel(resolvedModel) {
		writeAnthropicError(w, http.StatusForbidden, "permission_error", "model not allowed: "+body.Model)
		return
	}
	if !s.modelSupportsAnthropicMessages(resolvedModel) {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "/v1/messages/count_tokens is only available for models that support /v1/messages")
		return
	}
	if s.gw.Registry.LastRefresh > 0 && len(s.gw.Registry.AccountsFor(resolvedModel)) == 0 {
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", "no account serves model "+resolvedModel)
		return
	}
	chat := body.ToChatRequest()
	inputTokens, internalModel := s.gw.EstimateInputTokens(chat)
	headers := map[string]string{
		"x-ghcp-model":              body.Model,
		"request-id":                newID("req"),
		"anthropic-organization-id": "ghcp-pool",
	}
	if body.Model != internalModel {
		headers["x-ghcp-upstream-model"] = internalModel
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": inputTokens}, headers)
}

func (s *server) modelSupportsAnthropicMessages(model string) bool {
	if strings.HasPrefix(strings.ToLower(model), "claude") {
		return true
	}
	return s.gw.Registry.ModelSupportsEndpoint(model, endpointMessages)
}

func (s *server) embeddings(w http.ResponseWriter, r *http.Request) {
	p, ok := s.require(w, r, "inference")
	if !ok {
		return
	}
	var body EmbeddingsRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if !s.modelAllowed(w, body.Model, p) {
		writeError(w, http.StatusForbidden, "model not allowed: "+body.Model)
		return
	}
	inputs := body.InputTexts()
	if len(inputs) == 0 {
		writeError(w, http.StatusBadRequest, "embeddings input must not be empty")
		return
	}
	result, _, headers, err := s.gw.CreateEmbeddings(r.Context(), body.Model, inputs, body.Params())
	if err != nil {
		var noAccount NoAccountForModel
		var rateLimit RateLimitExceeded
		switch {
		case errors.Is(err, ErrEmbeddingsUnsupported):
			writeError(w, http.StatusNotImplemented, err.Error())
		case errors.As(err, &noAccount):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.As(err, &rateLimit):
			writeErrorWithHeaders(w, http.StatusTooManyRequests, err.Error(), map[string]string{"Retry-After": strconv.Itoa(int(rateLimit.RetryAfter))})
		default:
			writeError(w, http.StatusBadGateway, "backend error: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, embeddingResponse(result.Model, result.Embeddings, result.Usage), headers)
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
		ID:                  id,
		Label:               firstNonEmpty(stringFromAny(payload["label"]), id),
		TokenEnv:            stringsTrim(stringFromAny(payload["token_env"])),
		TokenKeyVaultSecret: stringsTrim(stringFromAny(payload["token_key_vault_secret"])),
		KeyVaultURL:         stringsTrim(stringFromAny(payload["key_vault_url"])),
		BaseDirectory:       baseDir,
		MaxConcurrency:      intFromPayload(payload["max_concurrency"], 32),
		Weight:              intFromPayload(payload["weight"], 1),
		RateLimitRPM:        intPtrFromPayload(payload["rate_limit_rpm"]),
		Allow:               stringSliceFromAny(firstAny(payload["allow"], []any{"*"})),
		Deny:                stringSliceFromAny(payload["deny"]),
		Models:              stringSliceFromAny(payload["models"]),
	}
	enabled := true
	if v, ok := payload["enabled"].(bool); ok {
		enabled = v
	}
	cfg.Enabled = &enabled
	if token := stringsTrim(stringFromAny(payload["token"])); token != "" && token != "null" {
		if cfg.TokenKeyVaultSecret != "" {
			if err := cfg.StoreToken(r.Context(), s.gw.Settings.KeyVaultURL, token); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		} else {
			cfg.RuntimeToken = token
			cfg.RuntimeTokenSource = "api_token"
		}
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
	writeJSON(w, http.StatusOK, map[string]any{"last_refresh": s.gw.Registry.LastRefresh, "refresh_interval_seconds": s.gw.Settings.ModelRefreshSeconds, "models_count": len(s.gw.Registry.AllModels()), "models": s.gw.Registry.ModelsIndex(), "capabilities": s.gw.Registry.CapabilitiesIndex(), "model_aliases": s.gw.Settings.ModelAliases}, nil)
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
	requested := r.URL.Query().Get("model")
	resolved := s.gw.Settings.ResolveModelAlias(requested)
	endpoint := r.URL.Query().Get("endpoint")
	info := s.gw.Router.ExplainEndpoint(resolved, endpoint)
	if endpoint != "" {
		info["selected_endpoint"] = s.gw.Registry.PickEndpoint(resolved, []string{endpoint})
	}
	info["requested_model"] = requested
	info["resolved_model"] = resolved
	writeJSON(w, http.StatusOK, info, nil)
}

func (s *server) adminModelAliases(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model_aliases": s.gw.Settings.ModelAliases, "model_map_path": s.gw.Settings.ModelMapPath}, nil)
}

func (s *server) adminSetModelAliases(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	var payload map[string]string
	if !decodeJSON(w, r, &payload) {
		return
	}
	aliases := map[string]string{}
	for alias, target := range payload {
		alias = strings.TrimSpace(alias)
		target = strings.TrimSpace(target)
		if alias == "" || target == "" {
			writeError(w, http.StatusBadRequest, "alias and target must be non-empty")
			return
		}
		aliases[alias] = target
	}
	if err := s.gw.SetModelAliases(aliases); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": len(aliases), "model_aliases": s.gw.Settings.ModelAliases, "model_map_path": s.gw.Settings.ModelMapPath}, nil)
}

func (s *server) adminRateLimits(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	accounts := []any{}
	for _, account := range s.gw.Pool.Snapshot() {
		accounts = append(accounts, account.Status())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rate_limits": map[string]any{
			"global_rpm":      s.gw.Settings.RateLimits.GlobalRPM,
			"per_account_rpm": s.gw.Settings.RateLimits.PerAccountRPM,
		},
		"global_retry_after": s.gw.Pool.GlobalRetryAfter(),
		"accounts":           accounts,
	}, nil)
}

func (s *server) adminSetRateLimits(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.require(w, r, "admin"); !ok {
		return
	}
	var payload map[string]any
	if !decodeJSON(w, r, &payload) {
		return
	}
	cfg := RateLimitConfig{
		GlobalRPM:     intFromAny(firstAny(payload["global_rpm"], s.gw.Settings.RateLimits.GlobalRPM), s.gw.Settings.RateLimits.GlobalRPM),
		PerAccountRPM: intFromAny(firstAny(payload["per_account_rpm"], s.gw.Settings.RateLimits.PerAccountRPM), s.gw.Settings.RateLimits.PerAccountRPM),
	}
	if cfg.GlobalRPM < 0 || cfg.PerAccountRPM < 0 {
		writeError(w, http.StatusBadRequest, "rate limits must be >= 0")
		return
	}
	s.gw.Pool.ConfigureRateLimits(cfg)
	s.gw.Settings.RateLimits = cfg
	writeJSON(w, http.StatusOK, map[string]any{"updated": true, "rate_limits": map[string]any{"global_rpm": cfg.GlobalRPM, "per_account_rpm": cfg.PerAccountRPM}}, nil)
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
	key := firstNonEmpty(r.Header.Get("Authorization"), r.Header.Get("x-api-key"), r.Header.Get("x-goog-api-key"), r.URL.Query().Get("key"))
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

func (s *server) requireAnthropic(w http.ResponseWriter, r *http.Request, scope string) (Principal, bool) {
	key := firstNonEmpty(r.Header.Get("Authorization"), r.Header.Get("x-api-key"), r.Header.Get("x-goog-api-key"), r.URL.Query().Get("key"))
	principal, ok := s.gw.Authenticator.Authenticate(key)
	if !ok {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid or missing API key")
		return Principal{}, false
	}
	if !principal.HasScope(scope) {
		writeAnthropicError(w, http.StatusForbidden, "permission_error", "missing scope: "+scope)
		return Principal{}, false
	}
	return principal, true
}

func (s *server) modelAllowed(_ http.ResponseWriter, requested string, principal Principal) bool {
	return principal.MayUseModel(s.gw.Settings.ResolveModelAlias(requested))
}

func (s *server) writeInferenceError(w http.ResponseWriter, err error) {
	var noAccount NoAccountForModel
	var routeErr RoutingError
	var rateLimit RateLimitExceeded
	switch {
	case errors.As(err, &noAccount):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.As(err, &rateLimit):
		writeErrorWithHeaders(w, http.StatusTooManyRequests, err.Error(), map[string]string{"Retry-After": strconv.Itoa(int(rateLimit.RetryAfter))})
	case errors.As(err, &routeErr):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func (s *server) writeAnthropicInferenceError(w http.ResponseWriter, err error) {
	var noAccount NoAccountForModel
	var routeErr RoutingError
	var rateLimit RateLimitExceeded
	switch {
	case errors.As(err, &noAccount):
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", err.Error())
	case errors.As(err, &rateLimit):
		writeAnthropicErrorWithHeaders(w, http.StatusTooManyRequests, "rate_limit_error", err.Error(), map[string]string{"Retry-After": strconv.Itoa(int(rateLimit.RetryAfter))})
	case errors.As(err, &routeErr):
		writeAnthropicError(w, http.StatusServiceUnavailable, "overloaded_error", err.Error())
	default:
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
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

func decodeJSONAnthropic(w http.ResponseWriter, r *http.Request, dest any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(dest); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return false
	}
	return true
}

func decodeAnthropicMessagesRaw(w http.ResponseWriter, r *http.Request) (map[string]any, AnthropicMessagesRequest, bool) {
	defer r.Body.Close()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, AnthropicMessagesRequest{}, false
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, AnthropicMessagesRequest{}, false
	}
	if raw == nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
		return nil, AnthropicMessagesRequest{}, false
	}
	var body AnthropicMessagesRequest
	if err := json.Unmarshal(data, &body); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, AnthropicMessagesRequest{}, false
	}
	return raw, body, true
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

func writeErrorWithHeaders(w http.ResponseWriter, status int, detail string, headers map[string]string) {
	writeJSON(w, status, map[string]any{"detail": detail}, headers)
}

func writeAnthropicError(w http.ResponseWriter, status int, errorType, message string) {
	writeAnthropicErrorWithHeaders(w, status, errorType, message, nil)
}

func writeAnthropicErrorWithHeaders(w http.ResponseWriter, status int, errorType, message string, headers map[string]string) {
	if errorType == "" {
		errorType = "api_error"
	}
	if message == "" {
		message = http.StatusText(status)
	}
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errorType,
			"message": message,
		},
	}, headers)
}

func normalizeAnthropicRequestedModel(model string, betaHeader string) string {
	model = normalizeAnthropicModelAlias(model)
	if strings.Contains(strings.ToLower(betaHeader), "context-1m") && !strings.HasSuffix(model, "-1m") {
		model += "-1m"
	}
	return model
}

func normalizeAnthropicModelAlias(model string) string {
	switch {
	case strings.HasPrefix(model, "claude-sonnet-4-6-"):
		return "claude-sonnet-4.6"
	case strings.HasPrefix(model, "claude-sonnet-4-5-"):
		return "claude-sonnet-4.5"
	case strings.HasPrefix(model, "claude-opus-4-6-"):
		return "claude-opus-4.6"
	case strings.HasPrefix(model, "claude-opus-4-5-"):
		return "claude-opus-4.5"
	case strings.HasPrefix(model, "claude-haiku-4-5-"):
		return "claude-haiku-4.5"
	default:
		return model
	}
}

func writeStream(w http.ResponseWriter, headers map[string]string, stream <-chan string, cancel context.CancelFunc) {
	defer cancel()
	for key, value := range headers {
		w.Header().Set(key, value)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for chunk := range stream {
		if _, err := w.Write([]byte(chunk)); err != nil {
			cancel()
			return
		}
		if strings.HasSuffix(chunk, "\n\n") {
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

func intPtrFromPayload(value any) *int {
	if value == nil {
		return nil
	}
	v := intFromAny(value, 0)
	return &v
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
