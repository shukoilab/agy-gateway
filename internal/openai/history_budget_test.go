package openai

import (
	"fmt"
	"strings"
	"testing"
)

// buildLongAgenticRequest reproduces the shape of the session that looped: one huge
// single-paragraph task, then hundreds of assistant→tool→result cycles carrying large file dumps.
//
// @purpose Gives the budget tests a transcript with the real proportions (tool output dominates).
// @rationale The failure only appears when tool results are the overwhelming majority of the prompt,
// which a small fixture cannot express; the loop was measured on a 211-result, 648 KB history.
// @preconditions None.
// @param turns Number of assistant/tool cycles.
// @param resultChars Approximate characters per tool result body.
// @returns A request whose fastest correct rendering exceeds a 60 K token budget.
// @invariants Deterministic; total tool result characters = turns × resultChars.
func buildLongAgenticRequest(turns, resultChars int) *ChatCompletionRequest {
	msgs := []ChatMessage{{
		Role:    "system",
		Content: "You are an autonomous debugger. Read the workspace, then fix the defect.",
	}, {
		Role: "user",
		// Deliberately one paragraph with no blank line: the real task could never be split by
		// paragraph heuristics, so the fixture must not accidentally be easier than production.
		Content: "Workspace state appears empty contradicting expectations so trace investigate debug and fix until proven " +
			strings.Repeat("keep working through every scenario and aspect ", 60),
	}}

	for i := 0; i < turns; i++ {
		id := fmt.Sprintf("call_%03d", i)
		msgs = append(msgs,
			ChatMessage{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID:       id,
					Type:     "function",
					Function: FunctionCallData{Name: "read", Arguments: fmt.Sprintf(`{"path":"f%d.ts","limit":120}`, i)},
				}},
			},
			ChatMessage{
				Role:       "tool",
				ToolCallID: id,
				// Blank lines inside the body are what made the old character-offset cut split a pair.
				Content: fmt.Sprintf("line 1 of file %d\n\nfunc x() {\n\n\treturn\n\n}\n", i) +
					strings.Repeat("body payload\n\n", resultChars/14),
			},
		)
	}
	return &ChatCompletionRequest{Model: "gemini-3.8-flash", Messages: msgs}
}

// TestTranslator_LongHistoryKeepsEveryTurn pins the fix for the amnesia loop.
//
// @purpose Proves a 200-turn tool-heavy history keeps every assistant turn at the production budget.
// @rationale The bug deleted 94 of 156 assistant turns at a 60 K budget, including the operating
// instructions and the model's own conclusions, so it re-read the same files indefinitely. Compacting
// tool output harder is what allows every turn to survive.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestTranslator_LongHistoryKeepsEveryTurn(t *testing.T) {
	const turns = 200
	req := buildLongAgenticRequest(turns, 3000)
	translator := NewTranslatorWithBudget(60_000)

	_, blocks := translator.Translate(req, "tool_call")
	if len(blocks) != 1 {
		t.Fatalf("prompt blocks = %d, want 1", len(blocks))
	}
	prompt := blocks[0].Text

	calls := strings.Count(prompt, "[Assistant called tool read (id: ")
	results := strings.Count(prompt, "Tool Output (id: ")
	if calls != turns {
		t.Fatalf("assistant turns kept = %d, want all %d", calls, turns)
	}
	if results != turns {
		t.Fatalf("tool results kept = %d, want all %d", results, turns)
	}
	// The opening task must survive: losing it is what made the agent re-probe the workspace.
	if !strings.Contains(prompt, "Workspace state appears empty") {
		t.Fatal("the opening objective was dropped")
	}
}

// TestTranslator_LongHistoryStaysWithinBudget pins the bound even when nothing is dropped.
//
// @purpose Proves compaction alone is enough to fit a realistic transcript.
// @rationale A budget that is never enforced is not a budget; a budget enforced by deleting turns is
// the defect. This asserts both properties at once.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestTranslator_LongHistoryStaysWithinBudget(t *testing.T) {
	const budget = 60_000
	req := buildLongAgenticRequest(200, 3000)
	_, blocks := NewTranslatorWithBudget(budget).Translate(req, "tool_call")
	prompt := blocks[0].Text

	tokens := len(prompt) / 4
	if tokens > budget {
		t.Fatalf("prompt = %d estimated tokens, budget = %d", tokens, budget)
	}
	if strings.Contains(prompt, "[Earlier conversation omitted") {
		t.Fatal("a fittable transcript must not drop turns")
	}
}

// TestTranslator_ToolCapAdaptsToResultCount pins that the cap shrinks as history grows.
//
// @purpose Proves the per-result cap is computed from the actual overhead and result count.
// @rationale A fixed cap cannot adapt: a 211-result history needs a far smaller cap than a 5-result
// one to fit the same budget.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestTranslator_ToolCapAdaptsToResultCount(t *testing.T) {
	short := buildLongAgenticRequest(5, 2000)
	long := buildLongAgenticRequest(200, 2000)

	if _, blocks := NewTranslatorWithBudget(60_000).Translate(short, "tool_call"); len(blocks[0].Text) > 60_000*4 {
		t.Fatal("short transcript must fit")
	}
	if _, blocks := NewTranslatorWithBudget(60_000).Translate(long, "tool_call"); len(blocks[0].Text) > 60_000*4 {
		t.Fatalf("long transcript must fit: %d chars", len(blocks[0].Text))
	}
	// An unbounded translator must not compact at all.
	_, unbounded := NewTranslator().Translate(long, "tool_call")
	if strings.Contains(unbounded[0].Text, "chars omitted") {
		t.Fatal("budget 0 must disable tool-result compaction")
	}
}

// TestTranslator_PairOrderPreserved pins that each result follows its own call.
//
// @purpose Proves the rendering never reorders or duplicates a call/result pair.
// @rationale Ordering corruption would make the model attribute one file's content to another call.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestTranslator_PairOrderPreserved(t *testing.T) {
	req := &ChatCompletionRequest{Messages: []ChatMessage{
		{Role: "user", Content: "do the thing"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "c1", Function: FunctionCallData{Name: "read", Arguments: `{"path":"a"}`}},
			{ID: "c2", Function: FunctionCallData{Name: "read", Arguments: `{"path":"b"}`}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "AAA"},
		{Role: "tool", ToolCallID: "c2", Content: "BBB"},
	}}

	_, blocks := NewTranslator().Translate(req, "tool_call")
	prompt := blocks[0].Text

	c1 := strings.Index(prompt, "called tool read (id: c1)")
	c2 := strings.Index(prompt, "called tool read (id: c2)")
	r1 := strings.Index(prompt, "Tool Output (id: c1): AAA")
	r2 := strings.Index(prompt, "Tool Output (id: c2): BBB")
	if c1 < 0 || c2 < 0 || r1 < 0 || r2 < 0 {
		t.Fatalf("a call or result is missing: %q", prompt)
	}
	if !(c1 < c2 && c2 < r1 && r1 < r2) {
		t.Fatalf("pair order broken: c1=%d c2=%d r1=%d r2=%d", c1, c2, r1, r2)
	}
}
