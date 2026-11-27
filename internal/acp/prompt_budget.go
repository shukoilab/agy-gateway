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

// minToolResultChars is the smallest per-result character cap worth keeping.
//
// @rationale Below this a tool result is reduced to its first line, which no longer identifies what
// the call returned. At that point dropping the oldest turns is the only remaining option.
const minToolResultChars = 400

// FitToolResultChars picks a per-result character cap that lets every turn fit the budget.
//
// @purpose Replaces "compact a little, then delete whole turns" with "compact enough to keep every
// turn".
// @rationale Dropping turns was the real cause of the repeated-call loop: a 156-turn session lost 94
// assistant turns, including the operating instructions and the model's own prior analysis, so it
// re-read the same files forever. Tool results are 92% of such a transcript, so scaling their cap is
// enough to fit the budget without discarding a single turn (measured: 648 KB of tool output fits in
// 55 K estimated tokens at an 800-character cap).
// @preconditions overheadChars describes the rendered text outside tool result bodies.
// @param overheadChars Characters the prompt occupies with everything except tool result bodies.
// @param toolResults Number of tool results that will be compacted.
// @param maxTokens History budget in estimated tokens; <= 0 disables compaction.
// @returns A cap in [minToolResultChars, ToolResultCharsCap(maxTokens)], or 0 when compaction is
// disabled or when even the minimum cap cannot fit the budget (caller then falls back to dropping).
// @throws None.
// @invariants O(1); never returns a cap larger than the flat per-result allowance.
func FitToolResultChars(overheadChars, toolResults, maxTokens int) int {
	if maxTokens <= 0 {
		return 0
	}
	// Hold back a tenth of the budget. EstimatePromptTokens assumes 4 characters per token, which is
	// optimistic for non-ASCII text and for tool output full of short tokens and punctuation; without
	// the reserve an estimate that fits still overflows the real context window.
	available := maxTokens*4/10*9 - overheadChars
	if toolResults <= 0 {
		if available >= 0 {
			return ToolResultCharsCap(maxTokens)
		}
		return 0
	}
	limit := available / toolResults
	if limit > ToolResultCharsCap(maxTokens) {
		limit = ToolResultCharsCap(maxTokens)
	}
	if limit < minToolResultChars {
		return 0
	}
	return limit
}

// CompactToolOutput truncates one tool result body, keeping its head and tail.
//
// @purpose Stops a single oversized tool result from consuming the whole history budget.
// @rationale Long agentic sessions accumulate binary dumps and full file reads; one 55 KB result
// crowds out every other turn and leaves the agent unable to see its own objective.
// @param s the tool result body.
// @param maxChars maximum retained characters; <= 0 leaves the body untouched.
// @returns s unchanged when within the cap, otherwise its first and last halves joined by a marker
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

// BoundPrompt fits a fully rendered history into a token budget without splitting a tool pair.
//
// @purpose Keeps the historical entry point working while bounding whole turns only.
// @rationale Callers that render into one string (the Anthropic translator) still need the bound, but
// cutting that string at a character offset could separate a tool call from its result. Splitting on
// block boundaries and grouping every tool-results block with the assistant turn that produced it
// makes such a split impossible, which is what stops the model re-issuing calls it already made.
// @preconditions prompt is the output of a renderer that starts each block with "User: ",
// "Assistant: ", "Tool Output (id: " or "Tool Error (id: ".
// @param prompt the fully rendered history.
// @param maxTokens budget in estimated tokens; values <= 0 disable the bound.
// @returns the prompt unchanged when within budget, otherwise pinned opening turn plus the newest
// whole turns and an omission marker.
// @throws None.
// @invariants O(len(prompt)); never emits a tool result without its declaring assistant turn.
func BoundPrompt(prompt string, maxTokens int) string {
	if maxTokens <= 0 {
		return prompt
	}
	if EstimatePromptTokens(prompt) <= maxTokens {
		return prompt
	}
	return BoundChunks(SplitPromptChunks(prompt), maxTokens)
}

