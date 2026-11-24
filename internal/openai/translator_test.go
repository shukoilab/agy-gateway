package openai

import (
	"strings"
	"testing"
)

func TestTranslator_MultiTurnHistoryAndCleanSystemPrompt(t *testing.T) {
	translator := NewTranslator()

	req := &ChatCompletionRequest{
		Model: "gemini-3.8-flash",
		Messages: []ChatMessage{
			{Role: "system", Content: "You are a helpful coding assistant."},
			{Role: "user", Content: "What is 2+2?"},
			{Role: "assistant", Content: "4"},
			{Role: "user", Content: "Calculate shipping now."},
		},
	}

	sysPrompt, promptBlocks := translator.Translate(req, "tool_call")

	if len(promptBlocks) != 1 {
		t.Fatalf("expected 1 prompt block, got %d", len(promptBlocks))
	}

	promptText := promptBlocks[0].Text

	// Verify system prompt is clean and contains no injected tool directives
	if !strings.Contains(sysPrompt, "You are a helpful coding assistant.") {
		t.Fatalf("missing system instructions in sysPrompt: %s", sysPrompt)
	}
	if strings.Contains(sysPrompt, "# CONSUMER TOOL-CALLING DIRECTIVES") {
		t.Fatalf("unexpected prompt-injected tool directives in sysPrompt: %s", sysPrompt)
	}
	if strings.Contains(sysPrompt, "<tool_call>") {
		t.Fatalf("unexpected <tool_call> tags in sysPrompt: %s", sysPrompt)
	}

	// Verify conversational prompt contains clean user turns without artificial system directives
	if strings.Contains(promptText, "[SYSTEM DIRECTIVE:") {
		t.Fatalf("unexpected [SYSTEM DIRECTIVE:] in promptText: %s", promptText)
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
}

func TestTranslator_SingleUserFastPath(t *testing.T) {
	translator := NewTranslator()

	req := &ChatCompletionRequest{
		Model: "gemini-3.8-flash",
		Messages: []ChatMessage{
			{Role: "system", Content: "You are an expert Go reviewer."},
			{Role: "user", Content: "Review this function"},
		},
	}

	sysPrompt, promptBlocks := translator.Translate(req, "tool_call")
	if sysPrompt != "You are an expert Go reviewer." {
		t.Fatalf("unexpected sysPrompt: %s", sysPrompt)
	}
	if len(promptBlocks) != 1 || promptBlocks[0].Text != "Review this function" {
		t.Fatalf("unexpected promptBlocks: %+v", promptBlocks)
	}
}

func TestTranslator_ToolExecutionResultsGrouping(t *testing.T) {
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
				},
			},
			{Role: "tool", ToolCallID: "call_1", Content: `{"status": "ok"}`},
		},
	}

	_, promptBlocks := translator.Translate(req, "tool_call")
	if len(promptBlocks) != 1 {
		t.Fatalf("expected 1 prompt block, got %d", len(promptBlocks))
	}

	prompt := promptBlocks[0].Text

	// Verify tool results are present without injected directives
	if !strings.Contains(prompt, "Tool Output (id: call_1): {\"status\": \"ok\"}") {
		t.Fatalf("expected tool execution output in prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "[SYSTEM DIRECTIVE:") {
		t.Fatalf("unexpected injected directive in prompt:\n%s", prompt)
	}
}
