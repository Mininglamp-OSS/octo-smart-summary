package pipeline

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// richTextImagePlaceholder is injected for image blocks when assembling the
// plain text of a RichText (type=14) message. Mirrors octo-lib's
// common.RichTextImagePlaceholder so summaries read consistently across repos.
const richTextImagePlaceholder = "[图片]"

// richTextBlock is a single element of a RichText (type=14) content array.
// Schema mirrors octo-lib common.RichTextBlock: a text block carries Text, an
// image block is rendered as a placeholder in plain text.
type richTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// payloadData is the decoded message payload.
//
// Content is kept as json.RawMessage so a RichText (type=14) payload — whose
// content may be either an ordered block array or (for backward compatibility)
// a plain string — can be decoded for every message type without breaking the
// existing type==1 string path. Plain is the top-level redundant plain text the
// server generates for RichText messages.
type payloadData struct {
	Type    int             `json:"type"`
	Content json.RawMessage `json:"content"`
	Plain   string          `json:"plain"`
}

// contentString returns the content interpreted as a JSON string. ok is false
// when content is absent or not a JSON string (e.g. a RichText block array).
func (d payloadData) contentString() (string, bool) {
	if len(d.Content) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(d.Content, &s); err != nil {
		return "", false
	}
	return s, true
}

// richTextPlain assembles the display/summary text for a RichText (type=14)
// payload: prefer the server-generated top-level plain; otherwise traverse the
// content block array (text blocks contribute their text, image blocks inject
// the [图片] placeholder); finally fall back to a string content for backward
// compatibility with the legacy string-content path.
func (d payloadData) richTextPlain() string {
	if strings.TrimSpace(d.Plain) != "" {
		return d.Plain
	}
	if len(d.Content) > 0 {
		var blocks []richTextBlock
		if err := json.Unmarshal(d.Content, &blocks); err == nil {
			var b strings.Builder
			for _, blk := range blocks {
				switch blk.Type {
				case "image":
					b.WriteString(richTextImagePlaceholder)
				default:
					// text block, or unknown type carrying text (forward-compatible).
					b.WriteString(blk.Text)
				}
			}
			if b.Len() > 0 {
				return b.String()
			}
		}
	}
	// Backward compatibility: legacy RichText with string content.
	if s, ok := d.contentString(); ok {
		return s
	}
	return ""
}

// extractFromData pulls the summary text out of a decoded payload.
//   - type==1 (text): return the string content;
//   - type==14 (RichText / 图文混排): return the assembled plain text;
//   - otherwise: ("", false).
func extractFromData(data payloadData) (string, bool) {
	switch data.Type {
	case 1:
		if s, ok := data.contentString(); ok && s != "" {
			return s, true
		}
		return "", false
	case 14:
		if text := data.richTextPlain(); text != "" {
			return text, true
		}
		return "", false
	default:
		return "", false
	}
}

// ExtractText decodes a message payload (raw JSON or base64-encoded JSON).
// Returns the text content for type==1 (plain text) and type==14 (RichText /
// 图文混排); otherwise returns ("", false).
func ExtractText(payload []byte) (string, bool) {
	if len(payload) == 0 {
		return "", false
	}

	// Try raw JSON first
	var data payloadData
	if err := json.Unmarshal(payload, &data); err == nil {
		return extractFromData(data)
	}

	// Try base64 decode then JSON
	decoded, err := base64.StdEncoding.DecodeString(string(payload))
	if err != nil {
		// Try URL-safe base64
		decoded, err = base64.URLEncoding.DecodeString(string(payload))
		if err != nil {
			// Try raw base64 (no padding)
			decoded, err = base64.RawStdEncoding.DecodeString(string(payload))
			if err != nil {
				return "", false
			}
		}
	}

	if err := json.Unmarshal(decoded, &data); err != nil {
		return "", false
	}
	return extractFromData(data)
}
