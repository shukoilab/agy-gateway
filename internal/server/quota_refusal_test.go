package server

import (
	"strings"
	"testing"
	"time"

	"agy-gateway/internal/retry"
)

// quotaRefusalSample is the exact assistant text measured from an exhausted account on the live
// container. Kept verbatim (including the warning glyph and newlines) so the detector is proven
// against the real payload rather than a cleaned-up approximation.
const quotaRefusalSample = "\n\u26a0 Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 160h23m9s.\n"

// TestQuotaRefusalGate_SuppressesRealRefusal pins the live-observed defect.
//
// @purpose Proves an exhausted-account message never reaches the client.
// @rationale On the live container the refusal arrived as an ordinary assistant message with
// `stopReason: end_turn`, so the gateway forwarded it verbatim to opencode and the end user saw a
// subscription upsell instead of being served by the account that still had quota.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestQuotaRefusalGate_SuppressesRealRefusal(t *testing.T) {
	g := &quotaRefusalGate{}
	if out := g.Feed(quotaRefusalSample); out != "" {
		t.Fatalf("refusal must not be emitted, got %q", out)
	}
	if !g.Matched() {
		t.Fatal("the measured refusal message must be recognised")
	}
	if out := g.Flush(); out != "" {
		t.Fatalf("nothing may be released at end of turn, got %q", out)
	}
	if txt := g.Text(); !strings.Contains(txt, "Individual quota reached") {
		t.Fatalf("the suppressed text must be retained for the reset window, got %q", txt)
	}
}

// TestQuotaRefusalGate_RefusalSplitAcrossChunks pins streaming resilience.
//
// @purpose Proves the detector works when the message arrives in arbitrary fragments.
// @rationale SSE chunk boundaries are chosen by the agent, not the gateway; a detector that only
// worked on a single chunk would silently regress.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestQuotaRefusalGate_RefusalSplitAcrossChunks(t *testing.T) {
	// Slice by RUNE, not byte: SSE chunks are always valid UTF-8, and splitting the warning glyph
	// would test byte-fragment handling rather than streaming.
	runes := []rune(quotaRefusalSample)
	for _, size := range []int{1, 3, 7, 20, 64} {
		g := &quotaRefusalGate{}
		var emitted strings.Builder
		for i := 0; i < len(runes); i += size {
			end := i + size
			if end > len(runes) {
				end = len(runes)
			}
			emitted.WriteString(g.Feed(string(runes[i:end])))
		}
		emitted.WriteString(g.Flush())
		if emitted.String() != "" {
			t.Fatalf("chunk size %d leaked %q", size, emitted.String())
		}
		if !g.Matched() {
			t.Fatalf("chunk size %d failed to detect the refusal", size)
		}
	}
}

// TestQuotaRefusalGate_PassesOrdinaryTextThrough pins zero added latency for normal answers.
//
// @purpose Proves a real answer is not delayed or dropped by the refusal detector.
// @rationale The gate holds text only while it is still a strict prefix of a marker; anything whose
// first character diverges must be released immediately or every response would buffer.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestQuotaRefusalGate_PassesOrdinaryTextThrough(t *testing.T) {
	cases := []string{
		"PONG",
		"Here is the fix you asked for.\n",
		"// Individual quota handling lives here\n",
		"1. Read the file\n",
		"I",
		"Ind",
	}
	for _, in := range cases {
		g := &quotaRefusalGate{}
		out := g.Feed(in) + g.Flush()
		if out != in {
			t.Fatalf("%q must pass through unchanged, got %q", in, out)
		}
		if g.Matched() {
			t.Fatalf("%q must not be treated as a refusal", in)
		}
	}
}

// TestQuotaRefusalGate_ShortFinalAnswerIsNotSwallowed pins the end-of-turn release.
//
// @purpose Proves a legitimate answer that happens to be a marker prefix is still delivered.
// @rationale "Ind" is a prefix of "individual quota reached"; without Flush it would be lost.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestQuotaRefusalGate_ShortFinalAnswerIsNotSwallowed(t *testing.T) {
	g := &quotaRefusalGate{}
	held := g.Feed("Ind")
	if held != "" {
		t.Fatalf("a live prefix must be held, not emitted yet, got %q", held)
	}
	if out := g.Flush(); out != "Ind" {
		t.Fatalf("end of turn must release the held prefix, got %q", out)
	}
	if g.Matched() {
		t.Fatal("a prefix is not a refusal")
	}
}

// TestParseResetDuration pins the cooldown derivation.
//
// @purpose Proves the account is cooled for exactly the window the upstream stated.
// @rationale The live refusal said "Resets in 160h23m9s"; cooling for the 30-minute default would
// retry an account that is out for six days.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestParseResetDuration(t *testing.T) {
	d, ok := parseResetDuration(quotaRefusalSample)
	if !ok {
		t.Fatal("the reset window must be recovered from the real message")
	}
	if want := 160*time.Hour + 23*time.Minute + 9*time.Second; d != want {
		t.Fatalf("reset = %v, want %v", d, want)
	}

	if _, ok := parseResetDuration("Individual quota reached."); ok {
		t.Fatal("a message without a reset window must report none")
	}
	if _, ok := parseResetDuration("Resets in soon"); ok {
		t.Fatal("an unparseable window must be rejected, not guessed")
	}
}

// TestQuotaRefusalError_ClassifiesAsQuotaExhausted pins the failover trigger.
//
// @purpose Proves the refusal reaches the rotation logic as a retryable quota failure.
// @rationale The message is human prose; without the sentinel it would fall through to
// NON_RETRYABLE and end the request instead of rotating.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestQuotaRefusalError_ClassifiesAsQuotaExhausted(t *testing.T) {
	err := error(&quotaRefusalError{message: "Individual quota reached."})
	if got := classifyForTest(err); got != "QUOTA_EXHAUSTED" {
		t.Fatalf("classification = %s, want QUOTA_EXHAUSTED", got)
	}
}

// classifyForTest exposes retry classification to this package's tests.
//
// @purpose Keeps the assertion on the production classifier instead of a local imitation.
// @rationale A duplicated rule would pass while the real one regressed.
// @preconditions err may be nil.
// @param err Error to classify.
// @returns The canonical class name.
// @throws None.
// @invariants O(1).
func classifyForTest(err error) string { return retry.ClassifyFailure(err).String() }
