package anthropic

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// statusRecorder counts status writes so a duplicated WriteHeader is detectable.
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

// TestNewSSEWriter_DoesNotCommit proves the writer defers the status line: a turn that fails before
// its first event must still be able to answer with a truthful HTTP error (S7).
func TestNewSSEWriter_DoesNotCommit(t *testing.T) {
	rec := &statusRecorder{ResponseRecorder: httptest.NewRecorder()}
	sw, err := NewSSEWriter(rec)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	if sw.Committed() {
		t.Fatal("constructing the writer must not commit the response")
	}
	if got := rec.writes.Load(); got != 0 {
		t.Fatalf("no status write may happen at construction, got %d", got)
	}

	rec.Header().Set("Content-Type", "application/json")
	rec.WriteHeader(http.StatusBadGateway)
	if got := rec.writes.Load(); got != 1 {
		t.Fatalf("expected exactly one status write, got %d", got)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
}

// TestSSEWriter_FirstEventCommitsOnce covers S8: after an event, errors go in-band and the status
// line is written exactly once.
func TestSSEWriter_FirstEventCommitsOnce(t *testing.T) {
	rec := &statusRecorder{ResponseRecorder: httptest.NewRecorder()}
	sw, err := NewSSEWriter(rec)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	if err := sw.WriteMessageStart("msg_1", "claude-test", 12); err != nil {
		t.Fatalf("WriteMessageStart: %v", err)
	}
	if !sw.Committed() {
		t.Fatal("a flushed event must mark the stream committed")
	}
	if err := sw.WriteError("api_error", "upstream gave up"); err != nil {
		t.Fatalf("WriteError: %v", err)
	}
	if got := rec.writes.Load(); got != 1 {
		t.Fatalf("status line must be written exactly once, got %d", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: message_start") || !strings.Contains(body, "event: error") {
		t.Fatalf("both events must be on the wire, got %q", body)
	}
}
