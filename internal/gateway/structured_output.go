package gateway

// Structured output support for the Copilot SDK/CLI backends.
//
// The Copilot SDK agent runtime has no equivalent of OpenAI's `response_format`:
// `MessageOptions` carries only a prompt, `SessionConfig` exposes no output
// schema, and `AssistantMessageData.Content` is a plain string. Upstream tracks
// this as github/copilot-sdk#41, which is still open.
//
// The gateway therefore reconstructs the contract from the primitives the SDK
// does provide, strongest first:
//
//  1. Schema-as-tool. A synthetic tool is declared whose JSON Schema is the
//     caller's schema, so the schema reaches the model natively rather than as
//     a prose hint. When the model calls it, its arguments are the answer.
//  2. Extraction. Models often fence JSON or wrap it in prose; the payload is
//     recovered before validation.
//  3. Validation. The payload is validated against the caller's schema.
//  4. Repair or fail. One corrective retry, then an explicit error, never a
//     success carrying non-conforming content.

import (
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/github/copilot-sdk/go"
	"github.com/google/jsonschema-go/jsonschema"
	lru "github.com/hashicorp/golang-lru/v2"
)

const (
	// structuredOutputToolName is the synthetic tool used to carry a
	// schema-constrained answer back from the agent runtime. It is stripped
	// from the result before the response reaches the client.
	structuredOutputToolName = "ghcp_structured_output"

	// structuredOutputFailure prefixes every structured-output error. It is
	// matched by isNonRetryableClientError so a schema mismatch is not retried
	// against other pooled accounts, where it would fail identically.
	structuredOutputFailure = "structured output does not satisfy response_format"
)

// responseFormatSpec is the normalized view of an OpenAI-style `response_format`
// (or the Responses API `text.format`) after parsing.
type responseFormatSpec struct {
	// Kind is "", "json_object" or "json_schema".
	Kind string
	// Name is the caller-supplied schema name, when present.
	Name string
	// Strict mirrors the caller's strict flag. It is informational: validation
	// is always applied when a schema is present.
	Strict bool
	// Schema is the caller's JSON Schema, when present.
	Schema map[string]any
}

// wantsJSON reports whether the caller asked for a JSON response at all.
func (s responseFormatSpec) wantsJSON() bool { return s.Kind != "" }

// hasSchema reports whether the caller supplied a schema to validate against.
func (s responseFormatSpec) hasSchema() bool { return len(s.Schema) > 0 }

// parseResponseFormat normalizes the `response_format` request parameter.
//
// Both the flat Responses-API shape and the nested Chat Completions shape are
// accepted:
//
//	{"type": "json_object"}
//	{"type": "json_schema", "schema": {...}}
//	{"type": "json_schema", "json_schema": {"name": "x", "strict": true, "schema": {...}}}
//
// A json_schema request with no usable schema degrades to json_object rather
// than erroring, so a malformed schema never blocks an otherwise valid request.
func parseResponseFormat(params map[string]any) responseFormatSpec {
	format := anyMap(params["response_format"])
	if format == nil {
		// Responses API requests carry the same contract under text.format.
		if text := anyMap(optionValue(anyMap(params["response_options"]), "text", nil)); text != nil {
			format = anyMap(text["format"])
		}
	}
	if format == nil {
		return responseFormatSpec{}
	}
	kind := strings.ToLower(stringFromAny(format["type"]))
	if !strings.Contains(kind, "json") {
		return responseFormatSpec{}
	}
	spec := responseFormatSpec{Kind: "json_object"}
	if !strings.Contains(kind, "schema") {
		return spec
	}

	// The schema may sit directly on the format object or nested under
	// "json_schema"; prefer the nested form, which also carries name/strict.
	container := format
	if nested := anyMap(format["json_schema"]); nested != nil {
		container = nested
	}
	schema := anyMap(container["schema"])
	if schema == nil {
		// Some clients inline the schema body itself under "json_schema".
		if _, ok := container["type"]; ok && container["properties"] != nil {
			schema = container
		}
	}
	if len(schema) == 0 {
		return spec
	}
	spec.Kind = "json_schema"
	spec.Schema = schema
	spec.Name = stringFromAny(container["name"])
	if strict, ok := container["strict"].(bool); ok {
		spec.Strict = strict
	}
	return spec
}

