package acp

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// historyOmittedMarker precedes a prompt whose middle turns were dropped.
const historyOmittedMarker = "[Earlier conversation omitted to fit the context budget]"

// EstimatePromptTokens returns a coarse token estimate (≈4 chars per token) for prompt sizing.
//
// @purpose Gives the translator a cheap, deterministic size signal without a tokenizer dependency.
// @rationale The budget only exists to stop pathological histories from collapsing the model; an
// approximate ratio is sufficient and keeps translation pure and allocation-light.
// @param s any prompt text.
// @returns estimated token count; 0 for an empty string.
// @invariants Monotonic in len(s) and free of allocation.
func EstimatePromptTokens(s string) int {
	return len(s) / 4
}

// KeepIntactToolResults is how many of the newest tool results are never compacted.
//
// @rationale The newest results are exactly what the agent needs to continue (the file it just
// read, the command it just ran). Compacting those would recreate the original failure.
const KeepIntactToolResults = 4

// ToolResultCharsCap derives the per-result character cap from the history budget.
//
// @purpose Keeps any single historical tool result from dominating the budget.
// @rationale A quarter of the token budget, expressed in characters, is large enough for a real
// file read yet small enough that a binary dump cannot crowd out the rest of the conversation.
// @param maxTokens the history budget in estimated tokens; <= 0 disables compaction.
// @returns the per-result character cap, or 0 when the budget is disabled.
func ToolResultCharsCap(maxTokens int) int {
	if maxTokens <= 0 {
		return 0
	}
	return maxTokens / 4
}

// CompactToolOutput truncates one tool result body, keeping its head and tail.
//
// @purpose Stops a single oversized tool result from consuming the whole history budget.
// @rationale Long agentic sessions accumulate binary dumps and full file reads; one 55 KB result
// crowds out every other turn and leaves the agent unable to see its own objective.
// @param s the tool result body.
// @param maxChars maximum retained characters; <= 0 leaves the body untouched.
// @returns s unchanged when within the cap, otherwise its first and last thirds joined by a marker
// naming the number of omitted characters.
// @invariants The result is at most maxChars + len of the marker.
func CompactToolOutput(s string, maxChars int) string {
	if maxChars <= 0 || len(s) <= maxChars {
		return s
	}
	omitted := len(s) - maxChars
	half := maxChars / 2
	// Keep both cut points on rune boundaries so retained text stays valid UTF-8.
	for half > 0 && !utf8.RuneStart(s[half]) {
		half--
	}
	tailStart := len(s) - (maxChars - half)
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	marker := "\n…[" + strconv.Itoa(omitted) + " chars omitted]…\n"
	return s[:half] + marker + s[tailStart:]
}

// BoundPrompt keeps the opening turn and the most recent turns of a rendered history within a token budget.
//
// @purpose Caps how much conversational history reaches the agent in a single prompt block.
// @rationale Without a bound the agent receives the entire client transcript verbatim; upstream
// evidence showed turns above ~100K prompt tokens stop converging and re-issue the same tool calls
// indefinitely. Dropping only the oldest turns is not enough: the opening turn carries the user's
// task statement, and an agent that loses it re-probes the environment to rediscover its goal (it
// says so: "Truncated context obscures the original objective"). Keeping the head plus the tail
// preserves both the goal and the newest tool results.
// @param prompt the fully rendered history.
// @param maxTokens budget in estimated tokens; values <= 0 disable the bound.
// @returns the prompt unchanged when within budget, otherwise the opening turn, an omission marker,
// and the most recent turns that fit; always split at a paragraph boundary so no turn is cut mid-line.
// @invariants When maxTokens > 0 the estimate of the result never exceeds maxTokens plus the marker.
func BoundPrompt(prompt string, maxTokens int) string {
	if maxTokens <= 0 {
		return prompt
	}
	if EstimatePromptTokens(prompt) <= maxTokens {
		return prompt
	}

	// Reserve at most a quarter of the budget for the opening turn so the newest turns still fit.
	headBudget := maxTokens / 4
	head := prompt
	rest := ""
	if idx := strings.Index(prompt, "\n\n"); idx >= 0 {
		head, rest = prompt[:idx], prompt[idx+2:]
	}
	if EstimatePromptTokens(head) > headBudget {
		head = ""
		rest = prompt
	}

	tailBudgetChars := (maxTokens - EstimatePromptTokens(head)) * 4
	if tailBudgetChars >= len(rest) {
		return head + "\n\n" + rest
	}
	if tailBudgetChars <= 0 {
		return historyOmittedMarker + "\n\n" + strings.TrimSpace(rest)
	}

	tail := rest[len(rest)-tailBudgetChars:]
	// Align forward to the next paragraph boundary so the kept text starts on a whole turn.
	if idx := strings.Index(tail, "\n\n"); idx >= 0 && idx < len(tail)-1 {
		tail = tail[idx+2:]
	}

	if head == "" {
		return historyOmittedMarker + "\n\n" + strings.TrimSpace(tail)
	}
	return head + "\n\n" + historyOmittedMarker + "\n\n" + strings.TrimSpace(tail)
}
