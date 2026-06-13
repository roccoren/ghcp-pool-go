package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstInt(raw string, values ...int) int {
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			return n
		}
	}
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func firstFloat(raw string, values ...float64) float64 {
	if raw != "" {
		if n, err := strconv.ParseFloat(raw, 64); err == nil {
			return n
		}
	}
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func envBool(name string, fallback bool) bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if raw == "" {
		return fallback
	}
	return raw == "1" || raw == "true" || raw == "yes" || raw == "on"
}

func ptrValue[T any](ptr *T) any {
	if ptr == nil {
		return nil
	}
	return *ptr
}

func emptyToNil(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nilIfEmptyMap(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	return m
}

func globMatch(pattern, value string) bool {
	ok, err := path.Match(pattern, value)
	return err == nil && ok
}

func matchesAny(value string, patterns []string) bool {
	for _, pattern := range patterns {
		if globMatch(pattern, value) {
			return true
		}
	}
	return false
}

func newID(prefix string) string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(buf[:])
}

func coerceText(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case map[string]any:
		return coerceText([]any{v})
	case []any:
		parts := make([]string, 0, len(v))
		for _, piece := range v {
			switch p := piece.(type) {
			case string:
				parts = append(parts, p)
			case map[string]any:
				ptype, _ := p["type"].(string)
				switch {
				case ptype == "refusal" && p["refusal"] != nil:
					parts = append(parts, fmt.Sprint(p["refusal"]))
				case ptype == "thinking" && p["thinking"] != nil:
					parts = append(parts, fmt.Sprint(p["thinking"]))
				case p["media_type"] == "text/plain" && p["data"] != nil:
					parts = append(parts, fmt.Sprint(p["data"]))
				case isTextPart(ptype) || (ptype == "" && p["text"] != nil):
					if p["text"] != nil {
						parts = append(parts, fmt.Sprint(p["text"]))
					}
				case ptype == "document":
					doc := []string{}
					if p["title"] != nil {
						doc = append(doc, fmt.Sprint(p["title"]))
					}
					if p["context"] != nil {
						doc = append(doc, fmt.Sprint(p["context"]))
					}
					if source := coerceText(p["source"]); source != "" {
						doc = append(doc, source)
					}
					if len(doc) > 0 {
						parts = append(parts, strings.Join(doc, "\n"))
					}
				case ptype == "search_result":
					doc := []string{}
					if p["title"] != nil {
						doc = append(doc, fmt.Sprint(p["title"]))
					}
					if p["source"] != nil {
						doc = append(doc, fmt.Sprint(p["source"]))
					}
					if text := coerceText(p["content"]); text != "" {
						doc = append(doc, text)
					}
					if len(doc) > 0 {
						parts = append(parts, strings.Join(doc, "\n"))
					}
				case isToolResultPart(ptype):
					if text := coerceText(p["content"]); text != "" {
						parts = append(parts, text)
					}
				case p["content"] != nil:
					if text := coerceText(p["content"]); text != "" {
						parts = append(parts, text)
					}
				}
			default:
				if piece != nil {
					parts = append(parts, fmt.Sprint(piece))
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprint(v)
	}
}

func isTextPart(ptype string) bool {
	switch ptype {
	case "text", "input_text", "output_text", "summary_text", "refusal":
		return true
	default:
		return false
	}
}

func isToolResultPart(ptype string) bool {
	switch ptype {
	case "tool_result", "web_search_tool_result", "web_fetch_tool_result", "code_execution_tool_result", "bash_code_execution_tool_result", "text_editor_code_execution_tool_result", "tool_search_tool_result":
		return true
	default:
		return false
	}
}

func normalizeTools(tools []map[string]any) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		fn := tool
		if nested, ok := tool["function"].(map[string]any); ok {
			fn = nested
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		kind, _ := tool["type"].(string)
		if kind == "" {
			kind = "function"
		}
		desc, _ := fn["description"].(string)
		params := fn["parameters"]
		if params == nil {
			params = fn["input_schema"]
		}
		spec := map[string]any{
			"type":        kind,
			"name":        name,
			"description": desc,
			"parameters":  params,
		}
		if format := firstAny(fn["format"], tool["format"]); format != nil {
			spec["format"] = format
		}
		out = append(out, spec)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func firstAny(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func sortedKeys[M ~map[string]V, V any](m M) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func compactMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for _, key := range sortedKeys(m) {
		if m[key] != nil {
			out[key] = m[key]
		}
	}
	return out
}

func toJSONString(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

var familyRE = regexp.MustCompile(`^[A-Za-z]+`)

func modelFamily(model string) string {
	match := familyRE.FindString(model)
	if match == "" {
		return "other"
	}
	return strings.ToLower(match)
}

func charChunks(text string, size int) []string {
	if size <= 0 {
		size = 24
	}
	if text == "" {
		return nil
	}
	chunks := []string{}
	runes := []rune(text)
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[i:end]))
	}
	return chunks
}
