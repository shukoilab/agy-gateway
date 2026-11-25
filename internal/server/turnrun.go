package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"agy-gateway/internal/acp"
	"agy-gateway/internal/config"
	"agy-gateway/internal/mcp"
	"agy-gateway/internal/models"
	"agy-gateway/internal/openai"
	"agy-gateway/internal/pool"
	"agy-gateway/internal/telemetry"
)

// turnInput carries everything one ACP attempt needs, independent of the HTTP protocol.
//
// @purpose Lets one executor serve both the OpenAI and Anthropic surfaces.
// @rationale The two handlers shared ~400 lines of near-identical turn logic that drifted apart
// (one had a mutex, the other did not, and only one discarded tool calls). A single executor with
// provider callbacks removes that class of divergence.
// @invariants Callbacks may be nil; PromptBlocks must be non-empty.
type turnInput struct {
	Ctx          context.Context
	ModelEntry   *models.ModelInfo
	ThoughtLevel string
	SystemPrompt string
	PromptBlocks []acp.PromptBlock

	// Loopback session used for client tool delegation. Empty when the caller declared no tools.
	LoopbackSessionID string
	LoopbackToken     string
	LoopbackState     *mcp.SessionState
	ClientToolNames   []string
	// StopSequences are client-supplied sequences that must truncate the stream gateway-side.
	StopSequences []string
	// ParseTextToolCalls recovers tool calls a model emitted as delimiter-tagged text instead of native
	// ACP tool_call updates. Provider-specific because the parser and payload shape differ.
	ParseTextToolCalls func(text string) (string, []toolCollectorEntry)
	// StreamTextToolCalls parses delimiter-tagged tool calls incrementally, so they are emitted at the
	// point the model produced them rather than only at end of turn.
	StreamTextToolCalls func(chunk string) (string, []toolCollectorEntry)

	// OnText receives text that is safe to emit (stop sequences already filtered).
	OnText func(string)
	// OnThought receives reasoning text.
	OnThought func(string)
	// OnTool receives a newly collected, sanitised client tool call.
	OnTool func(toolCollectorEntry)
	// OnFirstToken fires once, before the first observable output.
	OnFirstToken func()
	// OnInternalToolProgress receives progress text for agent-internal tool invocations.
	OnInternalToolProgress func(string)
}

// turnResult is the provider-neutral outcome of one attempt.
//
// @purpose Carries everything both response builders need without re-parsing the stream.
// @invariants Text and Thoughts are the full accumulated outputs; Tools is in first-seen order.
type turnResult struct {
	Text            string
	Thoughts        string
	Tools           []toolCollectorEntry
	StopReason      string
	Usage           UsageView
	UsageFromAgent  bool
	StopMatched     bool
	MatchedSequence string
	FirstTokenSeen  bool
}

// ErrFirstTokenTimeout reports that no token arrived within the configured budget.
var ErrFirstTokenTimeout = errors.New("time to first token exceeded")

