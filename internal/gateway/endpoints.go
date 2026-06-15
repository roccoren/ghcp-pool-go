package gateway

import "strings"

const (
	endpointChatCompletions = "/chat/completions"
	endpointResponses       = "/responses"
	endpointMessages        = "/v1/messages"
	endpointEmbeddings      = "/embeddings"

	internalEndpointParam = "__ghcp_endpoint"
)

func normalizeEndpoint(endpoint string) string {
	normalized := strings.TrimSpace(endpoint)
	normalized = strings.TrimPrefix(normalized, "/v1")
	if normalized == "" {
		return "/"
	}
	if !strings.HasPrefix(normalized, "/") {
		normalized = "/" + normalized
	}
	return normalized
}

func endpointMatches(a, b string) bool {
	return normalizeEndpoint(a) == normalizeEndpoint(b)
}

func supportedEndpoint(spec ModelSpec, endpoint string) bool {
	if endpoint == "" || len(spec.SupportedEndpoints) == 0 {
		return true
	}
	for _, supported := range spec.SupportedEndpoints {
		if endpointMatches(supported, endpoint) {
			return true
		}
	}
	return false
}

func fakeSupportedEndpoints(model string) []string {
	switch {
	case strings.HasPrefix(strings.ToLower(model), "claude"):
		return []string{endpointMessages, endpointChatCompletions}
	case strings.Contains(strings.ToLower(model), "responses-only"):
		return []string{endpointResponses}
	case strings.Contains(strings.ToLower(model), "chat-only"):
		return []string{endpointChatCompletions}
	case strings.Contains(strings.ToLower(model), "embed"):
		return []string{endpointEmbeddings}
	default:
		return []string{endpointChatCompletions, endpointResponses, endpointEmbeddings}
	}
}

func endpointFromParams(params map[string]any, fallback string) string {
	if endpoint, ok := params[internalEndpointParam].(string); ok && endpoint != "" {
		return endpoint
	}
	return fallback
}

func cleanBackendParams(params map[string]any) map[string]any {
	out := make(map[string]any, len(params))
	for key, value := range params {
		if strings.HasPrefix(key, "__ghcp_") {
			continue
		}
		out[key] = value
	}
	return out
}
