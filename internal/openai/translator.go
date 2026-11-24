package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"agy-gateway/internal/acp"
	"agy-gateway/internal/mcp"
)

// Translator converts OpenAI request messages and tool declarations into ACP prompt payloads.
type Translator struct {
	virtualAdapter *mcp.VirtualAdapter
}

// NewTranslator creates an initialized Translator.
func NewTranslator() *Translator {
	return &Translator{
		virtualAdapter: mcp.NewVirtualAdapter(),
	}
}

// cleanToolOutputContent sanitizes tool execution result text and strips duplicate turn headers.
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
	}
	if strings.HasPrefix(s, "Tool Output:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "Tool Output:"))
	}
	return s
}

// Translate extracts system instructions and formats the multi-turn conversational history.
// Consumer system instructions and tool calling directives are extracted into a dedicated systemPrompt
// string for native injection into ACP session/new via _meta.antigravity.systemPrompt.append.
// Conversational turns (user, assistant, tool execution results) are cleanly formatted into ACP prompt blocks.
// An active execution loop cue primes assistant continuation when turns end on tool outputs.
func (t *Translator) Translate(req *ChatCompletionRequest, delimiter string) (string, []acp.PromptBlock) {
	if delimiter == "" {
		delimiter = "tool_call"
	}

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

	// 2. Format Consumer JIT Tool Directives (Deduplicated by function name)
	var toolsBlock string
	if len(req.Tools) > 0 {
		var uniqueTools []mcp.OpenAIToolDefinition
		seen := make(map[string]bool)
		for _, tool := range req.Tools {
			if tool.Type != "function" || tool.Function.Name == "" {
				continue
			}
			if seen[tool.Function.Name] {
				continue
			}
			seen[tool.Function.Name] = true
			uniqueTools = append(uniqueTools, tool)
		}

		if len(uniqueTools) > 0 {
			parallel := true
			if req.ParallelToolCalls != nil {
				parallel = *req.ParallelToolCalls
			}
			var toolChoiceStr string
			switch tc := req.ToolChoice.(type) {
			case string:
				toolChoiceStr = tc
			}
			toolsBlock = t.virtualAdapter.BuildToolsPromptEnvelope(uniqueTools, toolChoiceStr, parallel, delimiter)
		}
	}

	// 3. Compose system prompt for ACP session/new _meta.antigravity.systemPrompt.append
	var systemBlocks []string
	if len(sysMessages) > 0 {
		sysBody := strings.TrimSpace(strings.Join(sysMessages, "\n\n"))
		if sysBody != "" {
			systemBlocks = append(systemBlocks, sysBody)
		}
	}
	if toolsBlock != "" {
		systemBlocks = append(systemBlocks, strings.TrimSpace(toolsBlock))
	}
	systemPrompt := strings.TrimSpace(strings.Join(systemBlocks, "\n\n"))

	var historyBuilder strings.Builder

	// 4. Reconstitute multi-turn conversation history without artificial envelope wrapping
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
					raw, _ := json.Marshal(map[string]any{
						"id":        tc.ID,
						"name":      tc.Function.Name,
						"arguments": json.RawMessage(tc.Function.Arguments),
					})
					calls = append(calls, fmt.Sprintf("<%s>%s</%s>", delimiter, string(raw), delimiter))
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

	// 5. Append Active Continuation Cue to prevent premature agent loops from stalling
	if len(nonSysMessages) > 0 {
		lastRole := strings.ToLower(nonSysMessages[len(nonSysMessages)-1].Role)
		if lastRole == "tool" {
			historyBuilder.WriteString(t.virtualAdapter.BuildActiveContinuationCue(true, delimiter))
		} else if lastRole == "user" {
			historyBuilder.WriteString(t.virtualAdapter.BuildActiveContinuationCue(false, delimiter))
		}
	} else {
		historyBuilder.WriteString("Assistant: ")
	}

	finalPrompt := strings.TrimSpace(historyBuilder.String())
	if strings.HasSuffix(finalPrompt, "Assistant:") {
		finalPrompt += " "
	}
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
