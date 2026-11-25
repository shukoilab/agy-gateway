package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"agy-gateway/internal/acp"
	"agy-gateway/internal/anthropic"
	"agy-gateway/internal/config"
	"agy-gateway/internal/mcp"
	"agy-gateway/internal/pool"
)

func TestBuildACPSessionNewParams_CapabilityNegotiationAndDelegation(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.Port = 8182
	cfg.Execution.StrictClientToolDelegation = true

	// 1. agy acp v1.5.16 capabilities (append only, no top-level override)
	wAppend := &pool.Worker{
		Cwd: "/var/lib/agy-gateway/workspaces/w0",
		Capabilities: acp.AgentCapabilities{
			// Vendor-dialect agent: agy 1.5.16 advertises only _meta.antigravity.systemPrompt.append.
			SupportsVendorSystemPrompt:  true,
			SupportsSystemPromptAppend:  true,
			SupportsSystemPromptOveride: false,
			SupportsSystemPrompt:        false,
		},
	}

	params, _ := buildACPSessionNewParams(SessionSetupInput{
		Worker:               wAppend,
		Config:               cfg,
		ConsumerSystemPrompt: "You are OpenCode running on host.",
		LoopbackSessionID:    "sess_abc123",
		ClientToolNames:      []string{"read", "bash"},
	})

	if params.SystemPrompt != nil {
		t.Fatalf("expected nil top-level SystemPrompt when SupportsSystemPrompt is false, got %+v", params.SystemPrompt)
	}

	agyMeta, ok := params.Meta["antigravity"].(map[string]any)
	if !ok {
		t.Fatalf("expected antigravity meta map")
	}
	sysMap, ok := agyMeta["systemPrompt"].(map[string]any)
	if !ok {
		t.Fatalf("expected systemPrompt meta map")
	}
	appended, ok := sysMap["append"].(string)
	if !ok || appended == "" {
		t.Fatalf("expected non-empty append systemPrompt, got %+v", sysMap)
	}
	if _, hasOverride := sysMap["override"]; hasOverride {
		t.Fatalf("must not emit override key when agent only supports append")
	}
	if !strings.Contains(appended, "You are OpenCode running on host.") {
		t.Fatalf("expected consumer prompt inside append payload")
	}
	if !strings.Contains(appended, "mcp_client-tools_read") || !strings.Contains(appended, "mcp_client-tools_bash") {
		t.Fatalf("expected ClientToolDelegationDirective inside append payload")
	}

	if len(params.MCPServers) != 1 || params.MCPServers[0].Name != "client-tools" {
		t.Fatalf("expected client-tools MCPServer entry, got %+v", params.MCPServers)
	}
	if params.MCPServers[0].Env == nil {
		t.Fatalf("expected non-nil empty Env slice on stdio MCPServerInfo")
	}

	// 2. Empty consumer prompt triggers DefaultExecutionProtocol
	paramsDefault, _ := buildACPSessionNewParams(SessionSetupInput{
		Worker: wAppend,
		Config: cfg,
	})
	defaultAppend := paramsDefault.Meta["antigravity"].(map[string]any)["systemPrompt"].(map[string]any)["append"].(string)
	if !strings.Contains(defaultAppend, "Strict Language Mirroring") {
		t.Fatalf("expected DefaultExecutionProtocol when consumer prompt is empty, got %q", defaultAppend)
	}

	// 3. Canonical ACP v1 agent advertising override capability
	wOverride := &pool.Worker{
		Cwd: "/var/lib/agy-gateway/workspaces/w1",
		Capabilities: acp.AgentCapabilities{
			SupportsSystemPrompt:        true,
			SupportsSystemPromptOveride: true,
		},
	}
	paramsOv, _ := buildACPSessionNewParams(SessionSetupInput{
		Worker:               wOverride,
		Config:               cfg,
		ConsumerSystemPrompt: "Custom prompt",
	})
	if paramsOv.SystemPrompt == nil || paramsOv.SystemPrompt.Mode != "override" {
		t.Fatalf("expected top-level SystemPrompt mode 'override' when advertised, got %+v", paramsOv.SystemPrompt)
	}
}

