package openai

import (
	"fmt"
	"strings"

	"agy-gateway/internal/acp"
)

// Translator converts OpenAI request messages into ACP prompt payloads.
//
// @purpose Maps OpenAI system prompts and multi-turn conversational history into standard ACP PromptBlocks.
// @rationale Eliminates all brittle prompt injection, XML <tool_call> tags, and synthetic system directives.
// All tool capabilities are registered natively in ACP session/new via LoopbackServer and mcpServers.
// @preconditions req must be non-nil.
// @invariants Thread-safe, pure deterministic transformation.
type Translator struct {
	// maxHistoryTokens bounds the rendered history; 0 disables the bound.
	maxHistoryTokens int
}

// NewTranslator creates an initialized Translator with history bounding disabled.
func NewTranslator() *Translator {
	return &Translator{}
}

// NewTranslatorWithBudget creates a Translator that bounds rendered history to maxHistoryTokens.
//
// @purpose Prevents pathological client histories from collapsing the agent into tool-call loops.
// @param maxHistoryTokens estimated-token budget; <= 0 disables the bound.
// @returns a Translator ready for concurrent use.
func NewTranslatorWithBudget(maxHistoryTokens int) *Translator {
	return &Translator{maxHistoryTokens: maxHistoryTokens}
}

// cleanToolOutputContent strips any legacy duplicate turn headers or wrapper markers from tool result text.
func cleanToolOutputContent(content, toolCallID string) string {
	s := strings.TrimSpace(content)
	for {
		if strings.HasPrefix(s, "User: [Tool Execution Results]") {
			s = strings.TrimSpace(strings.TrimPrefix(s, "User: [Tool Execution Results]"))
		} else if strings.HasPrefix(s, "[Tool Execution Results]") {
			s = strings.TrimSpace(strings.TrimPrefix(s, "[Tool Execution Results]"))
		} else {
			break
		}
	}
	if toolCallID != "" {
		expectedPrefix := fmt.Sprintf("Tool Output (id: %s):", toolCallID)
		if strings.HasPrefix(s, expectedPrefix) {
			s = strings.TrimSpace(strings.TrimPrefix(s, expectedPrefix))
		}
		expectedErrPrefix := fmt.Sprintf("Tool Error (id: %s):", toolCallID)
		if strings.HasPrefix(s, expectedErrPrefix) {
			s = strings.TrimSpace(strings.TrimPrefix(s, expectedErrPrefix))
		}
	}
	if strings.HasPrefix(s, "Tool Output:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "Tool Output:"))
	}
	if strings.HasPrefix(s, "Tool Error:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "Tool Error:"))
	}
	return s
}

// newestToolResultIndexes returns the indexes of the newest tool-result messages.
//
// @purpose Marks the tool results the agent must see verbatim so compaction never touches them.
// @rationale The last few results are the evidence the current turn is reasoning over; truncating
// them recreates the "cannot see what I just did" failure the history budget exists to prevent.
// @param msgs the non-system conversation messages.
// @returns a set of indexes into msgs, holding up to acp.KeepIntactToolResults entries.
// @invariants Scans at most len(msgs) messages and never includes a non-tool role.
func newestToolResultIndexes(msgs []ChatMessage) map[int]struct{} {
	out := make(map[int]struct{}, acp.KeepIntactToolResults)
	for i := len(msgs) - 1; i >= 0 && len(out) < acp.KeepIntactToolResults; i-- {
		if strings.ToLower(msgs[i].Role) == "tool" {
			out[i] = struct{}{}
		}
	}
	return out
}

// compactableToolResults counts the tool results the renderer will compact.
//
// @purpose Supplies the divisor for acp.FitToolResultChars.
// @param msgs the non-system conversation messages.
// @param intact indexes of results that stay verbatim.
// @returns the number of tool results that will be truncated.
// @invariants O(len(msgs)).
func compactableToolResults(msgs []ChatMessage, intact map[int]struct{}) int {
	n := 0
	for i := range msgs {
		if strings.ToLower(msgs[i].Role) != "tool" {
			continue
		}
		if _, keep := intact[i]; !keep {
			n++
		}
	}
	return n
}

