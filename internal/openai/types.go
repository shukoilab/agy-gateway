// Package openai provides 100% compliant OpenAI 2026 API wire schemas,
// streaming SSE formatting, and prompt translation engines.
package openai

import (
	"encoding/json"

	"agy-gateway/internal/mcp"
)

// ChatCompletionRequest represents an incoming OpenAI /v1/chat/completions payload.
type ChatCompletionRequest struct {
	Model               string                     `json:"model"`
	Messages            []ChatMessage              `json:"messages"`
	Tools               []mcp.OpenAIToolDefinition `json:"tools,omitempty"`
	ToolChoice          any                        `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool                      `json:"parallel_tool_calls,omitempty"`
	Temperature         *float64                   `json:"temperature,omitempty"`
	TopP                *float64                   `json:"top_p,omitempty"`
	N                   *int                       `json:"n,omitempty"`
	Stream              bool                       `json:"stream,omitempty"`
	StreamOptions       *StreamOptions             `json:"stream_options,omitempty"`
	Stop                any                        `json:"stop,omitempty"`
	MaxTokens           *int                       `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                       `json:"max_completion_tokens,omitempty"`
	PresencePenalty     *float64                   `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64                   `json:"frequency_penalty,omitempty"`
	ReasoningEffort     string                     `json:"reasoning_effort,omitempty"`
	ResponseFormat      any                        `json:"response_format,omitempty"`
	User                string                     `json:"user,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ChatMessage represents a single message in the conversation.
type ChatMessage struct {
	Role             string     `json:"role"` // "developer", "system", "user", "assistant", "tool"
	Content          any        `json:"content"` // string or []ContentPart
	Name             string     `json:"name,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
}

type ContentPart struct {
	Type     string    `json:"type"` // "text", "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

// ToolCall represents a model function call.
type ToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"` // "function"
	Function FunctionCallData `json:"function"`
}

type FunctionCallData struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// ChatCompletionResponse represents the synchronous JSON completion payload.
type ChatCompletionResponse struct {
	ID                string                 `json:"id"`
	Object            string                 `json:"object"`
	Created           int64                  `json:"created"`
	Model             string                 `json:"model"`
	SystemFingerprint string                 `json:"system_fingerprint,omitempty"`
	Choices           []ChatCompletionChoice `json:"choices"`
	Usage             UsageDetails           `json:"usage"`
}

type ChatCompletionChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatCompletionChunk represents an individual streaming SSE event.
type ChatCompletionChunk struct {
	ID                string                      `json:"id"`
	Object            string                      `json:"object"`
	Created           int64                       `json:"created"`
	Model             string                      `json:"model"`
	SystemFingerprint string                      `json:"system_fingerprint,omitempty"`
	Choices           []ChatCompletionChunkChoice `json:"choices"`
	Usage             *UsageDetails               `json:"usage,omitempty"`
}

type ChatCompletionChunkChoice struct {
	Index        int                      `json:"index"`
	Delta        ChatCompletionChunkDelta `json:"delta"`
	FinishReason *string                  `json:"finish_reason"`
}

type ChatCompletionChunkDelta struct {
	Role             string     `json:"role,omitempty"`
	Content          *string    `json:"content"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

type UsageDetails struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type ModelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type ModelListResponse struct {
	Object string        `json:"object"`
	Data   []ModelObject `json:"data"`
}

type ErrorResponse struct {
	Error ErrorDetails `json:"error"`
}

type ErrorDetails struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param,omitempty"`
	Code    *string `json:"code,omitempty"`
}

func (m *ChatMessage) ContentString() string {
	switch v := m.Content.(type) {
	case string:
		return v
	case []any:
		var sb string
		for _, part := range v {
			if pm, ok := part.(map[string]any); ok {
				if t, ok := pm["text"].(string); ok {
					sb += t
				}
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
