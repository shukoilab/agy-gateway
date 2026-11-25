package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"agy-gateway/internal/anthropic"
	"agy-gateway/internal/config"
	"agy-gateway/internal/openai"
)

// TestSuppliedUnsupportedParams_OpenAI proves FR-14 observes exactly the fields the caller supplied.
//
// @purpose Guards against silent parameter drops on the OpenAI surface.
// @rationale A field accepted then ignored is a correctness bug invisible to the caller.
// @preconditions None.
// @param t Go test handle.
// @returns None.
// @throws None.
// @invariants Deterministic; no I/O.
func TestSuppliedUnsupportedParams_OpenAI(t *testing.T) {
	temp := 0.5
	req := &openai.ChatCompletionRequest{
		Model:       "gemini-3.8-flash",
		Messages:    []openai.ChatMessage{{Role: "user", Content: "hi"}},
		Temperature: &temp,
		User:        "consumer",
	}
	got := suppliedOpenAIUnsupportedParams(req)
	want := map[string]bool{"temperature": true, "user": true}
	if len(got) != len(want) {
		t.Fatalf("expected %d fields, got %v", len(want), got)
	}
	for _, f := range got {
		if !want[f] {
			t.Fatalf("unexpected field %q", f)
		}
	}
	if got[0] != "temperature" || got[1] != "user" {
		t.Fatalf("expected sorted order [temperature user], got %v", got)
	}

	if fields := suppliedOpenAIUnsupportedParams(&openai.ChatCompletionRequest{}); len(fields) != 0 {
		t.Fatalf("expected no fields for an empty request, got %v", fields)
	}
}

