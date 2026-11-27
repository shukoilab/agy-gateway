package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"unicode/utf8"
)

// SSEWriter coordinates thread-safe Server-Sent Events output.
type SSEWriter struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	mu        sync.Mutex
	closed    bool
	committed bool
}

// Committed reports whether any byte has been flushed to the client.
//
// @purpose Lets a handler choose between an HTTP status error and an in-band SSE error without
// guessing from its own bookkeeping.
// @rationale The status line is sent implicitly by the first Flush, not by NewSSEWriter, so this
// writer is the only place that knows the truth. A handler tracking "first content chunk" instead
// produced `superfluous response.WriteHeader` whenever a keepalive had already committed the 200.
// @returns true once a chunk, keepalive, error frame or [DONE] has been flushed.
// @invariants Thread-safe; never resets.
func (sw *SSEWriter) Committed() bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.committed
}

// NewSSEWriter initializes headers and flusher for streaming HTTP responses.
func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("response writer does not support flushing")
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	return &SSEWriter{
		w:       w,
		flusher: flusher,
	}, nil
}

// WriteChunk serializes and writes an individual ChatCompletionChunk.
func (sw *SSEWriter) WriteChunk(chunk *ChatCompletionChunk) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	if sw.closed {
		return nil
	}

	data, err := json.Marshal(chunk)
	if err != nil {
		return err
	}

	if !utf8.Valid(data) {
		return fmt.Errorf("chunk contains invalid UTF-8 bytes")
	}

	_, err = fmt.Fprintf(sw.w, "data: %s\n\n", data)
	if err != nil {
		return err
	}
	sw.flusher.Flush()
	sw.committed = true
	return nil
}

// WriteKeepalive writes an SSE comment keepalive tick.
func (sw *SSEWriter) WriteKeepalive() error {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	if sw.closed {
		return nil
	}

	_, err := fmt.Fprint(sw.w, ": keepalive\n\n")
	if err != nil {
		return err
	}
	sw.flusher.Flush()
	sw.committed = true
	return nil
}

// WriteError serializes and writes an SSE error event and closes the stream.
func (sw *SSEWriter) WriteError(message, errType string) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	if sw.closed {
		return nil
	}

	payload := map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    errType,
		},
	}
	data, _ := json.Marshal(payload)
	_, err := fmt.Fprintf(sw.w, "data: %s\n\n", data)
	if err != nil {
		return err
	}
	sw.flusher.Flush()
	sw.committed = true
	return nil
}

// Close emits data: [DONE]\n\n and finalizes the stream.
func (sw *SSEWriter) Close() error {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	if sw.closed {
		return nil
	}
	sw.closed = true

	_, err := fmt.Fprint(sw.w, "data: [DONE]\n\n")
	if err != nil {
		return err
	}
	sw.flusher.Flush()
	sw.committed = true
	return nil
}
