package anthropic_test

import (
	"strings"
	"testing"

	"agy-gateway/internal/anthropic"
)

func TestStreamParser_PureText(t *testing.T) {
	parser := anthropic.NewStreamParser("tool_call")

	chunk1, tools1 := parser.ParseChunk("Hello ")
	if chunk1 != "Hello " || len(tools1) != 0 {
		t.Fatalf("unexpected chunk1: %q, tools: %v", chunk1, tools1)
	}

	chunk2, tools2 := parser.ParseChunk("world!")
	if chunk2 != "world!" || len(tools2) != 0 {
		t.Fatalf("unexpected chunk2: %q, tools: %v", chunk2, tools2)
	}
}

func TestStreamParser_ToolCallExtraction(t *testing.T) {
	parser := anthropic.NewStreamParser("tool_call")

	chunk1, tools1 := parser.ParseChunk("Here is the answer: <tool_call>")
	if chunk1 != "Here is the answer: " || len(tools1) != 0 {
		t.Fatalf("unexpected chunk1: %q, tools: %v", chunk1, tools1)
	}

	chunk2, tools2 := parser.ParseChunk(`{"id": "call_123", "name": "get_weather", "arguments": {"location": "San Francisco"}}`)
	if chunk2 != "" || len(tools2) != 0 {
		t.Fatalf("unexpected chunk2: %q, tools: %v", chunk2, tools2)
	}

	chunk3, tools3 := parser.ParseChunk("</tool_call> Hope this helps!")
	if len(tools3) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tools3))
	}
	if tools3[0].Name != "get_weather" {
		t.Errorf("expected tool name 'get_weather', got %q", tools3[0].Name)
	}
	if !strings.HasPrefix(tools3[0].ID, "toolu_") {
		t.Errorf("expected tool id to have prefix 'toolu_', got %q", tools3[0].ID)
	}
	if chunk3 != " Hope this helps!" {
		t.Errorf("expected trailing text ' Hope this helps!', got %q", chunk3)
	}
}

func TestParseCompleteResponse(t *testing.T) {
	raw := `Here is the data:
<tool_call>
{"id": "call_abc", "name": "calc", "arguments": {"expr": "2+2"}}
</tool_call>
Finished execution.`

	cleanText, tools, hasTools := anthropic.ParseCompleteResponse(raw, "tool_call")
	if !hasTools {
		t.Fatalf("expected hasTools=true")
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Name != "calc" {
		t.Errorf("expected tool name 'calc', got %q", tools[0].Name)
	}
	expectedText := "Here is the data:\n\nFinished execution."
	if cleanText != expectedText {
		t.Errorf("expected cleanText %q, got %q", expectedText, cleanText)
	}
}

func TestStreamParser_FlushTrailingBuffer(t *testing.T) {
	parser := anthropic.NewStreamParser("tool_call")

	delta, tools := parser.ParseChunk("Some leading content <tool_")
	if delta != "Some leading content " || len(tools) != 0 {
		t.Fatalf("unexpected delta: %q, tools: %v", delta, tools)
	}

	flushDelta, flushTools := parser.Flush()
	if flushDelta != "<tool_" || len(flushTools) != 0 {
		t.Fatalf("expected flushed delta '<tool_', got %q", flushDelta)
	}
}

func TestStreamParser_FlushUnclosedToolCall(t *testing.T) {
	parser := anthropic.NewStreamParser("tool_call")

	delta, tools := parser.ParseChunk(`<tool_call>{"id": "toolu_12345", "name": "read_file", "arguments": {"path": "/etc/hosts"}}`)
	if delta != "" || len(tools) != 0 {
		t.Fatalf("unexpected delta: %q, tools: %v", delta, tools)
	}

	flushDelta, flushTools := parser.Flush()
	if flushDelta != "" {
		t.Fatalf("unexpected flush delta: %q", flushDelta)
	}
	if len(flushTools) != 1 {
		t.Fatalf("expected 1 flushed tool, got %d", len(flushTools))
	}
	if flushTools[0].Name != "read_file" {
		t.Fatalf("expected tool name 'read_file', got %q", flushTools[0].Name)
	}
	if string(flushTools[0].Input) != `{"path": "/etc/hosts"}` {
		t.Fatalf("expected input '{\"path\": \"/etc/hosts\"}', got %q", string(flushTools[0].Input))
	}
}
