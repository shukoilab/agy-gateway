// Package mcp implements standard Model Context Protocol (MCP) server primitives
// and loopback tool interception for agy-gateway.
package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agy-gateway/internal/version"
)

var (
	ErrSessionNotFound = errors.New("mcp session not registered or expired")
	ErrMethodNotFound  = errors.New("mcp method not supported")
	// ErrTooManyTools is returned when a request declares more tools than the configured ceiling.
	// Failing loudly is required: silently truncating the list would make the model call tools that
	// the client never advertised.
	ErrTooManyTools = errors.New("too many client tools declared")
)

// MCPToolDefinition represents an official Model Context Protocol tool schema.
type MCPToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// InterceptedToolCall captures an invocation emitted by the model through MCP tools/call.
type InterceptedToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// SessionState encapsulates registered tools and intercepted calls for an active session.
type SessionState struct {
	SessionID  string
	Tools      []MCPToolDefinition
	Calls      chan InterceptedToolCall
	CreatedAt  time.Time
	LastActive time.Time
	mu         sync.Mutex
	closeOnce  sync.Once
	closed     bool
}

// ToolDefinitionSnapshot returns a copy of the declared tool schemas.
//
// @purpose Lets the turn layer sanitize tool arguments against the caller's declared schemas.
// @rationale Arguments must be validated against the schema the client supplied, not a heuristic.
// @preconditions None.
// @returns A copy of the tool definitions; empty when none were declared.
// @invariants Mutex-protected; the returned slice is independent of session state.
func (s *SessionState) ToolDefinitionSnapshot() []MCPToolDefinition {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]MCPToolDefinition, len(s.Tools))
	copy(out, s.Tools)
	return out
}

// Close gracefully terminates the session state channel and prevents further mutations.
func (s *SessionState) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.Calls)
		s.mu.Unlock()
	})
}

// LoopbackServer manages ephemeral in-memory MCP tool states and dispatches
// JSON-RPC 2.0 requests received from agy acp over HTTP or stdio bridge.
type LoopbackServer struct {
	mu       sync.RWMutex
	sessions map[string]*SessionState
	maxTools int
}

// NewLoopbackServer creates an initialized in-memory Loopback MCP Server.
//
// @purpose Coordinates real-time MCP tool discovery and execution interception without prompt injection.
// @rationale By presenting an official MCP server interface to agy acp, the model invokes tools natively
// through Google's internal function calling engine rather than generating raw XML tags in text streams.
// @invariants Thread-safe, non-blocking registration, automatic lifecycle cleanup.
func NewLoopbackServer(maxTools ...int) *LoopbackServer {
	limit := DefaultMaxClientTools
	if len(maxTools) > 0 && maxTools[0] > 0 {
		limit = maxTools[0]
	}
	return &LoopbackServer{
		sessions: make(map[string]*SessionState),
		maxTools: limit,
	}
}

// DefaultMaxClientTools is the fallback ceiling when the caller supplies none.
const DefaultMaxClientTools = 128

// RegisterSession registers an array of OpenAI tools under a unique session ID.
//
// @purpose Converts OpenAI function declarations into standard MCP tool schemas and opens an interception channel.
// @rationale Ensures zero prompt pollution; tool schemas are fetched by agy acp via MCP tools/list.
// @param sessionID Unique session correlation identifier.
// @param tools Array of OpenAI tool definitions from the consumer request.
// @returns Initialized SessionState with an active InterceptedToolCall channel.
// @invariants Thread-safe, idempotent overwrite if session ID exists.
func (s *LoopbackServer) RegisterSession(sessionID string, tools []OpenAIToolDefinition) (*SessionState, error) {
	defs := make([]MCPToolDefinition, 0, len(tools))
	for _, t := range tools {
		if t.Type != "function" || t.Function.Name == "" {
			continue
		}
		defs = append(defs, MCPToolDefinition{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: normalizeToolSchema(t.Function.Parameters),
		})
	}
	state, err := s.register(sessionID, defs)
	if err != nil {
		return nil, err
	}
	return state, nil
}

