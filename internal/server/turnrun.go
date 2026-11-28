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
	"agy-gateway/internal/retry"
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

	// Client-tool delegation state. ClientToolSessionID is the registry key the bridge's calls are
	// attributed to and is empty when the caller declared no tools; ClientToolRegistry owns the
	// SessionState for that key.
	ClientToolSessionID string
	// ClientToolSockets serves the registry over a per-turn Unix socket in the worker's TMPDIR.
	ClientToolSockets *mcp.SocketServer
	LoopbackState     *mcp.SessionState
	ClientToolNames   []string
	// StopSequences are client-supplied sequences that must truncate the stream gateway-side.
	StopSequences []string
	// WorkspaceDir overrides the directory this turn runs in. Empty means the worker's own Cwd (which
	// is already the shared workspace root). The provider handlers set it to a per-conversation
	// subdirectory when the harness supplied a conversation identifier.
	WorkspaceDir string
	// AdditionalDirectories are extra workspace roots the consumer granted (OpenAI has no field for
	// this, Anthropic neither; populated from the admin/config layer when available).
	AdditionalDirectories []string
	// ClientMCPServers are MCP servers the consumer declared, forwarded to session/new verbatim.
	ClientMCPServers []acp.MCPServerInfo
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
	// SessionTitle is the agent's auto-generated conversation title (session_info_update). It is the
	// only human-meaningful name a session ever gets, and both provider schemas lack a field for it,
	// so it is surfaced as a response header and through the admin API instead of being discarded.
	SessionTitle string
	// AppliedThoughtLevel is the reasoning level actually in force upstream. It can differ from the
	// requested one when the selected model does not offer it, so reporting it is what makes a
	// downgrade visible instead of silent.
	AppliedThoughtLevel string
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

	sessionID, promptBlocks, closeSession, appliedThoughtLevel, err := setupACPSession(ctx, worker, cfg, logger, in)
	if err != nil {
		return result, err
	}
	// Runs after the prompt turn finishes on every path; a defer inside setupACPSession closed the
	// session before the prompt and produced "Session not found".
	defer closeSession()
	if len(promptBlocks) > 0 {
		in.PromptBlocks = promptBlocks
	}
	result.AppliedThoughtLevel = appliedThoughtLevel

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
		// refusalGate holds back the leading bytes of an assistant message while they could still be a
		// quota refusal. agy reports an exhausted account as a normal end_turn with plain text, so the
		// only way to fail over instead of forwarding an upsell is to recognise that text here.
		refusalGate = &quotaRefusalGate{}
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
	// promptStart anchors both TTFT and the turn-duration measurement; declared here because the
	// first-token callback is defined before the prompt is actually submitted.
	var promptStart time.Time
	notifyFirstToken := func() {
		notifyOnce.Do(func() {
			result.FirstTokenSeen = true
			if !promptStart.IsZero() {
				telemetry.DefaultMetrics.TTFTMillisSum.Add(time.Since(promptStart).Milliseconds())
				telemetry.DefaultMetrics.TTFTCount.Add(1)
			}
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

	// The cancel timer is an IDLE watchdog, not a deadline after a tool call: it fires only when the
	// agent has gone completely silent while the gateway already holds something to return. Re-arming
	// it on every streaming chunk is what makes that true. Without the reset a model that keeps
	// thinking for more than one second after emitting a tool call — routine for a reasoning model —
	// had its turn cancelled mid-generation, so the reasoning text and any later calls were lost and
	// the client saw a truncated turn it would simply retry.
	var cancelTimer *time.Timer
	var cancelTimerMu sync.Mutex
	// cancelSent records that the agent was already asked to wind this turn down. Re-arming after
	// that point would send a second `session/cancel` for a turn that is already ending, so the
	// watchdog fires exactly once per attempt.
	var cancelSent bool
	stopCancelTimer := func() {
		cancelTimerMu.Lock()
		if cancelTimer != nil {
			cancelTimer.Stop()
			cancelTimer = nil
		}
		cancelTimerMu.Unlock()
	}
	armCancel := func() {
		cancelTimerMu.Lock()
		if cancelSent {
			cancelTimerMu.Unlock()
			return
		}
		if cancelTimer != nil {
			cancelTimer.Stop()
		}
		cancelTimer = NewTurnTimer(cfg.MCP.ClientToolCalling.ToolCallCollectWindow, func() {
			// Ask the agent to wind the turn down, then keep reading for one more window so updates it
			// already queued still reach the client. Cancelling promptCtx at this instant aborted the
			// read path and discarded them, which is how a reasoning model lost the calls it emitted
			// right after a silent think gap. If the agent does not end the turn itself within that
			// grace window the prompt is cancelled anyway, so a wedged agent cannot hold the lease;
			// InferenceTimeout remains the outer bound.
			cancelTimerMu.Lock()
			cancelSent = true
			cancelTimerMu.Unlock()
			_ = worker.Transport.Notify("session/cancel", acp.SessionCancelParams{SessionID: sessionID})
			cancelTimerMu.Lock()
			cancelTimer = NewTurnTimer(cfg.MCP.ClientToolCalling.ToolCallCollectWindow, cancelPrompt)
			cancelTimerMu.Unlock()
		})
		cancelTimerMu.Unlock()
	}
	// touchCancel restarts the idle window only once a tool call is already collected, i.e. only while
	// the gateway is holding something the client must receive. Called for every streamed chunk so a
	// model that keeps producing output is never cancelled for taking longer than the window.
	touchCancel := func() {
		cancelTimerMu.Lock()
		armed := cancelTimer != nil && !cancelSent
		cancelTimerMu.Unlock()
		if armed {
			armCancel()
		}
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
			touchCancel()
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
			// A quota refusal must never reach the client: check the gate before anything else can
			// treat this text as a real answer.
			if emit != "" {
				emit = refusalGate.Feed(emit)
			}
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
			if refusalGate.Matched() {
				// Stop the turn as soon as the refusal is identified: the account cannot serve this
				// request and every further chunk would be more of the same message.
				cancelPrompt()
			}

		case acp.UpdateAgentThoughtChunk:
			if update.Update.Content == nil {
				return
			}
			notifyFirstToken()
			touchCancel()
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

		case acp.UpdateSessionInfo:
			// The agent names the conversation itself; the title is the only stable human label a
			// session gets. Providers have no field for it, so it is reported out of band.
			if update.Update.Title != "" {
				mu.Lock()
				result.SessionTitle = update.Update.Title
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
		// Discard calls a previous attempt left buffered, so they cannot leak into this turn. On a
		// buffered channel len>0 guarantees the receive succeeds, so this drain never blocks.
		for len(loopbackState.Calls) > 0 {
			<-loopbackState.Calls
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
	promptStart = time.Now()
	telemetry.DefaultMetrics.PromptsStarted.Add(1)
	callErr := worker.Transport.Call(promptCtx, "session/prompt", acp.PromptParams{
		SessionID: sessionID,
		Prompt:    in.PromptBlocks,
	}, &promptResult)
	telemetry.DefaultMetrics.TurnMillisSum.Add(time.Since(promptStart).Milliseconds())
	telemetry.DefaultMetrics.TurnCount.Add(1)

	// Classified here so every early return below is counted exactly once without repeating the call
	// at each of them.
	promptSucceeded := false
	defer func() {
		if promptSucceeded {
			telemetry.DefaultMetrics.PromptsCompleted.Add(1)
			return
		}
		telemetry.DefaultMetrics.PromptsFailed.Add(1)
	}()

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
		if released := refusalGate.Feed(tail); released != "" {
			mu.Lock()
			text.WriteString(released)
			mu.Unlock()
			if in.OnText != nil {
				in.OnText(released)
			}
		}
	}
	// Anything the gate was still holding belongs to the client unless the turn was a refusal.
	if released := refusalGate.Flush(); released != "" {
		mu.Lock()
		text.WriteString(released)
		mu.Unlock()
		if in.OnText != nil {
			in.OnText(released)
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

	// The account refused this turn for want of quota. Report it as a failure so the failover
	// coordinator cools this account and retries on one that still has capacity, instead of handing the
	// client a subscription upsell as if it were the answer.
	if refusalGate.Matched() {
		// The refusal text was suppressed from the response, so the message has to come from the gate
		// itself; result.Text is empty by construction on this path.
		msg := strings.TrimSpace(refusalGate.Text())
		if msg == "" {
			msg = "account quota exhausted"
		}
		err := &quotaRefusalError{message: msg}
		if reset, ok := parseResetDuration(refusalGate.Text()); ok {
			err.reset, err.hasReset = reset, true
		}
		return result, pool.MarkZeroTokens(err)
	}

	if callErr == nil && result.StopReason == "end_turn" &&
		result.Text == "" && result.Thoughts == "" && len(result.Tools) == 0 {
		// A turn that ends "successfully" with no text, no thoughts and no tool calls produced nothing.
		// agy answers an internally-exhausted quota this way, so surfacing it as a normal completion
		// would report success for work that never happened.
		return result, retry.ErrEmptyCompletion
	}

	if callErr != nil {
		if ttftFired.Load() {
			return result, ErrFirstTokenTimeout
		}
		// A cancelled prompt that still produced output or tool calls is a normal terminal state:
		// the gateway itself cancelled once it had what the client needed.
		if errors.Is(callErr, context.Canceled) &&
			(result.Text != "" || len(result.Tools) > 0 || streamParser.IsTerminated() || result.StopMatched) {
			promptSucceeded = true
			return result, nil
		}
		// Record whether the turn produced anything, so failover can tell a refusal from a real
		// exhaustion without re-deriving it from the error text.
		if result.Text == "" && result.Thoughts == "" && len(result.Tools) == 0 {
			return result, pool.MarkZeroTokens(callErr)
		}
		return result, callErr
	}

	promptSucceeded = true
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
// @returns The negotiated session id, the caller's prompt blocks with the consumer system prompt
// prepended as a leading text block, a session-close closure, and an error when the session cannot be
// created or the model cannot be applied.
// @throws The agent error when session/new fails after one auth retry, an error when session/new
// returned an empty session id, or the model-application error from applySessionConfigOptions (an
// unavailable model is surfaced as 400 by the caller; an upstream set_config_option failure is a
// 500-class fault).
// @invariants Returns a closeSession closure instead of registering a defer: a defer inside this
// function would fire before the prompt turn and close the session early ("Session not found"), so the
// caller owns the lifetime and defers the returned closure after the turn completes.
func setupACPSession(
	ctx context.Context,
	worker *pool.Worker,
	cfg *config.Config,
	logger *slog.Logger,
	in turnInput,
) (string, []acp.PromptBlock, func(), string, error) {
	sessCtx, cancelSess := context.WithTimeout(ctx, cfg.Pool.HandshakeTimeout)
	defer cancelSess()

	// Publish the client-tool endpoint BEFORE session/new. agy spawns the stdio bridge while completing
	// the handshake, so a socket created afterwards races the MCP initialise and surfaces as a failed
	// tool registry.
	var toolListener *mcp.Listener
	var sockPath string
	if in.ClientToolSessionID != "" && in.ClientToolSockets != nil {
		// Short name: the unix path limit is ~108 bytes and TMPDIR already carries the worker id.
		name := "mcp-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "") + ".sock"
		ln, err := in.ClientToolSockets.Listen(worker.TmpDir, name, in.ClientToolSessionID)
		if err != nil {
			return "", nil, nil, "", err
		}
		toolListener = ln
		sockPath = ln.Path()
	}
	// Closed on every path below: without it a failed handshake would leave a listening socket for the
	// life of the worker.
	closeToolSocket := func() {
		if toolListener != nil {
			toolListener.Close()
		}
	}

	newParams := buildACPSessionNewParams(SessionSetupInput{
		Worker:                worker,
		Config:                cfg,
		ConsumerSystemPrompt:  in.SystemPrompt,
		ClientToolSessionID:   in.ClientToolSessionID,
		ClientToolSocketPath:  sockPath,
		ClientToolNames:       in.ClientToolNames,
		AdditionalDirectories: in.AdditionalDirectories,
		ClientMCPServers:      in.ClientMCPServers,
		WorkspaceDir:          in.WorkspaceDir,
	})

	var newResult acp.SessionNewResult
	if err := worker.Transport.Call(sessCtx, "session/new", newParams, &newResult); err != nil {
		var respErr *acp.ResponseError
		if errors.As(err, &respErr) && respErr.Code == acp.ErrCodeAuthRequired {
			if sleepErr := sleepCtx(sessCtx, time.Second); sleepErr != nil {
				closeToolSocket()
				return "", nil, nil, "", sleepErr
			}
			err = worker.Transport.Call(sessCtx, "session/new", newParams, &newResult)
		}
		if err != nil {
			closeToolSocket()
			return "", nil, nil, "", err
		}
	}
	if newResult.SessionID == "" {
		closeToolSocket()
		return "", nil, nil, "", errors.New("session/new returned an empty session id")
	}

	// Declare which directories this session may touch. The fs handlers refuse every path outside
	// these roots, so registering them is what makes the advertised fs capability safe rather than an
	// arbitrary file read/write primitive for a remote harness.
	if handlers := worker.Transport.Handlers(); handlers != nil {
		roots := make([]string, 0, len(newParams.AdditionalDirectories)+1)
		// Use the session's actual CWD, not the worker's default: a per-conversation subdirectory is
		// where the agent will read and write, so it must be inside the roots the fs handlers allow.
		roots = append(roots, newParams.CWD)
		roots = append(roots, newParams.AdditionalDirectories...)
		handlers.RegisterSession(newResult.SessionID, roots)
	}
	telemetry.DefaultMetrics.SessionsCreated.Add(1)

	// The closer MUST run after session/prompt completes. A defer here would tear the session down
	// as soon as this helper returns, so every subsequent prompt fails with "Session not found".
	closer := func() {
		// Drains and closes the client-tool socket first: the bridge's in-flight tools/call must reach
		// the registry, and the child must observe EOF instead of a rejected request.
		closeToolSocket()
		if handlers := worker.Transport.Handlers(); handlers != nil {
			// Kills any terminal the turn started and forgets the roots.
			handlers.UnregisterSession(newResult.SessionID)
		}
		telemetry.DefaultMetrics.SessionsClosed.Add(1)
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

	appliedThoughtLevel, err := "", error(nil)
	if _, appliedThoughtLevel, err = applySessionConfigOptions(
		sessCtx, worker, newResult,
		in.ModelEntry.UpstreamModel, in.ThoughtLevel,
		cfg.Pool.ConfigSwitchTimeout, logger,
	); err != nil {
		closer()
		return "", nil, nil, "", err
	}

	// The consumer system prompt is delivered as the first prompt block. It must never travel in
	// session/new: the vendor carrier is policy-guarded and can answer a synthetic 429 with zero
	// updates, leaving the turn to hang.
	blocks := prependPromptBlocks(in.PromptBlocks, effectiveSystemPrompt(SessionSetupInput{
		Config:               cfg,
		ConsumerSystemPrompt: in.SystemPrompt,
		ClientToolNames:      in.ClientToolNames,
	}))

	return newResult.SessionID, blocks, closer, appliedThoughtLevel, nil
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
