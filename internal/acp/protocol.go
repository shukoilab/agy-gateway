// Package acp implements the Agent Client Protocol (JSON-RPC 2.0 over stdio)
// for communication with the Google Antigravity CLI (agy acp).
package acp

import "encoding/json"

// JSON-RPC 2.0 constants
const (
	JSONRPCVersion = "2.0"

	// ACP Error Codes
	ErrCodeAuthRequired = -32000
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
type ContentChunk struct {
	Text string `json:"text,omitempty"`
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
