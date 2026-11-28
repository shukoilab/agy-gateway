package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
)

// Transport manages thread-safe JSON-RPC 2.0 communication over standard I/O pipes.
type Transport struct {
	in     io.WriteCloser
	out    io.ReadCloser
	logger *slog.Logger

	writeMu sync.Mutex
	reqSeq  atomic.Int64

	pendingMu sync.Mutex
	pending   map[int64]chan *Response

	updateListenerMu sync.RWMutex
	updateListener   func(update SessionUpdateParams)

	// handlers answers the agent→client requests implied by the advertised capabilities.
	// @purpose Turns an advertised capability into an answered one.
	// @rationale Advertising fs/terminal makes agy emit read_file/write_file/run_command; without a
	// handler the request is answered with method-not-found and the turn stalls. nil disables all
	// non-permission handlers (the sandbox profile).
	handlers *ClientHandlers

	closed atomic.Bool
	doneCh chan struct{}

	// autoApprove was removed with the YOLO refactor: permission requests are always approved, so
	// there is no policy to store.
}

// NewTransport creates an initialized Transport wrapping input and output streams.
func NewTransport(in io.WriteCloser, out io.ReadCloser, maxScannerBuffer int, logger *slog.Logger) *Transport {
	if maxScannerBuffer <= 0 {
		maxScannerBuffer = 134217728 // 128 MiB default (1M+ context support)
	}
	if logger == nil {
		logger = slog.Default()
	}

	t := &Transport{
		in:      in,
		out:     out,
		logger:  logger,
		pending: make(map[int64]chan *Response),
		doneCh:  make(chan struct{}),
	}

	go t.readLoop(maxScannerBuffer)
	return t
}

// SetUpdateListener registers a callback function to receive asynchronous session updates.
//
// @purpose Routes session/update notifications for the duration of one leased turn.
// @rationale There is exactly one listener slot and exactly one turn per worker at a time. Silently
// replacing an existing listener means two turns would interleave their output, so replacement is
// logged loudly to make a lease-discipline violation visible instead of corrupting both streams.
//
// @param fn Listener callback, or nil to clear the slot at end of turn.
// @returns None.
// @invariants Write-locked; the previous listener (if any and not nil) is reported.
func (t *Transport) SetUpdateListener(fn func(update SessionUpdateParams)) {
	t.updateListenerMu.Lock()
	defer t.updateListenerMu.Unlock()
	if t.updateListener != nil && fn != nil {
		t.logger.Error("update listener replaced while one was active; worker lease discipline violated")
	}
	t.updateListener = fn
}

// SetHandlers installs the agent→client request handlers.
//
// @purpose Wires the fs/terminal implementations to the transport once, before the first prompt.
// @rationale The handler set must match the advertised capabilities; installing it after the
// handshake would leave a window where the agent's first read_file request is unanswered.
//
// @param h Handler set, or nil to answer every non-permission request with method-not-found.
// @returns None.
// @invariants O(1); callers install it immediately after constructing the transport.
func (t *Transport) SetHandlers(h *ClientHandlers) {
	t.handlers = h
}

// Handlers returns the installed handler set, or nil.
//
// @purpose Lets the owner (worker) register session roots on the same object the transport uses.
// @rationale Without this accessor the worker would have to keep a second reference and could drift
// from the one actually serving requests.
// @param None.
// @returns The installed handler set, or nil.
// @invariants O(1).
func (t *Transport) Handlers() *ClientHandlers {
	return t.handlers
}

// Close gracefully closes the transport and releases active resources.
func (t *Transport) Close() error {
	if t.closed.CompareAndSwap(false, true) {
		close(t.doneCh)
		var errs []error
		if err := t.in.Close(); err != nil {
			errs = append(errs, err)
		}
		if err := t.out.Close(); err != nil {
			errs = append(errs, err)
		}

		t.pendingMu.Lock()
		for id, ch := range t.pending {
			close(ch)
			delete(t.pending, id)
		}
		t.pendingMu.Unlock()

		// Reap every terminal the session started. A child that outlives the transport is a zombie
		// process, and process-group kill is the only way to reach its grandchildren.
		if t.handlers != nil {
			t.handlers.Close()
		}

		return errors.Join(errs...)
	}
	return nil
}

