package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"agy-gateway/internal/mcp"
)

func TestTranslator_MultiTurnHistoryAndTools(t *testing.T) {
	translator := NewTranslator()

	req := &ChatCompletionRequest{
		Model: "gemini-3.8-flash",
		Messages: []ChatMessage{
			{Role: "system", Content: "You are a helpful coding assistant."},
			{Role: "user", Content: "What is 2+2?"},
			{Role: "assistant", Content: "4"},
			{Role: "user", Content: "Calculate shipping now."},
		},
		Tools: []mcp.OpenAIToolDefinition{
			{
				Type: "function",
				Function: mcp.FunctionSchema{
					Name:        "calculate_shipping",
					Description: "Calculate shipping cost",
					Parameters:  json.RawMessage(`{"type":"object"}`),
				},
			},
			// Duplicate tool with same name should be deduplicated
			{
				Type: "function",
				Function: mcp.FunctionSchema{
					Name:        "calculate_shipping",
					Description: "Duplicate calculate shipping",
					Parameters:  json.RawMessage(`{"type":"object"}`),
				},
			},
		},
	}

	promptBlocks := translator.Translate(req, "tool_call")

	if len(promptBlocks) != 1 {
		t.Fatalf("expected 1 prompt block, got %d", len(promptBlocks))
	}

	promptText := promptBlocks[0].Text

	// Verify system prompt and tools directives are safely wrapped in <important>...</important> XML tag
	if !strings.Contains(promptText, "<important>") || !strings.Contains(promptText, "You are a helpful coding assistant.") {
		t.Fatalf("missing or incorrect <important> system prompt wrapper in prompt: %s", promptText)
	}
	if !strings.Contains(promptText, "</important>") {
		t.Fatalf("missing closing </important> tag in prompt: %s", promptText)
	}

	if !strings.Contains(promptText, "User: What is 2+2?") {
		t.Fatalf("missing user turn in prompt: %s", promptText)
	}
	if !strings.Contains(promptText, "Assistant: 4") {
		t.Fatalf("missing assistant turn in prompt: %s", promptText)
	}
	if !strings.Contains(promptText, "User: Calculate shipping now.") {
		t.Fatalf("missing latest user turn in prompt: %s", promptText)
	}
	if !strings.Contains(promptText, "<tool_call>") {
		t.Fatalf("missing tool prompt directives in prompt: %s", promptText)
	}

	// Verify prompt primes assistant completion
	if !strings.HasSuffix(promptText, "Assistant: ") {
		t.Fatalf("expected prompt to end with 'Assistant: ', got: %s", promptText)
	}

	// Verify deduplication: [Tool 1] should appear, but NOT [Tool 2]
	if !strings.Contains(promptText, "[Tool 1] calculate_shipping") {
		t.Fatalf("missing [Tool 1] in prompt: %s", promptText)
	}
	if strings.Contains(promptText, "[Tool 2]") {
		t.Fatalf("expected tool deduplication, but found [Tool 2] in prompt: %s", promptText)
	}
}

func TestTranslator_ToolExecutionResultsGroupingAndContinuation(t *testing.T) {
	translator := NewTranslator()

	req := &ChatCompletionRequest{
		Model: "gemini-3.8-flash",
		Messages: []ChatMessage{
			{Role: "user", Content: "Run diagnostics on server 1 and 2."},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{
						ID:   "call_1",
						Type: "function",
						Function: FunctionCallData{
							Name:      "check_server",
							Arguments: `{"server_id": 1}`,
						},
					},
					{
						ID:   "call_2",
						Type: "function",
						Function: FunctionCallData{
							Name:      "check_server",
							Arguments: `{"server_id": 2}`,
						},
					},
				},
			},
			{Role: "tool", ToolCallID: "call_1", Content: `{"status": "ok"}`},
			{Role: "tool", ToolCallID: "call_2", Content: `{"status": "degraded"}`},
		},
		Tools: []mcp.OpenAIToolDefinition{
			{
				Type: "function",
				Function: mcp.FunctionSchema{
					Name:        "check_server",
					Description: "Check server status",
					Parameters:  json.RawMessage(`{"type":"object"}`),
				},
			},
		},
	}

	promptBlocks := translator.Translate(req, "tool_call")
	if len(promptBlocks) != 1 {
		t.Fatalf("expected 1 prompt block, got %d", len(promptBlocks))
	}

	prompt := promptBlocks[0].Text

	// Verify tool results are grouped into a single User turn
	if !strings.Contains(prompt, "User: [Tool Execution Results]\nTool Output (id: call_1): {\"status\": \"ok\"}\nTool Output (id: call_2): {\"status\": \"degraded\"}") {
		t.Fatalf("expected grouped tool execution results turn in prompt:\n%s", prompt)
	}

	// Verify active execution loop directive is present
	if !strings.Contains(prompt, "[SYSTEM DIRECTIVE: You are in an active execution loop.") {
		t.Fatalf("expected active loop directive in prompt:\n%s", prompt)
	}

	// Verify ending with Assistant:
	if !strings.HasSuffix(prompt, "Assistant: ") {
		t.Fatalf("expected prompt to end with 'Assistant: ', got:\n%s", prompt)
	}
}
