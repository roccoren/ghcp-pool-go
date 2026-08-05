package gateway

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/github/copilot-sdk/go"
)

func personSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"age":  map[string]any{"type": "integer"},
		},
		"required":             []any{"name", "age"},
		"additionalProperties": false,
	}
}

func TestParseResponseFormat(t *testing.T) {
	tests := []struct {
		name       string
		params     map[string]any
		wantKind   string
		wantSchema bool
		wantName   string
	}{
		{
			name:     "absent",
			params:   map[string]any{},
			wantKind: "",
		},
		{
			name:     "non json type ignored",
			params:   map[string]any{"response_format": map[string]any{"type": "text"}},
			wantKind: "",
		},
		{
			name:     "json object",
			params:   map[string]any{"response_format": map[string]any{"type": "json_object"}},
			wantKind: "json_object",
		},
		{
			name: "nested openai shape",
			params: map[string]any{"response_format": map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name":   "person",
					"strict": true,
					"schema": personSchema(),
				},
			}},
			wantKind:   "json_schema",
			wantSchema: true,
			wantName:   "person",
		},
		{
			name: "flat responses shape",
			params: map[string]any{"response_format": map[string]any{
				"type":   "json_schema",
				"name":   "person",
				"schema": personSchema(),
			}},
			wantKind:   "json_schema",
			wantSchema: true,
			wantName:   "person",
		},
		{
			name: "json schema without schema degrades to json object",
			params: map[string]any{"response_format": map[string]any{
				"type": "json_schema",
			}},
			wantKind: "json_object",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := parseResponseFormat(tt.params)
			if spec.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", spec.Kind, tt.wantKind)
			}
			if spec.hasSchema() != tt.wantSchema {
				t.Errorf("hasSchema() = %v, want %v", spec.hasSchema(), tt.wantSchema)
			}
			if spec.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", spec.Name, tt.wantName)
			}
		})
	}
}

func TestExtractJSONPayload(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
		wantOK  bool
	}{
		{name: "empty", content: "   ", wantOK: false},
		{name: "bare object", content: `{"a":1}`, want: `{"a":1}`, wantOK: true},
		{name: "bare array", content: `[1,2]`, want: `[1,2]`, wantOK: true},
		{
			name:    "fenced with language tag",
			content: "```json\n{\"a\":1}\n```",
			want:    `{"a":1}`,
			wantOK:  true,
		},
		{
			name:    "fenced without language tag",
			content: "```\n{\"a\":1}\n```",
			want:    `{"a":1}`,
			wantOK:  true,
		},
		{
			name:    "wrapped in prose",
			content: "Sure! Here is the result:\n{\"a\":1}\nHope that helps.",
			want:    `{"a":1}`,
			wantOK:  true,
		},
		{
			name:    "braces inside string literal do not confuse depth",
			content: `Here: {"a":"}{","b":2} done`,
			want:    `{"a":"}{","b":2}`,
			wantOK:  true,
		},
		{
			name:    "escaped quote inside string",
			content: `{"a":"say \"hi\"","b":1}`,
			want:    `{"a":"say \"hi\"","b":1}`,
			wantOK:  true,
		},
		{name: "prose only", content: "I cannot help with that.", wantOK: false},
		{name: "unbalanced", content: `{"a":1`, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractJSONPayload(tt.content)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (got %q)", ok, tt.wantOK, got)
			}
			if ok && got != tt.want {
				t.Errorf("payload = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateStructuredOutput(t *testing.T) {
	schemaSpec := responseFormatSpec{Kind: "json_schema", Schema: personSchema()}
	objectSpec := responseFormatSpec{Kind: "json_object"}

	tests := []struct {
		name        string
		content     string
		spec        responseFormatSpec
		wantErr     bool
		errContains string
	}{
		{name: "no format requested passes anything", content: "hello", spec: responseFormatSpec{}},
		{name: "valid against schema", content: `{"name":"ada","age":36}`, spec: schemaSpec},
		{name: "valid json object", content: `{"anything":true}`, spec: objectSpec},
		{
			name:        "empty response",
			content:     "  ",
			spec:        objectSpec,
			wantErr:     true,
			errContains: "empty response",
		},
		{
			name:        "prose is not json",
			content:     "Sorry, I cannot do that.",
			spec:        objectSpec,
			wantErr:     true,
			errContains: "not valid JSON",
		},
		{
			name:        "missing required property",
			content:     `{"name":"ada"}`,
			spec:        schemaSpec,
			wantErr:     true,
			errContains: "age",
		},
		{
			name:        "wrong property type",
			content:     `{"name":"ada","age":"old"}`,
			spec:        schemaSpec,
			wantErr:     true,
			errContains: "age",
		},
		{
			name:        "additional property rejected",
			content:     `{"name":"ada","age":36,"extra":1}`,
			spec:        schemaSpec,
			wantErr:     true,
			errContains: "extra",
		},
		{
			name:        "uncompilable schema is a client error, not a silent bypass",
			content:     `{"anything":true}`,
			spec:        responseFormatSpec{Kind: "json_schema", Schema: map[string]any{"type": 12345}},
			wantErr:     true,
			errContains: "could not be compiled",
		},
		{
			name:        "json_object rejects a non-object top level",
			content:     `[1,2]`,
			spec:        objectSpec,
			wantErr:     true,
			errContains: "must be a JSON object",
		},
		{
			name:        "json_object rejects a bare scalar",
			content:     `"hello"`,
			spec:        objectSpec,
			wantErr:     true,
			errContains: "must be a JSON object",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateStructuredOutput(tt.content, tt.spec)
			if tt.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if err == nil {
				return
			}
			if !strings.Contains(err.Error(), structuredOutputFailure) {
				t.Errorf("error %q missing marker %q", err, structuredOutputFailure)
			}
			if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
				t.Errorf("error %q does not mention %q", err, tt.errContains)
			}
		})
	}
}

