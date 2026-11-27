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
	toolCap := acp.ToolResultCharsCap(t.maxHistoryTokens)

	var historyBuilder strings.Builder
	for i := 0; i < len(nonSysMessages); i++ {
		msg := nonSysMessages[i]
		role := strings.ToLower(msg.Role)
		content := msg.ContentString()

		switch role {
		case "user":
			trimmed := strings.TrimSpace(content)
			if strings.HasPrefix(trimmed, "User: [Tool Execution Results]") || strings.HasPrefix(trimmed, "[Tool Execution Results]") {
				cleaned := cleanToolOutputContent(trimmed, "")
				currHistory := strings.TrimSpace(historyBuilder.String())
				if !strings.HasSuffix(currHistory, "[Tool Execution Results]") {
					historyBuilder.WriteString("User: [Tool Execution Results]\n")
				}
				historyBuilder.WriteString(cleaned)
				historyBuilder.WriteString("\n\n")
			} else {
				fmt.Fprintf(&historyBuilder, "User: %s\n\n", content)
			}
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				var calls []string
				for _, tc := range msg.ToolCalls {
					calls = append(calls, fmt.Sprintf("[Assistant called tool %s (id: %s) with arguments: %s]", tc.Function.Name, tc.ID, tc.Function.Arguments))
				}
				if content != "" {
					fmt.Fprintf(&historyBuilder, "Assistant: %s\n%s\n\n", content, strings.Join(calls, "\n"))
				} else {
					fmt.Fprintf(&historyBuilder, "Assistant: %s\n\n", strings.Join(calls, "\n"))
				}
			} else {
				fmt.Fprintf(&historyBuilder, "Assistant: %s\n\n", content)
			}
		case "tool":
			cleaned := cleanToolOutputContent(content, msg.ToolCallID)
			if _, intact := intactToolIdx[i]; !intact {
				cleaned = acp.CompactToolOutput(cleaned, toolCap)
			}
			currHistory := strings.TrimSpace(historyBuilder.String())
			if !strings.HasSuffix(currHistory, "[Tool Execution Results]") {
				if i == 0 || strings.ToLower(nonSysMessages[i-1].Role) != "tool" {
					historyBuilder.WriteString("User: [Tool Execution Results]\n")
				}
			}
			fmt.Fprintf(&historyBuilder, "Tool Output (id: %s): %s\n", msg.ToolCallID, cleaned)
			if i+1 == len(nonSysMessages) || strings.ToLower(nonSysMessages[i+1].Role) != "tool" {
				historyBuilder.WriteString("\n")
			}
		default:
			if i == len(nonSysMessages)-1 {
				fmt.Fprintf(&historyBuilder, "User: %s\n\n", content)
			}
		}
	}

	finalPrompt := strings.TrimSpace(historyBuilder.String())
	if finalPrompt == "" {
		finalPrompt = "Hello"
	}
	finalPrompt = acp.BoundPrompt(finalPrompt, t.maxHistoryTokens)

	return systemPrompt, []acp.PromptBlock{
		{
			Type: "text",
			Text: finalPrompt,
		},
	}
}
