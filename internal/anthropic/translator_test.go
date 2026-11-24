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
	promptBlocks := translator.Translate(req, "tool_call")

	if len(promptBlocks) != 1 {
		t.Fatalf("expected 1 prompt block, got %d", len(promptBlocks))
	}

	prompt := promptBlocks[0].Text

	// Verify System Instructions enclosed in <important> tags
	if !strings.Contains(prompt, "<important>\nYou are a senior systems engineer.\n</important>") {
		t.Errorf("prompt missing <important> system tags:\n%s", prompt)
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

	// Verify Consumer Tool-Calling Directives
	if !strings.Contains(prompt, "# CONSUMER TOOL-CALLING DIRECTIVES") {
		t.Errorf("prompt missing tool calling directives")
	}
}