// executeTurnAttempt performs one full ACP turn on a leased worker.
//
// @purpose The single implementation of session setup, config application, streaming fan-out, tool
// interception, and prompt completion shared by both protocol adapters.
// @rationale Duplicating this logic per provider previously produced an unsynchronised OpenAI path
// (data race on shared stream state), a discarded second tool call, unstopped timers, and no session
// filtering. One executor makes every FR enforceable in one place.
// @preconditions worker is leased to this turn exclusively and its transport is live.
// @param ctx Cancellation root for the turn.
// @param worker Leased worker owning the ACP transport.
// @param cfg Gateway configuration.
// @param logger Structured logger.
// @param in Turn input including prompt blocks and callbacks.
// @returns The attempt outcome, or a typed error describing why it failed.
// @throws ErrFirstTokenTimeout when the watchdog fires; the caller maps it to 504. Returns the agent's
// error otherwise. Returns ErrNonRetryableStream-wrapped errors upstream, never from here.
// @invariants Removes the update listener before reading result state; stops every timer; never leaves
// a goroutine blocked on an unread channel.
func executeTurnAttempt(
	ctx context.Context,
	worker *pool.Worker,
	cfg *config.Config,
	logger *slog.Logger,
	in turnInput,
) (turnResult, error) {
	var result turnResult
	if logger == nil {
		logger = slog.Default()
	}
	if len(in.PromptBlocks) == 0 {
		return result, errors.New("prompt contains no blocks")
	}
	if in.Ctx != nil {
		ctx = in.Ctx
	}

	sessionID, promptBlocks, closeSession, err := setupACPSession(ctx, worker, cfg, logger, in)
	if err != nil {
		return result, err
	}
	// Runs after the prompt turn finishes on every path; a defer inside setupACPSession closed the
	// session before the prompt and produced "Session not found".
	defer closeSession()
	if len(promptBlocks) > 0 {
		in.PromptBlocks = promptBlocks
	}

	delimiter := cfg.MCP.ClientToolCalling.DelimiterTag
	if delimiter == "" {
		delimiter = "tool_call"
	}
	streamParser := openai.NewStreamParser(delimiter)

	var (
		mu       sync.Mutex
		text     strings.Builder
		thoughts strings.Builder
		stopSeq  = NewStopSequenceMatcher(in.StopSequences)
		// bar serialises update-listener invocations against the post-Call snapshot: the snapshot
		// holds it for writing, every callback holds it for its whole body. late makes a callback
		// that lost the race refuse immediately instead of blocking the turn.
		bar  sync.Mutex
		late atomic.Bool
	)

	collector := NewToolCollector(cfg, nil)
	collector.onInvalid = func(id, name, reason string) {
		logger.Warn("rejected malformed client tool call", "toolCallId", id, "toolName", name, "reason", reason)
	}

	loopbackState := in.LoopbackState

	notifyOnce := sync.Once{}
	notifyFirstToken := func() {
		notifyOnce.Do(func() {
			result.FirstTokenSeen = true
			if in.OnFirstToken != nil {
				in.OnFirstToken()
			}
		})
	}

	// One stoppable timer replaces the previous eight unstoppable time.AfterFunc calls. It fires
	// `session/cancel` then cancels the prompt context, which unblocks the prompt call deterministically.
	promptCtx, cancelPrompt := context.WithTimeout(ctx, cfg.Pool.InferenceTimeout)
	defer cancelPrompt()

	var ttftFired atomic.Bool
	ttftDuration := cfg.Pool.FirstTokenTimeout
	if ttftDuration <= 0 {
		ttftDuration = 120 * time.Second
	}
	ttftTimer := time.NewTimer(ttftDuration)
	defer ttftTimer.Stop()

	go func() {
		select {
		case <-promptCtx.Done():
		case <-ttftTimer.C:
			ttftFired.Store(true)
			logger.Warn("time-to-first-token watchdog triggered", "timeout", ttftDuration, "workerID", worker.ID)
			cancelPrompt()
		}
	}()

	var cancelTimer *time.Timer
	var cancelTimerMu sync.Mutex
	stopCancelTimer := func() {
		cancelTimerMu.Lock()
		if cancelTimer != nil {
			cancelTimer.Stop()
			cancelTimer = nil
		}
		cancelTimerMu.Unlock()
	}
	armCancel := func() {
		stopCancelTimer()
		cancelTimerMu.Lock()
		cancelTimer = NewTurnTimer(cfg.MCP.ClientToolCalling.ToolCallCollectWindow, func() {
			_ = worker.Transport.Notify("session/cancel", acp.SessionCancelParams{SessionID: sessionID})
			cancelPrompt()
		})
		cancelTimerMu.Unlock()
	}
	defer stopCancelTimer()

	// FR-17: updates belonging to any other session on this worker are ignored.
	// Every callback holds barR for its whole body so the post-Call snapshot can wait for in-flight
	// invocations. SetUpdateListener(nil) only prevents *future* invocations: a callback that already
	// copied the function pointer under RLock keeps running, and would otherwise mutate the result
	// after the snapshot (or emit a content block after message_stop).
	worker.Transport.SetUpdateListener(func(update acp.SessionUpdateParams) {
		bar.Lock()
		defer bar.Unlock()
		// A transport goroutine may already hold the listener pointer when we clear it, so a late
		// invocation has to be refused outright rather than merely serialised.
		if late.Load() {
			return
		}
		if update.SessionID != "" && sessionID != "" && update.SessionID != sessionID {
			logger.Debug("dropping session update from another session", "expected", sessionID, "got", update.SessionID)
			return
		}

		switch update.Update.SessionUpdate {
		case acp.UpdateAgentMessageChunk:
			if update.Update.Content == nil {
				return
			}
			notifyFirstToken()
			if !ttftTimer.Stop() {
				select {
				case <-ttftTimer.C:
				default:
				}
			}
			chunk := update.Update.Content.Text
			if in.StreamTextToolCalls != nil {
				clean, entries := in.StreamTextToolCalls(chunk)
				for _, entry := range entries {
					if added, ok := collector.Add(entry.ID, entry.Name, entry.Arguments, SchemaFor(loopbackState, entry.Name)); ok {
						telemetry.DefaultMetrics.ToolCallsTotal.Add(1)
						if in.OnTool != nil {
							in.OnTool(added)
						}
						armCancel()
					}
				}
				chunk = clean
			}
			if chunk == "" {
				return
			}
			emit, matched := stopSeq.Feed(chunk)
			mu.Lock()
			if emit != "" {
				text.WriteString(emit)
			}
			mu.Unlock()
			if emit != "" && in.OnText != nil {
				in.OnText(emit)
			}
			if matched {
				cancelPrompt()
			}

		case acp.UpdateAgentThoughtChunk:
			if update.Update.Content == nil {
				return
			}
			notifyFirstToken()
			if !ttftTimer.Stop() {
				select {
				case <-ttftTimer.C:
				default:
				}
			}
			mu.Lock()
			thoughts.WriteString(update.Update.Content.Text)
			mu.Unlock()
			if in.OnThought != nil {
				in.OnThought(update.Update.Content.Text)
			}

		case acp.UpdateUsage:
			if update.Update.Used != nil && *update.Update.Used > 0 {
				mu.Lock()
				result.Usage.InputTokens = *update.Update.Used
				mu.Unlock()
			}

		case acp.UpdateToolCall:
			if loopbackState == nil {
				if in.OnInternalToolProgress != nil {
					in.OnInternalToolProgress(formatInternalToolProgress(update.Update))
				}
				return
			}
			normalizedName, matched := loopbackState.MatchAndNormalizeToolName(update.Update.Name)
			if !matched {
				if acp.IsBuiltInContainerTool(update.Update.Name) {
					logger.Warn("built-in container tool invoked during client-tool delegation turn",
						"toolName", update.Update.Name, "workerID", worker.ID)
				}
				if in.OnInternalToolProgress != nil {
					in.OnInternalToolProgress(formatInternalToolProgress(update.Update))
				}
				return
			}
			notifyFirstToken()
			callID := update.Update.ToolCallID
			if callID == "" {
				callID = newToolCallID()
			}
			args := update.Update.RawInput
			if len(args) == 0 || string(args) == "null" {
				args = json.RawMessage("{}")
			}
			if entry, added := collector.AddFrom(callID, normalizedName, args, SchemaFor(loopbackState, normalizedName), originNativeUpdate); added {
				telemetry.DefaultMetrics.ToolCallsTotal.Add(1)
				if in.OnTool != nil {
					in.OnTool(entry)
				}
				armCancel()
			}
		}
	})
	defer worker.Transport.SetUpdateListener(nil)

	// Drain every intercepted call for the whole turn. The previous implementation read exactly one
	// and returned, silently discarding multi-tool turns that agy really produces.
	//
	// The drainer is stopped with a per-attempt signal rather than by closing the session channel:
	// the session is registered once per CLIENT REQUEST but this function runs once per failover
	// ATTEMPT, so closing it here would leave attempt 2 with a dead channel and silently drop all
	// client tool delegation.
	var drainWG sync.WaitGroup
	drainStop := make(chan struct{})
	if loopbackState != nil {
		// Discard calls a previous attempt left buffered, so they cannot leak into this turn.
		for len(loopbackState.Calls) > 0 {
			select {
			case <-loopbackState.Calls:
			default:
				break
			}
		}
		drainWG.Add(1)
		go func() {
			defer drainWG.Done()
			for {
				select {
				case <-drainStop:
					return
				case call, ok := <-loopbackState.Calls:
					if !ok {
						return
					}
					normalizedName, matched := loopbackState.MatchAndNormalizeToolName(call.Name)
					if !matched {
						continue
					}
					lateNotify := func() {
						bar.Lock()
						defer bar.Unlock()
						if late.Load() {
							return
						}
						notifyFirstToken()
					}
					lateNotify()
					if entry, added := collector.AddFrom(call.ID, normalizedName, call.Arguments, SchemaFor(loopbackState, normalizedName), originLoopbackCall); added {
						telemetry.DefaultMetrics.ToolCallsTotal.Add(1)
						bar.Lock()
						refused := late.Load()
						var emit func()
						if !refused && in.OnTool != nil {
							emit = func() { in.OnTool(entry) }
						}
						bar.Unlock()
						if emit != nil {
							emit()
						}
						armCancel()
					}
				case <-promptCtx.Done():
					return
				}
			}
		}()
	}

	var promptResult acp.PromptResult
	callErr := worker.Transport.Call(promptCtx, "session/prompt", acp.PromptParams{
		SessionID: sessionID,
		Prompt:    in.PromptBlocks,
	}, &promptResult)

	// Close both producers before snapshotting. Order matters: refuse late callbacks first, then close
	// the loopback channel and join its drainer (which must not be blocked on `bar`), and only then
	// take `bar` — which waits out any listener invocation already in flight.
	late.Store(true)
	worker.Transport.SetUpdateListener(nil)
	// Signal (not close) the drainer: the loopback session outlives this attempt.
	close(drainStop)
	drainWG.Wait()

	// The snapshot region is serialised against any in-flight listener invocation: take `bar`, emit the
	// stop-sequence tail, capture text/thoughts/tools, then release. Everything after this point is
	// pure post-processing and must not block a late callback.
	bar.Lock()
	stopCancelTimer()

	// Emit the tail withheld for stop-sequence overlap detection, unless a sequence matched.
	if tail := stopSeq.Flush(); tail != "" {
		mu.Lock()
		text.WriteString(tail)
		mu.Unlock()
		if in.OnText != nil {
			in.OnText(tail)
		}
	}

	mu.Lock()
	result.Text = text.String()
	result.Thoughts = thoughts.String()
	mu.Unlock()
	result.Tools = collector.Snapshot()
	bar.Unlock()
	if len(result.Tools) == 0 && in.ParseTextToolCalls != nil {
		// Some models emit `<tool_call>{...}</tool_call>` in the text stream rather than a native ACP
		// tool_call update. Recover them so client tool delegation still works for those models.
		if clean, parsed := in.ParseTextToolCalls(result.Text); len(parsed) > 0 {
			result.Tools = parsed
			result.Text = clean
		}
	}
	result.StopReason = promptResult.StopReason
	result.StopMatched = stopSeq.Stopped()
	result.MatchedSequence = stopSeq.MatchedSequence()
	if usage, ok := FromPromptUsage(promptResult.Usage); ok {
		mu.Lock()
		result.Usage = usage
		mu.Unlock()
		result.UsageFromAgent = true
	}

	if callErr != nil {
		if ttftFired.Load() {
			return result, ErrFirstTokenTimeout
		}
		// A cancelled prompt that still produced output or tool calls is a normal terminal state:
		// the gateway itself cancelled once it had what the client needed.
		if errors.Is(callErr, context.Canceled) &&
			(result.Text != "" || len(result.Tools) > 0 || streamParser.IsTerminated() || result.StopMatched) {
			return result, nil
		}
		return result, callErr
	}

	return result, nil
}