func TestToolNameExtractionHelpers(t *testing.T) {
	oaTools := []mcp.OpenAIToolDefinition{
		{Type: "function", Function: mcp.FunctionSchema{Name: "read"}},
		{Type: "other", Function: mcp.FunctionSchema{Name: "ignored"}},
		{Type: "function", Function: mcp.FunctionSchema{Name: "bash"}},
	}
	oaNames := extractOpenAIToolNames(oaTools)
	if len(oaNames) != 2 || oaNames[0] != "read" || oaNames[1] != "bash" {
		t.Fatalf("unexpected OpenAI tool names: %v", oaNames)
	}

	anTools := []anthropic.Tool{
		{Name: "glob", Description: "Find files"},
		{Name: "edit", Description: "Edit file"},
	}
	defs, anNames := convertAnthropicTools(anTools)
	if len(defs) != 2 || len(anNames) != 2 || anNames[0] != "glob" || anNames[1] != "edit" {
		t.Fatalf("unexpected Anthropic converted tools/names: %v %v", defs, anNames)
	}
}

// @purpose Proves FR-02's fallback: when the agent advertises no system-prompt channel at all, the
// prompt text is handed back so the caller can fold it into the first prompt block instead of
// dropping it silently.
// @rationale The original defect class was a silently discarded system prompt; a fallback that is
// never asserted is the same defect with a comment.
// @preconditions None.
// @param t (*testing.T) - Go test harness.
// @returns None.
// @throws Calls t.Fatalf on any mismatch.
// @invariants Pure, no process is spawned.
func TestBuildACPSessionNewParams_NoChannelFoldsPrompt(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.Port = 8182

	silent := &pool.Worker{
		Cwd:          "/var/lib/agy-gateway/workspaces/w0",
		Capabilities: acp.AgentCapabilities{}, // no standard and no vendor channel
	}

	params, fold := buildACPSessionNewParams(SessionSetupInput{
		Worker:               silent,
		Config:               cfg,
		ConsumerSystemPrompt: "OPERATOR PROMPT",
	})
	if fold == "" {
		t.Fatal("expected the prompt to be folded into the first block when no channel is advertised")
	}
	if !strings.Contains(fold, "OPERATOR PROMPT") {
		t.Fatalf("folded prompt lost the consumer prompt: %q", fold)
	}
	if params.SystemPrompt != nil {
		t.Fatalf("no channel is advertised, so no systemPrompt may be sent: %+v", params.SystemPrompt)
	}
	if params.Meta != nil {
		if agy, ok := params.Meta["antigravity"].(map[string]any); ok {
			if _, has := agy["systemPrompt"]; has {
				t.Fatalf("no vendor channel is advertised, so no _meta systemPrompt may be sent: %+v", params.Meta)
			}
		}
	}

	// A vendor-dialect agent must instead receive the prompt on that channel and fold nothing.
	vendor := &pool.Worker{
		Cwd:          "/var/lib/agy-gateway/workspaces/w0",
		Capabilities: acp.AgentCapabilities{SupportsVendorSystemPrompt: true, SupportsSystemPromptAppend: true},
	}
	params2, fold2 := buildACPSessionNewParams(SessionSetupInput{
		Worker:               vendor,
		Config:               cfg,
		ConsumerSystemPrompt: "OPERATOR PROMPT",
	})
	if fold2 != "" {
		t.Fatalf("a supported channel must not also fold the prompt: %q", fold2)
	}
	if params2.Meta == nil {
		t.Fatal("vendor-dialect agent must receive _meta.antigravity.systemPrompt")
	}
}