// structuredOutputTool builds the synthetic schema-carrying tool.
//
// The tool is declared without a handler, matching the convention used for
// caller-supplied tools: the SDK exposes the declaration, the backend observes
// the resulting tool request, and the session is aborted once it arrives.
func structuredOutputTool(spec responseFormatSpec) (sdk.Tool, bool) {
	if !spec.hasSchema() {
		return sdk.Tool{}, false
	}
	description := "Return the final answer to the user. Call this tool exactly once, " +
		"with arguments matching its JSON Schema. This is the only way to deliver the answer."
	if spec.Name != "" {
		description = fmt.Sprintf("Return the final answer as %q. ", spec.Name) + description
	}
	return sdk.Tool{
		Name:        structuredOutputToolName,
		Description: description,
		Parameters:  spec.Schema,
	}, true
}

// withStructuredOutputTool appends the synthetic tool to a tool set, if the
// request calls for one and it is not already present.
//
// Every SessionConfig.Tools source must pass through here. The AvailableTools
// allowlist is derived independently, so a tool declared in one but not the
// other is allowlisted yet unusable.
// structuredOutputToolConflict reports whether the caller supplied a tool using
// the gateway-reserved name. Silently reusing it would let the gateway swallow
// a caller tool call, so the request is rejected instead.
func structuredOutputToolConflict(params map[string]any) error {
	spec := parseResponseFormat(params)
	if !spec.hasSchema() {
		return nil
	}
	tools, _ := params["tools"].([]map[string]any)
	for _, tool := range tools {
		if stringFromAny(tool["name"]) == structuredOutputToolName {
			return fmt.Errorf(
				"%s: tool name %q is reserved by the gateway when response_format requests a json_schema; rename the tool",
				structuredOutputFailure, structuredOutputToolName)
		}
	}
	return nil
}

// structuredOutputToolUsable reports whether the synthetic tool may be declared.
//
// It is withheld whenever the caller constrains tool selection, because the tool
// is invisible to the client: satisfying a "required" or forced tool_choice with
// a hidden tool would silently break that contract. Those requests fall back to
// prompt-plus-validation, which is weaker but honest.
func structuredOutputToolUsable(params map[string]any) bool {
	switch choice := params["tool_choice"].(type) {
	case nil:
		return true
	case string:
		trimmed := strings.ToLower(strings.TrimSpace(choice))
		return trimmed != "none" && trimmed != "required" && trimmed != "any"
	case map[string]any:
		kind := strings.ToLower(strings.TrimSpace(stringFromAny(choice["type"])))
		return kind == "" || kind == "auto"
	default:
		return true
	}
}

func withStructuredOutputTool(tools []sdk.Tool, params map[string]any) []sdk.Tool {
	if !structuredOutputToolUsable(params) {
		return tools
	}
	if structuredOutputToolConflict(params) != nil {
		return tools
	}
	structured, ok := structuredOutputTool(parseResponseFormat(params))
	if !ok {
		return tools
	}
	for _, tool := range tools {
		if tool.Name == structured.Name {
			return tools
		}
	}
	return append(tools, structured)
}

// structuredOutputInstruction is the prompt suffix reinforcing the requested
// output shape. It replaces the previous unconditional "Respond with valid JSON
// only." hint, which discarded the schema entirely.
func structuredOutputInstruction(spec responseFormatSpec, toolRegistered bool) string {
	switch {
	case !spec.wantsJSON():
		return ""
	case spec.hasSchema() && toolRegistered:
		return fmt.Sprintf(
			"Deliver the final answer by calling the `%s` tool exactly once, with arguments "+
				"conforming to its JSON Schema. Do not answer in prose and do not wrap the "+
				"arguments in markdown.",
			structuredOutputToolName)
	case spec.hasSchema():
		schema, err := json.Marshal(spec.Schema)
		if err != nil {
			return "Respond with a single valid JSON value and nothing else."
		}
		return "Respond with a single JSON value that conforms to this JSON Schema, and nothing " +
			"else. Do not wrap it in markdown code fences.\n\nJSON Schema:\n" + string(schema)
	default:
		return "Respond with a single valid JSON value and nothing else. Do not wrap it in " +
			"markdown code fences."
	}
}

