// Package mcp implements standard Model Context Protocol (MCP) server primitives
// and loopback tool interception for agy-gateway.
package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agy-gateway/internal/version"
)

var (
	ErrSessionNotFound = errors.New("mcp session not registered or expired")
	ErrMethodNotFound  = errors.New("mcp method not supported")
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
}

// NewLoopbackServer creates an initialized in-memory Loopback MCP Server.
//
// @purpose Coordinates real-time MCP tool discovery and execution interception without prompt injection.
// @rationale By presenting an official MCP server interface to agy acp, the model invokes tools natively
// through Google's internal function calling engine rather than generating raw XML tags in text streams.
// @invariants Thread-safe, non-blocking registration, automatic lifecycle cleanup.
func NewLoopbackServer() *LoopbackServer {
	return &LoopbackServer{
		sessions: make(map[string]*SessionState),
	}
}

// RegisterSession registers an array of OpenAI tools under a unique session ID.
//
// @purpose Converts OpenAI function declarations into standard MCP tool schemas and opens an interception channel.
// @rationale Ensures zero prompt pollution; tool schemas are fetched by agy acp via MCP tools/list.
// @param sessionID Unique session correlation identifier.
// @param tools Array of OpenAI tool definitions from the consumer request.
// @returns Initialized SessionState with an active InterceptedToolCall channel.
// @invariants Thread-safe, idempotent overwrite if session ID exists.
func (s *LoopbackServer) RegisterSession(sessionID string, tools []OpenAIToolDefinition) *SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()

	var mcpTools []MCPToolDefinition
	for _, t := range tools {
		if t.Type != "function" || t.Function.Name == "" {
			continue
		}
		schema := t.Function.Parameters
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		mcpTools = append(mcpTools, MCPToolDefinition{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}

	state := &SessionState{
		SessionID:  sessionID,
		Tools:      mcpTools,
		Calls:      make(chan InterceptedToolCall, 32),
		CreatedAt:  time.Now(),
		LastActive: time.Now(),
	}

	if existing, exists := s.sessions[sessionID]; exists {
		existing.Close()
	}
	s.sessions[sessionID] = state
	return state
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
func (s *LoopbackServer) RegisterAnthropicSession(sessionID string, tools []AnthropicToolDefinition) *SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()

	var mcpTools []MCPToolDefinition
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		schema := t.InputSchema
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		mcpTools = append(mcpTools, MCPToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}

	state := &SessionState{
		SessionID:  sessionID,
		Tools:      mcpTools,
		Calls:      make(chan InterceptedToolCall, 32),
		CreatedAt:  time.Now(),
		LastActive: time.Now(),
	}

	if existing, exists := s.sessions[sessionID]; exists {
		existing.Close()
	}
	s.sessions[sessionID] = state
	return state
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
func (s *LoopbackServer) UnregisterSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state, ok := s.sessions[sessionID]; ok {
		state.Close()
		delete(s.sessions, sessionID)
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
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
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

	state, err := s.GetSession(sessionID)
	if err != nil && req.Method != "initialize" && req.Method != "ping" {
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

	case "tools/list":
		toolsList := []MCPToolDefinition{}
		if state != nil {
			state.mu.Lock()
			toolsList = append(toolsList, state.Tools...)
			state.LastActive = time.Now()
			state.mu.Unlock()
		}
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

// ServeHTTP provides a native HTTP endpoint for MCP over HTTP transport.
// Route: POST /internal/mcp/:sessionId
func (s *LoopbackServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(pathParts) < 3 || pathParts[0] != "internal" || pathParts[1] != "mcp" {
		http.Error(w, "Invalid MCP endpoint URL path", http.StatusBadRequest)
		return
	}
	sessionID := pathParts[2]

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	respBytes, err := s.HandleJSONRPC(sessionID, body)
	if err != nil {
		http.Error(w, "MCP error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if respBytes == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
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