// Fixed wrapper lengths the renderer emits, charged so the overhead estimate matches the real prompt.
//
// @rationale Under-counting the wrappers made FitToolResultChars choose a cap that was a few percent
// too generous, which was still enough to overflow the budget and drop turns — the exact failure the
// cap exists to prevent. Each constant below is the literal text in Translate (see the fmt.Fprintf
// calls): "Assistant: ", "Tool Output (id: ): ", "User: [Tool Execution Results]\n", and the
// "[Assistant called tool  (id: ) with arguments: ]" bracket around a call, whose punctuation costs 48.
const (
	assistantPrefixChars   = len("Assistant: ")
	toolOutputPrefixChars  = len("Tool Output (id: ): ")
	toolResultsHeaderChars = len("User: [Tool Execution Results]\n")
	toolCallBracketChars   = 48
	// toolCompactMarkerChars is the fixed text CompactToolOutput inserts per compacted result
	// ("\n…[N chars omitted]…\n" with a 1–10 digit count). Charging it keeps the estimate from
	// under-counting by a few hundred bytes on a 200-result history, which is enough to overflow.
	toolCompactMarkerChars = 32
)

// renderedOverheadChars measures how much of the budget non-result text consumes.
//
// @purpose Tells acp.FitToolResultChars how much room is left for tool output.
// @rationale Charging every rendered word and wrapper — but not the compacted tool result bodies
// themselves — is exactly the residue the cap has to share.
// @param msgs the non-system conversation messages.
// @param intact indexes of tool results that stay verbatim.
// @param maxHistoryTokens the configured budget; <= 0 disables the calculation.
// @returns an approximate character count of everything except compacted tool bodies.
// @invariants O(len(msgs)); pure.
func renderedOverheadChars(msgs []ChatMessage, intact map[int]struct{}, maxHistoryTokens int) int {
	if maxHistoryTokens <= 0 {
		return 0
	}
	overhead := 0
	for i, m := range msgs {
		switch strings.ToLower(m.Role) {
		case "tool":
			// A results group header precedes the first result of each group; the prefix precedes
			// every result. Both are rendered text the cap cannot reclaim.
			overhead += toolOutputPrefixChars + len(m.ToolCallID) + 1
			if i == 0 || strings.ToLower(msgs[i-1].Role) != "tool" {
				overhead += toolResultsHeaderChars
			}
			if _, keep := intact[i]; keep {
				// Verbatim results still consume budget, so their body is charged in full.
				overhead += len(m.ContentString())
			} else {
				overhead += toolCompactMarkerChars
			}
		case "assistant":
			overhead += assistantPrefixChars + len(m.ContentString())
			for _, tc := range m.ToolCalls {
				overhead += len(tc.ID) + len(tc.Function.Name) + len(tc.Function.Arguments) + toolCallBracketChars
			}
		default:
			overhead += len(m.ContentString())
		}
	}
	return overhead
}