// Call performs a synchronous JSON-RPC 2.0 request/response exchange with timeout.
func (t *Transport) Call(ctx context.Context, method string, params any, result any) error {
	if t.closed.Load() {
		return errors.New("acp transport is closed")
	}

	reqID := t.reqSeq.Add(1)

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("failed to marshal params: %w", err)
		}
		rawParams = b
	}

	req := Request{
		JSONRPC: JSONRPCVersion,
		ID:      reqID,
		Method:  method,
		Params:  rawParams,
	}

	respCh := make(chan *Response, 1)
	t.pendingMu.Lock()
	t.pending[reqID] = respCh
	t.pendingMu.Unlock()

	defer func() {
		t.pendingMu.Lock()
		delete(t.pending, reqID)
		t.pendingMu.Unlock()
	}()

	if err := t.send(req); err != nil {
		return fmt.Errorf("failed to write request: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.doneCh:
		return errors.New("transport closed while awaiting response")
	case resp, ok := <-respCh:
		if !ok || resp == nil {
			return errors.New("nil response received")
		}
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("failed to unmarshal response result: %w", err)
			}
		}
		return nil
	}
}

// Notify sends an unsolicited JSON-RPC 2.0 notification (no ID) to the peer.
//
// @purpose Sends one-way asynchronous notifications such as session/cancel to the ACP agent.
// @rationale ACP specifies notifications for client-side cancellations and state synchronization.
// @preconditions Transport must not be closed.
// @param method (string) - JSON-RPC method name (e.g. "session/cancel").
// @param params (any) - Optional notification payload struct.
// @returns error - Returns error if transport is closed or pipe write fails.
// @throws None.
// @invariants Safe for concurrent invocations across goroutines.
func (t *Transport) Notify(method string, params any) error {
	if t.closed.Load() {
		return errors.New("acp transport is closed")
	}

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("failed to marshal notification params: %w", err)
		}
		rawParams = b
	}

	notif := Notification{
		JSONRPC: JSONRPCVersion,
		Method:  method,
		Params:  rawParams,
	}

	return t.send(notif)
}

func (t *Transport) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	_, err = t.in.Write(b)
	return err
}

// - @purpose: Continuously consumes JSON-RPC lines from the process stdout stream and dispatches them.
// - @rationale: Uses a dynamic line reader with a 128 MiB ceiling guard instead of rigid bufio.Scanner to prevent ErrTooLong fatal pipe closures during 1M context turns.
// - @preconditions: t.out must be a readable io.ReadCloser.
// - @param: maxBuffer (int) - Maximum permissible size for a single JSON-RPC message line in bytes.
// - @returns: None.
// - @throws: None.
// - @invariants: Linear time complexity O(N); cleans up transport on EOF or fatal stream failure.
func (t *Transport) readLoop(maxBuffer int) {
	if maxBuffer <= 0 {
		maxBuffer = 134217728 // 128 MiB default
	}

	reader := bufio.NewReaderSize(t.out, 256*1024)

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimSpace(line)
			if len(line) > 0 {
				if len(line) > maxBuffer {
					t.logger.Error("acp transport line exceeded max buffer ceiling", "len", len(line), "max", maxBuffer)
					continue
				}
				t.processLine(line)
			}
		}

		if err != nil {
			if !t.closed.Load() && !errors.Is(err, io.EOF) {
				t.logger.Error("acp transport stream error", "error", err)
			}
			break
		}
	}

	_ = t.Close()
}

// - @purpose: Decodes and routes an individual JSON-RPC response, request, or notification line.
// - @rationale: Decouples line framing from JSON-RPC protocol demuxing for high testability.
// - @preconditions: line must be non-empty trimmed bytes.
// - @param: line ([]byte) - Single line raw JSON-RPC payload.
// - @returns: None.
// - @throws: None.
// - @invariants: Thread-safe dispatch to pending response channels or registered listeners.
func (t *Transport) processLine(line []byte) {
	// First, probe whether this is a Response, Request, or Notification
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		t.logger.Warn("invalid JSON-RPC line received", "error", err, "bytes", len(line))
		return
	}

	// Handle Response (has id and (result or error))
	if _, hasID := raw["id"]; hasID {
		if _, hasMethod := raw["method"]; !hasMethod {
			var resp Response
			if err := json.Unmarshal(line, &resp); err == nil {
				t.dispatchResponse(&resp)
				return
			}
		}
	}

	// Handle Request from Agent (e.g. session/request_permission)
	if rawMethod, hasMethod := raw["method"]; hasMethod {
		var method string
		_ = json.Unmarshal(rawMethod, &method)

		if _, hasID := raw["id"]; hasID {
			var req Request
			if err := json.Unmarshal(line, &req); err == nil {
				t.handleIncomingRequest(&req)
				return
			}
		} else {
			// Handle Notification (e.g. session/update)
			var notif Notification
			if err := json.Unmarshal(line, &notif); err == nil {
				t.handleIncomingNotification(&notif)
				return
			}
		}
	}
}

