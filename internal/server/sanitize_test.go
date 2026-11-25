package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agy-gateway/internal/acp"
)

// @purpose Proves the mid-stream error class never leaks upstream text (FR-18, CWE-209).
// @rationale A streaming turn has already sent 200, so the only place its failure can be delivered
// is in-band; echoing err.Error() there exposed model ids, ACP internals and filesystem paths.
// @preconditions None.
// @param: t (*testing.T) - Go test harness.
// @returns None.
// @throws Calls t.Fatalf on any leak or classification error.
// @invariants Pure, no I/O.
func TestClientErrorKindNeverEchoesUpstreamText(t *testing.T) {
	secret := "failed to spawn agy acp: fork/exec /usr/local/bin/agy: permission denied"

	cases := []struct {
		err  error
		want string
	}{
		{errors.New(secret), "api_error"},
		{context.Canceled, "inference_cancelled"},
		{context.DeadlineExceeded, "inference_cancelled"},
		{ErrFirstTokenTimeout, "first_token_timeout"},
		{&UnsupportedParamError{Field: "temperature"}, "unsupported_parameter"},
		{&acp.ResponseError{Code: acp.ErrCodeAuthRequired, Message: secret}, "account_auth_required"},
		{&acp.ResponseError{Code: acp.ErrCodeInvalidParams, Message: secret}, "invalid_params"},
	}

	for _, tc := range cases {
		kind := ClientErrorKind(tc.err)
		if kind != tc.want {
			t.Fatalf("ClientErrorKind(%v) = %q; want %q", tc.err, kind, tc.want)
		}
		msg := ClientErrorMessage(kind)
		if strings.Contains(msg, secret) || strings.Contains(kind, "permission denied") {
			t.Fatalf("client-visible text leaked upstream detail: %q / %q", kind, msg)
		}
		if strings.Contains(msg, "/usr/local/bin") || strings.Contains(msg, "agy acp") {
			t.Fatalf("client-visible message leaked a path: %q", msg)
		}
	}
}

// @purpose Proves the recovery middleware returns a constant body on panic.
// @rationale The previous handler interpolated the panic value, which is operator data.
// @preconditions None.
// @param: t (*testing.T) - Go test harness.
// @returns None.
// @throws Calls t.Fatalf if the panic value or a path survives into the body.
// @invariants Fires a real panic through the real middleware.
func TestRecoveryReturnsConstantBody(t *testing.T) {
	mw := NewMiddleware(nil)
	handler := mw.Recovery(httpHandlerPanics("panic text /secret/path model-x"))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))

	if rec.Code != 500 {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "/secret/path") || strings.Contains(body, "model-x") || strings.Contains(body, "panic text") {
		t.Fatalf("recovery body leaked the panic value: %s", body)
	}
	if !strings.Contains(body, "internal server error") {
		t.Fatalf("expected the constant message, got %s", body)
	}
}

// @purpose Helper that panics inside the middleware chain.
// @rationale Declared at file scope so Recovery is exercised through a real handler call.
// @preconditions msg is any string.
// @param msg Panic payload.
// @returns Never returns.
// @throws Always panics with msg.
// @invariants Intentionally unrecoverable except by Recovery.
func httpHandlerPanics(msg string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		panic(msg)
	}
}
