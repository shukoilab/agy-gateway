package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// buildLongAnthropicRequest reproduces a tool-heavy history for the budget tests.
//
// @purpose Gives the Anthropic budget tests the same proportions as the OpenAI ones.
// @rationale The Anthropic translator had its own copy of the flat per-result cap, so it could still
// drop whole turns of a long history even after the OpenAI path was fixed.
// @preconditions None.
// @param turns Number of assistant/tool cycles.
// @param resultChars Approximate characters per tool result.
// @returns A request whose uncompacted rendering far exceeds a 60 K token budget.
// @invariants Deterministic.
func buildLongAnthropicRequest(turns, resultChars int) *MessageRequest {
	msgs := []Message{{Role: "user", Content: MessageContent{
		Text: "Workspace state appears empty contradicting expectations so trace investigate debug and fix " +
			strings.Repeat("keep working through every scenario ", 60),
	}}}
	for i := 0; i < turns; i++ {
		id := fmt.Sprintf("toolu_%03d", i)
		msgs = append(msgs,
			Message{Role: "assistant", Content: MessageContent{Blocks: []ContentBlock{{
				Type: "tool_use", ID: id, Name: "read",
				Input: json.RawMessage(`{"path":"a.ts"}`),
			}}}},
			Message{Role: "user", Content: MessageContent{Blocks: []ContentBlock{{
				Type: "tool_result", ToolUseID: id,
				Content: "func x() {}\n\n" + strings.Repeat("body payload\n\n", resultChars/14),
			}}}},
		)
	}
	return &MessageRequest{Model: "claude", Messages: msgs}
}

// TestAnthropic_LongHistoryKeepsEveryTurn pins the same no-amnesia rule as the OpenAI path.
//
// @purpose Proves a tool-heavy Anthropic history keeps every tool_use/tool_result pair.
// @rationale Both translators feed the same agent; fixing only one leaves the loop reachable through
// the other endpoint.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestAnthropic_LongHistoryKeepsEveryTurn(t *testing.T) {
	const turns = 200
	req := buildLongAnthropicRequest(turns, 3000)
	_, blocks := NewTranslatorWithBudget(60_000).Translate(req, "")
	prompt := blocks[0].Text

	if got := strings.Count(prompt, "[Assistant called tool read (id: "); got != turns {
		t.Fatalf("assistant turns kept = %d, want %d", got, turns)
	}
	if got := strings.Count(prompt, "Tool Output (id: "); got != turns {
		t.Fatalf("tool results kept = %d, want %d", got, turns)
	}
	if !strings.Contains(prompt, "Workspace state appears empty") {
		t.Fatal("the opening objective was dropped")
	}
	if tokens := len(prompt) / 4; tokens > 60_000 {
		t.Fatalf("prompt = %d tokens, budget = 60000", tokens)
	}
}

// TestAnthropic_PairOrderPreserved pins call/result pairing on the Anthropic path.
//
// @purpose Proves results follow the calls that produced them.
// @rationale A misordered pair makes the model attribute one tool's output to another's call.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestAnthropic_PairOrderPreserved(t *testing.T) {
	req := &MessageRequest{Messages: []Message{
		{Role: "user", Content: MessageContent{Text: "go"}},
		{Role: "assistant", Content: MessageContent{Blocks: []ContentBlock{
			{Type: "tool_use", ID: "u1", Name: "read", Input: json.RawMessage(`{"path":"a"}`)},
			{Type: "tool_use", ID: "u2", Name: "read", Input: json.RawMessage(`{"path":"b"}`)},
		}}},
		{Role: "user", Content: MessageContent{Blocks: []ContentBlock{
			{Type: "tool_result", ToolUseID: "u1", Content: "AAA"},
			{Type: "tool_result", ToolUseID: "u2", Content: "BBB"},
		}}},
	}}

	_, blocks := NewTranslator().Translate(req, "")
	prompt := blocks[0].Text
	c1 := strings.Index(prompt, "called tool read (id: u1)")
	c2 := strings.Index(prompt, "called tool read (id: u2)")
	r1 := strings.Index(prompt, "Tool Output (id: u1): AAA")
	r2 := strings.Index(prompt, "Tool Output (id: u2): BBB")
	if c1 < 0 || c2 < 0 || r1 < 0 || r2 < 0 {
		t.Fatalf("a call or result is missing: %q", prompt)
	}
	if !(c1 < c2 && c2 < r1 && r1 < r2) {
		t.Fatalf("pair order broken: c1=%d c2=%d r1=%d r2=%d", c1, c2, r1, r2)
	}
}