// newConfigOptionTestWorker builds a worker whose ACP peer answers any request through respond.
//
// @purpose Lets applySessionConfigOptions be exercised against a scripted agent without a process.
// @rationale The behaviour under test depends on the agent's response payload, not on agy itself;
// a pipe pair plus a responder goroutine drives the real Transport code path.
// @preconditions respond must write exactly one response per request via the supplied callback.
// @param t Standard Go test handle, used for cleanup registration.
// @param respond Handles one request: (id, method, params, write), where write answers that id.
// @returns (*pool.Worker) - Started worker wired to the fake peer.
// @throws Fails the test when the worker cannot be started.
// @invariants Spawns one process and one goroutine, both released by t.Cleanup.
func newConfigOptionTestWorker(t *testing.T, respond func(id int64, method string, params json.RawMessage, write func(id int64, result string))) *pool.Worker {
	t.Helper()
	tmp := t.TempDir()
	cfg := config.DefaultConfig()
	w := pool.NewWorker(0, tmp, tmp, tmp, nil, cfg, nil)
	w.StartFn = func(ctx context.Context) error {
		proc, err := acp.SpawnProcess(context.Background(), "sleep", []string{"60"}, nil, tmp, nil)
		if err != nil {
			return err
		}
		w.Process = proc
		return nil
	}
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("failed to start worker: %v", err)
	}

	outReader, outWriter := io.Pipe()
	inReader, inWriter := io.Pipe()
	w.Transport = acp.NewTransport(outWriter, inReader, 134217728, nil)

	write := func(id int64, result string) {
		raw, _ := json.Marshal(acp.Response{JSONRPC: acp.JSONRPCVersion, ID: id, Result: json.RawMessage(result)})
		_, _ = inWriter.Write(append(raw, '\n'))
	}

	go func() {
		scanner := bufio.NewScanner(outReader)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var req struct {
				ID     int64           `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(line, &req); err != nil {
				continue
			}
			switch req.Method {
			case "session/new":
				write(req.ID, `{"sessionId":"mock_sess_1"}`)
			case "session/set_config_option":
				respond(req.ID, req.Method, req.Params, write)
			default:
				write(req.ID, `{}`)
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

// TestApplySessionConfigOptions_TracksLiveOptionSet pins the two defects found live on agy 1.5.17.
//
// @purpose Stops the gateway (a) sending a thought level to a model that no longer offers the
// option, and (b) skipping the thought level and mode entirely for a model absent from the
// session/new catalogue.
// @rationale agy rewrites configOptions on every model switch: selecting claude-sonnet-4-6 drops
// thought_level completely, so the pre-switch snapshot is not evidence about the model in use.
// The previous early `return` after an unlisted model's success also silently skipped every
// later option.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic: the fake agent answers synchronously on a pipe.
func TestApplySessionConfigOptions_TracksLiveOptionSet(t *testing.T) {
	modelOnly := []acp.SessionConfigOption{
		{ID: acp.ConfigIDModel, Type: "select", CurrentValue: "gemini-4-hydrogen",
			Options: []acp.SessionConfigValue{{Value: "gemini-4-hydrogen"}, {Value: "claude-sonnet-4-6"}}},
		{ID: acp.ConfigIDThoughtLevel, Type: "select", CurrentValue: "high",
			Options: []acp.SessionConfigValue{{Value: "low"}, {Value: "medium"}, {Value: "high"}, {Value: "xhigh"}}},
		{ID: acp.ConfigIDMode, Type: "select", CurrentValue: "default",
			Options: []acp.SessionConfigValue{{Value: "default"}, {Value: "accept-edits"}}},
	}
	// What agy answers after switching to claude-sonnet-4-6: no thought_level at all.
	afterClaudeSwitch := []acp.SessionConfigOption{modelOnly[0], modelOnly[2]}

	cases := []struct {
		name          string
		model         string
		wantLevelSent bool
	}{
		{"model offering a level keeps it", "gemini-4-hydrogen", true},
		{"model dropping the level sends none", "claude-sonnet-4-6", false},
		// The model is absent from the advertised catalogue, so this exercises the branch that
		// previously returned early and skipped both later options. Both listed cases above take
		// the catalogue branch, which is why they never pinned that defect.
		{"model absent from catalogue still applies later options", "gemini-4-hydrogen-xhigh", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var sentIDs []string
			worker := newConfigOptionTestWorker(t, func(id int64, _ string, params json.RawMessage, write func(int64, string)) {
				var p struct {
					ConfigID string `json:"configId"`
					Value    string `json:"value"`
				}
				_ = json.Unmarshal(params, &p)
				mu.Lock()
				sentIDs = append(sentIDs, p.ConfigID)
				mu.Unlock()
				// Mirrors agy: only the Claude models drop thought_level on switch.
				next := modelOnly
				if p.ConfigID == acp.ConfigIDModel && p.Value == "claude-sonnet-4-6" {
					next = afterClaudeSwitch
				}
				raw, _ := json.Marshal(map[string]any{"configOptions": next})
				write(id, string(raw))
			})

			_, _, err := applySessionConfigOptions(context.Background(), worker,
				acp.SessionNewResult{SessionID: "sess_live", ConfigOptions: modelOnly},
				tc.model, "medium", "accept-edits", time.Second, nil)
			if err != nil {
				t.Fatalf("applySessionConfigOptions: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			levelSent := false
			modeSent := false
			for _, id := range sentIDs {
				if id == acp.ConfigIDThoughtLevel {
					levelSent = true
				}
				if id == acp.ConfigIDMode {
					modeSent = true
				}
			}
			if levelSent != tc.wantLevelSent {
				t.Fatalf("thought_level sent=%v, want %v (calls: %v)", levelSent, tc.wantLevelSent, sentIDs)
			}
			if !modeSent {
				t.Fatalf("mode must still be applied after a model switch (calls: %v)", sentIDs)
			}
		})
	}
}

// TestApplySessionConfigOptions_NoModelOptionStillAppliesLaterOptions pins D-03 exactly.
//
// @purpose Fails if the unlisted-model path ever returns early again and skips thought level
// and mode.
// @rationale The earlier test only covered models listed in the session/new catalogue, which take
// the `cfg != nil` branch; the early `return` this pins lives in the `cfg == nil` branch, so it
// needs an agent that advertises no `model` option at all.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic: the fake agent answers synchronously on a pipe.
func TestApplySessionConfigOptions_NoModelOptionStillAppliesLaterOptions(t *testing.T) {
	// No `model` option: the agent takes the model through some other path, which is the branch
	// that previously returned before thought level and mode were considered.
	noModelOption := []acp.SessionConfigOption{
		{ID: acp.ConfigIDThoughtLevel, Type: "select", CurrentValue: "high",
			Options: []acp.SessionConfigValue{{Value: "low"}, {Value: "medium"}, {Value: "high"}, {Value: "xhigh"}}},
		{ID: acp.ConfigIDMode, Type: "select", CurrentValue: "default",
			Options: []acp.SessionConfigValue{{Value: "default"}, {Value: "accept-edits"}}},
	}

	var mu sync.Mutex
	var sentIDs []string
	worker := newConfigOptionTestWorker(t, func(id int64, _ string, params json.RawMessage, write func(int64, string)) {
		var p struct {
			ConfigID string `json:"configId"`
			Value    string `json:"value"`
		}
		_ = json.Unmarshal(params, &p)
		mu.Lock()
		sentIDs = append(sentIDs, p.ConfigID)
		mu.Unlock()
		raw, _ := json.Marshal(map[string]any{"configOptions": noModelOption})
		write(id, string(raw))
	})

	_, appliedMode, err := applySessionConfigOptions(context.Background(), worker,
		acp.SessionNewResult{SessionID: "sess_nomodel", ConfigOptions: noModelOption},
		"gemini-4-hydrogen-xhigh", "medium", "accept-edits", time.Second, nil)
	if err != nil {
		t.Fatalf("applySessionConfigOptions: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	levelSent, modeSent := false, false
	for _, id := range sentIDs {
		if id == acp.ConfigIDThoughtLevel {
			levelSent = true
		}
		if id == acp.ConfigIDMode {
			modeSent = true
		}
	}
	if !levelSent {
		t.Fatalf("thought_level must still be applied when the agent advertises no model option (calls: %v)", sentIDs)
	}
	if !modeSent {
		t.Fatalf("mode must still be applied when the agent advertises no model option (calls: %v)", sentIDs)
	}
	if appliedMode != "accept-edits" {
		t.Fatalf("appliedMode = %q, want accept-edits", appliedMode)
	}
}
