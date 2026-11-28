package server

import (
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agy-gateway/internal/retry"
)

// quotaRefusalMarkers are the leading phrases agy uses when it reports an exhausted account as a
// normal assistant message rather than an error.
//
// @purpose Detects an account-level quota refusal that arrives as ordinary assistant text.
// @rationale Measured on a live container: an exhausted account answers a prompt with a plain
// `agent_message_chunk` of `"\n⚠ Individual quota reached. Please upgrade your subscription to
// increase your limits. Resets in 160h23m9s.\n"` and `stopReason: "end_turn"`. Because that is a
// successful turn with text, the gateway forwarded it verbatim and never failed over — the end user
// saw a subscription upsell instead of being served by another account that still had quota.
// @preconditions None.
// @returns Immutable data.
// @invariants Lowercase; every entry begins the refused response.
var quotaRefusalMarkers = []string{
	"individual quota reached",
	"please upgrade your subscription",
	"resource_exhausted",
}

// quotaRefusalGate holds back assistant text until it is provably not a quota refusal.
//
// @purpose Stops an exhausted-account message from reaching the client, and reports it as a failure so
// failover can rotate to an account that still has quota.
// @rationale SSE cannot retract a chunk. Checking after the turn is too late, and buffering the whole
// turn would add latency to every request. This holds text only while it is still a strict prefix of a
// known marker, so ordinary responses (whose first character diverges immediately) pass through with no
// added delay, while the refusal — which starts with the marker — is withheld.
// @preconditions Use through Feed/Matched/Flush only.
// @returns Zero value is ready to use.
// @invariants Safe for concurrent use; buffers at most maxQuotaMarkerLen-1 bytes.
type quotaRefusalGate struct {
	mu      sync.Mutex
	pending string
	// seen accumulates every suppressed byte once a marker matched, so the upstream's own
	// "Resets in …" window stays available even when it arrives in a later chunk.
	seen    strings.Builder
	matched bool
}

// maxQuotaMarkerLen is the longest marker, used to bound how much text can be held back.
var maxQuotaMarkerLen = func() int {
	longest := 0
	for _, m := range quotaRefusalMarkers {
		if len(m) > longest {
			longest = len(m)
		}
	}
	return longest
}()

// normaliseQuotaText lowercases and trims the decoration agy wraps the message in.
//
// @purpose Makes the prefix comparison independent of the "⚠ " glyph and surrounding newlines.
// @rationale The measured payload is `"\n⚠ Individual quota reached. …"`; matching on the raw bytes
// would tie the detector to cosmetic formatting.
// @preconditions None.
// @param s Raw assistant text.
// @returns Lowercase text with leading whitespace and non-letter decoration removed.
// @throws None.
// @invariants O(len(s)).
func normaliseQuotaText(s string) string {
	return strings.TrimLeftFunc(strings.ToLower(s), func(r rune) bool {
		return r == '\n' || r == '\r' || r == '\t' || r == ' ' || r == '\u26a0' || r == '\u2022' || r == '-'
	})
}

// Feed pushes assistant text and returns what may be emitted to the client.
//
// @purpose Withholds only the bytes that could still turn out to be a quota refusal.
// @rationale Once a marker matches, everything is suppressed because the whole response is the
// refusal; before that, the buffer is released the moment the text diverges from every marker.
// @preconditions None.
// @param chunk Newly received assistant text.
// @returns Emittable text (empty while held back or suppressed).
// @throws None.
// @invariants O(len(chunk) * len(markers)); holds back at most maxQuotaMarkerLen-1 bytes.
func (g *quotaRefusalGate) Feed(chunk string) string {
	if chunk == "" {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.matched {
		// The account already refused this turn. Nothing further belongs to the client, but the text is
		// retained so the reset window stated at the end of the message is not lost.
		g.seen.WriteString(chunk)
		return ""
	}

	buf := g.pending + chunk
	norm := normaliseQuotaText(buf)

	for _, marker := range quotaRefusalMarkers {
		if strings.HasPrefix(norm, marker) {
			g.matched = true
			g.pending = ""
			g.seen.WriteString(buf)
			return ""
		}
	}

	// Still a prefix of some marker and not yet longer than the longest one: keep holding.
	if len(norm) < maxQuotaMarkerLen {
		prefixOfSome := false
		for _, marker := range quotaRefusalMarkers {
			if strings.HasPrefix(marker, norm) {
				prefixOfSome = true
				break
			}
		}
		if prefixOfSome {
			g.pending = buf
			return ""
		}
	}

	// The text can no longer become a refusal. Release everything held, on a rune boundary so the
	// client never receives invalid UTF-8.
	g.pending = ""
	cut := len(buf)
	for cut > 0 && !utf8.ValidString(buf[:cut]) {
		cut--
	}
	if cut == 0 {
		// A partial rune at the very start: retain it rather than splitting it.
		g.pending = buf
		return ""
	}
	g.pending = buf[cut:]
	return buf[:cut]
}

// Matched reports whether the response was a quota refusal.
//
// @purpose Tells the turn executor to fail the attempt instead of reporting a successful empty turn.
// @returns true once a marker was observed.
// @throws None.
// @invariants O(1).
func (g *quotaRefusalGate) Matched() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.matched
}

