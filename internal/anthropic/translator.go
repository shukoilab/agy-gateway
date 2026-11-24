package anthropic

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"agy-gateway/internal/acp"
	"agy-gateway/internal/mcp"
)

// Translator converts Anthropic MessageRequest structures into ACP prompt payloads.
//
// @purpose Maps Anthropic system prompts, multi-turn history, tool definitions, tool results,
// and extended thinking blocks into standardized Google Antigravity ACP PromptBlocks.
// @rationale Guarantees zero-semantic-loss across protocols and complies with Google Antigravity
// Terms of Service by wrapping system instructions in <important>...</important> XML tags.
// @preconditions req must be validated; delimiter must be non-empty.
type Translator struct {
	virtualAdapter *mcp.VirtualAdapter
}

// NewTranslator creates an initialized Translator.
func NewTranslator() *Translator {
	return &Translator{
		virtualAdapter: mcp.NewVirtualAdapter(),
	}
}

// Translate converts the Anthropic conversation into an ACP prompt payload.
//
// - @param: req Pointer to MessageRequest.
// - @param: delimiter Tag name for client tool calling encapsulation (default: "tool_call").
// @returns Slice of acp.PromptBlock ready for transmission over ACP.
func (t *Translator) Translate(req *MessageRequest, delimiter string) []acp.PromptBlock {
	if delimiter == "" {
		delimiter = "tool_call"
	}

	var historyBuilder strings.Builder

	// 1. Process Consumer JIT Tool Directives (Deduplicated by function name)
	var toolsBlock string
	if len(req.Tools) > 0 {
		var openaiTools []mcp.OpenAIToolDefinition
		seen := make(map[string]bool)

		for _, tool := range req.Tools {
			if tool.Name == "" || seen[tool.Name] {
				continue
			}
			seen[tool.Name] = true

			openaiTools = append(openaiTools, mcp.OpenAIToolDefinition{
				Type: "function",
				Function: mcp.FunctionSchema{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  tool.InputSchema,
				},
			})
		}

		if len(openaiTools) > 0 {
			var toolChoiceStr string
			switch tc := req.ToolChoice.(type) {
			case string:
				toolChoiceStr = tc
			case map[string]any:
				if typ, ok := tc["type"].(string); ok {
					if typ == "tool" {
						if name, ok := tc["name"].(string); ok {
							toolChoiceStr = name
						}
					} else {
						toolChoiceStr = typ
					}
				}
			}

			// Format JIT tools envelope
			toolsBlock = t.virtualAdapter.BuildToolsPromptEnvelope(openaiTools, toolChoiceStr, true, delimiter)
		}
	}

	// 2. Inject System Instructions and Tool Directives into <important> envelope at the top
	var systemBlocks []string
	systemText := strings.TrimSpace(req.System.String())
	if systemText != "" {
		systemBlocks = append(systemBlocks, systemText)
	}
	if toolsBlock != "" {
		systemBlocks = append(systemBlocks, strings.TrimSpace(toolsBlock))
	}
	if len(systemBlocks) > 0 {
		fmt.Fprintf(&historyBuilder, "<important>\n%s\n</important>\n\n", strings.Join(systemBlocks, "\n\n"))
	}

	// 3. Reconstitute Multi-Turn Conversational History
	var lastTurnHadToolResults bool
	for _, msg := range req.Messages {
		role := strings.ToLower(msg.Role)

		switch role {
		case "user":
			lastTurnHadToolResults = t.formatUserMessage(&historyBuilder, msg)
		case "assistant":
			t.formatAssistantMessage(&historyBuilder, msg, delimiter)
			lastTurnHadToolResults = false
		}
	}

	// 4. Append Active Continuation Cue to prevent premature agent loops from stalling
	if len(req.Messages) > 0 {
		lastMsg := req.Messages[len(req.Messages)-1]
		lastRole := strings.ToLower(lastMsg.Role)
		if lastRole == "user" {
			historyBuilder.WriteString(t.virtualAdapter.BuildActiveContinuationCue(lastTurnHadToolResults, delimiter))
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

	return []acp.PromptBlock{
		{
			Type: "text",
			Text: finalPrompt,
		},
	}
}

// formatUserMessage formats user turns, handling text, images, and tool_result blocks.
// Returns true if the message contained tool_result blocks.
func (t *Translator) formatUserMessage(sb *strings.Builder, msg Message) bool {
	if len(msg.Content.Blocks) == 0 {
		if msg.Content.Text != "" {
			fmt.Fprintf(sb, "User: %s\n\n", msg.Content.Text)
		}
		return false
	}

	var textParts []string
	var toolResults []string
	var hasToolResults bool

	for _, block := range msg.Content.Blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				textParts = append(textParts, block.Text)
			}
		case "tool_result":
			hasToolResults = true
			resStr := ""
			switch c := block.Content.(type) {
			case string:
				resStr = c
			default:
				raw, _ := json.Marshal(c)
				resStr = string(raw)
			}
			if block.IsError {
				toolResults = append(toolResults, fmt.Sprintf("Tool Error (id: %s): %s", block.ToolUseID, resStr))
			} else {
				toolResults = append(toolResults, fmt.Sprintf("Tool Output (id: %s): %s", block.ToolUseID, resStr))
			}
		case "image":
			textParts = append(textParts, "[Image Attached]")
		}
	}

	var userParts []string
	if len(textParts) > 0 {
		userParts = append(userParts, strings.Join(textParts, "\n\n"))
	}
	if len(toolResults) > 0 {
		userParts = append(userParts, "[Tool Execution Results]\n"+strings.Join(toolResults, "\n"))
	}

	if len(userParts) > 0 {
		fmt.Fprintf(sb, "User: %s\n\n", strings.Join(userParts, "\n\n"))
	}
	return hasToolResults
}

