// Package server contains an internal test proving that a turn attempt does not
// destroy the loopback session it was handed.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"agy-gateway/internal/acp"
	"agy-gateway/internal/config"
	"agy-gateway/internal/mcp"
	"agy-gateway/internal/models"
	"agy-gateway/internal/pool"
)

// newTurnAttemptTestWorker builds a worker whose ACP peer answers the session
// handshake and replies to session/prompt with a fixed stop reason.
//
// @purpose Lets an attempt be executed end-to-end without a real agy process.
// @rationale executeTurnAttempt needs a live Transport; a pipe pair plus a tiny
// responder goroutine exercises the real code path instead of a mock interface.
// @preconditions t and cfg must be non-nil.
// @param t (testing.T) - Owning test, used for cleanup registration.
// @param cfg (*config.Config) - Config carrying the pool/MCP timeouts.
// @param onPrompt (func(io.Writer)) - Emits session/update frames before the
// prompt response is written.
// @returns (*pool.Worker) - Started worker wired to the fake peer.
// @throws Fails the test when the worker cannot be started.
// @invariants Spawns exactly one goroutine and one process, both released by
// t.Cleanup; the goroutine exits when the reader is closed.
func newTurnAttemptTestWorker(t *testing.T, cfg *config.Config, onPrompt func(io.Writer)) *pool.Worker {
	tmp := t.TempDir()
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
				writeFrame(inWriter, req.ID, `{"sessionId":"mock_sess_1"}`)
			case "session/set_config_option", "session/close":
				writeFrame(inWriter, req.ID, `{}`)
			case "session/prompt":
				// A turn that produces nothing is now rejected as EMPTY_COMPLETION, because agy signals
				// an internally exhausted quota exactly that way. Emit a real session/update notification
				// so the fake peer represents a successful turn rather than a silently empty one.
				if onPrompt != nil {
					onPrompt(inWriter)
					time.Sleep(20 * time.Millisecond)
				} else {
					notif := map[string]any{
						"jsonrpc": "2.0",
						"method":  "session/update",
						"params": map[string]any{
							"sessionId": "mock_sess_1",
							"update": map[string]any{
								"sessionUpdate": acp.UpdateAgentMessageChunk,
								"content":       map[string]any{"type": "text", "text": "ok"},
							},
						},
					}
					raw, _ := json.Marshal(notif)
					_, _ = inWriter.Write(append(raw, '\n'))
					time.Sleep(20 * time.Millisecond)
				}
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
	return w
}

// writeFrame writes one JSON-RPC response line for the given request id.
//
// @purpose Encapsulates the framing so the responder stays readable.
// @rationale Every fake response is a result object; centralising the envelope
// avoids repeating the marshal-and-append-newline dance.
// @preconditions w must be an open writer.
// @param w (io.Writer) - Destination pipe.
// @param id (int64) - Request id being answered.
// @param result (string) - Raw JSON result payload.
// @returns None.
// @throws None; write errors are ignored because a closed pipe ends the turn.
// @invariants O(n) in payload size; single Write call.
func writeFrame(w io.Writer, id int64, result string) {
	raw, _ := json.Marshal(acp.Response{
		JSONRPC: acp.JSONRPCVersion,
		ID:      id,
		Result:  json.RawMessage(result),
	})
	_, _ = w.Write(append(raw, '\n'))
}

// TestTurnAttemptKeepsLoopbackSessionOpenAcrossAttempts pins the failover
// invariant that a loopback session belongs to the client request, not to one
// attempt.
//
// @purpose Prevents a regression where the first attempt closed the shared
// session channel, leaving every failover retry without client tools.
// @rationale The session is registered once per HTTP request while
// executeTurnAttempt runs once per attempt; closing it inside the attempt made
// attempt 2 reuse a token that GetSession and ValidateToken already rejected,
// so tool delegation failed silently with HTTP 401 from the bridge.
// @preconditions A fresh loopback server and config.
// @param t (testing.T) - Standard Go test handle.
// @returns None.
// @throws Fails when the session is not reusable after an attempt.
// @invariants Deterministic; no timing dependence beyond the responder sleep.
func TestTurnAttemptKeepsLoopbackSessionOpenAcrossAttempts(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Pool.WorkerCount = 1
	cfg.Pool.HandshakeTimeout = 5 * time.Second
	cfg.Pool.InferenceTimeout = 5 * time.Second
	cfg.Pool.FirstTokenTimeout = 5 * time.Second
	cfg.MCP.ClientToolCalling.ToolCallCollectWindow = 50 * time.Millisecond

	loopback := mcp.NewLoopbackServer()
	state, err := loopback.RegisterSession("sess_attempt", nil)
	if err != nil {
		t.Fatalf("register session: %v", err)
	}
	if err := loopback.ValidateToken("sess_attempt", state.Token); err != nil {
		t.Fatalf("token must be valid before the attempt: %v", err)
	}

	w := newTurnAttemptTestWorker(t, cfg, nil)
	in := turnInput{
		Ctx:               context.Background(),
		ModelEntry:        &models.ModelInfo{ID: "test", UpstreamModel: "gemini-3.8-flash"},
		ThoughtLevel:      "medium",
		SystemPrompt:      "",
		PromptBlocks:      []acp.PromptBlock{{Type: "text", Text: "hi"}},
		LoopbackSessionID: "sess_attempt",
		LoopbackToken:     state.Token,
		LoopbackState:     state,
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := executeTurnAttempt(context.Background(), w, cfg, nil, in); err != nil {
			t.Fatalf("attempt %d failed: %v", attempt, err)
		}
		// The session must survive the attempt verbatim: attempt N+1 reuses this
		// exact token and channel.
		if _, err := loopback.GetSession("sess_attempt"); err != nil {
			t.Fatalf("session closed after attempt %d: %v", attempt, err)
		}
		if err := loopback.ValidateToken("sess_attempt", state.Token); err != nil {
			t.Fatalf("token rejected after attempt %d: %v", attempt, err)
		}
		select {
		case state.Calls <- mcp.InterceptedToolCall{ID: "probe", Name: "get_weather"}:
		case <-time.After(time.Second):
			t.Fatalf("attempt %d left the call channel unusable", attempt)
		}
	}
}
