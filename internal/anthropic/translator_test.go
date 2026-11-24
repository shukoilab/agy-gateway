package anthropic_test

import (
	"encoding/json"
	"strings"
	"testing"

	"agy-gateway/internal/anthropic"
)

func TestTranslator_MultiTurnHistoryAndTools(t *testing.T) {
	req := &anthropic.MessageRequest{
		Model:     "claude-3-5-sonnet",
		MaxTokens: 2048,
		System: anthropic.SystemPrompt{
			Text: "You are a senior systems engineer.",
		},
		Messages: []anthropic.Message{
			{
				Role: "user",
				Content: anthropic.MessageContent{
					Text: "Check the status of cluster alpha.",
				},
			},
			{
				Role: "assistant",
				Content: anthropic.MessageContent{
					Blocks: []anthropic.ContentBlock{
						{
							Type:     "thinking",
							Thinking: "I need to query cluster status using the tool.",
						},
						{
							Type: "text",
							Text: "Checking cluster alpha...",
						},
						{
							Type:  "tool_use",
							ID:    "toolu_cluster_1",
							Name:  "query_cluster",
							Input: json.RawMessage(`{"cluster_id": "alpha"}`),
						},
					},
				},
			},
			{
				Role: "user",
				Content: anthropic.MessageContent{
					Blocks: []anthropic.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "toolu_cluster_1",
							Content:   `{"status": "degraded", "active_nodes": 4, "total_nodes": 5}`,
						},
						{
							Type: "text",
							Text: "Here is the cluster output. What is wrong?",
						},
					},
				},
			},
		},
		Tools: []anthropic.Tool{
			{
				Name:        "query_cluster",
				Description: "Queries cluster health",
				InputSchema: json.RawMessage(`{"type": "object", "properties": {"cluster_id": {"type": "string"}}}`),
			},
		},
	}

	translator := anthropic.NewTranslator()
	sysPrompt, promptBlocks := translator.Translate(req, "tool_call")

	if len(promptBlocks) != 1 {
		t.Fatalf("expected 1 prompt block, got %d", len(promptBlocks))
	}

	prompt := promptBlocks[0].Text

	// Verify System Instructions and Tool Directives in sysPrompt
	if !strings.Contains(sysPrompt, "You are a senior systems engineer.") {
		t.Errorf("sysPrompt missing system instructions:\n%s", sysPrompt)
	}
	if !strings.Contains(sysPrompt, "# CONSUMER TOOL-CALLING DIRECTIVES") {
		t.Errorf("sysPrompt missing tool calling directives:\n%s", sysPrompt)
	}
	if !strings.Contains(sysPrompt, "query_cluster") {
		t.Errorf("sysPrompt missing query_cluster tool definition:\n%s", sysPrompt)
	}

	// Verify prompt is pure conversational turns without artificial <important> wrapper
	if strings.Contains(prompt, "<important>") {
		t.Errorf("prompt contains unexpected <important> wrapper:\n%s", prompt)
	}

	// Verify Multi-Turn conversation reconstruction
	if !strings.Contains(prompt, "User: Check the status of cluster alpha.") {
		t.Errorf("prompt missing user turn 1")
	}
	if !strings.Contains(prompt, "<tool_call>") || !strings.Contains(prompt, "toolu_cluster_1") {
		t.Errorf("prompt missing assistant tool_call envelope")
	}
	if !strings.Contains(prompt, "Tool Output (id: toolu_cluster_1):") {
		t.Errorf("prompt missing user tool_result output")
	}
	if !strings.Contains(prompt, "[Tool Execution Results]") {
		t.Errorf("prompt missing [Tool Execution Results] turn header")
	}

	// Verify Active Execution Loop continuation cue
	if !strings.Contains(prompt, "[SYSTEM DIRECTIVE: You are in an active execution loop.") {
		t.Errorf("prompt missing active execution loop directive:\n%s", prompt)
	}
	if !strings.HasSuffix(prompt, "Assistant: ") {
		t.Errorf("expected prompt to end with 'Assistant: ', got:\n%s", prompt)
	}
}

func TestTranslator_ToolOutputDeduplication(t *testing.T) {
	translator := anthropic.NewTranslator()

	req := &anthropic.MessageRequest{
		Model: "claude-3-5-sonnet",
		Messages: []anthropic.Message{
			{
				Role: "user",
				Content: anthropic.MessageContent{
					Text: "Check cluster",
				},
			},
			{
				Role: "assistant",
				Content: anthropic.MessageContent{
					Blocks: []anthropic.ContentBlock{
						{
							Type: "tool_use",
							ID:   "toolu_1",
							Name: "status",
						},
					},
				},
			},
			{
				Role: "user",
				Content: anthropic.MessageContent{
					Blocks: []anthropic.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "toolu_1",
							Content:   "User: [Tool Execution Results]\nTool Output (id: toolu_1): all systems green",
						},
					},
				},
			},
		},
	}

	_, promptBlocks := translator.Translate(req, "tool_call")
	prompt := promptBlocks[0].Text

	count := strings.Count(prompt, "[Tool Execution Results]")
	if count != 1 {
		t.Fatalf("expected exactly 1 '[Tool Execution Results]', got %d in prompt:\n%s", count, prompt)
	}

	callPrefixCount := strings.Count(prompt, "Tool Output (id: toolu_1):")
	if callPrefixCount != 1 {
		t.Fatalf("expected exactly 1 'Tool Output (id: toolu_1):', got %d in prompt:\n%s", callPrefixCount, prompt)
	}
}
