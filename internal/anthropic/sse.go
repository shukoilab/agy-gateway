package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// SSEWriter writes Anthropic-compliant Server-Sent Events to an HTTP client.
//
// @purpose Encapsulates the 7-stage streaming event protocol for Anthropic Claude clients.
// @rationale Provides thread-safe, non-buffering flushing of message, content block, and ping events.
// It deliberately does not write the status line: the first flushed event commits it, so a turn that
// fails before any event can still answer with a truthful HTTP error.
// @preconditions w must support http.Flusher.
type SSEWriter struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	mu        sync.Mutex
	committed bool
}

// Committed reports whether any event byte has been flushed to the client.
//
// @purpose Lets the handler distinguish "no event yet, a real HTTP error is still possible" from
// "the stream is live, only an in-band error event remains".
// @rationale NewSSEWriter used to call WriteHeader+Flush immediately, which made every later status
// decision a `superfluous response.WriteHeader`.
// @returns true once a single event has been flushed.
// @invariants Thread-safe; never resets.
func (sw *SSEWriter) Committed() bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.committed
}

// NewSSEWriter initializes a new Anthropic SSE writer and sets HTTP headers.
func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("underlying response writer %T does not support http.Flusher", w)
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("anthropic-version", "2023-06-01")

	return &SSEWriter{
		w:       w,
		flusher: flusher,
	}, nil
}

// writeEvent writes an event header and JSON data line, followed by a double newline and flush.
func (sw *SSEWriter) writeEvent(eventName string, payload any) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal SSE event %s: %w", eventName, err)
	}

	if _, err := fmt.Fprintf(sw.w, "event: %s\ndata: %s\n\n", eventName, string(data)); err != nil {
		return err
	}

	sw.flusher.Flush()
	sw.committed = true
	return nil
}

// WriteMessageStart sends the initial message_start event.
func (sw *SSEWriter) WriteMessageStart(id, model string, inputTokens int) error {
	evt := MessageStartEvent{
		Type: "message_start",
		Message: MessageResponse{
			ID:      id,
			Type:    "message",
			Role:    "assistant",
			Content: []ContentBlock{},
			Model:   model,
			Usage: Usage{
				InputTokens:  inputTokens,
				OutputTokens: 1,
			},
		},
	}
	return sw.writeEvent("message_start", evt)
}

// WriteContentBlockStart sends a content_block_start event for text, tool_use, or thinking blocks.
func (sw *SSEWriter) WriteContentBlockStart(index int, block ContentBlock) error {
	evt := ContentBlockStartEvent{
		Type:         "content_block_start",
		Index:        index,
		ContentBlock: block,
	}
	return sw.writeEvent("content_block_start", evt)
}

// WriteContentBlockDelta sends an incremental delta event (text_delta, input_json_delta, thinking_delta, signature_delta).
func (sw *SSEWriter) WriteContentBlockDelta(index int, delta ContentBlockDelta) error {
	evt := ContentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: index,
		Delta: delta,
	}
	return sw.writeEvent("content_block_delta", evt)
}

// WriteContentBlockStop sends a content_block_stop event closing a block.
func (sw *SSEWriter) WriteContentBlockStop(index int) error {
	evt := ContentBlockStopEvent{
		Type:  "content_block_stop",
		Index: index,
	}
	return sw.writeEvent("content_block_stop", evt)
}

// WriteMessageDelta sends final message metadata including stop reason and total output tokens.
func (sw *SSEWriter) WriteMessageDelta(stopReason string, stopSequence *string, outputTokens int) error {
	evt := MessageDeltaEvent{
		Type: "message_delta",
		Delta: MessageDeltaInfo{
			StopReason:   &stopReason,
			StopSequence: stopSequence,
		},
		Usage: MessageDeltaUsage{
			OutputTokens: outputTokens,
		},
	}
	return sw.writeEvent("message_delta", evt)
}

// WriteMessageStop sends the terminal message_stop event.
func (sw *SSEWriter) WriteMessageStop() error {
	evt := MessageStopEvent{
		Type: "message_stop",
	}
	return sw.writeEvent("message_stop", evt)
}

// WritePing sends a periodic keepalive ping.
func (sw *SSEWriter) WritePing() error {
	evt := PingEvent{
		Type: "ping",
	}
	return sw.writeEvent("ping", evt)
}

// WriteError sends an error event over the active SSE stream.
func (sw *SSEWriter) WriteError(errType, message string) error {
	evt := ErrorResponse{
		Type: "error",
		Error: ErrorDetails{
			Type:    errType,
			Message: message,
		},
	}
	return sw.writeEvent("error", evt)
}