// dispatchResponse routes a decoded JSON-RPC response to its pending caller.
//
// @purpose Delivers the response to the single waiter registered for this request id.
// @rationale The delivery MUST happen while holding pendingMu. Close() closes every pending channel
// under the same lock; releasing the lock before sending leaves a window where a concurrent Close()
// closes the channel and this goroutine panics with "send on closed channel", killing the whole
// process (the panic occurs on the transport read goroutine, which the HTTP recovery middleware
// cannot catch). Channels are capacity-1, so the send never blocks while the lock is held.
//
// @preconditions resp must be non-nil with a numeric ID.
// @param resp Decoded response envelope.
// @returns None.
// @throws None; unmatched ids are logged at Debug.
// @invariants O(1); lock scope contains only one non-blocking send.
func (t *Transport) dispatchResponse(resp *Response) {
	var id int64
	switch v := resp.ID.(type) {
	case float64:
		id = int64(v)
	case int64:
		id = v
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			t.logger.Warn("unexpected response ID number", "id", v)
			return
		}
		id = parsed
	default:
		t.logger.Warn("unexpected response ID type", "id", resp.ID)
		return
	}

	t.pendingMu.Lock()
	defer t.pendingMu.Unlock()

	ch, exists := t.pending[id]
	if !exists {
		t.logger.Debug("unmatched response ID received", "id", id)
		return
	}
	select {
	case ch <- resp:
	default:
		// Buffer already holds an unread response; dropping is safe because Call consumes exactly one.
		t.logger.Warn("duplicate response for request id discarded", "id", id)
	}
}

func (t *Transport) handleIncomingRequest(req *Request) {
	t.logger.Debug("acp incoming request received", "method", req.Method, "id", req.ID)
	switch req.Method {
	case "session/request_permission":
		t.handlePermissionRequest(req)
		return
	case "elicitation/create":
		// A strict YOLO gateway has nothing to ask the user; cancelling is the documented
		// "no answer" outcome and keeps the turn moving.
		t.replyRaw(req.ID, json.RawMessage(`{"action":"cancel"}`))
		return
	}

	if t.handlers != nil {
		result, handled, err := t.handlers.Handle(req.Method, req.Params)
		if handled {
			if err != nil {
				t.replyError(req.ID, err)
				return
			}
			t.replyRaw(req.ID, result)
			return
		}
	}

	// For unsupported agent requests, send method not found error
	resp := Response{
		JSONRPC: JSONRPCVersion,
		ID:      req.ID,
		Error: &ResponseError{
			Code:    ErrCodeMethodNotFound,
			Message: fmt.Sprintf("unsupported agent request method: %s", req.Method),
		},
	}
	_ = t.send(resp)
}

// replyRaw writes a successful JSON-RPC response for reqID.
//
// @purpose Single emission helper for handler results.
// @rationale Keeps the response envelope in one place so no path can omit jsonrpc/version.
// @preconditions result is valid JSON (or nil for an empty object).
// @param reqID The JSON-RPC id being answered.
// @param result Marshalled result payload.
// @returns None.
// @throws None; a marshal-level failure cannot occur for already-marshalled bytes.
// @invariants Emits at most one response per call.
func (t *Transport) replyRaw(reqID any, result json.RawMessage) {
	if len(result) == 0 {
		result = json.RawMessage("{}")
	}
	resp := Response{JSONRPC: JSONRPCVersion, ID: reqID, Result: result}
	if err := t.send(resp); err != nil {
		t.logger.Error("failed to send handler response", "error", err)
	}
}

// replyError writes a JSON-RPC error response for reqID.
//
// @purpose Reports a handler failure to the agent in the canonical error shape.
// @rationale agy expects `{code,message}` only; the error text is already sanitised by the handler
// so no filesystem path or internal state leaks upstream.
// @preconditions reqID identifies the request being answered.
// @param reqID The JSON-RPC id being answered.
// @param err The failure to report.
// @returns None.
// @throws None.
// @invariants Emits at most one response per call.
func (t *Transport) replyError(reqID any, err error) {
	code := ErrCodeInternalError
	message := "request failed"
	var respErr *ResponseError
	if errors.As(err, &respErr) {
		code = respErr.Code
		message = respErr.Message
	}
	resp := Response{
		JSONRPC: JSONRPCVersion,
		ID:      reqID,
		Error:   &ResponseError{Code: code, Message: message},
	}
	if sendErr := t.send(resp); sendErr != nil {
		t.logger.Error("failed to send error response", "error", sendErr)
	}
}

