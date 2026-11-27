package anthropic_test

import (
	"encoding/json"
	"strings"
	"testing"

	"agy-gateway/internal/anthropic"
)

func TestTranslator_MultiTurnHistoryAndCleanSystemPrompt(t *testing.T) {
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
	}

	translator := anthropic.NewTranslator()
	sysPrompt, promptBlocks := translator.Translate(req, "tool_call")

	if len(promptBlocks) != 1 {
		t.Fatalf("expected 1 prompt block, got %d", len(promptBlocks))
	}

	prompt := promptBlocks[0].Text

	// Verify System Instructions are clean
	if !strings.Contains(sysPrompt, "You are a senior systems engineer.") {
		t.Errorf("sysPrompt missing system instructions:\n%s", sysPrompt)
	}
	if strings.Contains(sysPrompt, "# CONSUMER TOOL-CALLING DIRECTIVES") {
		t.Errorf("sysPrompt contains unexpected tool calling directives:\n%s", sysPrompt)
	}

	// Verify prompt contains clean conversational turns
	if !strings.Contains(prompt, "User: Check the status of cluster alpha.") {
		t.Errorf("prompt missing user turn 1")
	}
	if !strings.Contains(prompt, "Tool Output (id: toolu_cluster_1):") {
		t.Errorf("prompt missing user tool_result output")
	}
	if strings.Contains(prompt, "[SYSTEM DIRECTIVE:") {
		t.Errorf("prompt contains unexpected injected system directive:\n%s", prompt)
	}
}

func TestTranslator_SingleUserFastPath(t *testing.T) {
	translator := anthropic.NewTranslator()

	req := &anthropic.MessageRequest{
		Model: "claude-3-5-sonnet",
		System: anthropic.SystemPrompt{
			Text: "You are a systems engineer.",
		},
		Messages: []anthropic.Message{
			{
				Role: "user",
				Content: anthropic.MessageContent{
					Text: "Inspect service health",
				},
			},
		},
	}

	sysPrompt, promptBlocks := translator.Translate(req, "tool_call")
	if sysPrompt != "You are a systems engineer." {
		t.Fatalf("unexpected sysPrompt: %s", sysPrompt)
	}
	if len(promptBlocks) != 1 || promptBlocks[0].Text != "Inspect service health" {
		t.Fatalf("unexpected promptBlocks: %+v", promptBlocks)
	}
}

// TestTranslator_ToolUseCarriesMatchingID asserts the historical assistant tool_use declares the
// same id as its tool_result, so the model can tell executed calls from proposed ones.
func TestTranslator_ToolUseCarriesMatchingID(t *testing.T) {
	translator := anthropic.NewTranslator()

	req := &anthropic.MessageRequest{
		Model: "claude-sonnet-4-6",
		Messages: []anthropic.Message{
			{Role: "user", Content: anthropic.MessageContent{
				Blocks: []anthropic.ContentBlock{{Type: "text", Text: "list files"}},
			}},
			{Role: "assistant", Content: anthropic.MessageContent{
				Blocks: []anthropic.ContentBlock{{
					Type:  "tool_use",
					ID:    "toolu_abc123",
					Name:  "terminal",
					Input: json.RawMessage(`{"command":"ls"}`),
				}},
			}},
			{Role: "user", Content: anthropic.MessageContent{
				Blocks: []anthropic.ContentBlock{{
					Type:      "tool_result",
					ToolUseID: "toolu_abc123",
					Content:   "a.go\nb.go",
				}},
			}},
		},
	}

	_, blocks := translator.Translate(req, "tool_call")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	prompt := blocks[0].Text

	if !strings.Contains(prompt, "[Assistant called tool terminal (id: toolu_abc123) with arguments:") {
		t.Fatalf("assistant tool_use does not carry its id:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Tool Output (id: toolu_abc123):") {
		t.Fatalf("tool_result id missing:\n%s", prompt)
	}
}
