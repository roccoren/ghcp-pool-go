package gateway

import (
	"strings"
	"testing"
)

func TestApproxTokensHandlesNonSpaceDelimitedScripts(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		atLeast int
	}{
		{name: "empty", text: "   ", atLeast: 0},
		{name: "english sentence", text: "Hello world, how are you today?", atLeast: 5},
		{name: "chinese sentence", text: "你好世界，今天过得怎么样？", atLeast: 8},
		{name: "chinese paragraph", text: strings.Repeat("这是一段用于测试令牌计数的中文文本内容。", 15), atLeast: 200},
		{name: "japanese", text: "こんにちは世界、今日はどうですか？", atLeast: 10},
		{name: "code without spaces", text: "const x=Math.floor(a/b)+c*d;", atLeast: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := approxTokens(tt.text)
			if got < tt.atLeast {
				t.Errorf("approxTokens(%d chars) = %d, want >= %d; whitespace-splitting collapses non-space-delimited scripts to one token",
					len([]rune(tt.text)), got, tt.atLeast)
			}
		})
	}
}

func TestApproxTokensComparableAcrossScripts(t *testing.T) {
	// Equivalent information content should not differ by two orders of magnitude.
	cn := approxTokens(strings.Repeat("这是一段中文文本。", 20))
	en := approxTokens(strings.Repeat("This is an English text. ", 20))
	if cn*10 < en || en*10 < cn {
		t.Errorf("chinese=%d english=%d: counts for comparable text must stay within an order of magnitude", cn, en)
	}
}

func TestNormalizedRecomputesStaleTotal(t *testing.T) {
	// Usage is seeded before the output count is known, which latched TotalTokens
	// to the input count for the rest of the turn.
	seeded := Usage{InputTokens: 35}.Normalized()
	if seeded.TotalTokens != 35 {
		t.Fatalf("seeded total = %d, want 35", seeded.TotalTokens)
	}
	seeded.OutputTokens = 324
	got := seeded.Normalized()
	if got.TotalTokens != 359 {
		t.Errorf("TotalTokens = %d, want 359: a total below input+output is stale and must be recomputed", got.TotalTokens)
	}
}

func TestNormalizedKeepsLargerUpstreamTotal(t *testing.T) {
	// Some providers report a total that includes reasoning tokens not present in
	// input+output; that must survive.
	got := Usage{InputTokens: 10, OutputTokens: 20, TotalTokens: 100}.Normalized()
	if got.TotalTokens != 100 {
		t.Errorf("TotalTokens = %d, want 100 preserved", got.TotalTokens)
	}
}

func TestTruncateApproxTokensPreservesNonSpaceText(t *testing.T) {
	cn := strings.Repeat("中文内容", 50)
	truncated, ok := truncateApproxTokens(cn, 10)
	if !ok {
		t.Fatalf("a %d-char chinese string must be truncatable to 10 tokens", len([]rune(cn)))
	}
	if strings.Contains(truncated, " ") {
		t.Errorf("truncated text gained spaces, corrupting the content: %q", truncated)
	}
	if len([]rune(truncated)) >= len([]rune(cn)) {
		t.Errorf("truncated length %d not shorter than original %d", len([]rune(truncated)), len([]rune(cn)))
	}
}

func TestTruncateApproxTokensKeepsEnglishWords(t *testing.T) {
	en := strings.Repeat("alpha beta gamma delta ", 20)
	truncated, ok := truncateApproxTokens(en, 5)
	if !ok {
		t.Fatal("english text must still be truncatable")
	}
	if len([]rune(truncated)) >= len([]rune(en)) {
		t.Error("truncation did not shorten the text")
	}
}

func TestStreamTerminalUsageKeepsInputSide(t *testing.T) {
	// A locally triggered terminal frame (stop sequence / max_tokens) used to
	// construct a fresh Usage from the emitted text alone, dropping the prompt
	// side entirely.
	in := make(chan StreamItem, 4)
	in <- StreamItem{Kind: "done", Usage: Usage{InputTokens: 40}}
	close(in)
	items := []StreamItem{}
	for item := range applyStreamOutputConstraints(t.Context(), in, nil) {
		items = append(items, item)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if got := items[0].Usage.InputTokens; got != 40 {
		t.Errorf("InputTokens = %d, want 40 carried through", got)
	}
}

func TestStreamStopSequenceTerminalReportsBothSides(t *testing.T) {
	in := make(chan StreamItem, 4)
	in <- StreamItem{Kind: "done", Usage: Usage{InputTokens: 40}}
	close(in)
	drain := applyStreamOutputConstraints(t.Context(), in, nil)
	for range drain {
	}

	in2 := make(chan StreamItem, 4)
	in2 <- StreamItem{Kind: "delta", Text: "hello STOP tail"}
	in2 <- StreamItem{Kind: "done", Usage: Usage{InputTokens: 40}}
	close(in2)
	var done StreamItem
	for item := range applyStreamOutputConstraints(t.Context(), in2, map[string]any{"stop": []any{"STOP"}}) {
		if item.Kind == "done" {
			done = item
		}
	}
	if done.FinishReason != "stop" {
		t.Fatalf("finish = %q, want stop", done.FinishReason)
	}
	if done.Usage.InputTokens == 0 {
		t.Error("a stop-sequence terminal must still report the input side")
	}
	if done.Usage.TotalTokens < done.Usage.InputTokens+done.Usage.OutputTokens {
		t.Errorf("total %d is below input+output", done.Usage.TotalTokens)
	}
}
