package openai

import (
	"testing"
)

func TestStreamParser_PureText(t *testing.T) {
	parser := NewStreamParser("tool_call")

	delta1, tools1 := parser.ParseChunk("Hello ")
	if delta1 != "Hello " || len(tools1) != 0 {
		t.Fatalf("unexpected chunk 1 output: delta=%q, tools=%v", delta1, tools1)
	}

	delta2, tools2 := parser.ParseChunk("world!")
	if delta2 != "world!" || len(tools2) != 0 {
		t.Fatalf("unexpected chunk 2 output: delta=%q, tools=%v", delta2, tools2)
	}
}

func TestStreamParser_ToolCall(t *testing.T) {
	parser := NewStreamParser("tool_call")

	chunk1 := "Let me check: <tool_call>{\"id\": \"call_123\", \"name\": \"calc\", \"arguments\": {\"x\": 10}}</tool_call> Done!"
	delta, tools := parser.ParseChunk(chunk1)

	if delta != "Let me check:  Done!" {
		t.Fatalf("unexpected text delta: %q", delta)
	}

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tools))
	}

	if tools[0].Function.Name != "calc" {
		t.Fatalf("expected function name 'calc', got %q", tools[0].Function.Name)
	}

	if tools[0].Function.Arguments != `{"x":10}` {
		t.Fatalf("expected arguments '{\"x\":10}', got %q", tools[0].Function.Arguments)
	}
}

func TestParseCompleteResponse(t *testing.T) {
	raw := `Here is the calculation:
<tool_call>{"id": "call_abc", "name": "get_stock_price", "arguments": {"ticker": "GOOG"}}</tool_call>
Let me know if you need anything else.`

	text, tools, hasTools := ParseCompleteResponse(raw, "tool_call")
	if !hasTools {
		t.Fatalf("expected hasTools to be true")
	}

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tools))
	}

	if tools[0].Function.Name != "get_stock_price" {
		t.Fatalf("expected get_stock_price, got %q", tools[0].Function.Name)
	}

	if text != "Here is the calculation:\n\nLet me know if you need anything else." {
		t.Fatalf("unexpected clean text: %q", text)
	}
}
