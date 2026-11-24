// Package acp implements the Agent Client Protocol (JSON-RPC 2.0 over stdio)
// for communication with the Google Antigravity CLI (agy acp).
package acp

import (
	"bytes"
	"encoding/json"
	"strings"
)

// JSON-RPC 2.0 constants
const (
	JSONRPCVersion = "2.0"

	// ACP Error Codes
	ErrCodeAuthRequired   = -32000
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternalError  = -32603

	// ACP Session Config IDs
	ConfigIDMode         = "mode"
	ConfigIDModel        = "model"
	ConfigIDThoughtLevel = "thought_level"

	// ACP Update Types
	UpdateAgentMessageChunk = "agent_message_chunk"
	UpdateAgentThoughtChunk = "agent_thought_chunk"
	UpdateToolCall          = "tool_call"
	UpdateToolCallUpdate    = "tool_call_update"
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

// MCPServerInfo describes an MCP server injected during session/new.
type MCPServerInfo struct {
	Name    string            `json:"name"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// SessionNewParams represents parameters sent in the session/new request.
type SessionNewParams struct {
	CWD                   string          `json:"cwd"`
	AdditionalDirectories []string        `json:"additionalDirectories,omitempty"`
	MCPServers            []MCPServerInfo `json:"mcpServers,omitempty"`
	Meta                  map[string]any  `json:"_meta,omitempty"`
}

// SessionNewResult represents the result returned by session/new.
type SessionNewResult struct {
	SessionID string `json:"sessionId"`
}

// SessionCloseParams represents parameters sent in the session/close request.
//
// @purpose Unloads an active ACP session and releases all associated memory state.
// @rationale Enforces true JIT lifecycle for ephemeral agent turns without long-lived session bloat.
// @invariants Thread-safe.
type SessionCloseParams struct {
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
	Type string `json:"type"` // "text", "resource", etc.
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

// SessionUpdate contains details of live turn streaming from the agent.
type SessionUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       *ContentChunk   `json:"content,omitempty"`
	ToolCallID    string          `json:"toolCallId,omitempty"`
	Name          string          `json:"name,omitempty"`
	Title         string          `json:"title,omitempty"`
	Kind          string          `json:"kind,omitempty"`
	Status        string          `json:"status,omitempty"`
	RawInput      json.RawMessage `json:"rawInput,omitempty"`
	RawOutput     json.RawMessage `json:"rawOutput,omitempty"`
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

// PermissionOutcome defines the chosen permission response.
type PermissionOutcome struct {
	Outcome  string `json:"outcome"` // "selected"
	OptionID string `json:"optionId"`
}

// RequestPermissionResult is sent as the JSON-RPC response to session/request_permission.
type RequestPermissionResult struct {
	Outcome PermissionOutcome `json:"outcome"`
}