// Translate extracts pure system instructions and formats conversational history into standard ACP prompt blocks.
// System instructions are returned separately and forwarded by the server layer as the first prompt
// block; neither the spec-level `systemPrompt` field (accepted and discarded by the agent) nor the
// vendor `_meta` dialect (policy-guarded, can answer a synthetic 429) is used.
// Tool declarations are NOT injected into prompt text; they are provided natively via LoopbackServer mcpServers.
func (t *Translator) Translate(req *ChatCompletionRequest, _ string) (string, []acp.PromptBlock) {
	var sysMessages []string
	var nonSysMessages []ChatMessage

	// 1. Separate system/developer messages from conversational turns
	for _, msg := range req.Messages {
		if msg.Role == "system" || msg.Role == "developer" {
			text := msg.ContentString()
			if text != "" {
				sysMessages = append(sysMessages, text)
			}
		} else {
			nonSysMessages = append(nonSysMessages, msg)
		}
	}

	systemPrompt := strings.TrimSpace(strings.Join(sysMessages, "\n\n"))

	// 2. Fast-path: single user message turn
	if len(nonSysMessages) == 1 && strings.ToLower(nonSysMessages[0].Role) == "user" {
		userText := strings.TrimSpace(nonSysMessages[0].ContentString())
		if userText == "" {
			userText = "Hello"
		}
		return systemPrompt, []acp.PromptBlock{
			{
				Type: "text",
				Text: userText,
			},
		}
	}

	// 3. Multi-turn conversational history
	// Identify the newest tool results: they are what the agent needs to continue, so they are
	// never compacted even when older, oversized results exhaust the budget.
	intactToolIdx := newestToolResultIndexes(nonSysMessages)

	// Size the per-result cap so the whole conversation fits the budget WITHOUT dropping turns.
	// Deleting turns is what collapsed long agentic sessions: the operating instructions and the
	// model's own prior conclusions vanished from a 156-turn history, so it re-read the same files
	// forever. Tool output is the overwhelming majority of such a transcript, so compacting it harder
	// keeps every turn. A cap of 0 means "no compaction", which keeps the old behaviour for a budget
	// of 0; the budgeted fallback in BoundPrompt still protects against a prompt built mostly of prose.
	toolCap := acp.FitToolResultChars(renderedOverheadChars(nonSysMessages, intactToolIdx, t.maxHistoryTokens), compactableToolResults(nonSysMessages, intactToolIdx), t.maxHistoryTokens)

	// Render into chunks, where one chunk is an assistant turn together with the tool results it
	// produced. That grouping is what lets the budget drop whole turns: a call is never separated from
	// its result, so the model cannot receive a result whose call was omitted (or a call whose result
	// was). Splitting a rendered string at an offset could not guarantee this, because tool output
	// contains blank lines that look like paragraph boundaries.
	var chunks []acp.HistoryChunk
	var cur strings.Builder
	// hasResultsHeader records whether cur already opens a tool-results section. Tracking it avoids
	// calling cur.String() (a copy of everything buffered) per tool message, which made rendering
	// quadratic on long histories.
	hasResultsHeader := false

	flush := func() {
		text := strings.TrimSpace(cur.String())
		cur.Reset()
		hasResultsHeader = false
		if text == "" {
			return
		}
		chunks = append(chunks, acp.HistoryChunk{Text: text, Pinned: len(chunks) == 0})
	}

	for i := 0; i < len(nonSysMessages); i++ {
		msg := nonSysMessages[i]
		role := strings.ToLower(msg.Role)
		content := msg.ContentString()

		switch role {
		case "user":
			trimmed := strings.TrimSpace(content)
			if strings.HasPrefix(trimmed, "User: [Tool Execution Results]") || strings.HasPrefix(trimmed, "[Tool Execution Results]") {
				if cur.Len() == 0 {
					cur.WriteString("User: [Tool Execution Results]\n")
				}
				cur.WriteString(cleanToolOutputContent(trimmed, ""))
				cur.WriteString("\n")
			} else {
				flush()
				fmt.Fprintf(&cur, "User: %s", content)
				flush()
			}
		case "assistant":
			flush()
			if len(msg.ToolCalls) > 0 {
				var calls []string
				for _, tc := range msg.ToolCalls {
					calls = append(calls, fmt.Sprintf("[Assistant called tool %s (id: %s) with arguments: %s]", tc.Function.Name, tc.ID, tc.Function.Arguments))
				}
				if content != "" {
					fmt.Fprintf(&cur, "Assistant: %s\n%s", content, strings.Join(calls, "\n"))
				} else {
					fmt.Fprintf(&cur, "Assistant: %s", strings.Join(calls, "\n"))
				}
			} else if content != "" {
				fmt.Fprintf(&cur, "Assistant: %s", content)
				flush()
			}
		case "tool":
			cleaned := cleanToolOutputContent(content, msg.ToolCallID)
			if _, intact := intactToolIdx[i]; !intact {
				cleaned = acp.CompactToolOutput(cleaned, toolCap)
			}
			if cur.Len() == 0 || !hasResultsHeader {
				if cur.Len() > 0 {
					cur.WriteString("\n")
				}
				cur.WriteString("User: [Tool Execution Results]\n")
				hasResultsHeader = true
			} else {
				cur.WriteString("\n")
			}
			fmt.Fprintf(&cur, "Tool Output (id: %s): %s", msg.ToolCallID, cleaned)
			if i+1 == len(nonSysMessages) || strings.ToLower(nonSysMessages[i+1].Role) != "tool" {
				flush()
			}
		default:
			if i == len(nonSysMessages)-1 {
				flush()
				fmt.Fprintf(&cur, "User: %s", content)
				flush()
			}
		}
	}
	flush()

	finalPrompt := acp.BoundChunks(chunks, t.maxHistoryTokens)
	if finalPrompt == "" {
		finalPrompt = "Hello"
	}
	return systemPrompt, []acp.PromptBlock{
		{
			Type: "text",
			Text: finalPrompt,
		},
	}
}
