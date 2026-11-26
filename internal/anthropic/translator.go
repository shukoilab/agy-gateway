package anthropic

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"agy-gateway/internal/acp"
)

// Translator converts Anthropic MessageRequest structures into ACP prompt payloads.
//
// @purpose Maps Anthropic system prompts and multi-turn conversational history into standardized ACP PromptBlocks.
// @rationale Eliminates prompt-injected tool XML envelopes and synthetic system directives. Tools are passed natively
// to ACP session/new via LoopbackServer mcpServers.
// @preconditions req must be non-nil.
// @invariants Thread-safe, pure deterministic transformation.
type Translator struct{}

// NewTranslator creates an initialized Translator.
func NewTranslator() *Translator {
	return &Translator{}
}

// Translate converts the Anthropic conversation into an ACP prompt payload.
// System instructions are returned separately and forwarded by the server layer on the append-only
// carrier (`_meta.antigravity.systemPrompt.append`); the spec-level `systemPrompt` field is
// deliberately not used because the agent accepts and discards it.
func (t *Translator) Translate(req *MessageRequest, _ string) (string, []acp.PromptBlock) {
	systemPrompt := strings.TrimSpace(req.System.String())

	// Fast path: single message from user
	if len(req.Messages) == 1 && strings.ToLower(req.Messages[0].Role) == "user" {
		msg := req.Messages[0]
		if len(msg.Content.Blocks) == 0 && msg.Content.Text != "" {
			return systemPrompt, []acp.PromptBlock{
				{
					Type: "text",
					Text: strings.TrimSpace(msg.Content.Text),
				},
			}
		}
	}

	var historyBuilder strings.Builder
	for _, msg := range req.Messages {
		role := strings.ToLower(msg.Role)
		switch role {
		case "user":
			t.formatUserMessage(&historyBuilder, msg)
		case "assistant":
			t.formatAssistantMessage(&historyBuilder, msg)
		}
	}

	finalPrompt := strings.TrimSpace(historyBuilder.String())
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

// cleanAnthropicToolContent sanitizes tool execution result text and strips duplicate turn headers.
func cleanAnthropicToolContent(content, toolUseID string) string {
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
	if toolUseID != "" {
		expectedPrefix := fmt.Sprintf("Tool Output (id: %s):", toolUseID)
		if strings.HasPrefix(s, expectedPrefix) {
			s = strings.TrimSpace(strings.TrimPrefix(s, expectedPrefix))
		}
		expectedErrPrefix := fmt.Sprintf("Tool Error (id: %s):", toolUseID)
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

// formatUserMessage formats user turns, handling text, images, and tool_result blocks.
func (t *Translator) formatUserMessage(sb *strings.Builder, msg Message) bool {
	if len(msg.Content.Blocks) == 0 {
		if msg.Content.Text != "" {
			trimmed := strings.TrimSpace(msg.Content.Text)
			if strings.HasPrefix(trimmed, "User: [Tool Execution Results]") || strings.HasPrefix(trimmed, "[Tool Execution Results]") {
				cleaned := cleanAnthropicToolContent(trimmed, "")
				curr := strings.TrimSpace(sb.String())
				if !strings.HasSuffix(curr, "[Tool Execution Results]") {
					sb.WriteString("User: [Tool Execution Results]\n")
				}
				sb.WriteString(cleaned)
				sb.WriteString("\n\n")
				return true
			}
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
			resStr = cleanAnthropicToolContent(resStr, block.ToolUseID)
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

// formatAssistantMessage formats assistant turns, reconstructing thinking blocks and native tool invocation summaries.
func (t *Translator) formatAssistantMessage(sb *strings.Builder, msg Message) {
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
			if block.Thinking != "" {
				textParts = append(textParts, fmt.Sprintf("[Reasoning: %s]", block.Thinking))
			}
		case "tool_use":
			rawArgs := string(block.Input)
			if rawArgs == "" || rawArgs == "null" {
				rawArgs = "{}"
			}
			toolCalls = append(toolCalls, fmt.Sprintf("[Assistant called tool %s with arguments: %s]", block.Name, rawArgs))
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
