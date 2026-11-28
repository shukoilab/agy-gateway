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

// TestBuildACPSessionNewParams_NeverCarriesPrompt proves session/new carries no prompt text at all,
// and that the consumer prompt is delivered as a leading block instead (S4/S5 of the matrix). The
// vendor append dialect is unreachable by construction: _meta does not exist as a field anymore.
func TestBuildACPSessionNewParams_NeverCarriesPrompt(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.Port = 8182

	worker := &pool.Worker{Cwd: "/var/lib/agy-gateway/workspaces/w0"}

	params := buildACPSessionNewParams(SessionSetupInput{
		Worker:               worker,
		Config:               cfg,
		ConsumerSystemPrompt: "OPERATOR PROMPT",
		ClientToolNames:      []string{"read"},
	})
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal session/new params: %v", err)
	}
	if strings.Contains(string(raw), "_meta") {
		t.Fatalf("session/new must never carry _meta, got %s", raw)
	}
	if strings.Contains(string(raw), "OPERATOR PROMPT") {
		t.Fatalf("session/new must never carry the consumer prompt, got %s", raw)
	}

	effective := effectiveSystemPrompt(SessionSetupInput{
		Config:               cfg,
		ConsumerSystemPrompt: "OPERATOR PROMPT",
		ClientToolNames:      []string{"read"},
	})
	if !strings.Contains(effective, "OPERATOR PROMPT") ||
		!strings.Contains(effective, "mcp_client-tools_read") {
		t.Fatalf("effective prompt must carry the persona and the directive, got %q", effective)
	}

	blocks := prependPromptBlocks([]acp.PromptBlock{{Type: "text", Text: "user turn"}}, effective)
	if len(blocks) != 2 {
		t.Fatalf("expected persona block plus the user block, got %d", len(blocks))
	}
	if blocks[0].Type != "text" || !strings.Contains(blocks[0].Text, "OPERATOR PROMPT") {
		t.Fatalf("persona must be the first block, got %+v", blocks[0])
	}
	if blocks[1].Text != "user turn" {
		t.Fatalf("user block must be preserved last, got %+v", blocks[1])
	}

	// Empty persona must leave the blocks untouched (including nil).
	if got := prependPromptBlocks(nil, "   "); got != nil {
		t.Fatalf("blank prompt must not prepend a block, got %+v", got)
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

			_, _, err := applySessionConfigOptions(context.Background(), worker,
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

	appliedModel, _, err := applySessionConfigOptions(context.Background(), worker,
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
