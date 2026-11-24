package mcp

import (
	"encoding/json"
	"testing"
)

func TestLoopbackServer_LifecycleAndToolCallInterception(t *testing.T) {
	server := NewLoopbackServer()
	sessionID := "test_sess_001"

	tools := []OpenAIToolDefinition{
		{
			Type: "function",
			Function: FunctionSchema{
				Name:        "write_file",
				Description: "Writes content to disk",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
			},
		},
	}

	state := server.RegisterSession(sessionID, tools)
	if state == nil {
		t.Fatalf("expected state to be non-nil")
	}

	// 1. Test initialize
	initReq := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	initRespBytes, err := server.HandleJSONRPC(sessionID, initReq)
	if err != nil {
		t.Fatalf("initialize failed: %v", err)
	}
	var initResp struct {
		Result struct {
			Capabilities struct {
				Tools any `json:"tools"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(initRespBytes, &initResp); err != nil {
		t.Fatalf("failed to unmarshal init response: %v", err)
	}
	if initResp.Result.Capabilities.Tools == nil {
		t.Fatalf("expected tools capabilities in init response")
	}

	// 2. Test tools/list
	listReq := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	listRespBytes, err := server.HandleJSONRPC(sessionID, listReq)
	if err != nil {
		t.Fatalf("tools/list failed: %v", err)
	}
	var listResp struct {
		Result struct {
			Tools []MCPToolDefinition `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(listRespBytes, &listResp); err != nil {
		t.Fatalf("failed to unmarshal tools/list response: %v", err)
	}
	if len(listResp.Result.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(listResp.Result.Tools))
	}
	if listResp.Result.Tools[0].Name != "write_file" {
		t.Fatalf("expected tool name 'write_file', got %q", listResp.Result.Tools[0].Name)
	}

	// 3. Test tools/call
	callReq := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"write_file","arguments":{"path":"main.go"}}}`)
	callRespBytes, err := server.HandleJSONRPC(sessionID, callReq)
	if err != nil {
		t.Fatalf("tools/call failed: %v", err)
	}
	var callResp struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(callRespBytes, &callResp); err != nil {
		t.Fatalf("failed to unmarshal tools/call response: %v", err)
	}
	if callResp.Result.IsError {
		t.Fatalf("expected isError to be false")
	}

	// Verify channel interception
	select {
	case intercepted := <-state.Calls:
		if intercepted.Name != "write_file" {
			t.Fatalf("expected intercepted tool 'write_file', got %q", intercepted.Name)
		}
		if string(intercepted.Arguments) != `{"path":"main.go"}` {
			t.Fatalf("expected arguments `{\"path\":\"main.go\"}`, got %s", string(intercepted.Arguments))
		}
	default:
		t.Fatalf("expected tool call to be intercepted on channel")
	}

	// 4. Test unregister
	server.UnregisterSession(sessionID)
	_, err = server.GetSession(sessionID)
	if err != ErrSessionNotFound {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestLoopbackServer_AnthropicSessionRegistration(t *testing.T) {
	server := NewLoopbackServer()
	sessionID := "test_anthropic_sess_002"

	tools := []AnthropicToolDefinition{
		{
			Name:        "edit_file",
			Description: "Edits a file",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"target":{"type":"string"}}}`),
		},
	}

	state := server.RegisterAnthropicSession(sessionID, tools)
	if state == nil {
		t.Fatalf("expected state to be non-nil")
	}

	listReq := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	listRespBytes, err := server.HandleJSONRPC(sessionID, listReq)
	if err != nil {
		t.Fatalf("tools/list failed: %v", err)
	}
	var listResp struct {
		Result struct {
			Tools []MCPToolDefinition `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(listRespBytes, &listResp); err != nil {
		t.Fatalf("failed to unmarshal tools/list response: %v", err)
	}
	if len(listResp.Result.Tools) != 1 || listResp.Result.Tools[0].Name != "edit_file" {
		t.Fatalf("unexpected tools returned: %+v", listResp.Result.Tools)
	}
}
