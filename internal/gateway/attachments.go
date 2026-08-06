package gateway

// Image attachments across the three client protocols.
//
// The Copilot SDK carries images as message attachments, not as content blocks,
// so the protocol-specific image shapes are normalized to a single neutral
// representation and converted once at the SDK boundary.
//
// Each protocol edge rewrites its own image blocks into the OpenAI `image_url`
// shape, so only one extractor has to understand content parts:
//
//	OpenAI     content[].image_url.url          (data: URI or http URL)
//	Anthropic  content[].source.{data,url}      with type image
//	Gemini     parts[].inlineData / .fileData

import (
	"encoding/base64"
	"fmt"
	"strings"

	sdk "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

// maxAttachmentBytes caps a single decoded image. The runtime resizes images
// that exceed model limits, but the gateway still holds the payload in memory
// per in-flight request, and the size is client-controlled.
const maxAttachmentBytes = 8 << 20

// MessageAttachment is the neutral form of an image supplied by any protocol.
type MessageAttachment struct {
	// MIMEType is the declared media type, e.g. "image/png".
	MIMEType string
	// Data is base64-encoded content, without a data: URI prefix.
	Data string
	// Name is an optional display name.
	Name string
}

// imageURLPart builds the OpenAI content part that every protocol edge
// normalizes to, so attachment extraction has a single shape to understand.
func imageURLPart(mimeType, base64Data string) map[string]any {
	return map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "data:" + mimeType + ";base64," + base64Data},
	}
}

// attachmentsFromContent collects images from a message content value.
//
// Unparseable or oversized entries are skipped rather than failing the request;
// callers that need to know whether anything was dropped compare counts.
func attachmentsFromContent(content any) []MessageAttachment {
	parts, ok := content.([]any)
	if !ok {
		return nil
	}
	var out []MessageAttachment
	for _, part := range parts {
		block, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if att, ok := attachmentFromBlock(block); ok {
			out = append(out, att)
		}
	}
	return out
}

func attachmentFromBlock(block map[string]any) (MessageAttachment, bool) {
	switch strings.ToLower(stringFromAny(block["type"])) {
	case "image_url":
		url := stringFromAny(anyMap(block["image_url"])["url"])
		if url == "" {
			url = stringFromAny(block["image_url"])
		}
		return attachmentFromDataURI(url)
	case "image", "input_image":
		if source := anyMap(block["source"]); source != nil {
			if data := stringFromAny(source["data"]); data != "" {
				mime := firstNonEmpty(
					stringFromAny(source["media_type"]),
					stringFromAny(source["mime_type"]),
					"image/png")
				return newAttachment(mime, data)
			}
			if att, ok := attachmentFromDataURI(stringFromAny(source["url"])); ok {
				return att, true
			}
		}
		if url := stringFromAny(block["image_url"]); url != "" {
			return attachmentFromDataURI(url)
		}
	}
	return MessageAttachment{}, false
}

// attachmentFromDataURI accepts only inline data: URIs. A remote URL cannot be
// forwarded as a blob, and fetching it server-side would turn any request into
// an outbound fetch the caller did not authorize.
func attachmentFromDataURI(uri string) (MessageAttachment, bool) {
	uri = strings.TrimSpace(uri)
	if !strings.HasPrefix(uri, "data:") {
		return MessageAttachment{}, false
	}
	rest := uri[len("data:"):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return MessageAttachment{}, false
	}
	meta, payload := rest[:comma], rest[comma+1:]
	if !strings.Contains(meta, ";base64") {
		return MessageAttachment{}, false
	}
	mime := strings.TrimSpace(strings.SplitN(meta, ";", 2)[0])
	if mime == "" {
		mime = "image/png"
	}
	return newAttachment(mime, payload)
}

func newAttachment(mimeType, data string) (MessageAttachment, bool) {
	data = strings.TrimSpace(data)
	if data == "" {
		return MessageAttachment{}, false
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return MessageAttachment{}, false
	}
	if len(decoded) > maxAttachmentBytes {
		return MessageAttachment{}, false
	}
	return MessageAttachment{MIMEType: mimeType, Data: data}, true
}

// countImageBlocks reports how many image-shaped blocks a content value holds,
// including ones that failed to convert, so a silent drop can be detected.
func countImageBlocks(content any) int {
	parts, ok := content.([]any)
	if !ok {
		return 0
	}
	n := 0
	for _, part := range parts {
		block, ok := part.(map[string]any)
		if !ok {
			continue
		}
		switch strings.ToLower(stringFromAny(block["type"])) {
		case "image_url", "image", "input_image":
			n++
		}
	}
	return n
}

// attachmentDropError reports images the gateway accepted syntactically but
// cannot forward. Returning an error beats the previous behavior, where a
// vision request quietly became a text request and the model guessed.
func attachmentDropError(declared, converted int) error {
	if declared <= converted {
		return nil
	}
	return fmt.Errorf(
		"%d of %d image attachments could not be forwarded: only inline data: URIs with base64 payloads under %d bytes are supported",
		declared-converted, declared, maxAttachmentBytes)
}

// sdkAttachments converts collected images to the SDK's blob form and reports
// any that were declared but could not be forwarded.
func sdkAttachments(messages []NeutralMessage) ([]sdk.Attachment, error) {
	declared, converted := 0, 0
	var out []sdk.Attachment
	for _, m := range messages {
		declared += m.DeclaredImages
		for _, att := range m.Attachments {
			converted++
			out = append(out, rpc.AttachmentBlob{
				Data:     att.Data,
				MIMEType: att.MIMEType,
			})
		}
	}
	if err := attachmentDropError(declared, converted); err != nil {
		return nil, err
	}
	return out, nil
}