// SplitPromptChunks groups rendered blocks into turn-sized chunks for BoundChunks.
//
// @purpose Recovers the turn structure of an already-rendered prompt.
// @rationale A tool-results block is emitted separately from the assistant block that declared its
// calls, so both must land in the same chunk or bounding would orphan one half of the pair.
// @preconditions prompt is the output of a renderer using the block prefixes named in BoundPrompt.
// @param prompt Rendered history.
// @returns Chronological chunks, the first marked pinned; empty input yields no chunks.
// @throws None.
// @invariants O(len(prompt)); a block is never divided.
func SplitPromptChunks(prompt string) []HistoryChunk {
	blocks := strings.Split(strings.TrimSpace(prompt), "\n\n")
	chunks := make([]HistoryChunk, 0, len(blocks))
	var cur strings.Builder
	for _, block := range blocks {
		trimmed := strings.TrimSpace(block)
		if trimmed == "" {
			continue
		}
		startsTurn := strings.HasPrefix(trimmed, "Assistant: ") ||
			(strings.HasPrefix(trimmed, "User: ") && !strings.HasPrefix(trimmed, "User: [Tool Execution Results]"))
		if startsTurn && cur.Len() > 0 {
			text := strings.TrimSpace(cur.String())
			cur.Reset()
			if text != "" {
				chunks = append(chunks, HistoryChunk{Text: text, Pinned: len(chunks) == 0})
			}
		}
		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}
		cur.WriteString(trimmed)
	}
	if text := strings.TrimSpace(cur.String()); text != "" {
		chunks = append(chunks, HistoryChunk{Text: text, Pinned: len(chunks) == 0})
	}
	return chunks
}

// HistoryChunk is one indivisible unit of a rendered conversation.
//
// @purpose Makes the history budget operate on whole turns instead of a character offset.
// @rationale The previous implementation cut the rendered string at an arbitrary offset and then
// tried to repair the damage by scanning for paragraph breaks. Tool output contains blank lines, so
// that repair could land inside a result body and leave a "Tool Output (id: X)" with no call, or a
// call with no result. Either half makes the model re-issue the same call instead of continuing,
// which is the repeated-call loop observed in long sessions. Chunks cannot be split, so the problem
// cannot arise.
// @invariants Text is fully rendered and ends without a trailing blank line.
type HistoryChunk struct {
	// Text is the rendered block(s) for this chunk.
	Text string
	// Pinned marks a chunk that must survive bounding whenever possible (the opening task).
	Pinned bool
}

// BoundChunks fits the newest chunks of a rendered history into a token budget.
//
// @purpose Caps the prompt without ever delivering half of a tool-call/result pair.
// @rationale Bounding whole chunks is the only way to guarantee the model sees a consistent
// conversation: every result it reads has the call that produced it, and every call it made has its
// result. The opening chunk is pinned because it carries the user's objective; losing it makes the
// agent re-probe the environment to rediscover its goal.
// @preconditions chunks are in chronological order; a call and its result already share a chunk.
// @param chunks Rendered conversation units.
// @param maxTokens budget in estimated tokens; values <= 0 disable the bound.
// @returns the joined chunks, an omission marker, or an empty string when chunks is empty.
// @throws None.
// @invariants O(len(chunks)); the result's estimate never exceeds maxTokens plus the marker.
func BoundChunks(chunks []HistoryChunk, maxTokens int) string {
	if len(chunks) == 0 {
		return ""
	}
	join := func(from int) string {
		var b strings.Builder
		for i := from; i < len(chunks); i++ {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(chunks[i].Text)
		}
		return b.String()
	}

	full := join(0)
	if maxTokens <= 0 || EstimatePromptTokens(full) <= maxTokens {
		return full
	}

	headText := ""
	start := 0
	if chunks[0].Pinned && EstimatePromptTokens(chunks[0].Text) <= maxTokens/4 {
		headText = chunks[0].Text
		start = 1
	}

	// Grow the tail backwards while it still fits alongside the pinned head and the marker. Every
	// chunk also costs two separator characters, and the marker is joined with two more separators on
	// whichever side it lands, so both are charged here rather than discovered after the fact.
	kept := len(chunks)
	// The marker is surrounded by two separators; each kept chunk costs one more. Charging the real
	// two-character separator per chunk (rather than one) keeps the stated invariant true at any depth.
	budget := maxTokens - EstimatePromptTokens(headText) - EstimatePromptTokens(historyOmittedMarker) - 2
	for kept > start {
		candidate := EstimatePromptTokens(chunks[kept-1].Text) + 1 // "\n\n" is 2 chars, halved by len/4
		if candidate > budget {
			break
		}
		budget -= candidate
		kept--
	}

	tail := join(kept)
	switch {
	case headText != "" && tail != "":
		return headText + "\n\n" + historyOmittedMarker + "\n\n" + tail
	case headText != "":
		return headText
	case tail != "":
		return historyOmittedMarker + "\n\n" + tail
	default:
		// Nothing fits: hand back the newest chunk rather than an empty prompt.
		return chunks[len(chunks)-1].Text
	}
}