// applyStructuredOutput lifts a synthetic tool call into the response content
// and normalizes that content to a bare JSON payload.
//
// The boolean reports whether a synthetic tool call was lifted, so callers can
// keep the SDK-reported token counts instead of re-approximating them.
func applyStructuredOutput(result ChatResult, spec responseFormatSpec) (ChatResult, bool) {
	if !spec.wantsJSON() {
		return result, false
	}
	lifted := false
	if len(result.ToolCalls) > 0 {
		kept := make([]ToolCall, 0, len(result.ToolCalls))
		for _, call := range result.ToolCalls {
			if call.Name != structuredOutputToolName {
				kept = append(kept, call)
				continue
			}
			if args := strings.TrimSpace(call.Arguments); args != "" {
				result.Content = args
				lifted = true
			}
		}
		if len(kept) == 0 {
			kept = nil
		}
		result.ToolCalls = kept
	}
	if len(result.ToolCalls) > 0 {
		return result, lifted
	}
	if payload, ok := extractJSONPayload(result.Content); ok {
		result.Content = payload
	}
	if result.FinishReason == "tool_calls" {
		result.FinishReason = "stop"
	}
	return result, lifted
}

// validateStructuredOutput checks that content is valid JSON and, when the
// caller supplied a schema, that it conforms to that schema.
//
// An uncompilable caller schema is a client error: silently skipping validation
// would report success for content the caller believes was schema-checked.
func validateStructuredOutput(content string, spec responseFormatSpec) error {
	if !spec.wantsJSON() {
		return nil
	}
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return fmt.Errorf("%s: model returned an empty response", structuredOutputFailure)
	}
	var instance any
	if err := json.Unmarshal([]byte(trimmed), &instance); err != nil {
		return fmt.Errorf("%s: response is not valid JSON: %v", structuredOutputFailure, err)
	}
	if _, ok := instance.(map[string]any); !ok {
		return fmt.Errorf("%s: response must be a JSON object", structuredOutputFailure)
	}
	if !spec.hasSchema() {
		return nil
	}
	resolved, err := resolveJSONSchema(spec.Schema)
	if err != nil {
		return fmt.Errorf("%s: request schema could not be compiled: %v", structuredOutputFailure, err)
	}
	if err := resolved.Validate(instance); err != nil {
		return fmt.Errorf("%s: %v", structuredOutputFailure, err)
	}
	return nil
}

// accumulateUsage sums usage across the attempts that made up one client
// request, so a repaired turn bills for the tokens it actually consumed.
//
// This is deliberately not mergeSDKUsage: that function overwrites, because it
// merges successive events describing a single turn. Reusing it here would
// silently discard the rejected attempt's tokens.
func accumulateUsage(base, next Usage) Usage {
	total := base.TotalTokens + next.TotalTokens
	base.InputTokens += next.InputTokens
	base.OutputTokens += next.OutputTokens
	base.CachedTokens += next.CachedTokens
	base.Credits += next.Credits
	base.DurationMS += next.DurationMS
	base.TotalTokens = total
	if next.APIEndpoint != "" {
		base.APIEndpoint = next.APIEndpoint
	}
	if next.ProviderCallID != "" {
		base.ProviderCallID = next.ProviderCallID
	}
	if len(next.QuotaSnapshots) > 0 {
		base.QuotaSnapshots = next.QuotaSnapshots
	}
	return base.Normalized()
}

// structuredOutputRepairMessages appends a corrective turn describing why the
// previous answer was rejected, for a single repair attempt.
func structuredOutputRepairMessages(messages []NeutralMessage, prior string, cause error) []NeutralMessage {
	reason := "the response was not valid JSON"
	if cause != nil {
		reason = strings.TrimPrefix(cause.Error(), structuredOutputFailure+": ")
	}
	repaired := make([]NeutralMessage, 0, len(messages)+2)
	repaired = append(repaired, messages...)
	if trimmed := strings.TrimSpace(prior); trimmed != "" {
		repaired = append(repaired, NeutralMessage{Role: "assistant", Content: trimmed})
	}
	repaired = append(repaired, NeutralMessage{
		Role: "user",
		Content: fmt.Sprintf(
			"That response was rejected: %s. Return only the corrected JSON value, with no "+
				"explanation and no markdown code fences.", reason),
	})
	return repaired
}

