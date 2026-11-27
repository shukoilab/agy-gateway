package acp

import (
	"strings"
	"testing"
)

// TestFitToolResultChars_PrefersCompactionOverDropping pins the sizing rule that keeps every turn.
//
// @purpose Proves the cap is derived from the real overhead so the whole transcript fits.
// @rationale Dropping turns is what made a long agentic session forget its own analysis and re-read
// the same files forever; the cap exists so compaction can absorb the excess instead.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic and allocation-free.
func TestFitToolResultChars_PrefersCompactionOverDropping(t *testing.T) {
	// 52 K of prose leaves ~188 K characters for 207 results at 60 K tokens.
	if got := FitToolResultChars(52_090, 207, 60_000); got < minToolResultChars || got > ToolResultCharsCap(60_000) {
		t.Fatalf("cap = %d, want within [%d, %d]", got, minToolResultChars, ToolResultCharsCap(60_000))
	}
	// Overhead alone exceeds the budget: no cap can help, the caller must fall back.
	if got := FitToolResultChars(400_000, 10, 60_000); got != 0 {
		t.Fatalf("impossible budget must yield 0, got %d", got)
	}
	if got := FitToolResultChars(1000, 10, 0); got != 0 {
		t.Fatalf("disabled budget must yield 0, got %d", got)
	}
	if got := FitToolResultChars(1000, 0, 60_000); got != ToolResultCharsCap(60_000) {
		t.Fatalf("no results to compact must yield the flat cap, got %d", got)
	}
	// Never exceed the flat per-result allowance even with a huge surplus.
	if got := FitToolResultChars(0, 1, 60_000); got != ToolResultCharsCap(60_000) {
		t.Fatalf("a single result must not exceed the flat cap, got %d", got)
	}
}

// TestSplitPromptChunks_KeepsToolResultsWithTheirCall pins the grouping that makes bounding safe.
//
// @purpose Proves a tool-results block is never a chunk of its own.
// @rationale A result delivered without the call that produced it (or the reverse) is what makes the
// model re-issue the call; only whole-turn chunks may be dropped.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestSplitPromptChunks_KeepsToolResultsWithTheirCall(t *testing.T) {
	prompt := strings.Join([]string{
		"User: OBJECTIVE fix the bug",
		"Assistant: [Assistant called tool read (id: c1) with arguments: {\"path\":\"a.ts\"}]",
		"User: [Tool Execution Results]\nTool Output (id: c1): line1\n\nline2\n\nline3",
		"Assistant: now write",
		"User: [Tool Execution Results]\nTool Output (id: c2): out",
	}, "\n\n")

	chunks := SplitPromptChunks(prompt)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3 (objective, call+result pair, latest turn): %+v", len(chunks), chunks)
	}
	if !chunks[0].Pinned {
		t.Fatal("the opening objective must be pinned")
	}
	if !strings.Contains(chunks[1].Text, "called tool read (id: c1)") ||
		!strings.Contains(chunks[1].Text, "Tool Output (id: c1)") {
		t.Fatalf("call and its result must share a chunk, got %q", chunks[1].Text)
	}
	// Blank lines inside tool output must not become a boundary.
	for _, c := range chunks[1:] {
		if strings.HasPrefix(c.Text, "line2") {
			t.Fatal("a blank line inside tool output was treated as a turn boundary")
		}
	}
}

// TestBoundChunks_NeverOrphansAToolPair pins the invariant across many budgets.
//
// @purpose Proves no budget can produce a result whose call was dropped, or the reverse.
// @rationale The previous character-offset cut did exactly that for 14 of 181 swept budgets, because
// tool output contains blank lines that looked like boundaries. Any orphaned half makes the model
// repeat the call instead of continuing, which is the observed loop.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; sweeps 60 budgets.
func TestBoundChunks_NeverOrphansAToolPair(t *testing.T) {
	var chunks []HistoryChunk
	chunks = append(chunks, HistoryChunk{Text: "User: OBJECTIVE ship the fix", Pinned: true})
	for i := 0; i < 60; i++ {
		id := "c" + strings.Repeat("x", 1+i%3) + string(rune('a'+i%26))
		body := strings.Repeat("payload line\n\n", 20) // blank lines inside the result body
		chunks = append(chunks, HistoryChunk{Text: "Assistant: [Assistant called tool read (id: " + id + ") with arguments: {}]\n\n" +
			"User: [Tool Execution Results]\nTool Output (id: " + id + "): " + body})
	}

	for budget := 500; budget <= 6000; budget += 92 {
		got := BoundChunks(chunks, budget)
		calls := countIDs(got, "[Assistant called tool read (id: ")
		results := countIDs(got, "Tool Output (id: ")
		if calls != results {
			t.Fatalf("budget %d: %d calls vs %d results — a tool pair was split", budget, calls, results)
		}
		if EstimatePromptTokens(got) > budget+EstimatePromptTokens(historyOmittedMarker)+2 {
			t.Fatalf("budget %d: bounded prompt is %d tokens", budget, EstimatePromptTokens(got))
		}
	}
}

// countIDs counts occurrences of a marker followed by an id, ignoring duplicates.
//
// @purpose Compares declared calls against delivered results in a rendered prompt.
// @param s Rendered prompt.
// @param marker Prefix that introduces the id.
// @returns Number of distinct ids found after marker.
// @invariants O(len(s)).
func countIDs(s, marker string) int {
	seen := make(map[string]struct{})
	for {
		idx := strings.Index(s, marker)
		if idx < 0 {
			break
		}
		rest := s[idx+len(marker):]
		end := strings.IndexAny(rest, ")]")
		if end < 0 {
			break
		}
		seen[rest[:end]] = struct{}{}
		s = rest[end:]
	}
	return len(seen)
}

// TestBoundChunks_KeepsObjectiveAndNewestTurns pins the bounding priorities.
//
// @purpose Proves the objective survives and the budget is honoured when turns must be dropped.
// @rationale Losing the objective is what made a long session re-probe an environment it had already
// mapped; the opening chunk is pinned for that reason.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestBoundChunks_KeepsObjectiveAndNewestTurns(t *testing.T) {
	chunks := []HistoryChunk{{Text: "User: OBJECTIVE keep me", Pinned: true}}
	for i := 0; i < 300; i++ {
		chunks = append(chunks, HistoryChunk{Text: "User: turn " + strings.Repeat("z", 400)})
	}

	got := BoundChunks(chunks, 2000)
	if !strings.Contains(got, "User: OBJECTIVE keep me") {
		t.Fatal("the pinned objective was dropped")
	}
	if !strings.Contains(got, historyOmittedMarker) {
		t.Fatal("dropping turns must be announced by the marker")
	}
	if !strings.Contains(got, "User: turn "+strings.Repeat("z", 400)) {
		t.Fatal("the newest turn was dropped")
	}
	if got := BoundChunks(nil, 100); got != "" {
		t.Fatalf("no chunks must yield an empty prompt, got %q", got)
	}
}
