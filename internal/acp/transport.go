package acp

import (
	"bufio"
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

	closed atomic.Bool
	doneCh chan struct{}
}

// NewTransport creates an initialized Transport wrapping input and output streams.
func NewTransport(in io.WriteCloser, out io.ReadCloser, maxScannerBuffer int, logger *slog.Logger) *Transport {
	if maxScannerBuffer <= 0 {
		maxScannerBuffer = 16 * 1024 * 1024 // 16 MiB default
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
func (t *Transport) SetUpdateListener(fn func(update SessionUpdateParams)) {
	t.updateListenerMu.Lock()
	defer t.updateListenerMu.Unlock()
	t.updateListener = fn
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

func (t *Transport) readLoop(maxBuffer int) {
	scanner := bufio.NewScanner(t.out)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, maxBuffer)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// First, probe whether this is a Response, Request, or Notification
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			t.logger.Warn("invalid JSON-RPC line received", "error", err, "line", string(line))
			continue
		}

		// Handle Response (has id and (result or error))
		if _, hasID := raw["id"]; hasID {
			if _, hasMethod := raw["method"]; !hasMethod {
				var resp Response
				if err := json.Unmarshal(line, &resp); err == nil {
					t.dispatchResponse(&resp)
					continue
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
					continue
				}
			} else {
				// Handle Notification (e.g. session/update)
				var notif Notification
				if err := json.Unmarshal(line, &notif); err == nil {
					t.handleIncomingNotification(&notif)
					continue
				}
			}
		}
	}

	if err := scanner.Err(); err != nil && !t.closed.Load() {
		t.logger.Error("acp transport scanner error", "error", err)
	}
	_ = t.Close()
}

func (t *Transport) dispatchResponse(resp *Response) {
	var id int64
	switch v := resp.ID.(type) {
	case float64:
		id = int64(v)
	case int64:
		id = v
	default:
		t.logger.Warn("unexpected response ID type", "id", resp.ID)
		return
	}

	t.pendingMu.Lock()
	ch, exists := t.pending[id]
	t.pendingMu.Unlock()

	if exists {
		ch <- resp
	} else {
		t.logger.Debug("unmatched response ID received", "id", id)
	}
}

func (t *Transport) handleIncomingRequest(req *Request) {
	if req.Method == "session/request_permission" {
		t.handlePermissionRequest(req)
		return
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

// handlePermissionRequest implements the Layer 3 YOLO auto-approval interceptor.
func (t *Transport) handlePermissionRequest(req *Request) {
	var params RequestPermissionParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.logger.Error("failed to unmarshal permission request", "error", err)
		return
	}

	// Priority resolution: "allow_always" -> "allow_once" -> contains "allow" -> options[0]
	var chosenOptionID string
	for _, opt := range params.Options {
		if opt.Kind == "allow_always" {
			chosenOptionID = opt.OptionID
			break
		}
	}
	if chosenOptionID == "" {
		for _, opt := range params.Options {
			if opt.Kind == "allow_once" {
				chosenOptionID = opt.OptionID
				break
			}
		}
	}
	if chosenOptionID == "" {
		for _, opt := range params.Options {
			if strings.Contains(strings.ToLower(opt.OptionID), "allow") {
				chosenOptionID = opt.OptionID
				break
			}
		}
	}
	if chosenOptionID == "" && len(params.Options) > 0 {
		chosenOptionID = params.Options[0].OptionID
	}

	result := RequestPermissionResult{
		Outcome: PermissionOutcome{
			Outcome:  "selected",
			OptionID: chosenOptionID,
		},
	}
	rawResult, _ := json.Marshal(result)

	resp := Response{
		JSONRPC: JSONRPCVersion,
		ID:      req.ID,
		Result:  rawResult,
	}

	t.logger.Info("auto-approved permission request", "toolCallId", params.ToolCallID, "optionId", chosenOptionID)
	_ = t.send(resp)
}

func (t *Transport) handleIncomingNotification(notif *Notification) {
	if notif.Method == "session/update" {
		var params SessionUpdateParams
		if err := json.Unmarshal(notif.Params, &params); err != nil {
			t.logger.Warn("failed to parse session/update notification", "error", err)
			return
		}

		t.updateListenerMu.RLock()
		listener := t.updateListener
		t.updateListenerMu.RUnlock()

		if listener != nil {
			listener(params)
		}
	}
}
