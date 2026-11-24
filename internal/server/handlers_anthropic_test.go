package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agy-gateway/internal/acp"
	"agy-gateway/internal/anthropic"
	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
	"agy-gateway/internal/models"
	"agy-gateway/internal/pool"
	"agy-gateway/internal/server"
)

// setupAnthropicTestEnvironment sets up mock dependencies and an Anthropic handler.
func setupAnthropicTestEnvironment(t *testing.T) (*server.AnthropicHandlers, *config.Config, *models.ModelRegistry, *pool.Pool, *pool.FailoverCoordinator, func()) {
	cfg := config.DefaultConfig()
	cfg.Security.APIKeys = []config.APIKeyConfig{
		{
			ID:        "test-client-key",
			KeySecret: "sk-ant-test-key-12345",
		},
	}
	cfg.Execution.MaxRetries = 1
	cfg.Pool.FirstTokenTimeout = 5 * time.Second
	cfg.Pool.InferenceTimeout = 5 * time.Second

	authMgr := auth.NewManager(cfg, nil)
	modelRegistry := models.NewModelRegistry(cfg, authMgr, nil)

	// Register test Claude models
	modelRegistry.RegisterModel(&models.ModelInfo{
		ID:               "claude-3-5-sonnet",
		DisplayName:      "Claude 3.5 Sonnet",
		UpstreamModel:    "claude-3-5-sonnet",
		OwnedBy:          "anthropic",
		ContextWindow:    200000,
		MaxOutputTokens:  8192,
		RequiresPaidTier: false,
	})

	workerPool := &pool.Pool{
		// Test pool stub
	}
	failoverCoord := pool.NewFailoverCoordinator(workerPool, authMgr, nil)

	handlers := server.NewAnthropicHandlers(cfg, workerPool, failoverCoord, modelRegistry, nil, nil)

	cleanup := func() {}
	return handlers, cfg, modelRegistry, workerPool, failoverCoord, cleanup
}

func TestAnthropicHandlers_ValidationErrors(t *testing.T) {
	handlers, _, _, _, _, _ := setupAnthropicTestEnvironment(t)

	// 1. Invalid JSON
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("invalid-json{"))
	rec1 := httptest.NewRecorder()
	handlers.HandleMessages(rec1, req1)
	if rec1.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d", rec1.Code)
	}
	if !strings.Contains(rec1.Body.String(), "invalid_request_error") {
		t.Errorf("expected invalid_request_error in body, got: %s", rec1.Body.String())
	}

	// 2. Non-existent model
	req2Payload := `{"model": "non-existent-model", "max_tokens": 1024, "messages": [{"role": "user", "content": "Hi"}]}`
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(req2Payload))
	rec2 := httptest.NewRecorder()
	handlers.HandleMessages(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown model, got %d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "not_found_error") {
		t.Errorf("expected not_found_error in body, got: %s", rec2.Body.String())
	}

	// 3. Empty messages array
	req3Payload := `{"model": "claude-3-5-sonnet", "max_tokens": 1024, "messages": []}`
	req3 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(req3Payload))
	rec3 := httptest.NewRecorder()
	handlers.HandleMessages(rec3, req3)
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty messages, got %d", rec3.Code)
	}
	if !strings.Contains(rec3.Body.String(), "messages array must not be empty") {
		t.Errorf("expected validation message, got: %s", rec3.Body.String())
	}
}

