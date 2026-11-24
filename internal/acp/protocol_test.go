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
