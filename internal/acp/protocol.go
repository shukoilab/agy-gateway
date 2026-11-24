// Package acp implements the Agent Client Protocol (JSON-RPC 2.0 over stdio)
// for communication with the Google Antigravity CLI (agy acp) per official ACP v1 specifications.
package acp

import (
	"bytes"
	"encoding/json"
	"strings"
)

// JSON-RPC 2.0 constants
const (
	JSONRPCVersion = "2.0"

	// ACP & JSON-RPC 2.0 Canonical Error Codes (RFC 2119 / ACP Schema v1)
	ErrCodeParseError       = -32700
	ErrCodeInvalidRequest   = -32600
	ErrCodeMethodNotFound   = -32601
	ErrCodeInvalidParams    = -32602
	ErrCodeInternalError    = -32603
	ErrCodeRequestCancelled = -32800
	ErrCodeAuthRequired     = -32000
	ErrCodeResourceNotFound = -32002

	// ACP Session Config IDs
	ConfigIDMode         = "mode"
	ConfigIDModel        = "model"
	ConfigIDThoughtLevel = "thought_level"

	// ACP Update Types (Canonical SessionUpdate variants)
	UpdateAgentMessageChunk = "agent_message_chunk"
	UpdateAgentThoughtChunk = "agent_thought_chunk"
	UpdateUserMessageChunk  = "user_message_chunk"
	UpdateToolCall          = "tool_call"
	UpdateToolCallUpdate    = "tool_call_update"
	UpdateUsage             = "usage_update"
	UpdateAvailableCommands = "available_commands_update"
	UpdateCurrentMode       = "current_mode_update"
	UpdateConfigOption      = "config_option_update"
	UpdateNotice            = "notice"
	UpdateCompaction        = "compaction_update"
	UpdateCompactionChunk   = "compaction_summary_chunk"
	UpdatePlan              = "plan"
	UpdatePlanUpdate        = "plan_update"
	UpdatePlanRemoved       = "plan_removed"
	UpdateSubagent          = "subagent_update"
	UpdateSessionMessage    = "session_message"
	UpdateSessionMsgChunk   = "session_message_chunk"
)

// Request is a standard JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a standard JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}

// ResponseError encapsulates JSON-RPC 2.0 errors.
type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *ResponseError) Error() string {
	return e.Message
}

// Notification is an unsolicited JSON-RPC 2.0 notification without an ID.
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// EnvVariable defines an environment variable for stdio MCP servers per ACP v1 specification.
type EnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// HTTPHeader defines an HTTP header for HTTP/SSE MCP servers per ACP v1 specification.
type HTTPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MCPServerInfo describes an MCP server configuration injected during session/new.
// Conforms to official ACP v1 McpServer union (stdio, http, sse).
type MCPServerInfo struct {
	Type    string        `json:"type,omitempty"` // "http", "sse", or empty for stdio
	Name    string        `json:"name"`
	Command string        `json:"command,omitempty"`
	Args    []string      `json:"args,omitempty"`
	Env     []EnvVariable `json:"env,omitempty"`
	URL     string        `json:"url,omitempty"`
	Headers []HTTPHeader  `json:"headers,omitempty"`
}

// DefaultExecutionProtocol is the ultra-smart system prompt applied when the API consumer
// does not specify a custom system prompt. It enforces strict language mirroring,
// first-principles analysis, high-signal directness, and Chain-of-Verification (CoVe).
const DefaultExecutionProtocol = `Apply the following execution protocol to every user request:

1. **Strict Language Mirroring**: Detect the user's input language and write the entire response in that exact language naturally, accurately, and professionally.
2. **First-Principles & Assumption Analysis**: Before concluding, decompose the core objective, underlying first principles, critical variables, key assumptions, and edge cases or counter-arguments. Calibrate depth to task complexity.
3. **High-Signal Directness**: Deliver concrete, actionable, and well-structured outputs (using Markdown headings, lists, or tables where helpful) immediately—omit introductory filler and closing summaries.
4. **Chain-of-Verification (CoVe) Checklist**: Audit every draft claim against these gates before finalizing output:
   - [ ] **Premise & Fact Verification**: Independently verify every claim, calculation, and logical step; state uncertainty or missing data explicitly rather than extrapolating.
   - [ ] **Falsifiability & Edge Cases**: Address boundary conditions, failure modes, and strongest alternative interpretations.
   - [ ] **Logical Coherence**: Ensure conclusions follow strictly from verified premises without internal contradiction.
   - [ ] **Language & Signal Compliance**: Confirm 100% adherence to the user's input language and maximum information density.`

