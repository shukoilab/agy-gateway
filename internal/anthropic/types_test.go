package anthropic_test

import (
	"encoding/json"
	"testing"

	"agy-gateway/internal/anthropic"
)

func TestPolymorphicMessageContent_String(t *testing.T) {
	raw := `{"role": "user", "content": "Hello Claude"}`
	var msg anthropic.Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("failed to unmarshal string content: %v", err)
	}

	if msg.Role != "user" {
		t.Errorf("expected role 'user', got %q", msg.Role)
	}
	if msg.ContentString() != "Hello Claude" {
		t.Errorf("expected content 'Hello Claude', got %q", msg.ContentString())
	}
	if len(msg.Content.Blocks) != 1 || msg.Content.Blocks[0].Type != "text" {
		t.Fatalf("expected 1 synthetic text block, got %+v", msg.Content.Blocks)
	}
}

func TestPolymorphicMessageContent_Blocks(t *testing.T) {
	raw := `{
		"role": "assistant",
		"content": [
			{"type": "thinking", "thinking": "Let's analyze...", "signature": "sig123"},
			{"type": "text", "text": "Here is the answer."},
			{"type": "tool_use", "id": "toolu_1", "name": "calc", "input": {"x": 10}}
		]
	}`
	var msg anthropic.Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("failed to unmarshal block content: %v", err)
	}

	if msg.Role != "assistant" {
		t.Errorf("expected role 'assistant', got %q", msg.Role)
	}
	if len(msg.Content.Blocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(msg.Content.Blocks))
	}
	if msg.Content.Blocks[0].Type != "thinking" || msg.Content.Blocks[0].Thinking != "Let's analyze..." {
		t.Errorf("unexpected thinking block: %+v", msg.Content.Blocks[0])
	}
	if msg.Content.Blocks[1].Type != "text" || msg.Content.Blocks[1].Text != "Here is the answer." {
		t.Errorf("unexpected text block: %+v", msg.Content.Blocks[1])
	}
	if msg.Content.Blocks[2].Type != "tool_use" || msg.Content.Blocks[2].Name != "calc" {
		t.Errorf("unexpected tool_use block: %+v", msg.Content.Blocks[2])
	}
	if msg.ContentString() != "Here is the answer." {
		t.Errorf("expected flattened text 'Here is the answer.', got %q", msg.ContentString())
	}
}

func TestPolymorphicSystemPrompt_String(t *testing.T) {
	raw := `{"model": "claude-3-5-sonnet", "max_tokens": 1024, "system": "System instructions here", "messages": []}`
	var req anthropic.MessageRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("failed to unmarshal string system prompt: %v", err)
	}

	if req.System.String() != "System instructions here" {
		t.Errorf("expected system string, got %q", req.System.String())
	}
}

func TestPolymorphicSystemPrompt_Blocks(t *testing.T) {
	raw := `{
		"model": "claude-3-5-sonnet",
		"max_tokens": 1024,
		"system": [
			{"type": "text", "text": "Part 1 of system prompt."},
			{"type": "text", "text": "Part 2 of system prompt."}
		],
		"messages": []
	}`
	var req anthropic.MessageRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("failed to unmarshal array system prompt: %v", err)
	}

	expected := "Part 1 of system prompt.\n\nPart 2 of system prompt."
	if req.System.String() != expected {
		t.Errorf("expected %q, got %q", expected, req.System.String())
	}
}
