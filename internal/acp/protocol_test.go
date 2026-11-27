package acp

import (
	"encoding/json"
	"io"
	"strings"
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

// TestTransport_PermissionSelectionMatrix covers every documented PermissionOptionKind combination,
// including the reject_* kinds that the old substring-based logic could accidentally select.
func TestTransport_PermissionSelectionMatrix(t *testing.T) {
	cases := []struct {
		name     string
		options  []PermissionOption
		wantKind string // "" means expect cancelled
	}{
		{
			name:     "reject_only_cancels",
			options:  []PermissionOption{{OptionID: "allow-ish", Kind: "reject_once"}, {OptionID: "nope", Kind: "reject_always"}},
			wantKind: "",
		},
		{
			name:     "allow_once_beats_unknown_allow_prefix",
			options:  []PermissionOption{{OptionID: "allow_custom", Kind: "allow_special"}, {OptionID: "a", Kind: "allow_once"}},
			wantKind: "allow_once",
		},
		{
			name:     "allow_always_wins",
			options:  []PermissionOption{{OptionID: "a", Kind: "allow_once"}, {OptionID: "b", Kind: "allow_always"}},
			wantKind: "allow_always",
		},
		{
			name:     "unknown_allow_kind_selected",
			options:  []PermissionOption{{OptionID: "custom", Kind: "allow_forever"}},
			wantKind: "allow_forever",
		},
		{
			name:     "empty_options_cancels",
			options:  nil,
			wantKind: "",
		},
		{
			// The old implementation selected options[0] blindly; a `deny` option could be chosen.
			name:     "deny_first_is_never_selected",
			options:  []PermissionOption{{OptionID: "deny", Kind: "reject_once"}},
			wantKind: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outReader, outWriter := io.Pipe()
			inReader, inWriter := io.Pipe()
			transport := NewTransport(outWriter, inReader, 1024*1024, nil)
			defer transport.Close()

			req := Request{JSONRPC: JSONRPCVersion, ID: int64(7), Method: "session/request_permission"}
			req.Params, _ = json.Marshal(RequestPermissionParams{SessionID: "s", Options: tc.options})
			reqBytes, _ := json.Marshal(req)

			go func() { _, _ = inWriter.Write(append(reqBytes, '\n')) }()

			var resp Response
			if err := json.NewDecoder(outReader).Decode(&resp); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}
			var result RequestPermissionResult
			if err := json.Unmarshal(resp.Result, &result); err != nil {
				t.Fatalf("failed to unmarshal result: %v", err)
			}

			if tc.wantKind == "" {
				if result.Outcome.Outcome != "cancelled" {
					t.Fatalf("expected cancelled, got %+v", result.Outcome)
				}
				return
			}
			if result.Outcome.Outcome != "selected" {
				t.Fatalf("expected selected, got %+v", result.Outcome)
			}
			var chosenKind string
			for _, o := range tc.options {
				if o.OptionID == result.Outcome.OptionID {
					chosenKind = o.Kind
				}
			}
			if chosenKind != tc.wantKind {
				t.Fatalf("expected kind %q, got %q (outcome %+v)", tc.wantKind, chosenKind, result.Outcome)
			}
		})
	}
}

func TestTransport_DispatchResponseNeverRacesClose(t *testing.T) {
	for i := 0; i < 200; i++ {
		outReader, outWriter := io.Pipe()
		inReader, _ := io.Pipe()
		transport := NewTransport(outWriter, inReader, 1024*1024, nil)

		// Register a pending waiter exactly as Call does.
		ch := make(chan *Response, 1)
		transport.pendingMu.Lock()
		transport.pending[int64(i)] = ch
		transport.pendingMu.Unlock()

		done := make(chan struct{})
		go func() {
			defer close(done)
			transport.dispatchResponse(&Response{JSONRPC: JSONRPCVersion, ID: float64(i)})
		}()
		go func() { _ = transport.Close() }()
		<-done

		// Drain so the pipe goroutines can exit.
		_, _ = io.Copy(io.Discard, outReader)
	}
}

