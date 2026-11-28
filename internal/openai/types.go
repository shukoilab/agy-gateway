// Package openai provides 100% compliant OpenAI 2026 API wire schemas,
// streaming SSE formatting, and prompt translation engines.
package openai

import (
	"encoding/json"

	"agy-gateway/internal/mcp"
)

// ChatCompletionRequest represents an incoming OpenAI /v1/chat/completions payload.
type ChatCompletionRequest struct {
	Model               string                     `json:"model" example:"gemini-3.8-flash"`
	Messages            []ChatMessage              `json:"messages"`
	Tools               []mcp.OpenAIToolDefinition `json:"tools,omitempty"`
	ToolChoice          any                        `json:"tool_choice,omitempty" swaggertype:"primitive,string" example:"auto"`
	ParallelToolCalls   *bool                      `json:"parallel_tool_calls,omitempty"`
	Temperature         *float64                   `json:"temperature,omitempty" example:"0.7"`
	TopP                *float64                   `json:"top_p,omitempty" example:"0.95"`
	N                   *int                       `json:"n,omitempty" example:"1"`
	Stream              bool                       `json:"stream,omitempty" example:"false"`
	StreamOptions       *StreamOptions             `json:"stream_options,omitempty"`
	Stop                any                        `json:"stop,omitempty" swaggertype:"primitive,string" example:"\n"`
	MaxTokens           *int                       `json:"max_tokens,omitempty" example:"2048"`
	MaxCompletionTokens *int                       `json:"max_completion_tokens,omitempty" example:"2048"`
	PresencePenalty     *float64                   `json:"presence_penalty,omitempty" example:"0.0"`
	FrequencyPenalty    *float64                   `json:"frequency_penalty,omitempty" example:"0.0"`
	ReasoningEffort     string                     `json:"reasoning_effort,omitempty" enums:"low,medium,high" example:"medium"`
	ResponseFormat      any                        `json:"response_format,omitempty" swaggertype:"object"`
	User                string                     `json:"user,omitempty" example:"client_user_123"`
	// PromptCacheKey is the body-level conversation identifier several harnesses send instead of a
	// header (LangChain/Fireworks and OpenCode both populate it). Honoured only when no affinity header
	// is present, and used solely to pick a per-conversation workspace directory.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	// Seed, Logprobs, TopLogprobs and LogitBias exist only so the FR-14 policy can see
	// them: decoding drops unknown keys, which would let them be silently ignored.
	Seed        *int64         `json:"seed,omitempty" example:"42"`
	Logprobs    *bool          `json:"logprobs,omitempty" example:"false"`
	TopLogprobs *int           `json:"top_logprobs,omitempty" example:"0"`
	LogitBias   map[string]int `json:"logit_bias,omitempty" swaggertype:"object"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty" example:"true"`
}

// ChatMessage represents a single message in the conversation.
type ChatMessage struct {
	Role             string     `json:"role" enums:"developer,system,user,assistant,tool" example:"user"`                                        // "developer", "system", "user", "assistant", "tool"
	Content          any        `json:"content" swaggertype:"primitive,string" example:"Explain the architecture of distributed actor systems."` // string or []ContentPart
	Name             string     `json:"name,omitempty" example:"analyst"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty" example:"call_9f3b18e2"`
	ReasoningContent string     `json:"reasoning_content,omitempty" example:"Let's examine actor isolation, message mailboxes, and supervision trees..."`
}

type ContentPart struct {
	Type     string    `json:"type" example:"text"` // "text", "image_url"
	Text     string    `json:"text,omitempty" example:"Describe this architecture diagram"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url" example:"https://example.com/diagram.png"`
}

// ToolCall represents a model function call.
type ToolCall struct {
	Index    *int             `json:"index,omitempty" example:"0"`
	ID       string           `json:"id,omitempty" example:"call_abc123"`
	Type     string           `json:"type,omitempty" example:"function"` // "function"
	Function FunctionCallData `json:"function"`
}

type FunctionCallData struct {
	Name      string `json:"name,omitempty" example:"get_weather"`
	Arguments string `json:"arguments" example:"{\"location\":\"Tokyo\"}"`
}

// ChatCompletionResponse represents the synchronous JSON completion payload.
type ChatCompletionResponse struct {
	ID                string                 `json:"id" example:"chatcmpl-9f3b18e2a1b2c3"`
	Object            string                 `json:"object" example:"chat.completion"`
	Created           int64                  `json:"created" example:"1774350000"`
	Model             string                 `json:"model" example:"gemini-3.8-flash"`
	SystemFingerprint string                 `json:"system_fingerprint,omitempty" example:"fp_agy_v34"`
	Choices           []ChatCompletionChoice `json:"choices"`
	Usage             UsageDetails           `json:"usage"`
}

type ChatCompletionChoice struct {
	Index        int         `json:"index" example:"0"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason" example:"stop"`
}