// TestApplyUnsupportedParamPolicy covers both policies and the response header contract.
//
// @purpose Proves FR-14 is observable: either a typed rejection or a header.
// @rationale Silent acceptance is the defect this FR exists to remove.
// @preconditions None.
// @param t Go test handle.
// @returns None.
// @throws None.
// @invariants Deterministic.
func TestApplyUnsupportedParamPolicy(t *testing.T) {
	fields := []string{"temperature", "max_tokens", "not_a_real_field"}

	rec := httptest.NewRecorder()
	remaining, err := applyUnsupportedParamPolicy(fields, "strip_and_warn", rec)
	if err != nil {
		t.Fatalf("strip_and_warn must not fail, got %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("expected the two known fields reported, got %v", remaining)
	}
	hdr := rec.Header().Get("X-Agy-Gateway-Ignored-Params")
	if hdr != "max_tokens,temperature" {
		t.Fatalf("expected sorted header 'max_tokens,temperature', got %q", hdr)
	}

	rec2 := httptest.NewRecorder()
	if _, err := applyUnsupportedParamPolicy(fields, "reject", rec2); err == nil {
		t.Fatal("reject policy must return an error")
	} else if field, ok := IsUnsupportedParam(err); !ok || field != "max_tokens" {
		t.Fatalf("expected IsUnsupportedParam(alpha), got %q %v", field, ok)
	}

	rec3 := httptest.NewRecorder()
	if _, err := applyUnsupportedParamPolicy(nil, "reject", rec3); err != nil {
		t.Fatalf("empty field list must never fail, got %v", err)
	}
	if rec3.Header().Get("X-Agy-Gateway-Ignored-Params") != "" {
		t.Fatal("no header should be written when nothing was ignored")
	}
}

// TestVisionDetection proves image payloads are detected on both provider surfaces.
//
// @purpose Guards FR-07: agy advertises image support and then fails with INVALID_ARGUMENT.
// @rationale Forwarding an image makes the model answer about content it never received.
// @preconditions None.
// @param t Go test handle.
// @returns None.
// @throws None.
// @invariants Deterministic.
func TestVisionDetection(t *testing.T) {
	oa := &openai.ChatCompletionRequest{
		Messages: []openai.ChatMessage{
			{Role: "user", Content: "describe this"},
			{Role: "user", Content: []any{map[string]any{"type": "image_url"}}},
		},
	}
	if where := openAIVisionRequest(oa); where == "" {
		t.Fatal("expected image to be detected on the OpenAI surface")
	}
	if where := openAIVisionRequest(&openai.ChatCompletionRequest{
		Messages: []openai.ChatMessage{{Role: "user", Content: "text only"}},
	}); where != "" {
		t.Fatalf("expected no image detection for text-only input, got %q", where)
	}

	an := &anthropic.MessageRequest{
		Messages: []anthropic.Message{{
			Role: "user",
			Content: anthropic.MessageContent{
				Blocks: []anthropic.ContentBlock{{Type: "image"}},
			},
		}},
	}
	if where := anthropicVisionRequest(an); where == "" {
		t.Fatal("expected image to be detected on the Anthropic surface")
	}
}

// TestVisionModeRejects proves the default policy is reject, not flatten.
//
// @purpose Pins the documented default (FR-07).
// @rationale Flattening makes the model hallucinate a description of an image it never saw.
// @preconditions None.
// @param t Go test handle.
// @returns None.
// @throws None.
// @invariants Deterministic.
func TestVisionModeRejects(t *testing.T) {
	cfg := config.DefaultConfig()
	if !visionModeRejects(cfg) {
		t.Fatal("default vision_mode must reject images")
	}
	cfg.Execution.VisionMode = "flatten"
	if visionModeRejects(cfg) {
		t.Fatal("vision_mode=flatten must not reject")
	}
	cfg.Execution.VisionMode = "REJECT"
	if !visionModeRejects(cfg) {
		t.Fatal("vision_mode must be matched case-insensitively")
	}
}

// TestMapStopReason covers the full FR-13 mapping table.
//
// @purpose Proves ACP stop reasons reach the providers with the correct vocabulary.
// @rationale Today max_tokens and refusal are indistinguishable from a clean stop.
// @preconditions None.
// @param t Go test handle.
// @returns None.
// @throws None.
// @invariants Deterministic.
func TestMapStopReason(t *testing.T) {
	cases := []struct{ acp, oa, an string }{
		{"end_turn", "stop", "end_turn"},
		{"max_tokens", "length", "max_tokens"},
		{"max_turn_requests", "length", "max_tokens"},
		{"refusal", "content_filter", "refusal"},
		{"cancelled", "stop", "end_turn"},
		{"error", "stop", "end_turn"},
		{"", "stop", "end_turn"},
		{"vendor_unknown", "stop", "end_turn"},
	}
	for _, c := range cases {
		oa, an := MapStopReason(c.acp)
		if oa != c.oa || an != c.an {
			t.Fatalf("%q: expected %q/%q, got %q/%q", c.acp, c.oa, c.an, oa, an)
		}
	}
}

// TestStopSequenceMatcher covers chunk-boundary safety (EC-27) and passthrough.
//
// @purpose Proves stop sequences truncate exactly once, even when split across chunks.
// @rationale A naive strings.Index would miss a sequence spanning two SSE chunks.
// @preconditions None.
// @param t Go test handle.
// @returns None.
// @throws None.
// @invariants Deterministic.
func TestStopSequenceMatcher(t *testing.T) {
	m := NewStopSequenceMatcher([]string{"STOP", "END"})
	if !m.Active() {
		t.Fatal("matcher with sequences must be active")
	}

	var out string
	for _, chunk := range []string{"hello ST", "OP world"} {
		emit, _ := m.Feed(chunk)
		out += emit
	}
	if out != "hello " {
		t.Fatalf("expected truncated %q, got %q", "hello ", out)
	}
	if !m.Stopped() || m.MatchedSequence() != "STOP" {
		t.Fatalf("expected STOP match, got stopped=%v seq=%q", m.Stopped(), m.MatchedSequence())
	}
	if emit, _ := m.Feed("more text"); emit != "" {
		t.Fatalf("text after the stop sequence must be suppressed, got %q", emit)
	}
	if flush := m.Flush(); flush != "" {
		t.Fatalf("flush after stop must emit nothing, got %q", flush)
	}

	idle := NewStopSequenceMatcher(nil)
	if idle.Active() {
		t.Fatal("matcher without sequences must be inactive")
	}
	if emit, _ := idle.Feed("everything"); emit != "everything" {
		t.Fatalf("inactive matcher must pass text through, got %q", emit)
	}
}

// TestToolCollectorCrossOriginDedupe pins the invariant that one logical invocation reported on both
// transports (native session/update and loopback tools/call) is emitted to the client exactly once.
//
// @purpose Guards against the duplicate-tool-execution defect observed live against agy 1.5.16.
// @rationale agy reports the same call twice with unrelated ids; deduplication must not rely on id alone.
// @preconditions None.
// @param None.
// @returns None.
// @throws None.
// @invariants Pure, single-goroutine.
func TestToolCollectorCrossOriginDedupe(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
	c := NewToolCollector(&config.Config{}, nil)
	c.maxCalls = 8

	args := json.RawMessage(`{"city":"Jakarta","toolSummary":"Get weather"}`)

	first, added := c.AddFrom("call_native_1", "get_weather", args, schema, originNativeUpdate)
	if !added {
		t.Fatal("first observation must be collected")
	}
	if got := string(first.Arguments); got != `{"city":"Jakarta"}` {
		t.Fatalf("vendor key must be stripped, got %s", got)
	}

	if _, dup := c.AddFrom("call_loopback_9", "get_weather", args, schema, originLoopbackCall); dup {
		t.Fatal("the same invocation arriving from the other transport must be deduplicated")
	}

	// The same origin reporting the same signature twice is a genuine repeat (e.g. a real second call
	// with identical arguments) and must be preserved.
	if _, repeat := c.AddFrom("call_native_2", "get_weather", args, schema, originNativeUpdate); !repeat {
		t.Fatal("same-origin repeat with a new id must be collected")
	}

	if _, other := c.AddFrom("call_native_3", "get_weather", json.RawMessage(`{"city":"Tokyo"}`), schema, originNativeUpdate); !other {
		t.Fatal("different arguments must never be deduplicated")
	}

	if n := c.Len(); n != 3 {
		t.Fatalf("expected 3 collected calls, got %d", n)
	}
}

// @purpose Pins the surplus-counting dedupe against the interleaving that broke the previous
// last-writer-wins slot, and against an unpaired observation.
// @rationale A single signature slot dropped a distinct call whenever the two transports did not
// report their duplicates adjacently; dropping a real tool call is worse than emitting a duplicate.
// @preconditions None.
// @param None.
// @returns None.
// @throws None.
// @invariants Pure, single-goroutine.
func TestToolCollectorCrossOriginDedupeInterleaved(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
	c := NewToolCollector(&config.Config{}, nil)
	c.maxCalls = 8

	args := json.RawMessage(`{"city":"Jakarta"}`)

	// Reversed interleaving: loopback reports first, native reports second, both times.
	if _, added := c.AddFrom("loop_1", "get_weather", args, schema, originLoopbackCall); !added {
		t.Fatal("loopback observation #1 must be collected")
	}
	if _, added := c.AddFrom("native_1", "get_weather", args, schema, originNativeUpdate); added {
		t.Fatal("native #1 must be deduplicated against the pending loopback surplus")
	}
	if _, added := c.AddFrom("native_2", "get_weather", args, schema, originNativeUpdate); !added {
		t.Fatal("native #2 is a distinct invocation and must be collected")
	}
	if _, added := c.AddFrom("loop_2", "get_weather", args, schema, originLoopbackCall); added {
		t.Fatal("loopback #2 must be deduplicated against the pending native surplus")
	}
	if n := c.Len(); n != 2 {
		t.Fatalf("expected 2 collected calls, got %d", n)
	}

	// Unpaired observation: a call seen on one transport only must survive.
	c2 := NewToolCollector(&config.Config{}, nil)
	c2.maxCalls = 8
	if _, added := c2.AddFrom("solo_1", "get_weather", json.RawMessage(`{"city":"Tokyo"}`), schema, originNativeUpdate); !added {
		t.Fatal("unpaired observation must be collected")
	}
	if n := c2.Len(); n != 1 {
		t.Fatalf("expected 1 collected call, got %d", n)
	}
}

// @purpose Proves strict mode validates arguments against the declared inputSchema, not merely
// against "is a JSON object".
// @rationale The previous check accepted {"city":42} for a declared {"type":"string"} property,
// forwarding a tool call the consumer's own validator would reject.
// @preconditions None.
// @param None.
// @returns None.
// @throws None.
// @invariants Pure, single-goroutine.
func TestToolCollectorStrictSchemaValidation(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["city"],"properties":{"city":{"type":"string"},"units":{"type":"string","enum":["c","f"]},"days":{"type":"array","items":{"type":"integer"}}}}`)

	newStrict := func() *toolCollector {
		c := NewToolCollector(&config.Config{MCP: config.MCPConfig{ClientToolCalling: config.MCPClientToolCallingConfig{StrictSchemaValidation: true, MaxInterceptedToolCalls: 8}}}, nil)
		c.maxCalls = 8
		return c
	}

	valid := []string{
		`{"city":"Jakarta"}`,
		`{"city":"Jakarta","units":"c"}`,
		`{"city":"Jakarta","days":[1,2,3]}`,
		`{"city":"Jakarta","extra_undeclared_key":true}`,
	}
	for _, args := range valid {
		c := newStrict()
		if _, added := c.AddFrom("id", "get_weather", json.RawMessage(args), schema, originNativeUpdate); !added {
			t.Fatalf("expected %s to satisfy the schema", args)
		}
	}

	invalid := map[string]string{
		`{"city":42}`:                         "expected a string",
		`{}`:                                  "missing required argument city",
		`{"city":"Jakarta","units":"kelvin"}`: "allowed options",
		`{"city":"Jakarta","days":"monday"}`:  "expected an array",
		`{"city":"Jakarta","days":["x"]}`:     "array item",
		`not-json`:                            "not a JSON object",
		// encoding/json coerces these into the target type; a strict validator must not.
		`{"city":null}`:                   "expected a string",
		`{"city":5}`:                      "expected a string",
		`{"city":true}`:                   "expected a string",
		`{"city":"Jakarta","days":["5"]}`: "array item: expected a number",
		`{"city":"Jakarta","days":[1.5]}`: "array item: expected an integer",
	}
	for args, want := range invalid {
		c := newStrict()
		if _, added := c.AddFrom("id", "get_weather", json.RawMessage(args), schema, originNativeUpdate); added {
			t.Fatalf("expected %s to be rejected", args)
		}
		if n := c.Len(); n != 0 {
			t.Fatalf("rejected call must not be stored, got %d", n)
		}
		if reason := validateAgainstSchema(json.RawMessage(args), schema); !strings.Contains(reason, want) {
			t.Fatalf("reason for %s should mention %q, got %q", args, want, reason)
		}
	}

	// A schema-less tool must still accept any object: nothing declared, nothing to violate.
	c := newStrict()
	if _, added := c.AddFrom("id", "free_form", json.RawMessage(`{"anything":1}`), nil, originNativeUpdate); !added {
		t.Fatal("tool without a declared schema must accept any object")
	}
}

// TestValidateAgainstSchemaNestedRecursion proves strict validation descends into nested
// objects and array-item objects instead of stopping at "this is an object" (NEW-03).
//
// @purpose Pins the depth-2 guarantee: a nested schema's own required/properties are enforced.
// @rationale The first implementation checked only the top level, so `{"a":{}}` passed a schema
// declaring `a.required`, and `{"arr":[{}]}` passed an item schema declaring required fields.
// @preconditions None.
// @param t Testing handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no shared state between cases.
func TestValidateAgainstSchemaNestedRecursion(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["a"],"properties":{
		"a":{"type":"object","required":["b"],"properties":{"b":{"type":"string"}}},
		"arr":{"type":"array","items":{"type":"object","required":["id"],"properties":{"id":{"type":"integer"}}}}}}`)

	valid := []string{
		`{"a":{"b":"ok"}}`,
		`{"a":{"b":"ok"},"arr":[{"id":1},{"id":2}]}`,
		`{"a":{"b":"ok"},"arr":[]}`,
		`{"a":{"b":"ok"},"arr":[{"id":1,"extra":true}]}`,
	}
	for _, args := range valid {
		if reason := validateAgainstSchema(json.RawMessage(args), schema); reason != "" {
			t.Fatalf("expected %s to satisfy the nested schema, got %q", args, reason)
		}
	}

	invalid := map[string]string{
		`{"a":{}}`:                            "missing required argument b",
		`{"a":{"b":1}}`:                       "expected a string",
		`{"a":{"b":"ok"},"arr":[{}]}`:         "missing required argument id",
		`{"a":{"b":"ok"},"arr":[{"id":"x"}]}`: "expected a number",
	}
	for args, want := range invalid {
		reason := validateAgainstSchema(json.RawMessage(args), schema)
		if !strings.Contains(reason, want) {
			t.Fatalf("reason for %s should mention %q, got %q", args, want, reason)
		}
	}
}