// SystemPrompt defines system-level instructions passed during session setup per ACP v1.
type SystemPrompt struct {
	Mode    string        `json:"mode"` // "append" or "override"
	Content []PromptBlock `json:"content"`
}

// SessionNewParams represents parameters sent in the session/new request per ACP v1 NewSessionRequest.
type SessionNewParams struct {
	CWD                   string          `json:"cwd"`
	AdditionalDirectories []string        `json:"additionalDirectories,omitempty"`
	MCPServers            []MCPServerInfo `json:"mcpServers,omitempty"`
	SystemPrompt          *SystemPrompt   `json:"systemPrompt,omitempty"`
	Meta                  map[string]any  `json:"_meta,omitempty"`
}

// SessionNewResult represents the result returned by session/new per ACP v1 NewSessionResponse.
type SessionNewResult struct {
	SessionID     string          `json:"sessionId"`
	Models        json.RawMessage `json:"models,omitempty"`
	Modes         json.RawMessage `json:"modes,omitempty"`
	ConfigOptions json.RawMessage `json:"configOptions,omitempty"`
	Meta          json.RawMessage `json:"_meta,omitempty"`
}

// SessionCloseParams represents parameters sent in the session/close request.
//
// @purpose Unloads an active ACP session and releases all associated memory state.
// @rationale Enforces true JIT lifecycle for ephemeral agent turns without long-lived session bloat.
// @invariants Thread-safe.
type SessionCloseParams struct {
	SessionID string `json:"sessionId"`
}

// SessionCancelParams represents parameters sent in the session/cancel notification.
//
// @purpose Aborts an active ACP session prompt turn.
// @rationale Conforms to official Agent Client Protocol specification for client-initiated cancellation.
// @invariants Thread-safe.
type SessionCancelParams struct {
	SessionID string `json:"sessionId"`
}

// SetConfigOptionParams represents parameters sent in session/set_config_option.
type SetConfigOptionParams struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Value     string `json:"value"`
}

// PromptBlock represents rich content in an ACP session/prompt request.
type PromptBlock struct {
	Type string `json:"type"` // "text", "resource", "resource_link", etc.
	Text string `json:"text,omitempty"`
}

// PromptParams represents parameters sent in session/prompt.
type PromptParams struct {
	SessionID string        `json:"sessionId"`
	Prompt    []PromptBlock `json:"prompt"`
}

// PromptResult represents the completion of a session/prompt turn.
type PromptResult struct {
	StopReason string `json:"stopReason,omitempty"`
}

// UsageCost defines cumulative monetary or credit cost in an ACP usage_update.
type UsageCost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// UsageUpdate defines context window token usage reported via sessionUpdate: "usage_update".
type UsageUpdate struct {
	Used int64      `json:"used"`
	Size int64      `json:"size"`
	Cost *UsageCost `json:"cost,omitempty"`
}

// SessionUpdateParams represents parameters received in session/update notifications.
type SessionUpdateParams struct {
	SessionID string        `json:"sessionId"`
	Update    SessionUpdate `json:"update"`
}