// createMockWorkerWithPipe creates a Worker whose ACP transport is connected to an in-memory mock responder.
func createMockWorkerWithPipe(t *testing.T, id int, cfg *config.Config, responder func(method string, params json.RawMessage, inWriter io.Writer)) *pool.Worker {
	tmp := t.TempDir()
	w := pool.NewWorker(id, tmp, tmp, tmp, nil, cfg, nil)
	w.StartFn = func(ctx context.Context) error {
		proc, err := acp.SpawnProcess(context.Background(), "sleep", []string{"60"}, nil, tmp, nil)
		if err != nil {
			return err
		}
		w.Process = proc
		return nil
	}
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("failed to start mock worker process: %v", err)
	}

	// Create bidirection pipes:
	// outReader reads what Transport writes (requests from gateway)
	// inWriter writes what Transport reads (responses into gateway)
	outReader, outWriter := io.Pipe()
	inReader, inWriter := io.Pipe()

	transport := acp.NewTransport(outWriter, inReader, 134217728, nil)
	w.Transport = transport

	// Background mock ACP server loop
	go func() {
		scanner := bufio.NewScanner(outReader)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			var req struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      int64           `json:"id"`
				Method  string          `json:"method"`
				Params  json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal([]byte(line), &req); err != nil {
				continue
			}

			// Handle default ACP calls
			switch req.Method {
			case "session/new":
				resp := acp.Response{
					JSONRPC: acp.JSONRPCVersion,
					ID:      req.ID,
					Result:  json.RawMessage(`{"sessionId":"mock_sess_1"}`),
				}
				raw, _ := json.Marshal(resp)
				_, _ = inWriter.Write(append(raw, '\n'))
			case "session/set_config_option", "session/close":
				resp := acp.Response{
					JSONRPC: acp.JSONRPCVersion,
					ID:      req.ID,
					Result:  json.RawMessage(`{}`),
				}
				raw, _ := json.Marshal(resp)
				_, _ = inWriter.Write(append(raw, '\n'))
			case "session/prompt":
				// Custom responder handles emitting updates before resolving session/prompt
				if responder != nil {
					responder(req.Method, req.Params, inWriter)
					time.Sleep(20 * time.Millisecond)
				}
				resp := acp.Response{
					JSONRPC: acp.JSONRPCVersion,
					ID:      req.ID,
					Result:  json.RawMessage(`{"stopReason":"end_turn"}`),
				}
				raw, _ := json.Marshal(resp)
				_, _ = inWriter.Write(append(raw, '\n'))
			}
		}
	}()

	t.Cleanup(func() {
		_ = outReader.Close()
		_ = outWriter.Close()
		_ = inReader.Close()
		_ = inWriter.Close()
		w.Close()
	})

	return w
}

