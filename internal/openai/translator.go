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

// Translate extracts system instructions and formats the multi-turn conversational history.
// Consumer system instructions are wrapped in <important>...</important> XML tags in accordance
// with Google Antigravity ToS for consumer OAuth accounts. Consumer tools are deduplicated by function name.
func (t *Translator) Translate(req *ChatCompletionRequest, delimiter string) []acp.PromptBlock {
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

	var historyBuilder strings.Builder

	// 2. Wrap system/developer instructions safely in <important></important> tags as user guidance
	if len(sysMessages) > 0 {
		sysBody := strings.TrimSpace(strings.Join(sysMessages, "\n\n"))
		if sysBody != "" {
			fmt.Fprintf(&historyBuilder, "<important>\n%s\n</important>\n\n", sysBody)
		}
	}

	// 3. Reconstitute multi-turn conversation history
	for i, msg := range nonSysMessages {
		role := strings.ToLower(msg.Role)
		content := msg.ContentString()

		switch role {
		case "user":
			fmt.Fprintf(&historyBuilder, "User: %s\n\n", content)
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
			fmt.Fprintf(&historyBuilder, "Tool Output (id: %s): %s\n\n", msg.ToolCallID, content)
		default:
			if i == len(nonSysMessages)-1 {
				fmt.Fprintf(&historyBuilder, "User: %s\n\n", content)
			}
		}
	}

	// 4. Inject Consumer JIT Tool Directives (Deduplicated by function name)
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
			toolsBlock := t.virtualAdapter.BuildToolsPromptEnvelope(uniqueTools, toolChoiceStr, parallel, delimiter)
			historyBuilder.WriteString(toolsBlock)
		}
	}

	finalPrompt := strings.TrimSpace(historyBuilder.String())
	if finalPrompt == "" {
		finalPrompt = "Hello"
	}

	return []acp.PromptBlock{
		{
			Type: "text",
			Text: finalPrompt,
		},
	}
}
