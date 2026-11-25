package mcp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestServeHTTPRejectsMissingToken proves FR-23: the internal bridge endpoint is authenticated.
//
// @purpose Prevents unauthenticated session enumeration or hijacking.
// @rationale The endpoint is reachable on the container port; session id secrecy is not auth.
// @preconditions None.
// @param t Go test handle.
// @returns None.
// @throws None.
// @invariants Deterministic.
func TestServeHTTPRejectsMissingToken(t *testing.T) {
	srv := NewLoopbackServer()
	state, err := srv.RegisterSession("sess_test_token", nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	for _, hdr := range []string{"", "Bearer wrong", "Bearer "} {
		req := httptest.NewRequest(http.MethodPost, "/internal/mcp/sess_test_token", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("header %q: expected 401, got %d", hdr, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/internal/mcp/sess_test_token", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+state.Token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid token: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestValidateToken covers the constant-time comparison contract.
//
// @purpose Proves the token check rejects every mismatch without leaking a hint.
// @rationale Auth must fail closed on missing sessions and empty tokens.
// @preconditions None.
// @param t Go test handle.
// @returns None.
// @throws None.
// @invariants Deterministic.
func TestValidateToken(t *testing.T) {
	srv := NewLoopbackServer()
	state, err := srv.RegisterSession("sess_v", nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	if err := srv.ValidateToken("sess_v", state.Token); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if err := srv.ValidateToken("sess_v", state.Token+"x"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized for a wrong token, got %v", err)
	}
	if err := srv.ValidateToken("sess_unknown", state.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized for an unknown session, got %v", err)
	}
	if err := srv.ValidateToken("sess_v", ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized for an empty token, got %v", err)
	}
}

// TestRegisterSessionEnforcesToolCeiling proves FR-08's bound.
//
// @purpose Prevents unbounded tool declarations from a single client.
// @rationale Every declaration is copied into a session snapshot; unbounded input is a memory risk.
// @preconditions None.
// @param t Go test handle.
// @returns None.
// @throws None.
// @invariants Deterministic.
func TestRegisterSessionEnforcesToolCeiling(t *testing.T) {
	srv := NewLoopbackServer(2)
	tools := []OpenAIToolDefinition{
		{Type: "function", Function: FunctionSchema{Name: "a"}},
		{Type: "function", Function: FunctionSchema{Name: "b"}},
		{Type: "function", Function: FunctionSchema{Name: "c"}},
	}
	if _, err := srv.RegisterSession("sess_ceiling", tools); !errors.Is(err, ErrTooManyTools) {
		t.Fatalf("expected ErrTooManyTools, got %v", err)
	}

	if _, err := srv.RegisterSession("sess_ok", tools[:2]); err != nil {
		t.Fatalf("expected two tools to be accepted, got %v", err)
	}
}