func TestApplyStructuredOutputLiftsSyntheticToolCall(t *testing.T) {
	spec := responseFormatSpec{Kind: "json_schema", Schema: personSchema()}
	result := ChatResult{
		FinishReason: "tool_calls",
		ToolCalls: []ToolCall{
			{ID: "call_1", Name: structuredOutputToolName, Arguments: `{"name":"ada","age":36}`},
		},
	}
	got := applyStructuredOutput(result, spec)
	if got.Content != `{"name":"ada","age":36}` {
		t.Errorf("Content = %q, want lifted tool arguments", got.Content)
	}
	if len(got.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %v, want synthetic call stripped", got.ToolCalls)
	}
	if got.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", got.FinishReason, "stop")
	}
}

func TestApplyStructuredOutputPreservesCallerToolCalls(t *testing.T) {
	spec := responseFormatSpec{Kind: "json_schema", Schema: personSchema()}
	result := ChatResult{
		FinishReason: "tool_calls",
		ToolCalls: []ToolCall{
			{ID: "call_1", Name: "get_weather", Arguments: `{"city":"sf"}`},
			{ID: "call_2", Name: structuredOutputToolName, Arguments: `{"name":"ada","age":36}`},
		},
	}
	got := applyStructuredOutput(result, spec)
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("ToolCalls = %v, want only the caller tool preserved", got.ToolCalls)
	}
	if got.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls to survive", got.FinishReason)
	}
}

func TestApplyStructuredOutputUnwrapsFencedContent(t *testing.T) {
	got := applyStructuredOutput(
		ChatResult{Content: "```json\n{\"name\":\"ada\",\"age\":36}\n```", FinishReason: "stop"},
		responseFormatSpec{Kind: "json_object"},
	)
	if got.Content != `{"name":"ada","age":36}` {
		t.Errorf("Content = %q, want unfenced JSON", got.Content)
	}
}

func TestApplyStructuredOutputIsNoopWithoutResponseFormat(t *testing.T) {
	original := ChatResult{Content: "```json\n{\"a\":1}\n```", FinishReason: "stop"}
	got := applyStructuredOutput(original, responseFormatSpec{})
	if got.Content != original.Content {
		t.Errorf("Content = %q, want untouched %q", got.Content, original.Content)
	}
}

