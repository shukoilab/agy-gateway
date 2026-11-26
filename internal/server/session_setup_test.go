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

func TestBuildACPSessionNewParams_AppendOnly(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.Port = 8182
	cfg.Execution.StrictClientToolDelegation = true

	worker := &pool.Worker{
		Cwd:          "/var/lib/agy-gateway/workspaces/w0",
		Capabilities: acp.AgentCapabilities{SupportsVendorSystemPrompt: true},
	}

	// 1. Consumer prompt plus declared tools travel on the single append carrier.
	params := buildACPSessionNewParams(SessionSetupInput{
		Worker:               worker,
		Config:               cfg,
		ConsumerSystemPrompt: "You are OpenCode running on host.",
		LoopbackSessionID:    "sess_abc123",
		ClientToolNames:      []string{"read", "bash"},
	})

	agyMeta, ok := params.Meta["antigravity"].(map[string]any)
	if !ok {
		t.Fatalf("expected antigravity meta map, got %+v", params.Meta)
	}
	sysMap, ok := agyMeta["systemPrompt"].(map[string]any)
	if !ok {
		t.Fatalf("expected systemPrompt meta map, got %+v", agyMeta)
	}
	appended, ok := sysMap["append"].(string)
	if !ok || appended == "" {
		t.Fatalf("expected non-empty append systemPrompt, got %+v", sysMap)
	}
	if _, hasOverride := sysMap["override"]; hasOverride {
		t.Fatalf("override must never be emitted; it hangs the agent turn")
	}
	if !strings.Contains(appended, "You are OpenCode running on host.") {
		t.Fatalf("consumer prompt missing from append payload: %q", appended)
	}
	if !strings.Contains(appended, "mcp_client-tools_read") || !strings.Contains(appended, "mcp_client-tools_bash") {
		t.Fatalf("delegation directive missing from append payload: %q", appended)
	}

	// 2. Zero prompt injection: no default persona, no precedence banner.
	if strings.Contains(appended, "AUTHORITATIVE SYSTEM INSTRUCTIONS") {
		t.Fatalf("hardcoded precedence banner must never be injected: %q", appended)
	}
	if strings.Contains(appended, "Strict Language Mirroring") {
		t.Fatalf("hardcoded default execution protocol must never be injected: %q", appended)
	}

	// 3. Empty consumer prompt omits _meta entirely rather than sending an empty payload.
	emptyParams := buildACPSessionNewParams(SessionSetupInput{Worker: worker, Config: cfg})
	if emptyParams.Meta != nil {
		t.Fatalf("no prompt must mean no _meta, got %+v", emptyParams.Meta)
	}

	// 4. Whitespace-only prompt is treated as empty.
	blankParams := buildACPSessionNewParams(SessionSetupInput{
		Worker: worker, Config: cfg, ConsumerSystemPrompt: "   \n\t  ",
	})
	if blankParams.Meta != nil {
		t.Fatalf("whitespace-only prompt must mean no _meta, got %+v", blankParams.Meta)
	}

	// 5. Prompt without declared tools carries no delegation directive.
	noTools := buildACPSessionNewParams(SessionSetupInput{
		Worker: worker, Config: cfg, ConsumerSystemPrompt: "Plain prompt",
	})
	plain := noTools.Meta["antigravity"].(map[string]any)["systemPrompt"].(map[string]any)["append"].(string)
	if plain != "Plain prompt" {
		t.Fatalf("prompt without tools must be forwarded verbatim, got %q", plain)
	}

	// 6. The loopback MCP server entry is still wired.
	if len(params.MCPServers) != 1 || params.MCPServers[0].Name != "client-tools" {
		t.Fatalf("expected client-tools MCPServer entry, got %+v", params.MCPServers)
	}
	if params.MCPServers[0].Env == nil {
		t.Fatalf("expected non-nil empty Env slice on stdio MCPServerInfo")
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

// TestBuildACPSessionNewParams_UnadvertisedChannelStillSends proves the gateway does not silently
// drop a consumer prompt just because the agent failed to advertise the append capability. The
// warning is emitted at worker start; the payload itself is unchanged.
func TestBuildACPSessionNewParams_UnadvertisedChannelStillSends(t *testing.T) {
	cfg := config.DefaultConfig()
	silent := &pool.Worker{Cwd: "/var/lib/agy-gateway/workspaces/w0"}

	params := buildACPSessionNewParams(SessionSetupInput{
		Worker: silent, Config: cfg, ConsumerSystemPrompt: "OPERATOR PROMPT",
	})
	agy, ok := params.Meta["antigravity"].(map[string]any)
	if !ok {
		t.Fatalf("expected antigravity meta even for an unadvertised channel, got %+v", params.Meta)
	}
	if agy["systemPrompt"].(map[string]any)["append"] != "OPERATOR PROMPT" {
		t.Fatalf("consumer prompt must be forwarded verbatim, got %+v", agy["systemPrompt"])
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
		{ID: "mode", Type: "select", CurrentValue: "default",
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

			_, err := applySessionConfigOptions(context.Background(), worker,
				acp.SessionNewResult{SessionID: "sess_live", ConfigOptions: modelOnly},
				tc.model, "medium", time.Second, nil)
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
				if id == "mode" {
					modeSent = true
				}
			}
			if levelSent != tc.wantLevelSent {
				t.Fatalf("thought_level sent=%v, want %v (calls: %v)", levelSent, tc.wantLevelSent, sentIDs)
			}
			if modeSent {
				t.Fatalf("session mode must never be sent: plan vs build is the consumer's system prompt (calls: %v)", sentIDs)
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
		{ID: "mode", Type: "select", CurrentValue: "default",
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

	appliedModel, err := applySessionConfigOptions(context.Background(), worker,
		acp.SessionNewResult{SessionID: "sess_nomodel", ConfigOptions: noModelOption},
		"gemini-4-hydrogen-xhigh", "medium", time.Second, nil)
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
		if id == "mode" {
			modeSent = true
		}
	}
	if !levelSent {
		t.Fatalf("thought_level must still be applied when the agent advertises no model option (calls: %v)", sentIDs)
	}
	if modeSent {
		t.Fatalf("session mode must never be sent (calls: %v)", sentIDs)
	}
	if appliedModel != "gemini-4-hydrogen-xhigh" {
		t.Fatalf("appliedModel = %q, want gemini-4-hydrogen-xhigh", appliedModel)
	}
}