// Text returns the suppressed refusal message.
//
// @purpose Lets the caller recover the upstream's own "Resets in …" window.
// @rationale The reset duration is what the account should be cooled for; guessing a configured
// default would either retry an account that is out for days or keep a recovered one benched.
// @returns The accumulated text, or empty when the turn was not a refusal.
// @throws None.
// @invariants O(1) read; does not clear the buffer.
func (g *quotaRefusalGate) Text() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.seen.String()
}

// Flush releases any text still held back at end of turn.
//
// @purpose Prevents a short legitimate answer that happened to be a marker prefix from being dropped.
// @rationale The gate cannot know it is at end of turn; the caller does.
// @returns Held-back text, or empty when the response was a refusal.
// @throws None.
// @invariants O(1).
func (g *quotaRefusalGate) Flush() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.matched {
		return ""
	}
	out := g.pending
	g.pending = ""
	return out
}

// resetDurationPattern matches the "Resets in 160h23m9s" suffix agy appends to a quota refusal.
//
// @purpose Recovers the real cooldown instead of guessing the configured default.
// @rationale The upstream refusal states exactly when capacity returns; using it keeps the account
// out of rotation for the true window and never longer.
const resetDurationPrefix = "resets in "

// parseResetDuration extracts the reset window from a quota refusal message.
//
// @purpose Turns the upstream's own reset statement into the cooldown window.
// @rationale Cooling for a configured default (30m) would re-try an account that is out for six days,
// burning a request each time; cooling for the stated window is both cheaper and truthful.
// @preconditions None.
// @param text The refusal message.
// @returns The parsed duration and true when present and valid, otherwise (0, false).
// @throws None.
// @invariants O(len(text)).
func parseResetDuration(text string) (time.Duration, bool) {
	lower := strings.ToLower(text)
	idx := strings.Index(lower, resetDurationPrefix)
	if idx < 0 {
		return 0, false
	}
	rest := lower[idx+len(resetDurationPrefix):]
	// The duration ends at the first character that cannot belong to one: agy writes it as the last
	// token, optionally followed by punctuation.
	end := len(rest)
	for i, r := range rest {
		if !(r >= '0' && r <= '9') && r != 'h' && r != 'm' && r != 's' && r != '.' {
			end = i
			break
		}
	}
	candidate := strings.Trim(strings.TrimSpace(rest[:end]), ".")
	if candidate == "" {
		return 0, false
	}
	d, err := time.ParseDuration(candidate)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// quotaRefusalError is the typed failure raised when an account refuses a turn for want of quota.
//
// @purpose Carries the upstream's own reset window to the failover coordinator.
// @rationale Classification only needs the sentinel, but the cooldown needs the duration; one type
// carries both so the message never has to be re-parsed downstream.
// @preconditions Produced only by the turn executor.
// @returns An error matching errors.Is(err, retry.ErrQuotaRefusal).
// @invariants Immutable after construction.
type quotaRefusalError struct {
	message  string
	reset    time.Duration
	hasReset bool
}

// Error implements error.
func (e *quotaRefusalError) Error() string { return e.message }

// Unwrap exposes the sentinel so classification and tests can match without string comparison.
func (e *quotaRefusalError) Unwrap() error { return retry.ErrQuotaRefusal }

// ResetWindow reports the cooldown the upstream asked for, when it stated one.
//
// @purpose Lets failover cool the account for exactly as long as it is actually out.
// @returns The duration and whether it was present.
// @invariants Pure read.
func (e *quotaRefusalError) ResetWindow() (time.Duration, bool) { return e.reset, e.hasReset }
