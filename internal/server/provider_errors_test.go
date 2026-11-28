package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agy-gateway/internal/acp"
	"agy-gateway/internal/pool"
)

// acpErr builds a JSON-RPC error the way the agent emits it: code and message, no data.
func acpErr(code int, message string) error {
	return &acp.ResponseError{Code: code, Message: message}
}

// decodeOpenAIError pulls the provider error envelope out of a recorder.
func decodeOpenAIError(t *testing.T, rec *httptest.ResponseRecorder) (code, errType, errCode, param string) {
	t.Helper()
	var body struct {
		Error struct {
			Message string  `json:"message"`
			Type    *string `json:"type"`
			Code    *string `json:"code"`
			Param   *string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode openai error body: %v (body=%q)", err, rec.Body.String())
	}
	if body.Error.Type != nil {
		errType = *body.Error.Type
	}
	if body.Error.Code != nil {
		errCode = *body.Error.Code
	}
	if body.Error.Param != nil {
		param = *body.Error.Param
	}
	return body.Error.Message, errType, errCode, param
}

// decodeAnthropicError pulls the Anthropic error envelope out of a recorder.
func decodeAnthropicError(t *testing.T, rec *httptest.ResponseRecorder) (errType, message string) {
	t.Helper()
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode anthropic error body: %v (body=%q)", err, rec.Body.String())
	}
	if body.Type != "error" {
		t.Fatalf("anthropic envelope type = %q, want \"error\"", body.Type)
	}
	return body.Error.Type, body.Error.Message
}

// TestOpenAIWriteTurnError_InvalidParamsNamesModelWhenModelRelated pins S18's -32602 mapping.
//
// @purpose Proves an ACP invalid-params fault becomes a caller-fixable 400 naming the field.
// @rationale The previous mapping answered `model_not_available`, which is a different condition
// (a catalog miss) and gave the caller nothing to correct.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; pure error mapping.
func TestOpenAIWriteTurnError_InvalidParamsNamesModelWhenModelRelated(t *testing.T) {
	h := &OpenAIHandlers{logger: slog.Default()}

	for _, message := range []string{
		`Thinking level "high" is not offered for model "Claude Sonnet 4.6 (Thinking)"`,
		"Unknown config option: thought_level",
		"model gemini-9 does not exist",
	} {
		rec := httptest.NewRecorder()
		h.writeTurnError(rec, acpErr(acp.ErrCodeInvalidParams, message), time.Second)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%q must map to 400, got %d", message, rec.Code)
		}
		_, errType, _, param := decodeOpenAIError(t, rec)
		if errType != "invalid_request_error" {
			t.Fatalf("%q type = %q, want invalid_request_error", message, errType)
		}
		if param != "model" {
			t.Fatalf("%q param = %q, want model", message, param)
		}
	}
}

// TestOpenAIWriteTurnError_InvalidParamsOmitsParamForSessionFaults pins the negative half.
//
// @purpose Proves a session-level -32602 does not fabricate `param:"model"`.
// @rationale `-32602` also covers an unknown session id; blaming the model there would send the
// caller chasing a field that was never wrong.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestOpenAIWriteTurnError_InvalidParamsOmitsParamForSessionFaults(t *testing.T) {
	h := &OpenAIHandlers{logger: slog.Default()}
	rec := httptest.NewRecorder()
	h.writeTurnError(rec, acpErr(acp.ErrCodeInvalidParams, "Session not found: abc-123"), time.Second)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("session fault must still be a 400, got %d", rec.Code)
	}
	_, _, _, param := decodeOpenAIError(t, rec)
	if param != "" {
		t.Fatalf("a session fault must not name a param, got %q", param)
	}
}

// TestOpenAIWriteTurnError_MethodNotFoundIsOpaque500 pins the -32601 mapping.
//
// @purpose Proves an unsupported ACP method becomes a 500 without leaking the method name.
// @rationale The method name is internal protocol detail; a caller can neither fix it nor act on it,
// and echoing it invites fingerprinting of the agent build.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestOpenAIWriteTurnError_MethodNotFoundIsOpaque500(t *testing.T) {
	h := &OpenAIHandlers{logger: slog.Default()}
	rec := httptest.NewRecorder()
	h.writeTurnError(rec, acpErr(acp.ErrCodeMethodNotFound, "Method not found: sessions/teleport"), time.Second)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unsupported method must be 500, got %d", rec.Code)
	}
	message, errType, errCode, _ := decodeOpenAIError(t, rec)
	if errType != "api_error" || errCode != "inference_error" {
		t.Fatalf("unexpected error type/code: %q/%q", errType, errCode)
	}
	if strings.Contains(message, "teleport") || strings.Contains(message, "session/") {
		t.Fatalf("response leaks the ACP method name: %q", message)
	}
}