// formatAssistantMessage formats assistant turns, reconstructing thinking blocks and tool_use tags.
func (t *Translator) formatAssistantMessage(sb *strings.Builder, msg Message, delimiter string) {
	if len(msg.Content.Blocks) == 0 {
		if msg.Content.Text != "" {
			fmt.Fprintf(sb, "Assistant: %s\n\n", msg.Content.Text)
		}
		return
	}

	var textParts []string
	var toolCalls []string

	for _, block := range msg.Content.Blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				textParts = append(textParts, block.Text)
			}
		case "thinking":
			// If previous assistant turn included thinking, preserve summary context
			if block.Thinking != "" {
				textParts = append(textParts, fmt.Sprintf("[Reasoning: %s]", block.Thinking))
			}
		case "tool_use":
			rawArgs := string(block.Input)
			if rawArgs == "" || rawArgs == "null" {
				rawArgs = "{}"
			}
			rawCall, _ := json.Marshal(map[string]any{
				"id":        block.ID,
				"name":      block.Name,
				"arguments": json.RawMessage(rawArgs),
			})
			toolCalls = append(toolCalls, fmt.Sprintf("<%s>%s</%s>", delimiter, string(rawCall), delimiter))
		}
	}

	contentStr := strings.Join(textParts, "\n")
	callsStr := strings.Join(toolCalls, "\n")

	if contentStr != "" && callsStr != "" {
		fmt.Fprintf(sb, "Assistant: %s\n%s\n\n", contentStr, callsStr)
	} else if contentStr != "" {
		fmt.Fprintf(sb, "Assistant: %s\n\n", contentStr)
	} else if callsStr != "" {
		fmt.Fprintf(sb, "Assistant: %s\n\n", callsStr)
	}
}

// GenerateThinkingSignature generates a deterministic signature string for an assistant thinking trace.
func GenerateThinkingSignature(thinkingText string) string {
	h := hmac.New(sha256.New, []byte("agy-gateway-thinking-secret"))
	h.Write([]byte(thinkingText))
	return "sig_" + hex.EncodeToString(h.Sum(nil))[:32]
}