func TestTransport_UpdateListenerReplacementIsReported(t *testing.T) {
	_, outWriter := io.Pipe()
	inReader, _ := io.Pipe()
	transport := NewTransport(outWriter, inReader, 1024*1024, nil)
	defer transport.Close()

	first := func(SessionUpdateParams) {}
	second := func(SessionUpdateParams) {}
	transport.SetUpdateListener(first)
	transport.SetUpdateListener(second) // must log but not panic
	transport.SetUpdateListener(nil)
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
	}

	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("failed to marshal SessionNewParams: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	// 1. No prompt carrier exists at all: neither the spec-level systemPrompt nor the vendor _meta
	//    dialect may appear, because agy accepts the former and can refuse the latter.
	for _, key := range []string{"systemPrompt", "_meta"} {
		if _, present := parsed[key]; present {
			t.Fatalf("%s must never be emitted, got %+v", key, parsed[key])
		}
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

// TestSessionNewParams_NoPromptCarrier proves the payload never grows a prompt field: the struct has
// no _meta at all, so no future refactor can quietly reintroduce the policy-guarded carrier.
func TestSessionNewParams_NoPromptCarrier(t *testing.T) {
	raw, err := json.Marshal(SessionNewParams{CWD: "/w"})
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	for _, key := range []string{"_meta", "systemPrompt"} {
		if _, present := parsed[key]; present {
			t.Fatalf("%s must be absent from session/new, got %v", key, parsed[key])
		}
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

func TestParseAgentCapabilities(t *testing.T) {
	// 1. Nil input safety — never panics, everything defaults false.
	emptyCaps := ParseAgentCapabilities(nil)
	if emptyCaps.SupportsSessionClose || emptyCaps.LoadSession {
		t.Fatalf("expected all false for nil initResult, got %+v", emptyCaps)
	}

	// 2. Real agy acp v1.5.16 shape. The vendor _meta block is present in the real response but must
	//    contribute nothing: the gateway never sends a system prompt through session/new.
	agyPayload := map[string]any{
		"protocolVersion": float64(1),
		"agentCapabilities": map[string]any{
			"loadSession": true,
			"promptCapabilities": map[string]any{
				"image":           true,
				"audio":           false,
				"embeddedContext": true,
			},
			"sessionCapabilities": map[string]any{
				"resume":                map[string]any{},
				"close":                 map[string]any{},
				"load":                  map[string]any{},
				"list":                  map[string]any{},
				"additionalDirectories": map[string]any{},
			},
		},
	}
	caps := ParseAgentCapabilities(agyPayload)
	if caps.ProtocolVersion != 1 || !caps.LoadSession || !caps.SupportsAdditionalDirs ||
		!caps.SupportsSessionClose {
		t.Fatalf("agy 1.5.16 capability parse wrong: %+v", caps)
	}
	if !caps.PromptImage || caps.PromptEmbeddedContext != true {
		t.Fatalf("expected promptCapabilities image/embeddedContext parsed; got %+v", caps)
	}

	// 3. Vendor snake_case duplicates are accepted as aliases.
	aliased := ParseAgentCapabilities(map[string]any{
		"agentCapabilities": map[string]any{
			"load_session": true,
			"session_capabilities": map[string]any{
				"close": map[string]any{},
			},
		},
	})
	if !aliased.LoadSession || !aliased.SupportsSessionClose {
		t.Fatalf("snake_case capability aliases must be honoured; got %+v", aliased)
	}

	// 4. Malformed / hostile JSON types never panic and never produce false positives.
	hostile := ParseAgentCapabilities(map[string]any{
		"protocolVersion":   "one",
		"agentCapabilities": "nope",
	})
	if hostile.ProtocolVersion != 0 || hostile.LoadSession || hostile.SupportsSessionClose {
		t.Fatalf("malformed payload must degrade silently; got %+v", hostile)
	}
	hostile2 := ParseAgentCapabilities(map[string]any{
		"agentCapabilities": map[string]any{
			"sessionCapabilities": map[string]any{
				"systemPrompt": "not-an-object",
			},
			"mcpCapabilities": map[string]any{"http": "yes"},
			"_meta":           map[string]any{"antigravity": 42},
		},
	})
	if hostile2.SupportsMCPHTTP || hostile2.SupportsSessionClose {
		t.Fatalf("non-boolean capability values must not be treated as true; got %+v", hostile2)
	}
}

func TestSanitizeToolArguments(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"},"unit":{"type":"string"}},"required":["city"]}`)

	cases := []struct {
		name   string
		raw    string
		schema json.RawMessage
		want   string
	}{
		{
			name:   "strips vendor toolSummary",
			raw:    `{"city":"Jakarta","toolSummary":"Get weather for Jakarta"}`,
			schema: schema,
			want:   `{"city":"Jakarta"}`,
		},
		{
			name:   "keeps every declared property",
			raw:    `{"city":"Jakarta","unit":"celsius"}`,
			schema: schema,
			want:   `{"city":"Jakarta","unit":"celsius"}`,
		},
		{
			name:   "drops undeclared keys when schema is usable",
			raw:    `{"city":"Jakarta","hallucinated":1}`,
			schema: schema,
			want:   `{"city":"Jakarta"}`,
		},
		{
			name:   "without schema only known vendor keys are dropped",
			raw:    `{"city":"Jakarta","toolSummary":"x"}`,
			schema: nil,
			want:   `{"city":"Jakarta"}`,
		},
		{
			name:   "null degrades to empty object",
			raw:    `null`,
			schema: schema,
			want:   `{}`,
		},
		{
			name:   "empty degrades to empty object",
			raw:    ``,
			schema: schema,
			want:   `{}`,
		},
		{
			name:   "non-object degrades to empty object",
			raw:    `["city"]`,
			schema: schema,
			want:   `{}`,
		},
		{
			name:   "everything stripped yields empty object",
			raw:    `{"toolSummary":"only vendor key"}`,
			schema: schema,
			want:   `{}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeToolArguments(json.RawMessage(tc.raw), tc.schema, VendorToolArgumentKeys...)
			var gotObj, wantObj map[string]any
			if err := json.Unmarshal(got, &gotObj); err != nil {
				t.Fatalf("sanitized output is not valid JSON object: %s", string(got))
			}
			if err := json.Unmarshal([]byte(tc.want), &wantObj); err != nil {
				t.Fatalf("bad want fixture: %v", err)
			}
			if len(gotObj) != len(wantObj) {
				t.Fatalf("got %s, want %s", string(got), tc.want)
			}
			for k, v := range wantObj {
				if gotObj[k] != v {
					t.Fatalf("key %q: got %v, want %v (full: %s)", k, gotObj[k], v, string(got))
				}
			}
		})
	}

	// The toolSummary key must never survive, regardless of schema shape.
	for _, sch := range []string{``, `{}`, `{"properties":{}}`, `not-json`} {
		out := SanitizeToolArguments(json.RawMessage(`{"city":"X","toolSummary":"Y"}`), json.RawMessage(sch), VendorToolArgumentKeys...)
		if strings.Contains(string(out), "toolSummary") {
			t.Fatalf("toolSummary survived for schema %q: %s", sch, string(out))
		}
	}
}

func TestRequestPermissionParams_ResolvedToolCallID(t *testing.T) {
	nested := &RequestPermissionParams{
		SessionID: "s1",
		ToolCall:  &PermissionToolCall{ToolCallID: "call_nested", Name: "t", Args: map[string]any{"a": 1}},
		Options:   []PermissionOption{{OptionID: "allow", Kind: "allow_once"}},
	}
	if got := nested.ResolvedToolCallID(); got != "call_nested" {
		t.Fatalf("expected nested toolCallId, got %q", got)
	}

	flat := &RequestPermissionParams{SessionID: "s1", ToolCallID: "call_flat"}
	if got := flat.ResolvedToolCallID(); got != "call_flat" {
		t.Fatalf("expected flat fallback, got %q", got)
	}

	var nilParams *RequestPermissionParams
	if got := nilParams.ResolvedToolCallID(); got != "" {
		t.Fatalf("nil receiver must return empty, got %q", got)
	}
}

func TestSessionNewResult_ConfigOptionByID(t *testing.T) {
	var raw = `{
      "sessionId":"abc",
      "configOptions":[
        {"id":"thought_level","category":"thought_level","type":"select","currentValue":"high",
         "options":[{"value":"low"},{"value":"medium"},{"value":"high"},{"value":"xhigh","name":"Extra"}]}
      ],
      "usage":{"inputTokens":10,"outputTokens":20,"cachedReadTokens":3,"cachedWriteTokens":0,"thoughtTokens":5,"totalTokens":30}
    }`
	var res SessionNewResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("failed to decode session/new result: %v", err)
	}
	opt := res.ConfigOptionByID("thought_level")
	if opt == nil || len(opt.Options) != 4 {
		t.Fatalf("expected 4 thought_level options, got %+v", opt)
	}
	if opt.Options[3].Value != "xhigh" || opt.Options[3].Name != "Extra" {
		t.Fatalf("wire value must be xhigh with label Extra, got %+v", opt.Options[3])
	}
	if res.ConfigOptionByID("nope") != nil {
		t.Fatalf("unknown option must return nil")
	}
	var nilRes *SessionNewResult
	if nilRes.ConfigOptionByID("model") != nil {
		t.Fatalf("nil receiver must return nil")
	}
}

func TestClientToolDelegationAndBuiltInChecks(t *testing.T) {
	if !IsBuiltInContainerTool("read_file") || !IsBuiltInContainerTool("run_command") {
		t.Fatalf("expected read_file and run_command to be detected as built-in container tools")
	}
	if IsBuiltInContainerTool("mcp_client-tools_read") || IsBuiltInContainerTool("read") {
		t.Fatalf("client tools should not be flagged as built-in container tools")
	}

	if d := ClientToolDelegationDirective(nil); d != "" {
		t.Fatalf("expected empty directive for nil toolNames, got %q", d)
	}
	directive := ClientToolDelegationDirective([]string{"read", "bash"})
	if !strings.Contains(directive, "mcp_client-tools_read") || !strings.Contains(directive, "mcp_client-tools_bash") {
		t.Fatalf("expected prefixed mcp_client-tools_ names in directive, got %q", directive)
	}
	if !strings.Contains(directive, "NEVER call any built-in container tools") {
		t.Fatalf("expected strict prohibition on built-in container tools")
	}
}
