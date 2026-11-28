package openai

import (
	"context"
	"encoding/json"
	"testing"

	"agy-gateway/internal/media"
)

// tinyImagePNG is a 1×1 transparent PNG, base64 without the data: prefix.
const tinyImagePNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// imageTestResolver accepts only the two types the fixture uses and never touches the network.
func imageTestResolver() *media.Resolver {
	return media.NewResolver(media.Config{
		MaxBytes:         1 << 20,
		AllowedMimeTypes: []string{"image/png", "image/jpeg"},
	})
}

// TestTranslateWithMedia_ForwardsImageBlock pins the multimodal path.
//
// @purpose Proves a consumer image part becomes a real ACP image block with its payload.
// @rationale The old decoder dropped image parts entirely (ContentString concatenated only text),
// so a vision request silently arrived as a text-only turn and the agent answered about nothing.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no network.
func TestTranslateWithMedia_ForwardsImageBlock(t *testing.T) {
	tr := NewTranslatorWithBudget(0).WithMedia(imageTestResolver())
	req := &ChatCompletionRequest{
		Model: "gemini-3.8-flash",
		Messages: []ChatMessage{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": "What colour is this?"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/png;base64," + tinyImagePNG,
				}},
			},
		}},
	}

	_, blocks, err := tr.TranslateWithMedia(context.Background(), req, "")
	if err != nil {
		t.Fatalf("TranslateWithMedia: %v", err)
	}

	var images []int
	for i, b := range blocks {
		if b.Type == "image" {
			images = append(images, i)
			if b.Data != tinyImagePNG {
				t.Fatalf("image block payload does not match the source image")
			}
			if b.MimeType != "image/png" {
				t.Fatalf("image block mimeType = %q, want image/png", b.MimeType)
			}
		}
	}
	if len(images) != 1 {
		t.Fatalf("expected exactly one image block, got %d (blocks: %d)", len(images), len(blocks))
	}

	// The declaration that the agent reads must be present too, otherwise ask about "this" fails.
	joined := ""
	for _, b := range blocks {
		if b.Type == "text" {
			joined += b.Text
		}
	}
	if joined == "" {
		t.Fatal("a multimodal request must still carry its text")
	}
}

// TestTranslateWithMedia_RejectsDisallowedScheme pins the policy boundary.
//
// @purpose Proves a consumer-supplied URL that the media policy forbids is refused, not fetched.
// @rationale Without this the gateway becomes an arbitrary-URL fetcher for any API caller.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no network.
func TestTranslateWithMedia_RejectsDisallowedScheme(t *testing.T) {
	tr := NewTranslatorWithBudget(0).WithMedia(imageTestResolver())
	req := &ChatCompletionRequest{
		Model: "gemini-3.8-flash",
		Messages: []ChatMessage{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": "read this"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "file:///etc/passwd"}},
			},
		}},
	}

	_, _, err := tr.TranslateWithMedia(context.Background(), req, "")
	if err == nil {
		t.Fatal("a file: image reference must be refused by the media policy")
	}
}

// TestTranslate_WithoutMediaStaysTextOnly pins the default (no policy wired) path.
//
// @purpose Proves the plain Translate entry point never resolves images.
// @rationale Callers that did not opt into media must not gain network fetches; and internal
// re-renders (history replay) must not re-upload attachments.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no network.
func TestTranslate_WithoutMediaStaysTextOnly(t *testing.T) {
	tr := NewTranslatorWithBudget(0)
	req := &ChatCompletionRequest{
		Model: "gemini-3.8-flash",
		Messages: []ChatMessage{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": "hello"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://127.0.0.1/x.png"}},
			},
		}},
	}

	_, blocks := tr.Translate(req, "")
	for _, b := range blocks {
		if b.Type == "image" {
			t.Fatal("Translate without a media resolver must not emit image blocks")
		}
	}
}

// TestContentParts_DecodesAllImageShapes pins the decoder that makes the above possible.
//
// @purpose Proves every image_part spelling OpenAI clients emit is understood.
// @rationale Several harnesses send the nested map form or a part-level `url`; only one shape was
// ever handled, so the rest were silently dropped.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Pure.
func TestContentParts_DecodesAllImageShapes(t *testing.T) {
	msg := ChatMessage{Content: []any{
		map[string]any{"type": "text", "text": "a"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://x/1.png"}},
		map[string]any{"type": "image_url", "image_url": "http://x/2.png"},
		map[string]any{"type": "input_image", "url": "http://x/3.png"},
		map[string]any{"type": "input_text", "text": "b"},
		map[string]any{"type": "not_a_part"},
	}}
	parts := msg.ContentParts()

	var texts, images []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			texts = append(texts, p.Text)
		case "image_url":
			if p.ImageURL != nil {
				images = append(images, p.ImageURL.URL)
			}
		}
	}
	if len(texts) != 2 || len(images) != 3 {
		t.Fatalf("decoded text=%v images=%v", texts, images)
	}
	if !msg.HasImage() {
		t.Fatal("HasImage must be true when image parts exist")
	}

	// JSON round-trip through the wire type must not lose them either.
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ChatMessage
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.ContentParts()) != len(parts) {
		t.Fatalf("wire round-trip lost parts: %d != %d", len(back.ContentParts()), len(parts))
	}
}
