package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// statusRecorder records how many times a status line was written, which is what
// net/http turns into the `superfluous response.WriteHeader` server log.
// statusRecorder counts status commits, including the implicit one net/http performs on the first
// body write. A duplicate WriteHeader is what the server logs as
// "superfluous response.WriteHeader call".
type statusRecorder struct {
	*httptest.ResponseRecorder
	writes atomic.Int32
}

func (r *statusRecorder) WriteHeader(code int) {
	r.writes.Add(1)
	r.ResponseRecorder.WriteHeader(code)
}

// Write mirrors net/http, which commits an implicit 200 on the first body write when no explicit
// status has been sent yet.
func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.writes.Load() == 0 {
		r.writes.Add(1)
	}
	return r.ResponseRecorder.Write(b)
}

// TestSSEWriter_KeepaliveThenErrorWritesStatusOnce covers S8: a keepalive commits the response, so a
// later in-band error must not attempt a second WriteHeader.
func TestSSEWriter_KeepaliveThenErrorWritesStatusOnce(t *testing.T) {
	rec := &statusRecorder{ResponseRecorder: httptest.NewRecorder()}

	sw, err := NewSSEWriter(rec)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	if sw.Committed() {
		t.Fatal("a writer that has flushed nothing must report Committed() == false")
	}

	if err := sw.WriteKeepalive(); err != nil {
		t.Fatalf("WriteKeepalive: %v", err)
	}
	if !sw.Committed() {
		t.Fatal("a flushed keepalive must mark the stream committed")
	}

	if err := sw.WriteError("upstream gave up", "api_error"); err != nil {
		t.Fatalf("WriteError: %v", err)
	}
	if got := rec.writes.Load(); got != 1 {
		t.Fatalf("status line must be written exactly once, got %d", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ": keepalive") || !strings.Contains(body, `"type":"api_error"`) {
		t.Fatalf("keepalive and in-band error must both be on the wire, got %q", body)
	}
}

// TestSSEWriter_ChunkCommits proves a content chunk also commits the stream, so the handler can rely
// on Committed() rather than tracking its own first-chunk flag.
func TestSSEWriter_ChunkCommits(t *testing.T) {
	rec := &statusRecorder{ResponseRecorder: httptest.NewRecorder()}
	sw, err := NewSSEWriter(rec)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	content := "hi"
	if err := sw.WriteChunk(&ChatCompletionChunk{
		ID:     "chatcmpl-test",
		Object: "chat.completion.chunk",
		Model:  "test-model",
		Choices: []ChatCompletionChunkChoice{{
			Index: 0,
			Delta: ChatCompletionChunkDelta{Content: &content},
		}},
	}); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}
	if !sw.Committed() {
		t.Fatal("a flushed chunk must mark the stream committed")
	}
	if got := rec.writes.Load(); got != 1 {
		t.Fatalf("status line must be written exactly once, got %d", got)
	}
}

// TestSSEWriter_UncommittedAllowsStatusError covers S7: a writer that flushed nothing must leave the
// handler free to answer with a real HTTP status.
func TestSSEWriter_UncommittedAllowsStatusError(t *testing.T) {
	rec := &statusRecorder{ResponseRecorder: httptest.NewRecorder()}
	sw, err := NewSSEWriter(rec)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	if sw.Committed() {
		t.Fatal("fresh writer must be uncommitted")
	}
	// The handler path: no byte flushed, so a JSON error with a real status is still legal.
	rec.Header().Set("Content-Type", "application/json")
	rec.WriteHeader(http.StatusGatewayTimeout)
	payload, _ := json.Marshal(map[string]string{"error": "timeout"})
	if _, err := rec.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := rec.writes.Load(); got != 1 {
		t.Fatalf("expected exactly one status write, got %d", got)
	}
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d", rec.Code)
	}
}
