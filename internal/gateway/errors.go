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

func anthropicErrorFromBackend(err error) (status int, errorType string, message string) {
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
