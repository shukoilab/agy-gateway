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

// TestTranslator_ToolCallCarriesMatchingID asserts the historical assistant tool call
// declares the same id as its result. Without it the model cannot tell executed calls from
// proposed ones and loops re-issuing the same probe (see the harness loop regression).
func TestTranslator_ToolCallCarriesMatchingID(t *testing.T) {
	translator := NewTranslator()

	req := &ChatCompletionRequest{
		Model: "gemini-3.8-flash-low",
		Messages: []ChatMessage{
			{Role: "user", Content: "list files"},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{ID: "call_abc123", Type: "function", Function: FunctionCallData{
						Name: "terminal", Arguments: `{"command":"ls"}`,
					}},
				},
			},
			{Role: "tool", ToolCallID: "call_abc123", Content: "a.go\nb.go"},
		},
	}

	_, blocks := translator.Translate(req, "tool_call")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	prompt := blocks[0].Text

	if !strings.Contains(prompt, "[Assistant called tool terminal (id: call_abc123) with arguments:") {
		t.Fatalf("assistant tool call does not carry its id:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Tool Output (id: call_abc123):") {
		t.Fatalf("tool result id missing:\n%s", prompt)
	}
}

// TestTranslator_HistoryBudgetDropsOldestTurns asserts the budget bound is applied.
func TestTranslator_HistoryBudgetDropsOldestTurns(t *testing.T) {
	translator := NewTranslatorWithBudget(200)

	msgs := []ChatMessage{{Role: "user", Content: "BEGINNING OF CONVERSATION"}}
	for i := 0; i < 400; i++ {
		msgs = append(msgs,
			ChatMessage{Role: "user", Content: strings.Repeat("padding ", 20)},
			ChatMessage{Role: "assistant", Content: strings.Repeat("reply ", 20)},
		)
	}
	msgs = append(msgs, ChatMessage{Role: "user", Content: "FINAL QUESTION"})

	_, blocks := translator.Translate(&ChatCompletionRequest{Model: "m", Messages: msgs}, "tool_call")
	prompt := blocks[0].Text

	if !strings.Contains(prompt, "BEGINNING OF CONVERSATION") {
		t.Fatal("budget must preserve the opening objective")
	}
	if !strings.Contains(prompt, "FINAL QUESTION") {
		t.Fatal("budget dropped the newest turn")
	}
	if !strings.Contains(prompt, "[Earlier conversation omitted") {
		t.Fatal("budget must mark the omission")
	}
	if len(prompt) >= 200*4*3 {
		t.Fatalf("bounded prompt still oversized: ~%d tokens", len(prompt)/4)
	}
}