// extractJSONPayload recovers a bare JSON payload from model output that may be
// fenced in markdown or surrounded by prose. The boolean reports whether a
// parseable payload was found; content is left untouched otherwise.
func extractJSONPayload(content string) (string, bool) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return "", false
	}
	if unfenced, ok := stripCodeFence(trimmed); ok {
		trimmed = unfenced
	}
	if json.Valid([]byte(trimmed)) {
		return trimmed, true
	}
	if candidate, ok := firstJSONValue(trimmed); ok {
		return candidate, true
	}
	return "", false
}

// stripCodeFence removes a surrounding markdown code fence, with or without a
// language tag.
func stripCodeFence(content string) (string, bool) {
	if !strings.HasPrefix(content, "```") {
		return content, false
	}
	rest := strings.TrimPrefix(content, "```")
	if idx := strings.IndexByte(rest, '\n'); idx >= 0 {
		// Drop the language tag on the opening fence line.
		if tag := strings.TrimSpace(rest[:idx]); !strings.ContainsAny(tag, "{}[]\"") {
			rest = rest[idx+1:]
		}
	}
	if idx := strings.LastIndex(rest, "```"); idx >= 0 {
		rest = rest[:idx]
	}
	return strings.TrimSpace(rest), true
}

// firstJSONValue scans for the first balanced JSON object or array, honoring
// string literals and escapes so that braces inside strings do not confuse the
// depth count.
func firstJSONValue(content string) (string, bool) {
	start := strings.IndexAny(content, "{[")
	if start < 0 {
		return "", false
	}
	open := content[start]
	close := byte('}')
	if open == '[' {
		close = ']'
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(content); i++ {
		c := content[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// Structural characters inside a string are not counted.
		case c == open:
			depth++
		case c == close:
			depth--
			if depth == 0 {
				candidate := content[start : i+1]
				if json.Valid([]byte(candidate)) {
					return candidate, true
				}
				return "", false
			}
		}
	}
	return "", false
}

// schemaCacheSize bounds the compiled-schema cache. Cache keys are the
// caller's schema JSON, so an unbounded map would let clients grow gateway
// memory without limit; eviction keeps the working set for real traffic while
// capping the worst case.
const schemaCacheSize = 512

// maxSchemaBytes caps a single caller schema. The LRU bounds entry count, not
// bytes, so without this a few enormous schemas could still dominate memory.
const maxSchemaBytes = 128 << 10

var schemaCache = mustNewSchemaCache()

func mustNewSchemaCache() *lru.Cache[string, *jsonschema.Resolved] {
	cache, err := lru.New[string, *jsonschema.Resolved](schemaCacheSize)
	if err != nil {
		panic(fmt.Sprintf("structured output: invalid schema cache size %d: %v", schemaCacheSize, err))
	}
	return cache
}

// resolveJSONSchema compiles a caller-supplied JSON Schema, caching the result.
// Schemas that fail to compile are cached as nil so a hot bad schema is not
// recompiled on every request.
func resolveJSONSchema(schema map[string]any) (*jsonschema.Resolved, error) {
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxSchemaBytes {
		return nil, fmt.Errorf("schema is %d bytes, limit is %d", len(raw), maxSchemaBytes)
	}
	key := string(raw)
	if resolved, ok := schemaCache.Get(key); ok {
		if resolved == nil {
			return nil, fmt.Errorf("schema is not usable")
		}
		return resolved, nil
	}
	resolved, err := compileJSONSchema(raw)
	if err != nil {
		schemaCache.Add(key, nil)
		return nil, err
	}
	schemaCache.Add(key, resolved)
	return resolved, nil
}

func compileJSONSchema(raw []byte) (*jsonschema.Resolved, error) {
	var parsed jsonschema.Schema
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	resolved, err := parsed.Resolve(nil)
	if err != nil {
		return nil, err
	}
	return resolved, nil
}