// @purpose Proves stop-sequence buffering never emits a truncated multi-byte rune.
// @rationale The overlap tail was sliced by byte, which could split a rune and hand the client
// invalid UTF-8; JSON encoders then emit U+FFFD.
// @preconditions None.
// @param None.
// @returns None.
// @throws None.
// @invariants Pure, single-goroutine.
func TestStopSequenceMatcherPreservesRunes(t *testing.T) {
	matcher := NewStopSequenceMatcher([]string{"ZZ"}) // overlap = 1 byte
	if matcher == nil || !matcher.Active() {
		t.Fatal("expected an active matcher")
	}

	// "é" is two bytes; a one-byte overlap would split it.
	first, matched := matcher.Feed("café")
	if matched {
		t.Fatal("no stop sequence present")
	}
	if !utf8.ValidString(first + matcher.Flush()) {
		t.Fatalf("matcher produced invalid UTF-8: %q", first)
	}

	// Replay the whole stream through the matcher and confirm every emitted fragment is valid UTF-8.
	stream := "naïve résumé 日本語テキスト done"
	m := NewStopSequenceMatcher([]string{"ZZ"})
	var out strings.Builder
	for _, r := range stream {
		frag, _ := m.Feed(string(r))
		if !utf8.ValidString(frag) {
			t.Fatalf("invalid UTF-8 fragment at rune %q: %q", r, frag)
		}
		out.WriteString(frag)
	}
	out.WriteString(m.Flush())
	if got := out.String(); got != stream {
		t.Fatalf("buffering altered the stream: got %q want %q", got, stream)
	}
}