func TestStructuredOutputToolCarriesCallerSchema(t *testing.T) {
	spec := parseResponseFormat(map[string]any{"response_format": map[string]any{
		"type":        "json_schema",
		"json_schema": map[string]any{"name": "person", "schema": personSchema()},
	}})
	tool, ok := structuredOutputTool(spec)
	if !ok {
		t.Fatal("structuredOutputTool() returned false for a schema request")
	}
	if tool.Name != structuredOutputToolName {
		t.Errorf("Name = %q, want %q", tool.Name, structuredOutputToolName)
	}
	if tool.Parameters["type"] != "object" {
		t.Errorf("Parameters = %v, want the caller schema", tool.Parameters)
	}
	if tool.Handler != nil {
		t.Error("Handler must stay nil so the backend observes the tool request")
	}
}

func TestStructuredOutputToolSkippedWithoutSchema(t *testing.T) {
	if _, ok := structuredOutputTool(responseFormatSpec{Kind: "json_object"}); ok {
		t.Error("json_object must not register a synthetic tool")
	}
}

func TestSDKCustomToolsRegistersStructuredOutputTool(t *testing.T) {
	params := map[string]any{"response_format": map[string]any{
		"type":        "json_schema",
		"json_schema": map[string]any{"schema": personSchema()},
	}}
	tools := sdkCustomToolsFromParams(params)
	if len(tools) != 1 || tools[0].Name != structuredOutputToolName {
		t.Fatalf("tools = %v, want the synthetic structured output tool", tools)
	}
}

func TestSDKCustomToolsSkipsStructuredToolWhenToolChoiceNone(t *testing.T) {
	params := map[string]any{
		"tool_choice": "none",
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"schema": personSchema()},
		},
	}
	if tools := sdkCustomToolsFromParams(params); len(tools) != 0 {
		t.Errorf("tools = %v, want none when tool_choice is none", tools)
	}
}

func TestStructuredOutputRepairMessagesCarryCause(t *testing.T) {
	messages := []NeutralMessage{{Role: "user", Content: "Return data"}}
	cause := validateStructuredOutput(`{"name":"ada"}`, responseFormatSpec{Kind: "json_schema", Schema: personSchema()})
	if cause == nil {
		t.Fatal("expected a validation failure to build the repair turn from")
	}
	repaired := structuredOutputRepairMessages(messages, `{"name":"ada"}`, cause)
	if len(repaired) != 3 {
		t.Fatalf("len = %d, want original + assistant + correction", len(repaired))
	}
	if repaired[1].Role != "assistant" || repaired[1].Content != `{"name":"ada"}` {
		t.Errorf("second turn = %+v, want the rejected answer", repaired[1])
	}
	if repaired[2].Role != "user" || !strings.Contains(repaired[2].Content, "age") {
		t.Errorf("third turn = %+v, want a correction naming the failure", repaired[2])
	}
	if strings.Contains(repaired[2].Content, structuredOutputFailure) {
		t.Error("correction should not leak the internal error marker to the model")
	}
}

func TestApplySDKOutputConstraintsCoercesSchemaRequest(t *testing.T) {
	params := map[string]any{"response_format": map[string]any{
		"type":        "json_schema",
		"json_schema": map[string]any{"name": "person", "schema": personSchema()},
	}}
	result := ChatResult{
		FinishReason: "tool_calls",
		ToolCalls: []ToolCall{
			{ID: "call_1", Name: structuredOutputToolName, Arguments: `{"name":"ada","age":36}`},
		},
		Usage: Usage{InputTokens: 10},
	}

	got := applySDKOutputConstraints(result, params)

	if got.Content != `{"name":"ada","age":36}` {
		t.Errorf("Content = %q, want the schema-conforming payload", got.Content)
	}
	if len(got.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %v, want the synthetic call hidden from the client", got.ToolCalls)
	}
	if got.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", got.FinishReason)
	}
	if err := validateStructuredOutput(got.Content, parseResponseFormat(params)); err != nil {
		t.Errorf("coerced content failed validation: %v", err)
	}
}

