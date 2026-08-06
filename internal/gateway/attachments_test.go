package gateway

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// redPNG is a 1x1 red PNG, small enough to keep inline.
const redPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAAEklEQVR4nGP8z4AAjAxQFhAAAA8YASWTeR8AAAAASUVORK5CYII="

func dataURI(mime, b64 string) string { return "data:" + mime + ";base64," + b64 }

func TestOpenAIImageURLBecomesAttachment(t *testing.T) {
	req := ChatCompletionRequest{
		Model: "m",
		Messages: []ChatMessage{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": "what colour?"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURI("image/png", redPNG)}},
			},
		}},
	}
	msgs := req.NeutralMessages()
	if len(msgs) != 1 || len(msgs[0].Attachments) != 1 {
		t.Fatalf("attachments = %+v, want exactly one", msgs[0].Attachments)
	}
	if got := msgs[0].Attachments[0].MIMEType; got != "image/png" {
		t.Errorf("MIMEType = %q, want image/png", got)
	}
	if msgs[0].Attachments[0].Data != redPNG {
		t.Error("base64 payload was altered")
	}
	if !strings.Contains(msgs[0].Content, "what colour?") {
		t.Errorf("text lost alongside the image: %q", msgs[0].Content)
	}
}

func TestAnthropicImageBlockBecomesAttachment(t *testing.T) {
	req := AnthropicMessagesRequest{
		Model: "claude-sonnet-4.6",
		Messages: []AnthropicMessage{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": "what colour?"},
				map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": "image/png", "data": redPNG,
				}},
			},
		}},
	}
	msgs := req.ToChatRequest().NeutralMessages()
	var found int
	for _, m := range msgs {
		found += len(m.Attachments)
	}
	if found != 1 {
		t.Fatalf("attachments = %d, want 1 (messages=%+v)", found, msgs)
	}
	joined := ""
	for _, m := range msgs {
		joined += m.Content
	}
	if !strings.Contains(joined, "what colour?") {
		t.Errorf("text lost alongside the image: %q", joined)
	}
}

func TestGeminiInlineDataBecomesAttachment(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[
		{"text":"what colour?"},
		{"inlineData":{"mimeType":"image/png","data":"` + redPNG + `"}}
	]}]}`
	var req GeminiGenerateContentRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	chat, err := geminiToChatRequest("gpt-5-mini", req, false)
	if err != nil {
		t.Fatalf("geminiToChatRequest: %v", err)
	}
	msgs := chat.NeutralMessages()
	var found int
	for _, m := range msgs {
		found += len(m.Attachments)
	}
	if found != 1 {
		t.Fatalf("attachments = %d, want 1 (messages=%+v)", found, msgs)
	}
}

func TestAttachmentRejectsUnsupportedSources(t *testing.T) {
	tests := []struct {
		name  string
		block map[string]any
	}{
		{name: "remote http url", block: map[string]any{
			"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}}},
		{name: "data uri without base64", block: map[string]any{
			"type": "image_url", "image_url": map[string]any{"url": "data:image/png,rawbytes"}}},
		{name: "corrupt base64", block: map[string]any{
			"type": "image_url", "image_url": map[string]any{"url": dataURI("image/png", "!!!not base64!!!")}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := attachmentFromBlock(tt.block); ok {
				t.Error("must not be accepted as an attachment")
			}
			if countImageBlocks([]any{tt.block}) != 1 {
				t.Error("must still be counted as declared, so the drop is reported")
			}
		})
	}
}

func TestAttachmentRejectsOversizedPayload(t *testing.T) {
	big := base64.StdEncoding.EncodeToString(make([]byte, maxAttachmentBytes+1))
	if _, ok := newAttachment("image/png", big); ok {
		t.Errorf("a payload above %d bytes must be rejected", maxAttachmentBytes)
	}
}

func TestUnforwardableImageIsAnErrorNotASilentDrop(t *testing.T) {
	// The previous behavior turned a vision request into a text request and let
	// the model guess. A declared-but-unconvertible image must surface instead.
	msgs := []NeutralMessage{{Role: "user", Content: "hi", DeclaredImages: 1}}
	if _, err := sdkAttachments(msgs); err == nil {
		t.Fatal("expected an error for an image that could not be forwarded")
	}
}

func TestForwardableImagesProduceBlobs(t *testing.T) {
	msgs := []NeutralMessage{{
		Role:           "user",
		Content:        "hi",
		DeclaredImages: 1,
		Attachments:    []MessageAttachment{{MIMEType: "image/png", Data: redPNG}},
	}}
	atts, err := sdkAttachments(msgs)
	if err != nil {
		t.Fatalf("sdkAttachments: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("attachments = %d, want 1", len(atts))
	}
	if string(atts[0].Type()) != "blob" {
		t.Errorf("attachment type = %q, want blob", atts[0].Type())
	}
}
