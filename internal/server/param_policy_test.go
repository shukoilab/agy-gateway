package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agy-gateway/internal/config"
	"agy-gateway/internal/openai"
)

// TestIgnoredOpenAIParams_EnumeratesUnrepresentableFields pins the enumeration.
//
// @purpose Proves the header names exactly the fields ACP cannot express, and nothing else.
// @rationale Listing an honoured field would make a correct request look degraded; omitting an
// ignored one restores the silent-lie failure mode this policy exists to remove.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Pure.
func TestIgnoredOpenAIParams_EnumeratesUnrepresentableFields(t *testing.T) {
	temp := 0.7
	topP := 0.9
	maxTokens := 100
	seed := int64(7)
	logprobs := true
	topLogprobs := 3
	parallel := true

	req := &openai.ChatCompletionRequest{
		Model:             "gemini-3.8-flash",
		Temperature:       &temp,
		TopP:              &topP,
		MaxTokens:         &maxTokens,
		Seed:              &seed,
		Logprobs:          &logprobs,
		TopLogprobs:       &topLogprobs,
		ParallelToolCalls: &parallel,
		LogitBias:         map[string]int{"1": 2},
		ReasoningEffort:   "high",
		Stop:              []any{"END"},
	}

	got := strings.Join(ignoredOpenAIParams(req), ",")
	want := "temperature,top_p,max_tokens,seed,logit_bias,logprobs,top_logprobs,parallel_tool_calls"
	if got != want {
		t.Fatalf("ignored params = %q, want %q", got, want)
	}

	// Honoured fields must never appear: model/messages/tools/stream/reasoning_effort/stop.
	for _, honoured := range []string{"model", "messages", "tools", "stream", "reasoning_effort", "stop"} {
		if strings.Contains(got, honoured) {
			t.Fatalf("%q is honoured but was reported as ignored (got %q)", honoured, got)
		}
	}
}

// TestIgnoredOpenAIParams_EmptyWhenNothingUnsupported pins the clean request.
//
// @purpose Proves a minimal request produces no header at all.
// @rationale Emitting an empty header would make every request look degraded and would break
// harnesses that treat header presence as a warning.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Pure.
func TestIgnoredOpenAIParams_EmptyWhenNothingUnsupported(t *testing.T) {
	if got := ignoredOpenAIParams(&openai.ChatCompletionRequest{Model: "m"}); len(got) != 0 {
		t.Fatalf("a request with no unsupported field must report none, got %v", got)
	}
	if got := ignoredOpenAIParams(nil); len(got) != 0 {
		t.Fatalf("nil request must report none, got %v", got)
	}
}

// TestIgnoredAnthropicParams_OmitsMandatoryMaxTokens pins the schema constraint.
//
// @purpose Proves `max_tokens` is not reported despite ACP having no equivalent.
// @rationale It is mandatory in the Anthropic schema; reporting it would make the `reject` policy
// fail every syntactically valid request, so it can only ever be advisory.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Pure.
func TestIgnoredAnthropicParams_OmitsMandatoryMaxTokens(t *testing.T) {
	temp := 0.2
	topK := 40
	view := &anthropicRequestView{
		Temperature:   &temp,
		TopK:          &topK,
		StopSequences: []string{"END"},
	}
	got := strings.Join(ignoredAnthropicParams(view), ",")
	if got != "temperature,top_k" {
		t.Fatalf("ignored params = %q, want temperature,top_k", got)
	}
}

// TestAnnotateIgnoredParams_WritesHeader pins the reporting surface.
//
// @purpose Proves the header is set with the exact names.
// @rationale The header is the only machine-readable contract for this behaviour.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestAnnotateIgnoredParams_WritesHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	joined := annotateIgnoredParams(rec, []string{"temperature", "top_p"})
	if joined != "temperature,top_p" {
		t.Fatalf("joined = %q", joined)
	}
	if got := rec.Header().Get(IgnoredParamsHeader); got != "temperature,top_p" {
		t.Fatalf("%s = %q", IgnoredParamsHeader, got)
	}

	clean := httptest.NewRecorder()
	if v := annotateIgnoredParams(clean, nil); v != "" {
		t.Fatalf("empty list must return \"\", got %q", v)
	}
	if got := clean.Header().Get(IgnoredParamsHeader); got != "" {
		t.Fatalf("empty list must not set the header, got %q", got)
	}
}

// TestRejectUnsupportedParams_RespectsPolicy pins both policy branches.
//
// @purpose Proves `ignore` never refuses and `reject` refuses exactly once with the provider shape.
// @rationale A policy that refuses in `ignore` mode would break every ordinary OpenAI client; one
// that accepts in `reject` mode would silently ignore a deliberate operator decision.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestRejectUnsupportedParams_RespectsPolicy(t *testing.T) {
	params := []string{"temperature"}

	ignoreCfg := config.DefaultConfig()
	ignoreCfg.Execution.UnsupportedParameterPolicy = config.UnsupportedParameterPolicyIgnore
	called := false
	write := func(int, string, string, string, string) { called = true }
	if rejectUnsupportedParams(ignoreCfg, params, write) {
		t.Fatal("ignore policy must not refuse the request")
	}
	if called {
		t.Fatal("ignore policy must not write an error")
	}

	rejectCfg := config.DefaultConfig()
	rejectCfg.Execution.UnsupportedParameterPolicy = config.UnsupportedParameterPolicyReject
	var gotStatus int
	var gotParam, gotCode, gotType string
	rejectWrite := func(status int, _ string, errType, errCode, param string) {
		gotStatus, gotType, gotCode, gotParam = status, errType, errCode, param
	}
	if !rejectUnsupportedParams(rejectCfg, params, rejectWrite) {
		t.Fatal("reject policy must refuse the request")
	}
	if gotStatus != http.StatusBadRequest || gotParam != "temperature" ||
		gotCode != "unsupported_parameter" || gotType != "invalid_request_error" {
		t.Fatalf("unexpected error shape: status=%d type=%q code=%q param=%q",
			gotStatus, gotType, gotCode, gotParam)
	}

	// No unsupported params: neither policy may refuse.
	if rejectUnsupportedParams(rejectCfg, nil, rejectWrite) {
		t.Fatal("a clean request must never be refused")
	}
}