// TestAnthropicWriteTurnError_MapsACPShapes pins the same two mappings on the Anthropic surface.
//
// @purpose Proves both provider surfaces agree, since they are separate code paths.
// @rationale Testing only OpenAI would leave the Anthropic branch free to regress silently.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestAnthropicWriteTurnError_MapsACPShapes(t *testing.T) {
	h := &AnthropicHandlers{logger: slog.Default()}

	rec := httptest.NewRecorder()
	h.writeAnthropicTurnError(rec, acpErr(acp.ErrCodeInvalidParams, "Unknown config option: thought_level"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid params must be 400 on Anthropic, got %d", rec.Code)
	}
	if errType, _ := decodeAnthropicError(t, rec); errType != "invalid_request_error" {
		t.Fatalf("Anthropic invalid params type = %q", errType)
	}

	rec2 := httptest.NewRecorder()
	h.writeAnthropicTurnError(rec2, acpErr(acp.ErrCodeMethodNotFound, "Method not found: sessions/teleport"))
	if rec2.Code != http.StatusInternalServerError {
		t.Fatalf("unsupported method must be 500 on Anthropic, got %d", rec2.Code)
	}
	errType, message := decodeAnthropicError(t, rec2)
	if errType != "api_error" {
		t.Fatalf("unsupported method type = %q, want api_error", errType)
	}
	if strings.Contains(message, "teleport") {
		t.Fatalf("response leaks the ACP method name: %q", message)
	}
}

// TestWriteTurnError_NoAccountsIs429OnBothSurfaces pins the onboarding contract.
//
// @purpose Proves the empty-vault state still answers the documented 429 `no_accounts`.
// @rationale Harnesses key their retry logic off this exact code; the refactor must not drift it.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestWriteTurnError_NoAccountsIs429OnBothSurfaces(t *testing.T) {
	openaiH := &OpenAIHandlers{logger: slog.Default()}
	rec := httptest.NewRecorder()
	openaiH.writeTurnError(rec, pool.ErrNoAccounts, time.Second)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("no accounts must be 429, got %d", rec.Code)
	}
	if _, _, errCode, _ := decodeOpenAIError(t, rec); errCode != "no_accounts" {
		t.Fatalf("code = %q, want no_accounts", errCode)
	}

	anthropicH := &AnthropicHandlers{logger: slog.Default()}
	rec2 := httptest.NewRecorder()
	anthropicH.writeAnthropicTurnError(rec2, fmt.Errorf("wrapped: %w", pool.ErrNoAccounts))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("no accounts must be 429 on Anthropic, got %d", rec2.Code)
	}
	if errType, _ := decodeAnthropicError(t, rec2); errType != "rate_limit_error" {
		t.Fatalf("Anthropic no-accounts type = %q, want rate_limit_error", errType)
	}
}

// TestWriteTurnError_AcpErrorsMatchThroughWrapping pins errors.As traversal.
//
// @purpose Proves a wrapped ACP error is still recognised by code.
// @rationale The turn pipeline wraps errors at several layers; a mapping that only matched the bare
// type would silently fall back to a 500 for every real failure.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestWriteTurnError_AcpErrorsMatchThroughWrapping(t *testing.T) {
	h := &OpenAIHandlers{logger: slog.Default()}
	wrapped := fmt.Errorf("turn failed: %w", acpErr(acp.ErrCodeInvalidParams, "Unknown config option: thought_level"))
	rec := httptest.NewRecorder()
	h.writeTurnError(rec, wrapped, time.Second)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a wrapped -32602 must still map to 400, got %d", rec.Code)
	}
}

// TestWriteTurnError_UnknownErrorNeverLeaksInternals pins the fallback.
//
// @purpose Proves an unrecognised error answers a generic 500 without its text.
// @rationale The fallback is where credential paths, argv, and stderr would otherwise escape.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestWriteTurnError_UnknownErrorNeverLeaksInternals(t *testing.T) {
	secretish := errors.New("failed to exec /var/lib/agy-gateway/workers/w0/bin/agy: exit status 9")
	h := &OpenAIHandlers{logger: slog.Default()}
	rec := httptest.NewRecorder()
	h.writeTurnError(rec, secretish, time.Second)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unknown error must be 500, got %d", rec.Code)
	}
	message, _, _, _ := decodeOpenAIError(t, rec)
	if strings.Contains(message, "workers/w0") || strings.Contains(message, "exit status 9") {
		t.Fatalf("fallback leaked internal detail: %q", message)
	}
}