func TestApplySDKOutputConstraintsUnwrapsFencedJSONObject(t *testing.T) {
	params := map[string]any{"response_format": map[string]any{"type": "json_object"}}
	result := ChatResult{Content: "Here you go:\n```json\n{\"ok\":true}\n```", FinishReason: "stop"}

	got := applySDKOutputConstraints(result, params)

	if got.Content != `{"ok":true}` {
		t.Errorf("Content = %q, want unfenced JSON", got.Content)
	}
}

func TestApplySDKOutputConstraintsLeavesPlainRequestsAlone(t *testing.T) {
	result := ChatResult{Content: "```json\n{\"a\":1}\n```", FinishReason: "stop"}

	got := applySDKOutputConstraints(result, map[string]any{})

	if got.Content != result.Content {
		t.Errorf("Content = %q, want untouched when no response_format is set", got.Content)
	}
}

func TestStructuredOutputFailureIsNonRetryable(t *testing.T) {
	err := validateStructuredOutput("not json", responseFormatSpec{Kind: "json_object"})
	if err == nil {
		t.Fatal("expected a validation failure")
	}
	if !isNonRetryableClientError(err.Error()) {
		t.Error("schema failures must not be retried against other pooled accounts")
	}
}

func TestWithStructuredOutputToolCoversWebSearchToolSets(t *testing.T) {
	params := map[string]any{
		"tools": []map[string]any{{"type": "function", "name": "web_search"}},
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"schema": personSchema()},
		},
	}

	declared := withStructuredOutputTool(sdkCLIWebSearchTools(), params)

	found := false
	for _, tool := range declared {
		if tool.Name == structuredOutputToolName {
			found = true
		}
	}
	if !found {
		t.Fatal("web-search tool sets must also declare the structured output tool")
	}

	allowlisted := false
	for _, name := range sdkCustomToolNames(params) {
		if name == structuredOutputToolName {
			allowlisted = true
		}
	}
	if allowlisted != found {
		t.Errorf("declared=%v allowlisted=%v: Tools and AvailableTools must agree", found, allowlisted)
	}
}

func TestWithStructuredOutputToolIsIdempotent(t *testing.T) {
	params := map[string]any{"response_format": map[string]any{
		"type":        "json_schema",
		"json_schema": map[string]any{"schema": personSchema()},
	}}
	once := withStructuredOutputTool(nil, params)
	twice := withStructuredOutputTool(once, params)
	if len(twice) != len(once) {
		t.Errorf("len = %d, want no duplicate declaration (%d)", len(twice), len(once))
	}
}

func sdkCustomToolNames(params map[string]any) []string {
	var names []string
	for _, tool := range sdkCustomToolsFromParams(params) {
		names = append(names, tool.Name)
	}
	return names
}

func TestResolveJSONSchemaCachesAndBounds(t *testing.T) {
	schema := personSchema()

	first, err := resolveJSONSchema(schema)
	if err != nil {
		t.Fatalf("resolveJSONSchema() error = %v", err)
	}
	second, err := resolveJSONSchema(schema)
	if err != nil {
		t.Fatalf("resolveJSONSchema() second call error = %v", err)
	}
	if first != second {
		t.Error("identical schemas should resolve to the same cached instance")
	}

	for i := 0; i < schemaCacheSize*2; i++ {
		_, _ = resolveJSONSchema(map[string]any{
			"type":       "object",
			"properties": map[string]any{"f": map[string]any{"type": "string"}},
			"title":      strings.Repeat("x", i%7) + string(rune('a'+i%26)) + itoaTest(i),
		})
	}
	if got := schemaCache.Len(); got > schemaCacheSize {
		t.Errorf("cache length = %d, want <= %d (client-controlled keys must not grow unbounded)", got, schemaCacheSize)
	}
}

func TestResolveJSONSchemaNegativeCachesBadSchema(t *testing.T) {
	bad := map[string]any{"type": 12345}
	if _, err := resolveJSONSchema(bad); err == nil {
		t.Fatal("expected an unusable schema to fail compilation")
	}
	if _, err := resolveJSONSchema(bad); err == nil {
		t.Fatal("expected the cached failure to be reported on the second call")
	}
}