func TestAnthropicHandlers_EndToEnd_Sync(t *testing.T) {
	handlers, cfg, _, _, _, _ := setupAnthropicTestEnvironment(t)

	// Create mock worker that emits thinking + message text
	mockWorker := createMockWorkerWithPipe(t, 0, cfg, func(method string, params json.RawMessage, inWriter io.Writer) {
		// Emit thinking chunk
		thoughtUpdate := map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "mock_sess_1",
				"update": map[string]any{
					"sessionUpdate": acp.UpdateAgentThoughtChunk,
					"content": map[string]any{
						"type": "text",
						"text": "Analyzing the distributed architecture...",
					},
				},
			},
		}
		rawThought, _ := json.Marshal(thoughtUpdate)
		_, _ = inWriter.Write(append(rawThought, '\n'))

		// Emit text chunk
		textUpdate := map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "mock_sess_1",
				"update": map[string]any{
					"sessionUpdate": acp.UpdateAgentMessageChunk,
					"content": map[string]any{
						"type": "text",
						"text": "Distributed actor systems guarantee spatial isolation.",
					},
				},
			},
		}
		rawText, _ := json.Marshal(textUpdate)
		_, _ = inWriter.Write(append(rawText, '\n'))
	})

	// Inject worker into pool lease channel
	testPool := pool.NewTestPoolWithWorker(mockWorker, cfg)
	failoverCoord := pool.NewFailoverCoordinator(testPool, nil, nil)
	handlers = server.NewAnthropicHandlers(cfg, testPool, failoverCoord, handlers.ModelRegistry(), nil, nil)

	payload := `{
		"model": "claude-3-7-sonnet-20250219",
		"max_tokens": 2048,
		"thinking": {"type": "enabled", "budget_tokens": 1024},
		"messages": [
			{"role": "user", "content": "Explain actor isolation."}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	rec := httptest.NewRecorder()

	handlers.HandleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if ver := rec.Header().Get("anthropic-version"); ver != "2023-06-01" {
		t.Errorf("expected anthropic-version '2023-06-01', got %q", ver)
	}

	var resp anthropic.MessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Role != "assistant" {
		t.Errorf("expected role 'assistant', got %q", resp.Role)
	}
	if resp.StopReason == nil || *resp.StopReason != "end_turn" {
		t.Errorf("expected stop_reason 'end_turn', got %v", resp.StopReason)
	}

	// Verify content blocks: thinking block + text block
	if len(resp.Content) != 2 {
		t.Fatalf("expected 2 content blocks (thinking + text), got %d", len(resp.Content))
	}
	if resp.Content[0].Type != "thinking" || resp.Content[0].Thinking != "Analyzing the distributed architecture..." {
		t.Errorf("unexpected thinking block: %+v", resp.Content[0])
	}
	if resp.Content[0].Signature == "" {
		t.Errorf("expected thinking block signature to be populated")
	}
	if resp.Content[1].Type != "text" || !strings.Contains(resp.Content[1].Text, "Distributed actor systems guarantee spatial isolation.") {
		t.Errorf("unexpected text block: %+v", resp.Content[1])
	}

	if resp.Usage.InputTokens <= 0 || resp.Usage.OutputTokens <= 0 {
		t.Errorf("expected positive usage metrics, got %+v", resp.Usage)
	}
}

func TestAnthropicHandlers_EndToEnd_ToolCalling(t *testing.T) {
	handlers, cfg, _, _, _, _ := setupAnthropicTestEnvironment(t)

	mockWorker := createMockWorkerWithPipe(t, 0, cfg, func(method string, params json.RawMessage, inWriter io.Writer) {
		// Emit tool call encapsulated in delimiter
		textUpdate := map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "mock_sess_1",
				"update": map[string]any{
					"sessionUpdate": acp.UpdateAgentMessageChunk,
					"content": map[string]any{
						"type": "text",
						"text": `<tool_call>{"id":"call_weather_1","name":"get_weather","arguments":{"location":"Tokyo"}}</tool_call>`,
					},
				},
			},
		}
		rawText, _ := json.Marshal(textUpdate)
		_, _ = inWriter.Write(append(rawText, '\n'))
	})

	testPool := pool.NewTestPoolWithWorker(mockWorker, cfg)
	failoverCoord := pool.NewFailoverCoordinator(testPool, nil, nil)
	handlers = server.NewAnthropicHandlers(cfg, testPool, failoverCoord, handlers.ModelRegistry(), nil, nil)

	payload := `{
		"model": "claude-3-5-sonnet",
		"max_tokens": 2048,
		"messages": [
			{"role": "user", "content": "What is the weather in Tokyo?"}
		],
		"tools": [
			{
				"name": "get_weather",
				"description": "Fetch weather",
				"input_schema": {"type": "object", "properties": {"location": {"type": "string"}}}
			}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	rec := httptest.NewRecorder()

	handlers.HandleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp anthropic.MessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.StopReason == nil || *resp.StopReason != "tool_use" {
		t.Errorf("expected stop_reason 'tool_use', got %v", resp.StopReason)
	}

	if len(resp.Content) != 1 || resp.Content[0].Type != "tool_use" {
		t.Fatalf("expected 1 tool_use content block, got %+v", resp.Content)
	}
	if resp.Content[0].Name != "get_weather" {
		t.Errorf("expected tool name 'get_weather', got %q", resp.Content[0].Name)
	}
	if !strings.HasPrefix(resp.Content[0].ID, "toolu_") {
		t.Errorf("expected tool ID to start with 'toolu_', got %q", resp.Content[0].ID)
	}
}

func TestAnthropicHandlers_EndToEnd_Streaming(t *testing.T) {
	handlers, cfg, _, _, _, _ := setupAnthropicTestEnvironment(t)

	mockWorker := createMockWorkerWithPipe(t, 0, cfg, func(method string, params json.RawMessage, inWriter io.Writer) {
		// Emit thinking chunk
		thoughtUpdate := map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "mock_sess_1",
				"update": map[string]any{
					"sessionUpdate": acp.UpdateAgentThoughtChunk,
					"content": map[string]any{
						"type": "text",
						"text": "Thinking deeply...",
					},
				},
			},
		}
		rawThought, _ := json.Marshal(thoughtUpdate)
		_, _ = inWriter.Write(append(rawThought, '\n'))

		// Emit text chunk
		textUpdate := map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "mock_sess_1",
				"update": map[string]any{
					"sessionUpdate": acp.UpdateAgentMessageChunk,
					"content": map[string]any{
						"type": "text",
						"text": "Hello from streaming Claude!",
					},
				},
			},
		}
		rawText, _ := json.Marshal(textUpdate)
		_, _ = inWriter.Write(append(rawText, '\n'))
	})

	testPool := pool.NewTestPoolWithWorker(mockWorker, cfg)
	failoverCoord := pool.NewFailoverCoordinator(testPool, nil, nil)
	handlers = server.NewAnthropicHandlers(cfg, testPool, failoverCoord, handlers.ModelRegistry(), nil, nil)

	payload := `{
		"model": "claude-3-5-sonnet",
		"max_tokens": 1024,
		"stream": true,
		"messages": [
			{"role": "user", "content": "Stream hello"}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	rec := httptest.NewRecorder()

	handlers.HandleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	contentType := rec.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/event-stream") {
		t.Errorf("expected text/event-stream content type, got %s", contentType)
	}

	body := rec.Body.String()

	// Verify all 6 mandatory Anthropic stream events are present
	expectedEvents := []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	}

	for _, evt := range expectedEvents {
		if !strings.Contains(body, evt) {
			t.Errorf("streaming output missing expected event %q:\n%s", evt, body)
		}
	}

	// Verify thinking_delta was streamed
	if !strings.Contains(body, "thinking_delta") {
		t.Errorf("expected stream to contain thinking_delta event")
	}

	// Verify text_delta was streamed
	if !strings.Contains(body, "text_delta") || !strings.Contains(body, "Hello from streaming Claude!") {
		t.Errorf("expected stream to contain text_delta with message text")
	}
}

func TestAnthropicHandlers_EndToEnd_StreamingToolUse(t *testing.T) {
	handlers, cfg, _, _, _, _ := setupAnthropicTestEnvironment(t)

	mockWorker := createMockWorkerWithPipe(t, 0, cfg, func(method string, params json.RawMessage, inWriter io.Writer) {
		// Emit tool call in message chunk
		textUpdate := map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "mock_sess_tool_use",
				"update": map[string]any{
					"sessionUpdate": acp.UpdateAgentMessageChunk,
					"content": map[string]any{
						"type": "text",
						"text": `<tool_call>{"id": "call_123", "name": "write", "arguments": {"command": "pwd && git status --porcelain"}}</tool_call>`,
					},
				},
			},
		}
		rawText, _ := json.Marshal(textUpdate)
		_, _ = inWriter.Write(append(rawText, '\n'))
	})

	testPool := pool.NewTestPoolWithWorker(mockWorker, cfg)
	failoverCoord := pool.NewFailoverCoordinator(testPool, nil, nil)
	handlers = server.NewAnthropicHandlers(cfg, testPool, failoverCoord, handlers.ModelRegistry(), nil, nil)

	payload := `{
		"model": "claude-3-5-sonnet",
		"max_tokens": 1024,
		"stream": true,
		"messages": [
			{"role": "user", "content": "Run git status"}
		],
		"tools": [
			{
				"name": "write",
				"description": "Execute write command",
				"input_schema": {
					"type": "object",
					"properties": {
						"command": {"type": "string"}
					},
					"required": ["command"]
				}
			}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	rec := httptest.NewRecorder()

	handlers.HandleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()

	// 1. Verify content_block_start has input: {} (empty JSON object as mandated by Anthropic spec)
	if !strings.Contains(body, `"type":"tool_use"`) {
		t.Fatalf("expected stream to contain tool_use content block start:\n%s", body)
	}
	if !strings.Contains(body, `"input":{}`) {
		t.Errorf("expected content_block_start to have empty 'input:{}' object to prevent client duplication, got:\n%s", body)
	}

	// 2. Verify input_json_delta delivers arguments
	if !strings.Contains(body, `"type":"input_json_delta"`) {
		t.Errorf("expected stream to contain input_json_delta event")
	}

	// 3. Simulate client parsing (e.g. OpenCode / @anthropic-ai/sdk)
	var accumulatedJSON string
	lines := strings.Split(body, "\n")
	for _, l := range lines {
		if strings.HasPrefix(l, "data: ") {
			dataStr := strings.TrimPrefix(l, "data: ")
			var evt map[string]any
			if err := json.Unmarshal([]byte(dataStr), &evt); err == nil {
				if delta, ok := evt["delta"].(map[string]any); ok {
					if delta["type"] == "input_json_delta" {
						if pj, ok := delta["partial_json"].(string); ok {
							accumulatedJSON += pj
						}
					}
				}
			}
		}
	}

	var parsedArgs map[string]any
	if err := json.Unmarshal([]byte(accumulatedJSON), &parsedArgs); err != nil {
		t.Fatalf("client failed to parse accumulated tool JSON arguments: %v (raw: %q)", err, accumulatedJSON)
	}

	if cmd, ok := parsedArgs["command"].(string); !ok || cmd != "pwd && git status --porcelain" {
		t.Errorf("expected command 'pwd && git status --porcelain', got %v", parsedArgs["command"])
	}
}