// AnthropicToolDefinition mirrors an Anthropic tool schema for protocol translation.
type AnthropicToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// RegisterAnthropicSession registers an array of Anthropic tools under a unique session ID.
//
// @purpose Converts Anthropic tool declarations into standard MCP tool schemas and opens an interception channel.
// @rationale Ensures zero prompt pollution for Anthropic client turns.
// @param sessionID Unique session correlation identifier.
// @param tools Array of Anthropic tool definitions from the consumer request.
// @returns Initialized SessionState with an active InterceptedToolCall channel.
// @invariants Thread-safe, idempotent overwrite if session ID exists.
func (s *LoopbackServer) RegisterAnthropicSession(sessionID string, tools []AnthropicToolDefinition) (*SessionState, error) {
	defs := make([]MCPToolDefinition, 0, len(tools))
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		defs = append(defs, MCPToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: normalizeToolSchema(t.InputSchema),
		})
	}
	return s.register(sessionID, defs)
}

// normalizeToolSchema substitutes an empty object schema when a caller omits parameters.
//
// @purpose Guarantees every declared tool carries a usable JSON Schema for MCP clients.
// @rationale agy validates tool arguments against this schema; a null schema makes the tool
// uncallable and the failure surfaces as an opaque -32602.
// @preconditions None.
// @param raw The caller-supplied schema, possibly nil or literal null.
// @returns A non-empty JSON Schema object.
// @throws None.
// @invariants O(1).
func normalizeToolSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return raw
}