func itoaTest(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestStructuredOutputToolSurvivesWebSearchFiltering(t *testing.T) {
	if isSDKWebSearchInternalTool(structuredOutputToolName) {
		t.Errorf("%s must not be filtered as an internal web-search tool; "+
			"it shares the ghcp_ prefix, so a prefix-based refactor of "+
			"isSDKWebSearchInternalTool would silently drop structured output",
			structuredOutputToolName)
	}
	if isSDKNativeWebTool(structuredOutputToolName) {
		t.Errorf("%s must not be filtered as a native web tool", structuredOutputToolName)
	}
	for _, name := range []string{"ghcp_report_intent", "ghcp_web_fetch", "ghcp_web_search"} {
		if !isSDKWebSearchInternalTool(name) {
			t.Errorf("%s should still be filtered as an internal web-search tool", name)
		}
	}
}

func TestAccumulateUsageSumsAcrossAttempts(t *testing.T) {
	tests := []struct {
		name string
		base Usage
		next Usage
		want Usage
	}{
		{
			name: "sums tokens credits and duration",
			base: Usage{InputTokens: 10, OutputTokens: 5, CachedTokens: 2, Credits: 1.5, DurationMS: 100, TotalTokens: 15},
			next: Usage{InputTokens: 20, OutputTokens: 7, CachedTokens: 3, Credits: 2.5, DurationMS: 200, TotalTokens: 27},
			want: Usage{InputTokens: 30, OutputTokens: 12, CachedTokens: 5, Credits: 4.0, DurationMS: 300, TotalTokens: 42},
		},
		{
			name: "derives total when neither attempt reported one",
			base: Usage{InputTokens: 10, OutputTokens: 5},
			next: Usage{InputTokens: 20, OutputTokens: 7},
			want: Usage{InputTokens: 30, OutputTokens: 12, TotalTokens: 42},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := accumulateUsage(tt.base, tt.next)
			if got.InputTokens != tt.want.InputTokens || got.OutputTokens != tt.want.OutputTokens ||
				got.CachedTokens != tt.want.CachedTokens || got.TotalTokens != tt.want.TotalTokens ||
				got.Credits != tt.want.Credits || got.DurationMS != tt.want.DurationMS {
				t.Errorf("accumulateUsage() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestAccumulateUsageDiffersFromMergeSDKUsage(t *testing.T) {
	base := Usage{InputTokens: 10, OutputTokens: 5}
	next := Usage{InputTokens: 20, OutputTokens: 7}

	merged := mergeSDKUsage(base, next)
	accumulated := accumulateUsage(base, next)

	if merged.InputTokens != 20 {
		t.Errorf("mergeSDKUsage InputTokens = %d, want 20 (overwrite semantics)", merged.InputTokens)
	}
	if accumulated.InputTokens != 30 {
		t.Errorf("accumulateUsage InputTokens = %d, want 30 (billing must sum both attempts)", accumulated.InputTokens)
	}
}

// Regression coverage for the Oracle review findings.

func TestParseResponseFormatReadsResponsesTextFormat(t *testing.T) {
	params := map[string]any{
		"response_options": map[string]any{
			"text": map[string]any{
				"format": map[string]any{
					"type":   "json_schema",
					"name":   "person",
					"schema": personSchema(),
				},
			},
		},
	}
	spec := parseResponseFormat(params)
	if spec.Kind != "json_schema" {
		t.Fatalf("Kind = %q, want json_schema: /v1/responses carries the contract under text.format", spec.Kind)
	}
	if !spec.hasSchema() {
		t.Error("Responses text.format schema must be picked up")
	}
	if spec.Name != "person" {
		t.Errorf("Name = %q, want person", spec.Name)
	}
}

func TestParseResponseFormatPrefersChatResponseFormat(t *testing.T) {
	params := map[string]any{
		"response_format":  map[string]any{"type": "json_object"},
		"response_options": map[string]any{"text": map[string]any{"format": map[string]any{"type": "json_schema", "schema": personSchema()}}},
	}
	if spec := parseResponseFormat(params); spec.Kind != "json_object" {
		t.Errorf("Kind = %q, want the explicit response_format to win", spec.Kind)
	}
}

func TestReservedToolNameCollisionIsRejected(t *testing.T) {
	params := map[string]any{
		"tools": []map[string]any{
			{"type": "function", "name": structuredOutputToolName, "description": "caller's own tool"},
		},
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"schema": personSchema()},
		},
	}
	err := structuredOutputToolConflict(params)
	if err == nil {
		t.Fatal("a caller tool using the reserved name must be rejected, not silently swallowed")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("error %q should explain the name is reserved", err)
	}
}

func TestReservedNameAllowedWithoutSchemaRequest(t *testing.T) {
	params := map[string]any{
		"tools": []map[string]any{{"type": "function", "name": structuredOutputToolName}},
	}
	if err := structuredOutputToolConflict(params); err != nil {
		t.Errorf("no json_schema requested, so the name is not reserved: %v", err)
	}
}

func TestStructuredOutputToolWithheldForConstrainedToolChoice(t *testing.T) {
	tests := []struct {
		name       string
		toolChoice any
		wantUsable bool
	}{
		{name: "absent", toolChoice: nil, wantUsable: true},
		{name: "auto", toolChoice: "auto", wantUsable: true},
		{name: "none", toolChoice: "none", wantUsable: false},
		{name: "required", toolChoice: "required", wantUsable: false},
		{name: "any", toolChoice: "any", wantUsable: false},
		{
			name:       "forced specific function",
			toolChoice: map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
			wantUsable: false,
		},
		{name: "auto object", toolChoice: map[string]any{"type": "auto"}, wantUsable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := map[string]any{
				"tool_choice": tt.toolChoice,
				"response_format": map[string]any{
					"type":        "json_schema",
					"json_schema": map[string]any{"schema": personSchema()},
				},
			}
			if got := structuredOutputToolUsable(params); got != tt.wantUsable {
				t.Errorf("structuredOutputToolUsable() = %v, want %v", got, tt.wantUsable)
			}
			declared := withStructuredOutputTool(nil, params)
			if (len(declared) > 0) != tt.wantUsable {
				t.Errorf("declared %d tools, want usable=%v: a hidden tool must not satisfy a caller-visible tool_choice",
					len(declared), tt.wantUsable)
			}
		})
	}
}

func TestApplyStructuredOutputLiftsValidEmptyObject(t *testing.T) {
	spec := responseFormatSpec{Kind: "json_schema", Schema: map[string]any{"type": "object"}}
	got := applyStructuredOutput(ChatResult{
		FinishReason: "tool_calls",
		ToolCalls:    []ToolCall{{ID: "c1", Name: structuredOutputToolName, Arguments: `{}`}},
	}, spec)

	if got.Content != `{}` {
		t.Errorf("Content = %q, want {}", got.Content)
	}
	if err := validateStructuredOutput(got.Content, spec); err != nil {
		t.Errorf("{} should validate against a bare object schema: %v", err)
	}
}

func TestApplySDKOutputConstraintsKeepsSDKTokensOnLift(t *testing.T) {
	params := map[string]any{"response_format": map[string]any{
		"type":        "json_schema",
		"json_schema": map[string]any{"schema": personSchema()},
	}}
	result := ChatResult{
		FinishReason: "tool_calls",
		ToolCalls:    []ToolCall{{Name: structuredOutputToolName, Arguments: `{"name":"ada","age":36}`}},
		Usage:        Usage{InputTokens: 100, OutputTokens: 4242},
	}

	got := applySDKOutputConstraints(result, params)

	if got.Usage.OutputTokens != 4242 {
		t.Errorf("OutputTokens = %d, want the SDK-reported 4242; approximating from lifted JSON under-reports usage",
			got.Usage.OutputTokens)
	}
}

func TestResolveJSONSchemaRejectsOversizedSchema(t *testing.T) {
	props := map[string]any{}
	for i := 0; i < 20000; i++ {
		props["field_"+itoaTest(i)] = map[string]any{"type": "string", "description": strings.Repeat("d", 20)}
	}
	_, err := resolveJSONSchema(map[string]any{"type": "object", "properties": props})
	if err == nil {
		t.Fatal("an oversized schema must be rejected: the LRU bounds entry count, not bytes")
	}
	if !strings.Contains(err.Error(), "limit is") {
		t.Errorf("error %q should report the size limit", err)
	}
}

func TestReservedToolNameCollisionDetectedOnNormalizedTools(t *testing.T) {
	// normalizeTools flattens the OpenAI {"function":{"name":...}} wire shape,
	// so the conflict check must run against the flattened form the backend sees.
	req := ChatCompletionRequest{
		Model: "m",
		Tools: []map[string]any{
			{"type": "function", "function": map[string]any{
				"name":       structuredOutputToolName,
				"parameters": map[string]any{"type": "object"},
			}},
		},
		ResponseFormat: map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"schema": personSchema()},
		},
	}
	if err := structuredOutputToolConflict(req.SamplingParams()); err == nil {
		t.Fatal("collision must be detected through the normalized tool shape a real request produces")
	}
}

