package acp

import (
	"encoding/json"
	"io"
	"testing"
)

func TestTransport_YOLOAutoApprovalPriority(t *testing.T) {
	outReader, outWriter := io.Pipe()
	inReader, inWriter := io.Pipe()

	transport := NewTransport(outWriter, inReader, 1024*1024, nil)
	defer transport.Close()

	// 1. Test "allow_always" priority
	req1 := Request{
		JSONRPC: JSONRPCVersion,
		ID:      int64(1),
		Method:  "session/request_permission",
	}
	params1 := RequestPermissionParams{
		SessionID: "sess_1",
		Options: []PermissionOption{
			{OptionID: "deny_opt", Kind: "deny"},
			{OptionID: "once_opt", Kind: "allow_once"},
			{OptionID: "always_opt", Kind: "allow_always"},
		},
	}
	req1.Params, _ = json.Marshal(params1)
	reqBytes, _ := json.Marshal(req1)

	go func() {
		_, _ = inWriter.Write(append(reqBytes, '\n'))
	}()

	var resp Response
	decoder := json.NewDecoder(outReader)
	if err := decoder.Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	var result RequestPermissionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("failed to unmarshal permission result: %v", err)
	}

	if result.Outcome.OptionID != "always_opt" {
		t.Fatalf("expected always_opt to be selected, got %q", result.Outcome.OptionID)
	}
}

func TestSessionUpdate_PolymorphicContentChunk(t *testing.T) {
	tests := []struct {
		name         string
		jsonPayload  string
		expectedText string
		expectedType string
	}{
		{
			name:         "Single object content",
			jsonPayload:  `{"sessionId":"sess-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello world"}}}`,
			expectedText: "hello world",
			expectedType: "text",
		},
		{
			name:         "Array of objects content",
			jsonPayload:  `{"sessionId":"sess-1","update":{"sessionUpdate":"agent_message_chunk","content":[{"type":"text","text":"part 1 - "},{"type":"text","text":"part 2"}]}}`,
			expectedText: "part 1 - part 2",
			expectedType: "text",
		},
		{
			name:         "Array of raw strings content",
			jsonPayload:  `{"sessionId":"sess-1","update":{"sessionUpdate":"agent_thought_chunk","content":["thinking chunk 1, ", "thinking chunk 2"]}}`,
			expectedText: "thinking chunk 1, thinking chunk 2",
			expectedType: "",
		},
		{
			name:         "Plain string content",
			jsonPayload:  `{"sessionId":"sess-1","update":{"sessionUpdate":"agent_message_chunk","content":"single string value"}}`,
			expectedText: "single string value",
			expectedType: "",
		},
		{
			name:         "Null content",
			jsonPayload:  `{"sessionId":"sess-1","update":{"sessionUpdate":"tool_call","toolCallId":"call_123","content":null}}`,
			expectedText: "",
			expectedType: "",
		},
		{
			name:         "Missing content",
			jsonPayload:  `{"sessionId":"sess-1","update":{"sessionUpdate":"tool_call","toolCallId":"call_123"}}`,
			expectedText: "",
			expectedType: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var params SessionUpdateParams
			if err := json.Unmarshal([]byte(tt.jsonPayload), &params); err != nil {
				t.Fatalf("failed to unmarshal JSON: %v", err)
			}

			if tt.expectedText == "" {
				if params.Update.Content != nil && params.Update.Content.Text != "" {
					t.Fatalf("expected empty content text, got %q", params.Update.Content.Text)
				}
			} else {
				if params.Update.Content == nil {
					t.Fatalf("expected content not to be nil")
				}
				if params.Update.Content.Text != tt.expectedText {
					t.Errorf("expected text %q, got %q", tt.expectedText, params.Update.Content.Text)
				}
				if tt.expectedType != "" && params.Update.Content.Type != tt.expectedType {
					t.Errorf("expected type %q, got %q", tt.expectedType, params.Update.Content.Type)
				}
			}
		})
	}
}

func TestSessionNewParams_CanonicalACPv1(t *testing.T) {
	params := SessionNewParams{
		CWD: "/workspace/project",
		MCPServers: []MCPServerInfo{
			{
				Name:    "client-tools",
				Command: "/usr/bin/tool-bridge",
				Args:    []string{"--session", "123"},
				Env: []EnvVariable{
					{Name: "NODE_ENV", Value: "production"},
				},
			},
		},
		SystemPrompt: &SystemPrompt{
			Mode: "append",
			Content: []PromptBlock{
				{
					Type: "text",
					Text: "You are an expert systems programmer.",
				},
			},
		},
		Meta: map[string]any{
			"antigravity": map[string]any{
				"systemPrompt": map[string]any{
					"append": "You are an expert systems programmer.",
				},
			},
		},
	}

	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("failed to marshal SessionNewParams: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	// 1. Verify root systemPrompt
	sysPrompt, ok := parsed["systemPrompt"].(map[string]any)
	if !ok {
		t.Fatalf("expected root systemPrompt object, got %T", parsed["systemPrompt"])
	}
	if sysPrompt["mode"] != "append" {
		t.Fatalf("expected mode 'append', got %v", sysPrompt["mode"])
	}
	contentArr, ok := sysPrompt["content"].([]any)
	if !ok || len(contentArr) == 0 {
		t.Fatalf("expected non-empty content array, got %v", sysPrompt["content"])
	}

	// 2. Verify mcpServers env is an array of objects
	servers, ok := parsed["mcpServers"].([]any)
	if !ok || len(servers) == 0 {
		t.Fatalf("expected non-empty mcpServers array")
	}
	server0 := servers[0].(map[string]any)
	envArr, ok := server0["env"].([]any)
	if !ok || len(envArr) == 0 {
		t.Fatalf("expected env to be an array of EnvVariable, got %T", server0["env"])
	}
	env0 := envArr[0].(map[string]any)
	if env0["name"] != "NODE_ENV" || env0["value"] != "production" {
		t.Fatalf("unexpected EnvVariable payload: %+v", env0)
	}
}

func TestSessionUpdate_UsageUpdate(t *testing.T) {
	rawPayload := `{
		"sessionId": "sess-42",
		"update": {
			"sessionUpdate": "usage_update",
			"used": 15420,
			"size": 1048576,
			"cost": {
				"amount": 0.0035,
				"currency": "USD"
			}
		}
	}`

	var params SessionUpdateParams
	if err := json.Unmarshal([]byte(rawPayload), &params); err != nil {
		t.Fatalf("failed to unmarshal usage_update: %v", err)
	}

	if params.Update.SessionUpdate != UpdateUsage {
		t.Fatalf("expected UpdateUsage, got %q", params.Update.SessionUpdate)
	}
	if params.Update.Used == nil || *params.Update.Used != 15420 {
		t.Fatalf("expected used=15420, got %v", params.Update.Used)
	}
	if params.Update.Size == nil || *params.Update.Size != 1048576 {
		t.Fatalf("expected size=1048576, got %v", params.Update.Size)
	}
	if params.Update.Cost == nil || params.Update.Cost.Amount != 0.0035 || params.Update.Cost.Currency != "USD" {
		t.Fatalf("unexpected cost payload: %+v", params.Update.Cost)
	}
}