// ContentChunk represents inner text content within a session update.
//
// @purpose Encapsulates polymorphic content payloads received in ACP session/update notifications.
// @rationale Supports both single object and array representation (e.g. [{"type": "text", "text": "..."}])
// sent by diverse upstream agy acp agent runtimes without unmarshaling failures.
// @preconditions Valid JSON input bytes representing an object, an array of objects/strings, or a plain string.
// @invariants Thread-safe, deterministic concatenation of array chunks.
type ContentChunk struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

// UnmarshalJSON deserializes ContentChunk from either an object, an array of chunks, or a plain string.
func (c *ContentChunk) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}

	// 1. JSON Array: [{"type":"text","text":"..."}, ...] or ["...", ...]
	if trimmed[0] == '[' {
		var rawItems []json.RawMessage
		if err := json.Unmarshal(trimmed, &rawItems); err != nil {
			return err
		}
		var sb strings.Builder
		for _, rawItem := range rawItems {
			itemTrimmed := bytes.TrimSpace(rawItem)
			if len(itemTrimmed) == 0 || bytes.Equal(itemTrimmed, []byte("null")) {
				continue
			}
			if itemTrimmed[0] == '{' {
				var obj struct {
					Type string `json:"type,omitempty"`
					Text string `json:"text,omitempty"`
				}
				if err := json.Unmarshal(itemTrimmed, &obj); err == nil {
					sb.WriteString(obj.Text)
					if c.Type == "" && obj.Type != "" {
						c.Type = obj.Type
					}
					continue
				}
			}
			var str string
			if err := json.Unmarshal(itemTrimmed, &str); err == nil {
				sb.WriteString(str)
				continue
			}
		}
		c.Text = sb.String()
		return nil
	}

	// 2. JSON Object: {"type": "text", "text": "..."} or {"text": "..."}
	if trimmed[0] == '{' {
		var obj struct {
			Type string `json:"type,omitempty"`
			Text string `json:"text,omitempty"`
		}
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return err
		}
		c.Type = obj.Type
		c.Text = obj.Text
		return nil
	}

	// 3. Plain JSON String: "text chunk"
	var str string
	if err := json.Unmarshal(trimmed, &str); err == nil {
		c.Text = str
		return nil
	}

	return nil
}

// SessionUpdate contains details of live turn streaming from the agent per ACP v1 specification.
type SessionUpdate struct {
	SessionUpdate     string           `json:"sessionUpdate"`
	Content           *ContentChunk    `json:"content,omitempty"`
	ToolCallID        string           `json:"toolCallId,omitempty"`
	Name              string           `json:"name,omitempty"`
	Title             string           `json:"title,omitempty"`
	Kind              string           `json:"kind,omitempty"`
	Status            string           `json:"status,omitempty"`
	RawInput          json.RawMessage  `json:"rawInput,omitempty"`
	RawOutput         json.RawMessage  `json:"rawOutput,omitempty"`
	Usage             *UsageUpdate     `json:"usage,omitempty"`
	Used              *int64           `json:"used,omitempty"`
	Size              *int64           `json:"size,omitempty"`
	Cost              *UsageCost       `json:"cost,omitempty"`
	AvailableCommands []map[string]any `json:"availableCommands,omitempty"`
}

// PermissionOption describes an option offered during session/request_permission.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name,omitempty"`
	Kind     string `json:"kind,omitempty"` // "allow_always", "allow_once", "deny"
}

// RequestPermissionParams represents incoming permission requests from agy acp.
type RequestPermissionParams struct {
	SessionID  string             `json:"sessionId"`
	ToolCallID string             `json:"toolCallId,omitempty"`
	Options    []PermissionOption `json:"options"`
}

// PermissionOutcome defines the chosen permission response per ACP v1.
type PermissionOutcome struct {
	Outcome  string `json:"outcome"` // "selected" or "cancelled"
	OptionID string `json:"optionId,omitempty"`
}

// RequestPermissionResult is sent as the JSON-RPC response to session/request_permission.
type RequestPermissionResult struct {
	Outcome PermissionOutcome `json:"outcome"`
}
