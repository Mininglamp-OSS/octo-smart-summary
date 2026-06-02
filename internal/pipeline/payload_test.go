package pipeline

import (
	"encoding/base64"
	"testing"
)

func TestExtractText_RawJSONType1(t *testing.T) {
	payload := []byte(`{"type":1,"content":"hello world"}`)
	text, ok := ExtractText(payload)
	if !ok {
		t.Fatal("expected ok=true for type=1 raw JSON")
	}
	if text != "hello world" {
		t.Errorf("got %q, want %q", text, "hello world")
	}
}

func TestExtractText_RawJSONType2(t *testing.T) {
	payload := []byte(`{"type":2,"content":"image.png"}`)
	text, ok := ExtractText(payload)
	if ok {
		t.Fatal("expected ok=false for type=2")
	}
	if text != "" {
		t.Errorf("got %q, want empty", text)
	}
}

func TestExtractText_Base64Type1(t *testing.T) {
	raw := `{"type":1,"content":"base64 text message"}`
	encoded := base64.StdEncoding.EncodeToString([]byte(raw))
	text, ok := ExtractText([]byte(encoded))
	if !ok {
		t.Fatal("expected ok=true for base64 encoded type=1")
	}
	if text != "base64 text message" {
		t.Errorf("got %q, want %q", text, "base64 text message")
	}
}

func TestExtractText_NilPayload(t *testing.T) {
	text, ok := ExtractText(nil)
	if ok {
		t.Fatal("expected ok=false for nil payload")
	}
	if text != "" {
		t.Errorf("got %q, want empty", text)
	}
}

func TestExtractText_EmptyPayload(t *testing.T) {
	text, ok := ExtractText([]byte{})
	if ok {
		t.Fatal("expected ok=false for empty payload")
	}
	if text != "" {
		t.Errorf("got %q, want empty", text)
	}
}

func TestExtractText_InvalidJSON(t *testing.T) {
	payload := []byte(`not json at all`)
	text, ok := ExtractText(payload)
	if ok {
		t.Fatal("expected ok=false for invalid JSON")
	}
	if text != "" {
		t.Errorf("got %q, want empty", text)
	}
}

func TestExtractText_EmptyContent(t *testing.T) {
	payload := []byte(`{"type":1,"content":""}`)
	text, ok := ExtractText(payload)
	if ok {
		t.Fatal("expected ok=false for empty content")
	}
	if text != "" {
		t.Errorf("got %q, want empty", text)
	}
}

// --- RichText (type=14) ---

func TestExtractText_RichTextPlainOnly(t *testing.T) {
	payload := []byte(`{"type":14,"plain":"看这张图 [图片] 不错"}`)
	text, ok := ExtractText(payload)
	if !ok {
		t.Fatal("expected ok=true for type=14 with plain")
	}
	if text != "看这张图 [图片] 不错" {
		t.Errorf("got %q, want %q", text, "看这张图 [图片] 不错")
	}
}

func TestExtractText_RichTextBlocksOnly(t *testing.T) {
	payload := []byte(`{"type":14,"content":[{"type":"text","text":"看这张图 "},{"type":"image","url":"https://x/a.png","width":10,"height":10},{"type":"text","text":" 不错"}]}`)
	text, ok := ExtractText(payload)
	if !ok {
		t.Fatal("expected ok=true for type=14 with blocks")
	}
	if text != "看这张图 [图片] 不错" {
		t.Errorf("got %q, want %q", text, "看这张图 [图片] 不错")
	}
}

func TestExtractText_RichTextPlainAndBlocks(t *testing.T) {
	// plain takes precedence over re-traversing blocks.
	payload := []byte(`{"type":14,"plain":"权威纯文本","content":[{"type":"text","text":"忽略我"},{"type":"image","url":"https://x/a.png","width":10,"height":10}]}`)
	text, ok := ExtractText(payload)
	if !ok {
		t.Fatal("expected ok=true for type=14 with plain+blocks")
	}
	if text != "权威纯文本" {
		t.Errorf("got %q, want %q", text, "权威纯文本")
	}
}

func TestExtractText_RichTextLegacyStringContent(t *testing.T) {
	// Backward compatibility: legacy RichText whose content is a plain string.
	payload := []byte(`{"type":14,"content":"老版本字符串内容"}`)
	text, ok := ExtractText(payload)
	if !ok {
		t.Fatal("expected ok=true for type=14 with legacy string content")
	}
	if text != "老版本字符串内容" {
		t.Errorf("got %q, want %q", text, "老版本字符串内容")
	}
}

func TestExtractText_RichTextImageOnly(t *testing.T) {
	payload := []byte(`{"type":14,"content":[{"type":"image","url":"https://x/a.png","width":10,"height":10}]}`)
	text, ok := ExtractText(payload)
	if !ok {
		t.Fatal("expected ok=true for type=14 image-only block")
	}
	if text != "[图片]" {
		t.Errorf("got %q, want %q", text, "[图片]")
	}
}

func TestExtractText_RichTextEmpty(t *testing.T) {
	payload := []byte(`{"type":14,"content":[]}`)
	text, ok := ExtractText(payload)
	if ok {
		t.Fatal("expected ok=false for type=14 with empty content and no plain")
	}
	if text != "" {
		t.Errorf("got %q, want empty", text)
	}
}

func TestExtractText_RichTextBase64(t *testing.T) {
	raw := `{"type":14,"content":[{"type":"text","text":"base64 富文本"},{"type":"image","url":"https://x/a.png","width":10,"height":10}]}`
	encoded := base64.StdEncoding.EncodeToString([]byte(raw))
	text, ok := ExtractText([]byte(encoded))
	if !ok {
		t.Fatal("expected ok=true for base64 type=14")
	}
	if text != "base64 富文本[图片]" {
		t.Errorf("got %q, want %q", text, "base64 富文本[图片]")
	}
}
