package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"agy-gateway/internal/acp"
	"agy-gateway/internal/config"
	"agy-gateway/internal/mcp"
	"agy-gateway/internal/models"
	"agy-gateway/internal/pool"
)

// newWatchdogTestWorker builds a worker whose ACP peer runs script and counts session/cancel calls.
//
// @purpose Lets a test drive the idle-watchdog code path with a controllable turn duration.
// @rationale The watchdog only misbehaves when a turn outlives the collect window, so the peer has to
// emit updates on a real clock; a pipe pair plus a responder goroutine exercises the production path
// instead of mocking the transport.
// @preconditions cfg carries the pool/MCP timeouts under test.
// @param t Standard Go test handle.
// @param cfg Config with the collect window to exercise.
// @param script Emits session/update notifications for one session/prompt.
// @returns The started worker and a counter of received session/cancel requests.
// @throws Fails the test when the worker cannot be started.
// @invariants Spawns one goroutine and one process, both released by t.Cleanup.
func newWatchdogTestWorker(t *testing.T, cfg *config.Config, script func(emit func(map[string]any))) (*pool.Worker, *atomic.Int32) {
	t.Helper()
	tmp := t.TempDir()
	w := pool.NewWorker(0, tmp, tmp, tmp, nil, cfg, nil)
	w.StartFn = func(context.Context) error {
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

	var cancels atomic.Int32
	go func() {
		scanner := bufio.NewScanner(outReader)
		for scanner.Scan() {
			var req struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(scanner.Bytes(), &req) != nil {
				continue
			}
			switch req.Method {
			case "session/new":
				writeFrame(inWriter, req.ID, `{"sessionId":"mock_sess_1"}`)
			case "session/set_config_option", "session/close":
				writeFrame(inWriter, req.ID, `{}`)
			case "session/cancel":
				cancels.Add(1)
			case "session/prompt":
				emit := func(update map[string]any) {
					notif := map[string]any{"jsonrpc": "2.0", "method": "session/update",
						"params": map[string]any{"sessionId": "mock_sess_1", "update": update}}
					raw, _ := json.Marshal(notif)
					_, _ = inWriter.Write(append(raw, '\n'))
				}
				script(emit)
				writeFrame(inWriter, req.ID, `{"stopReason":"end_turn"}`)
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
	return w, &cancels
}

// turnAttemptInputFor builds the turn input the watchdog tests execute.
//
// @purpose Keeps the two watchdog tests from repeating seven fields each.
// @param sessionID Loopback session registered for the attempt.
// @param token Token of that session.
// @param state The session state holding the call channel.
// @returns A turnInput ready for executeTurnAttempt.
// @invariants Pure.
func turnAttemptInputFor(sessionID, token string, state *mcp.SessionState) turnInput {
	return turnInput{
		Ctx:               context.Background(),
		ModelEntry:        &models.ModelInfo{ID: "test", UpstreamModel: "gemini-3.8-flash"},
		ThoughtLevel:      "medium",
		PromptBlocks:      []acp.PromptBlock{{Type: "text", Text: "hi"}},
		LoopbackSessionID: sessionID,
		LoopbackToken:     token,
		LoopbackState:     state,
	}
}

// registerReadTool registers a loopback session exposing one `read` tool.
//
// @purpose Gives the attempt a real schema so collected calls can be validated.
// @param t Standard Go test handle.
// @param sessionID Session to register.
// @returns The session state.
// @throws Fails the test when registration fails.
// @invariants Deterministic.
func registerReadTool(t *testing.T, sessionID string) *mcp.SessionState {
	t.Helper()
	loopback := mcp.NewLoopbackServer()
	state, err := loopback.RegisterSession(sessionID, []mcp.OpenAIToolDefinition{{
		Type: "function",
		Function: mcp.FunctionSchema{Name: "read",
			Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
	}})
	if err != nil {
		t.Fatalf("register session: %v", err)
	}
	return state
}

// thoughtUpdate builds a reasoning-chunk session/update payload.
//
// @purpose Centralises the update shape the watchdog treats as activity.
// @param text Chunk text.
// @returns The update map.
// @invariants Pure.
func thoughtUpdate(text string) map[string]any {
	return map[string]any{
		"sessionUpdate": acp.UpdateAgentThoughtChunk,
		"content":       map[string]any{"type": "text", "text": text},
	}
}

// toolCallUpdate builds a native tool-call session/update payload.
//
// @purpose Centralises the shape that arms the idle watchdog.
// @param id Tool call id.
// @param path Argument value.
// @returns The update map.
// @invariants Pure.
func toolCallUpdate(id, path string) map[string]any {
	return map[string]any{"sessionUpdate": acp.UpdateToolCall,
		"toolCallId": id, "name": "read", "rawInput": map[string]any{"path": path}}
}

// TestTurnAttempt_ActivityAfterToolCallDoesNotCancel pins the idle-watchdog reset.
//
// @purpose Proves a reasoning model that keeps emitting chunks after a tool call is not cancelled.
// @rationale The watchdog used to be a one-shot deadline armed by the call itself and never reset, so
// a turn still producing output one second later was cancelled mid-generation. The client then saw a
// truncated turn and retried it, which is the observed loop.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded by the scripted 1.2s of activity.
func TestTurnAttempt_ActivityAfterToolCallDoesNotCancel(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Pool.HandshakeTimeout = 5 * time.Second
	cfg.Pool.InferenceTimeout = 30 * time.Second
	cfg.Pool.FirstTokenTimeout = 30 * time.Second
	cfg.MCP.ClientToolCalling.ToolCallCollectWindow = time.Second

	w, cancels := newWatchdogTestWorker(t, cfg, func(emit func(map[string]any)) {
		emit(toolCallUpdate("call_first", "a.ts"))
		// The model keeps thinking after the call: this must not end the turn.
		for i := 0; i < 6; i++ {
			time.Sleep(200 * time.Millisecond)
			emit(thoughtUpdate("still analysing"))
		}
		emit(toolCallUpdate("call_second", "b.ts"))
	})

	state := registerReadTool(t, "sess_watchdog_activity")
	res, err := executeTurnAttempt(context.Background(), w, cfg, nil,
		turnAttemptInputFor("sess_watchdog_activity", state.Token, state))
	if err != nil {
		t.Fatalf("attempt failed: %v", err)
	}
	if got := cancels.Load(); got != 0 {
		t.Fatalf("a turn still producing output was cancelled %d time(s)", got)
	}
	ids := make([]string, 0, len(res.Tools))
	for _, tc := range res.Tools {
		ids = append(ids, tc.ID)
	}
	if len(ids) != 2 {
		t.Fatalf("activity after a tool call must not end the turn: got %v, want 2 calls", ids)
	}
}

// TestTurnAttempt_SilentGapAfterToolCallKeepsBothCalls pins the grace window.
//
// @purpose Proves a silent reasoning gap longer than the collect window does not truncate the turn.
// @rationale This is the exact production shape: a tool call, then three seconds of silence while the
// model thinks (TTFT reached 63 s in the looping session), then the next call. The watchdog now asks
// the agent to wind down but keeps reading for one more window, so the second call still arrives.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded by the scripted 3.2s gap.
func TestTurnAttempt_SilentGapAfterToolCallKeepsBothCalls(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Pool.HandshakeTimeout = 5 * time.Second
	cfg.Pool.InferenceTimeout = 60 * time.Second
	cfg.Pool.FirstTokenTimeout = 60 * time.Second
	cfg.MCP.ClientToolCalling.ToolCallCollectWindow = 500 * time.Millisecond

	w, cancels := newWatchdogTestWorker(t, cfg, func(emit func(map[string]any)) {
		emit(toolCallUpdate("call_a", "a.ts"))
		// Silent internal reasoning: no chunks at all, longer than the collect window.
		time.Sleep(1600 * time.Millisecond)
		emit(toolCallUpdate("call_b", "b.ts"))
		time.Sleep(100 * time.Millisecond)
	})

	state := registerReadTool(t, "sess_watchdog_gap")
	res, err := executeTurnAttempt(context.Background(), w, cfg, nil,
		turnAttemptInputFor("sess_watchdog_gap", state.Token, state))
	if err != nil {
		t.Fatalf("attempt failed: %v", err)
	}
	if len(res.Tools) != 2 {
		t.Fatalf("silent thinking gap truncated the turn: got %d call(s), want 2", len(res.Tools))
	}
	if got := cancels.Load(); got > 1 {
		t.Fatalf("idle watchdog sent %d cancels, want at most one graceful request", got)
	}
}
