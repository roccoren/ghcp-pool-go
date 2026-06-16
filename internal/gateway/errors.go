package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type conflictError struct{ message string }

func (e *conflictError) Error() string { return e.message }

type CopilotUpstreamError struct {
	StatusCode int
	Body       []byte
}

func (e *CopilotUpstreamError) Error() string {
	body := strings.TrimSpace(string(e.Body))
	if len(body) > 512 {
		body = body[:512]
	}
	return fmt.Sprintf("copilot upstream error %d: %s", e.StatusCode, body)
}

func isNonRetryableBackendError(err error) bool {
	var upstream *CopilotUpstreamError
	if errors.As(err, &upstream) {
		errorType, _ := anthropicErrorFromBody(upstream.Body)
		return upstream.StatusCode == 400 && (errorType == "" || errorType == "invalid_request_error")
	}
	return isNonRetryableClientError(err.Error())
}

func isTransientOverloadError(err error) bool {
	if err == nil {
		return false
	}
	var upstream *CopilotUpstreamError
	if errors.As(err, &upstream) {
		return upstream.StatusCode == 529 || upstream.StatusCode == 500 || upstream.StatusCode == 502 || upstream.StatusCode == 503 || upstream.StatusCode == 504
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "529") ||
		strings.Contains(msg, "overloaded") ||
		strings.Contains(msg, "at capacity") ||
		strings.Contains(msg, "capacity")
}

func anthropicErrorFromBackend(err error) (status int, errorType string, message string) {
	if isTransientOverloadError(err) {
		return 503, "overloaded_error", err.Error()
	}
	var upstream *CopilotUpstreamError
	if errors.As(err, &upstream) {
		errorType, message = anthropicErrorFromBody(upstream.Body)
		if errorType == "" {
			errorType = "api_error"
		}
		if message == "" {
			message = err.Error()
		}
		return upstream.StatusCode, errorType, message
	}
	return 502, "api_error", "backend error: " + err.Error()
}

func anthropicErrorFromBody(body []byte) (string, string) {
	var payload struct {
		Message string `json:"message"`
		Error   struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", ""
	}
	if payload.Error.Type == "" && payload.Message != "" {
		return "invalid_request_error", payload.Message
	}
	return payload.Error.Type, payload.Error.Message
}