// TestChatStreamBuffersStructuredOutput pins the deliberate trade-off in
// chatStreamStructured: validation and repair are impossible once tokens have
// reached the client, so response_format requests are completed before any
// delta is emitted and must not take the incremental streaming path.
func TestChatStreamBuffersStructuredOutput(t *testing.T) {
	backend := NewCopilotBackendWithOptions("acct", "gho_test", "", CopilotBackendOptions{})
	messages := []NeutralMessage{{Role: "user", Content: "hi"}}

	streamed := false
	origClient, origCreate := copilotSDKClientForParams, copilotSDKCreateSession
	origDisconnect, origStream := copilotSDKDisconnectSession, copilotSDKStreamSession
	t.Cleanup(func() {
		copilotSDKClientForParams, copilotSDKCreateSession = origClient, origCreate
		copilotSDKDisconnectSession, copilotSDKStreamSession = origDisconnect, origStream
	})
	copilotSDKClientForParams = func(*CopilotBackend, context.Context, map[string]any) (*sdk.Client, func(), error) {
		return nil, func() {}, nil
	}
	copilotSDKCreateSession = func(*sdk.Client, context.Context, *sdk.SessionConfig) (*sdk.Session, error) {
		return nil, nil
	}
	copilotSDKDisconnectSession = func(*sdk.Session) {}
	copilotSDKStreamSession = func(ctx context.Context, _ *sdk.Session, _, _ string, out chan<- StreamItem) {
		streamed = true
		emitStreamItem(ctx, out, StreamItem{Kind: "done", FinishReason: "stop"})
	}

	drain := func(ch <-chan StreamItem) {
		for range ch {
		}
	}

	// A plain request must still stream incrementally.
	ch, err := backend.ChatStream(context.Background(), "m", messages, map[string]any{})
	if err != nil {
		t.Fatalf("plain ChatStream error: %v", err)
	}
	drain(ch)
	if !streamed {
		t.Fatal("a plain request must take the incremental streaming path")
	}

	// A response_format request must not reach the streaming seam at all. The
	// buffered path uses the real client, which is unauthenticated here, so it
	// fails — reaching that failure is itself the proof it did not stream.
	streamed = false
	ch, err = backend.ChatStream(context.Background(), "m", messages, map[string]any{
		"response_format": map[string]any{"type": "json_object"},
	})
	if err == nil {
		drain(ch)
	}
	if streamed {
		t.Error("a response_format request must not stream incrementally: its content cannot be validated or repaired after emission")
	}
}

func TestChatStreamStructuredIsSelectedByResponseFormat(t *testing.T) {
	for _, tt := range []struct {
		name     string
		params   map[string]any
		buffered bool
	}{
		{name: "no response_format", params: map[string]any{}},
		{name: "json_object", params: map[string]any{"response_format": map[string]any{"type": "json_object"}}, buffered: true},
		{
			name: "json_schema",
			params: map[string]any{"response_format": map[string]any{
				"type": "json_schema", "json_schema": map[string]any{"schema": personSchema()}}},
			buffered: true,
		},
		{
			name: "responses text.format",
			params: map[string]any{"response_options": map[string]any{
				"text": map[string]any{"format": map[string]any{"type": "json_object"}}}},
			buffered: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseResponseFormat(tt.params).wantsJSON(); got != tt.buffered {
				t.Errorf("wantsJSON() = %v, want %v: this is what selects the buffered stream path", got, tt.buffered)
			}
		})
	}
}