// ChatCompletionChunk represents an individual streaming SSE event.
type ChatCompletionChunk struct {
	ID                string                      `json:"id" example:"chatcmpl-9f3b18e2a1b2c3"`
	Object            string                      `json:"object" example:"chat.completion.chunk"`
	Created           int64                       `json:"created" example:"1774350000"`
	Model             string                      `json:"model" example:"gemini-3.8-flash"`
	SystemFingerprint string                      `json:"system_fingerprint,omitempty" example:"fp_agy_v34"`
	Choices           []ChatCompletionChunkChoice `json:"choices"`
	Usage             *UsageDetails               `json:"usage,omitempty"`
}

type ChatCompletionChunkChoice struct {
	Index        int                      `json:"index" example:"0"`
	Delta        ChatCompletionChunkDelta `json:"delta"`
	FinishReason *string                  `json:"finish_reason" example:"stop"`
}

type ChatCompletionChunkDelta struct {
	Role             string     `json:"role,omitempty" example:"assistant"`
	Content          *string    `json:"content"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

type UsageDetails struct {
	PromptTokens            int                      `json:"prompt_tokens" example:"35"`
	CompletionTokens        int                      `json:"completion_tokens" example:"180"`
	TotalTokens             int                      `json:"total_tokens" example:"215"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens" example:"0"`
}

type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens" example:"120"`
}

type ModelObject struct {
	ID              string `json:"id" example:"gemini-3.8-flash"`
	Object          string `json:"object" example:"model"`
	Created         int64  `json:"created" example:"1774350000"`
	OwnedBy         string `json:"owned_by" example:"google"`
	ContextWindow   int    `json:"context_window,omitempty" example:"1048576"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty" example:"65536"`
}

type ModelListResponse struct {
	Object string        `json:"object" example:"list"`
	Data   []ModelObject `json:"data"`
}

type ErrorResponse struct {
	Error ErrorDetails `json:"error"`
}

type ErrorDetails struct {
	Message string  `json:"message" example:"Missing or invalid Bearer token"`
	Type    string  `json:"type" example:"invalid_request_error"`
	Param   *string `json:"param,omitempty" example:"authorization"`
	Code    *string `json:"code,omitempty" example:"invalid_api_key"`
}

func (m *ChatMessage) ContentString() string {
	switch v := m.Content.(type) {
	case string:
		return v
	case []any:
		var sb string
		for _, part := range v {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := pm["text"].(string); ok {
				sb += t
				continue
			}
			// An image part has no textual form. Saying so is mandatory: previously this branch
			// dropped it, so a conversation history containing an image looked to the agent as if
			// the user had sent nothing at all.
			if typ, _ := pm["type"].(string); typ == "image_url" || typ == "input_image" {
				sb += "[image]"
			}
		}
		return sb
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// ContentParts decodes the polymorphic `content` field into ordered typed parts.
//
// @purpose Gives the translator a real content-part view instead of a text-only string.
// @rationale ContentString concatenated only `part["text"]`, so an image part vanished without a
// trace: the request "succeeded" with the image silently dropped. Images are a first-class ACP
// prompt block, so they must survive decoding.
//
// @preconditions None; Content may be a string, a []any of maps, or nil.
// @param None.
// @returns Ordered parts whose Type is "text" or "image_url"; a plain string yields one text part.
// @throws None; malformed parts are skipped rather than failing the whole request.
// @invariants O(len(parts)).
func (m *ChatMessage) ContentParts() []ContentPart {
	switch v := m.Content.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []ContentPart{{Type: "text", Text: v}}
	case []any:
		parts := make([]ContentPart, 0, len(v))
		for _, raw := range v {
			pm, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := pm["type"].(string)
			switch typ {
			case "text", "input_text":
				text, _ := pm["text"].(string)
				parts = append(parts, ContentPart{Type: "text", Text: text})
			case "image_url", "input_image":
				part := ContentPart{Type: "image_url"}
				switch img := pm["image_url"].(type) {
				case map[string]any:
					if u, ok := img["url"].(string); ok {
						part.ImageURL = &ImageURL{URL: u}
					}
				case string:
					part.ImageURL = &ImageURL{URL: img}
				}
				// Some clients send `url` at the part level rather than nested.
				if part.ImageURL == nil {
					if u, ok := pm["url"].(string); ok {
						part.ImageURL = &ImageURL{URL: u}
					}
				}
				if part.ImageURL != nil && part.ImageURL.URL != "" {
					parts = append(parts, part)
				}
			}
		}
		return parts
	case nil:
		return nil
	default:
		return nil
	}
}

// HasImage reports whether the message carries at least one image part.
//
// @purpose Lets callers branch on image presence without decoding twice.
// @param None.
// @returns true when a decodable image part exists.
// @invariants O(len(parts)).
func (m *ChatMessage) HasImage() bool {
	for _, p := range m.ContentParts() {
		if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL != "" {
			return true
		}
	}
	return false
}