// handlePermissionRequest answers a session/request_permission request from the agent.
//
// @purpose Prevents the agent turn from stalling while guaranteeing a deterministic, auditable decision.
// @rationale Verified on agy 1.5.16: an unanswered permission request hangs the turn indefinitely and no
// session/prompt response is ever produced. Selection is driven by the closed `kind` enum, never by
// substrings of the agent-defined `optionId`, so a localised or randomised id cannot be misread.
//
// @preconditions req.Params must decode into RequestPermissionParams; the transport must be open.
// @param req Incoming JSON-RPC request envelope.
// @returns None.
// @throws None; malformed payloads produce a `cancelled` outcome rather than silence.
// @invariants Always emits exactly one response for req.ID; never blocks.
func (t *Transport) handlePermissionRequest(req *Request) {
	var params RequestPermissionParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.logger.Error("failed to unmarshal permission request", "error", err)
		t.replyPermission(req.ID, PermissionOutcome{Outcome: "cancelled"}, "", "")
		return
	}

	// Priority resolution over the closed kind enum: allow_always -> allow_once -> any "allow_*" kind.
	// reject_once / reject_always are recognised and never selected.
	var chosen *PermissionOption
	for i := range params.Options {
		if params.Options[i].Kind == "allow_always" {
			chosen = &params.Options[i]
			break
		}
	}
	if chosen == nil {
		for i := range params.Options {
			if params.Options[i].Kind == "allow_once" {
				chosen = &params.Options[i]
				break
			}
		}
	}
	if chosen == nil {
		for i := range params.Options {
			if strings.HasPrefix(params.Options[i].Kind, "allow") {
				chosen = &params.Options[i]
				break
			}
		}
	}

	if chosen == nil {
		t.logger.Warn("permission request offered no allow option; cancelling",
			"sessionID", params.SessionID,
			"toolCallId", params.ResolvedToolCallID(),
			"optionCount", len(params.Options))
		t.replyPermission(req.ID, PermissionOutcome{Outcome: "cancelled"}, "", "")
		return
	}

	t.logger.Info("auto-approved permission request",
		"sessionID", params.SessionID,
		"toolCallId", params.ResolvedToolCallID(),
		"toolName", permissionToolName(&params),
		"optionId", chosen.OptionID,
		"kind", chosen.Kind)
	t.replyPermission(req.ID, PermissionOutcome{Outcome: "selected", OptionID: chosen.OptionID}, chosen.OptionID, chosen.Kind)
}

// permissionToolName extracts the tool name from whichever permission payload shape was received.
func permissionToolName(p *RequestPermissionParams) string {
	if p == nil || p.ToolCall == nil {
		return ""
	}
	return p.ToolCall.Name
}

// replyPermission writes the JSON-RPC response for a permission request.
//
// @purpose Single emission point so every path answers exactly once.
// @rationale Prevents both silent stalls (no response) and double responses (protocol corruption).
// @param reqID The JSON-RPC id being answered.
// @param outcome The selected or cancelled outcome.
// @returns None.
// @invariants Never panics on marshal failure; logs and returns.
func (t *Transport) replyPermission(reqID any, outcome PermissionOutcome, _, _ string) {
	rawResult, err := json.Marshal(RequestPermissionResult{Outcome: outcome})
	if err != nil {
		t.logger.Error("failed to marshal permission response", "error", err)
		return
	}
	resp := Response{JSONRPC: JSONRPCVersion, ID: reqID, Result: rawResult}
	if err := t.send(resp); err != nil {
		t.logger.Error("failed to send permission response", "error", err)
	}
}

func (t *Transport) handleIncomingNotification(notif *Notification) {
	t.logger.Debug("acp incoming notification received", "method", notif.Method, "rawBytes", len(notif.Params))
	if notif.Method == "session/update" {
		var params SessionUpdateParams
		if err := json.Unmarshal(notif.Params, &params); err != nil {
			t.logger.Warn("failed to parse session/update notification", "error", err, "bytes", len(notif.Params))
			return
		}

		t.logger.Debug("acp session/update parsed",
			"sessionId", params.SessionID,
			"sessionUpdate", params.Update.SessionUpdate,
			"hasContent", params.Update.Content != nil,
			"toolCallId", params.Update.ToolCallID,
		)

		t.updateListenerMu.RLock()
		listener := t.updateListener
		t.updateListenerMu.RUnlock()

		if listener != nil {
			listener(params)
		}
	}
}