// setupACPSession negotiates session/new and applies the negotiated config options.
//
// @purpose Separates the one-time session handshake from the streaming turn body.
// @rationale Keeps the executor readable and makes the handshake independently testable.
// @preconditions worker is leased; cfg non-nil.
// @param ctx Turn root context.
// @param worker Leased worker.
// @param cfg Gateway configuration.
// @param logger Structured logger.
// @param in Turn input.
// @returns The negotiated session id, the prompt blocks to use (with the system prompt folded in when
// no capability channel exists), and an error when the session cannot be created or closed cleanly.
// @throws Returns the agent error when session/new fails after one auth retry.
// @invariants Registers a session/close defer that always runs, gated on the advertised capability.
func setupACPSession(
	ctx context.Context,
	worker *pool.Worker,
	cfg *config.Config,
	logger *slog.Logger,
	in turnInput,
) (string, []acp.PromptBlock, func(), error) {
	sessCtx, cancelSess := context.WithTimeout(ctx, cfg.Pool.HandshakeTimeout)
	defer cancelSess()

	newParams, foldPrompt := buildACPSessionNewParams(SessionSetupInput{
		Worker:               worker,
		Config:               cfg,
		ConsumerSystemPrompt: in.SystemPrompt,
		LoopbackSessionID:    in.LoopbackSessionID,
		LoopbackToken:        in.LoopbackToken,
		ClientToolNames:      in.ClientToolNames,
	})

	var newResult acp.SessionNewResult
	if err := worker.Transport.Call(sessCtx, "session/new", newParams, &newResult); err != nil {
		var respErr *acp.ResponseError
		if errors.As(err, &respErr) && respErr.Code == acp.ErrCodeAuthRequired {
			if sleepErr := sleepCtx(sessCtx, time.Second); sleepErr != nil {
				return "", nil, nil, sleepErr
			}
			err = worker.Transport.Call(sessCtx, "session/new", newParams, &newResult)
		}
		if err != nil {
			return "", nil, nil, err
		}
	}
	if newResult.SessionID == "" {
		return "", nil, nil, errors.New("session/new returned an empty session id")
	}

	// The closer MUST run after session/prompt completes. A defer here would tear the session down
	// as soon as this helper returns, so every subsequent prompt fails with "Session not found".
	closer := func() {
		if !worker.Capabilities.SupportsSessionClose {
			return
		}
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelClose()
		err := worker.Transport.Call(closeCtx, "session/close", acp.SessionCloseParams{SessionID: newResult.SessionID}, nil)
		var respErr *acp.ResponseError
		if err != nil && !(errors.As(err, &respErr) && respErr.Code == acp.ErrCodeResourceNotFound) {
			logger.Debug("session/close reported an error", "sessionId", newResult.SessionID, "err", err)
		}
	}

	mode := cfg.Execution.AcpSessionMode
	if _, _, err := applySessionConfigOptions(
		sessCtx, worker, newResult,
		in.ModelEntry.UpstreamModel, in.ThoughtLevel, mode,
		cfg.Pool.ConfigSwitchTimeout, logger,
	); err != nil {
		closer()
		return "", nil, nil, err
	}

	blocks := in.PromptBlocks
	if foldPrompt != "" {
		// No capability channel: prepend instead of dropping the prompt.
		logger.Warn("agent advertises no system prompt channel; folding system prompt into the first block",
			"workerID", worker.ID)
		folded := make([]acp.PromptBlock, 0, len(blocks)+1)
		folded = append(folded, acp.PromptBlock{Type: "text", Text: foldPrompt + "\n\n"})
		folded = append(folded, blocks...)
		blocks = folded
	}

	return newResult.SessionID, blocks, closer, nil
}

// sleepCtx waits for d or until the context is done.
//
// @purpose Replaces a blocking time.Sleep inside the request path with a cancellable wait.
// @rationale The previous auth retry held a worker lease for a full second regardless of client
// disconnects.
// @param ctx Cancellation source.
// @param d Delay.
// @returns ctx.Err() when cancelled, nil otherwise.
// @invariants O(1) resources; timer stopped on every path.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// newToolCallID mints a stable tool call identifier when the agent omits one.
//
// @purpose Guarantees every emitted tool call has an id, as both provider schemas require.
// @rationale A missing id makes deduplication and multi-call correlation impossible.
// @returns A provider-agnostic id prefixed with "call_".
// @invariants O(1).
func newToolCallID() string {
	return fmt.Sprintf("call_%s", strings.ReplaceAll(uuid.NewString()[:18], "-", ""))
}

// requestIDFromContext returns the request id for logging, or an empty string.
//
// @purpose Correlates error logs with the client-visible X-Request-ID.
// @param r HTTP request.
// @returns The request id header value.
// @invariants Pure.
func requestIDFromContext(r *http.Request) string {
	if r == nil {
		return ""
	}
	return r.Header.Get("X-Request-ID")
}
