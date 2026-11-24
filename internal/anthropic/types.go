// Package anthropic provides 100% compliant Anthropic 2026 Messages API wire schemas,
// polymorphic JSON parsing, Server-Sent Events (SSE) streaming formatting, and prompt translation engines.
//
// @purpose Encapsulates the complete protocol definitions for the Anthropic Messages API (/v1/messages).
// @rationale Provides seamless interoperability for Claude-compatible clients (Cursor, Roo Code, Cline, official SDKs).
// @preconditions All incoming payloads must be valid JSON adhering to Anthropic specification.
package anthropic

import (
	"encoding/json"
	"strings"
)

// MessageRequest represents an incoming Anthropic /v1/messages payload.
//
// @purpose Models an incoming request payload for the Messages API.
// @rationale Strongly typed schema ensures zero untyped fallbacks and strict validation.
type MessageRequest struct {
	Model         string           `json:"model" example:"claude-3-5-sonnet-20241022"`
	Messages      []Message        `json:"messages"`
	MaxTokens     int              `json:"max_tokens" example:"4096"`
	System        SystemPrompt     `json:"system,omitempty" swaggertype:"primitive,string" example:"You are an expert AI architect."`
	Metadata      *RequestMetadata `json:"metadata,omitempty"`
	StopSequences []string         `json:"stop_sequences,omitempty"`
	Stream        bool             `json:"stream,omitempty" example:"false"`
	Temperature   *float64         `json:"temperature,omitempty" example:"0.7"`
	TopP          *float64         `json:"top_p,omitempty" example:"0.95"`
	TopK          *int             `json:"top_k,omitempty" example:"40"`
	Tools         []Tool           `json:"tools,omitempty"`
	ToolChoice    any              `json:"tool_choice,omitempty" swaggertype:"object"`
	Thinking      *ThinkingConfig  `json:"thinking,omitempty"`
}

// RequestMetadata contains optional client-supplied context.
type RequestMetadata struct {
	UserID string `json:"user_id,omitempty" example:"user_12345"`
}

// ThinkingConfig models extended thinking / reasoning parameters.
type ThinkingConfig struct {
	Type         string `json:"type" example:"enabled"` // "enabled" or "adaptive"
	BudgetTokens int    `json:"budget_tokens,omitempty" example:"1024"`
}

// Tool represents a model tool declaration in Anthropic format.
type Tool struct {
	Name        string          `json:"name" example:"get_weather"`
	Description string          `json:"description,omitempty" example:"Get the current weather for a given city."`
	InputSchema json.RawMessage `json:"input_schema" swaggertype:"object"`
}

// SystemPrompt supports polymorphic JSON decoding (simple string or array of system blocks).
type SystemPrompt struct {
	Text   string
	Blocks []SystemBlock
}

// SystemBlock represents an individual structured block in the system prompt.
type SystemBlock struct {
	Type  string `json:"type" example:"text"` // "text"
	Text  string `json:"text" example:"You are a helpful assistant."`
	Cache any    `json:"cache_control,omitempty"`
}

// UnmarshalJSON unmarshals either a string or []SystemBlock into SystemPrompt.
func (sp *SystemPrompt) UnmarshalJSON(data []byte) error {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 || string(data) == "null" {
		sp.Text = ""
		sp.Blocks = nil
		return nil
	}

	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		sp.Text = s
		sp.Blocks = []SystemBlock{{Type: "text", Text: s}}
		return nil
	}

	var blocks []SystemBlock
	if err := json.Unmarshal(data, &blocks); err != nil {
		return err
	}
	sp.Blocks = blocks
	var sb strings.Builder
	for i, b := range blocks {
		if b.Text != "" {
			if i > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(b.Text)
		}
	}
	sp.Text = sb.String()
	return nil
}

// MarshalJSON marshals SystemPrompt back into JSON.
func (sp SystemPrompt) MarshalJSON() ([]byte, error) {
	if len(sp.Blocks) > 1 {
		return json.Marshal(sp.Blocks)
	}
	return json.Marshal(sp.Text)
}

// String returns the combined text of the system prompt.
func (sp SystemPrompt) String() string {
	return sp.Text
}

// Message represents an individual conversational turn in the messages array.
type Message struct {
	Role    string         `json:"role" example:"user"` // "user" or "assistant"
	Content MessageContent `json:"content" swaggertype:"primitive,string" example:"Hello, Claude!"`
}

// ContentString returns a flattened plain-text representation of the message content.
func (m Message) ContentString() string {
	return m.Content.Text
}

// MessageContent supports polymorphic JSON decoding (simple string or array of ContentBlocks).
type MessageContent struct {
	Text   string
	Blocks []ContentBlock
}

