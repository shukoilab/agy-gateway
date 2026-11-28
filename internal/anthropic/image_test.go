package anthropic

import (
	"context"
	"testing"

	"agy-gateway/internal/media"
)

// tinyAnthropicPNG is a 1×1 transparent PNG, base64 without the data: prefix.
const tinyAnthropicPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// imageTestResolver accepts the two media types the fixtures use and never touches the network.
func anthropicImageTestResolver() *media.Resolver {
	return media.NewResolver(media.Config{
		MaxBytes:         1 << 20,
		AllowedMimeTypes: []string{"image/png", "image/jpeg"},
	})
}

// TestTranslateWithMedia_AnthropicBase64Image pins the base64 path.
//
// @purpose Proves an Anthropic base64 image block becomes a real ACP image block.
// @rationale The history renderer replaced images with the literal "[Image Attached]", so vision
// requests reached the agent as text and the answer was fabricated.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no network.
func TestTranslateWithMedia_AnthropicBase64Image(t *testing.T) {
	tr := NewTranslatorWithBudget(0).WithMedia(anthropicImageTestResolver())
	req := &MessageRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 256,
		Messages: []Message{{
			Role: "user",
			Content: MessageContent{Blocks: []ContentBlock{
				{Type: "text", Text: "What is in this image?"},
				{Type: "image", Source: &ImageSource{
					Type: "base64", MediaType: "image/png", Data: tinyAnthropicPNG,
				}},
			}},
		}},
	}

	_, blocks, err := tr.TranslateWithMedia(context.Background(), req, "")
	if err != nil {
		t.Fatalf("TranslateWithMedia: %v", err)
	}

	var found bool
	for _, b := range blocks {
		if b.Type != "image" {
			continue
		}
		found = true
		if b.Data != tinyAnthropicPNG {
			t.Fatal("image payload does not match the source image")
		}
		if b.MimeType != "image/png" {
			t.Fatalf("mimeType = %q, want image/png", b.MimeType)
		}
	}
	if !found {
		t.Fatalf("no image block emitted (blocks=%d)", len(blocks))
	}
}

// TestTranslateWithMedia_AnthropicURLImage pins the url source type.
//
// @purpose Proves the url form goes through the same policy as OpenAI's image_url.
// @rationale One policy surface means a scheme banned for OpenAI cannot slip in via Anthropic.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no network.
func TestTranslateWithMedia_AnthropicURLImage(t *testing.T) {
	tr := NewTranslatorWithBudget(0).WithMedia(anthropicImageTestResolver())
	req := &MessageRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 256,
		Messages: []Message{{
			Role: "user",
			Content: MessageContent{Blocks: []ContentBlock{
				{Type: "text", Text: "fetch this"},
				{Type: "image", Source: &ImageSource{Type: "url", URL: "file:///etc/passwd"}},
			}},
		}},
	}

	if _, _, err := tr.TranslateWithMedia(context.Background(), req, ""); err == nil {
		t.Fatal("a file: image source must be refused by the media policy")
	}
}

// TestTranslate_AnthropicWithoutMediaStaysTextOnly pins the default path.
//
// @purpose Proves the plain Translate entry point never resolves images.
// @rationale A caller that did not opt into media must not fetch anything, and history re-renders
// must not re-upload old attachments.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no network.
func TestTranslate_AnthropicWithoutMediaStaysTextOnly(t *testing.T) {
	tr := NewTranslatorWithBudget(0)
	req := &MessageRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 256,
		Messages: []Message{{
			Role: "user",
			Content: MessageContent{Blocks: []ContentBlock{
				{Type: "text", Text: "hi"},
				{Type: "image", Source: &ImageSource{Type: "base64", MediaType: "image/png", Data: tinyAnthropicPNG}},
			}},
		}},
	}

	_, blocks := tr.Translate(req, "")
	for _, b := range blocks {
		if b.Type == "image" {
			t.Fatal("Translate without a media resolver must not emit image blocks")
		}
	}
}