// register stores a session state under the enforced tool ceiling.
//
// @purpose Single registration path so both protocol adapters share the limit and token rules.
// @rationale Duplicated logic previously allowed the ceilings to drift between OpenAI and Anthropic.
// @preconditions defs already normalised.
// @param sessionID Unique loopback session id.
// @param defs Canonical MCP tool definitions.
// @returns The stored session state, or ErrTooManyTools.
// @throws None; the ceiling breach is returned as a value.
// @invariants O(n) in tool count; atomic under s.mu.
func (s *LoopbackServer) register(sessionID string, defs []MCPToolDefinition) (*SessionState, error) {
	if len(defs) > s.maxTools {
		return nil, fmt.Errorf("%w: %d declared, limit %d", ErrTooManyTools, len(defs), s.maxTools)
	}

	state := &SessionState{
		SessionID:  sessionID,
		Tools:      defs,
		Calls:      make(chan InterceptedToolCall, 32),
		CreatedAt:  time.Now(),
		LastActive: time.Now(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, exists := s.sessions[sessionID]; exists {
		existing.Close()
	}
	s.sessions[sessionID] = state
	return state, nil
}

// GetSession retrieves the session state associated with a session ID.
func (s *LoopbackServer) GetSession(sessionID string) (*SessionState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.sessions[sessionID]
	if !ok || state.closed {
		return nil, ErrSessionNotFound
	}
	return state, nil
}

// UnregisterSession removes a session and closes its interception channel.
//
// @purpose Releases per-session state so a stale bridge cannot keep writing into it.
// @rationale The bridge child is not terminated here — the gateway does not own that process and the
// agent may still hold its stdio. The bridge itself exits when its token is rejected, which is
// observable through the debug log below.
// @preconditions None; safe for an unknown session.
// @param sessionID Session to forget.
// @throws None.
// @invariants O(1) map delete under the server lock.
func (s *LoopbackServer) UnregisterSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state, ok := s.sessions[sessionID]; ok {
		state.Close()
		delete(s.sessions, sessionID)
		slog.Debug("loopback session unregistered", "sessionID", sessionID)
	}
}

// jsonrpcRequest represents an incoming JSON-RPC 2.0 message.
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonrpcResponse represents an outgoing JSON-RPC 2.0 message.
type jsonrpcResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id"`
	Result  any           `json:"result,omitempty"`
	Error   *jsonrpcError `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// HandleJSONRPC processes a single raw JSON-RPC 2.0 payload from stdio bridge or HTTP.
//
// @purpose Evaluates standard MCP lifecycle methods (initialize, tools/list, tools/call, ping).
// @rationale Intercepts tools/call in memory and immediately pushes the tool payload to the gateway channel.
// @param sessionID Session identifier associated with this MCP connection.
// @param reqBytes Raw JSON-RPC 2.0 frame.
// @returns Serialized JSON-RPC response bytes, or nil for notifications.
// @invariants Thread-safe, non-panicking on malformed inputs.
func (s *LoopbackServer) HandleJSONRPC(sessionID string, reqBytes []byte) ([]byte, error) {
	var req jsonrpcRequest
	if err := json.Unmarshal(reqBytes, &req); err != nil {
		resp := jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      nil,
			Error:   &jsonrpcError{Code: -32700, Message: "Parse error: " + err.Error()},
		}
		return json.Marshal(resp)
	}

	// JSON-RPC 2.0 notifications (no id, or notifications/*) MUST NEVER produce a response,
	// even when a session has already been unregistered during teardown.
	if req.ID == nil || strings.HasPrefix(req.Method, "notifications/") {
		return nil, nil
	}

	state, err := s.GetSession(sessionID)
	if err != nil && req.Method != "initialize" && req.Method != "ping" && req.Method != "resources/list" && req.Method != "prompts/list" {
		resp := jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &jsonrpcError{Code: -32002, Message: "Session not found: " + sessionID},
		}
		return json.Marshal(resp)
	}

	var result any
	var rpcErr *jsonrpcError

	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2024-11-05",
			"serverInfo": map[string]any{
				"name":    "agy-gateway-loopback",
				"version": version.Version,
			},
			"capabilities": map[string]any{
				"tools": map[string]any{
					"listChanged": false,
				},
			},
		}

	case "notifications/initialized":
		return nil, nil // No response for JSON-RPC notifications

	case "ping":
		result = map[string]any{}

	case "resources/list":
		result = map[string]any{"resources": []any{}}

	case "prompts/list":
		result = map[string]any{"prompts": []any{}}

	case "tools/list":
		toolsList := []MCPToolDefinition{}
		if state != nil {
			state.mu.Lock()
			toolsList = append(toolsList, state.Tools...)
			state.LastActive = time.Now()
			state.mu.Unlock()
		}
		slog.Debug("loopback tools/list served", "sessionID", sessionID, "toolCount", len(toolsList))
		result = map[string]any{
			"tools": toolsList,
		}

	case "tools/call":
		var callParams struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &callParams); err != nil {
			rpcErr = &jsonrpcError{Code: -32602, Message: "Invalid tools/call params: " + err.Error()}
			break
		}

		callID := fmt.Sprintf("call_%s", strings.ReplaceAll(uuid.New().String()[:12], "-", ""))
		normArgs := callParams.Arguments
		if len(normArgs) == 0 || string(normArgs) == "null" {
			normArgs = json.RawMessage("{}")
		}

		intercepted := InterceptedToolCall{
			ID:        callID,
			Name:      callParams.Name,
			Arguments: normArgs,
		}

		if state != nil {
			state.mu.Lock()
			state.LastActive = time.Now()
			if !state.closed {
				select {
				case state.Calls <- intercepted:
				default:
				}
			}
			state.mu.Unlock()
		}

		result = map[string]any{
			"content": []map[string]any{
				{
					"type": "text",
					"text": fmt.Sprintf("Tool %s executed cleanly by gateway client runtime", callParams.Name),
				},
			},
			"isError": false,
		}

	default:
		rpcErr = &jsonrpcError{Code: -32601, Message: fmt.Sprintf("Method '%s' not found", req.Method)}
	}

	if req.ID == nil {
		return nil, nil
	}

	resp := jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
		Error:   rpcErr,
	}
	return json.Marshal(resp)
}

// MatchAndNormalizeToolName checks if an incoming tool name matches any registered client tool.
// If it matches (directly or with standard prefixes like "mcp_client-tools_" or "client-tools_"),
// it returns (canonicalName, true).
// If it does not match any registered client tool (such as internal agent tools like update_steps),
// it returns ("", false).
//
// @purpose Deterministically normalizes tool names and isolates client tools from internal agent tools.
// @rationale Antigravity prefixes tools registered via MCP with "mcp_<server>_" while built-in tools
// lack this prefix. This method strips runtime prefixes to ensure the API consumer receives the exact
// tool name declared in its request schema, while filtering out non-client internal agent tools.
// @param rawName Raw tool name received from ACP session/update or MCP tools/call.
// @returns canonicalName Normalized tool name declared in client schema, and matched bool flag.
// @invariants Thread-safe, non-mutating.
func (s *SessionState) MatchAndNormalizeToolName(rawName string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, t := range s.Tools {
		if rawName == t.Name {
			return t.Name, true
		}
		if strings.HasSuffix(rawName, "_"+t.Name) {
			return t.Name, true
		}
	}
	return "", false
}