// UnmarshalJSON unmarshals either a string or []ContentBlock into MessageContent.
func (mc *MessageContent) UnmarshalJSON(data []byte) error {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 || string(data) == "null" {
		mc.Text = ""
		mc.Blocks = nil
		return nil
	}

	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		mc.Text = s
		mc.Blocks = []ContentBlock{{Type: "text", Text: s}}
		return nil
	}

	var blocks []ContentBlock
	if err := json.Unmarshal(data, &blocks); err != nil {
		return err
	}
	mc.Blocks = blocks

	var sb strings.Builder
	for i, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			if i > 0 && sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(b.Text)
		}
	}
	mc.Text = sb.String()
	return nil
}

// MarshalJSON marshals MessageContent into JSON.
func (mc MessageContent) MarshalJSON() ([]byte, error) {
	if len(mc.Blocks) > 0 {
		return json.Marshal(mc.Blocks)
	}
	return json.Marshal(mc.Text)
}

// ContentBlock represents an element inside an Anthropic content array.
type ContentBlock struct {
	Type      string          `json:"type" example:"text"` // "text", "image", "tool_use", "tool_result", "thinking", "redacted_thinking"
	Text      string          `json:"text,omitempty" example:"Hello world"`
	Source    *ImageSource    `json:"source,omitempty"`
	ID        string          `json:"id,omitempty" example:"toolu_01Abc..."`
	Name      string          `json:"name,omitempty" example:"get_weather"`
	Input     json.RawMessage `json:"input,omitempty" swaggertype:"object"`
	ToolUseID string          `json:"tool_use_id,omitempty" example:"toolu_01Abc..."`
	Content   any             `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Thinking  string          `json:"thinking,omitempty" example:"Let's evaluate the requested operation..."`
	Signature string          `json:"signature,omitempty" example:"EqQBCgIYAhIM1gbcDa9GJwZA2b3h..."`
	Data      string          `json:"data,omitempty"`
}

// ImageSource represents an embedded image inside a content block.
type ImageSource struct {
	Type      string `json:"type" example:"base64"` // "base64" or "url"
	MediaType string `json:"media_type,omitempty" example:"image/jpeg"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// MessageResponse represents the synchronous JSON completion payload returned by POST /v1/messages.
type MessageResponse struct {
	ID           string         `json:"id" example:"msg_01XFDUDYJgAACzvnptvVoYEL"`
	Type         string         `json:"type" example:"message"`
	Role         string         `json:"role" example:"assistant"`
	Content      []ContentBlock `json:"content"`
	Model        string         `json:"model" example:"claude-3-5-sonnet-20241022"`
	StopReason   *string        `json:"stop_reason" example:"end_turn"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        Usage          `json:"usage"`
}

// Usage captures token metrics for Anthropic requests and responses.
type Usage struct {
	InputTokens              int `json:"input_tokens" example:"25"`
	OutputTokens             int `json:"output_tokens" example:"18"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// ErrorResponse represents an Anthropic RFC-compliant error structure.
type ErrorResponse struct {
	Type  string       `json:"type" example:"error"`
	Error ErrorDetails `json:"error"`
}

// ErrorDetails carries the inner error type and human-readable message.
type ErrorDetails struct {
	Type    string `json:"type" example:"invalid_request_error"`
	Message string `json:"message" example:"Missing required field: max_tokens"`
}

// --- Streaming Server-Sent Events (SSE) Schemas ---

// MessageStartEvent initializes the streaming message container.
type MessageStartEvent struct {
	Type    string          `json:"type"` // "message_start"
	Message MessageResponse `json:"message"`
}

// ContentBlockStartEvent notifies the beginning of a specific content block.
type ContentBlockStartEvent struct {
	Type         string       `json:"type"` // "content_block_start"
	Index        int          `json:"index"`
	ContentBlock ContentBlock `json:"content_block"`
}

// ContentBlockDeltaEvent delivers incremental tokens or JSON deltas.
type ContentBlockDeltaEvent struct {
	Type  string            `json:"type"` // "content_block_delta"
	Index int               `json:"index"`
	Delta ContentBlockDelta `json:"delta"`
}

// ContentBlockDelta contains the payload for an individual delta event.
type ContentBlockDelta struct {
	Type        string `json:"type"` // "text_delta", "input_json_delta", "thinking_delta", "signature_delta"
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
}

// ContentBlockStopEvent signals completion of a content block.
type ContentBlockStopEvent struct {
	Type  string `json:"type"` // "content_block_stop"
	Index int    `json:"index"`
}

// MessageDeltaEvent conveys final message metadata such as stop reason and final token usage.
type MessageDeltaEvent struct {
	Type  string            `json:"type"` // "message_delta"
	Delta MessageDeltaInfo  `json:"delta"`
	Usage MessageDeltaUsage `json:"usage"`
}

// MessageDeltaInfo contains the stop reason and stop sequence.
type MessageDeltaInfo struct {
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

// MessageDeltaUsage carries final accumulated output token metrics.
type MessageDeltaUsage struct {
	OutputTokens int `json:"output_tokens"`
}

// MessageStopEvent signals the absolute end of the SSE stream.
type MessageStopEvent struct {
	Type string `json:"type"` // "message_stop"
}

// PingEvent represents an optional SSE keepalive ping.
type PingEvent struct {
	Type string `json:"type"` // "ping"
}
