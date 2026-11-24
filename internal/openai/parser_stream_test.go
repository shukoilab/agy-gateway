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

func TestStreamParser_FlushTrailingBuffer(t *testing.T) {
	parser := NewStreamParser("tool_call")

	// Model emits text ending with a prefix of <tool_call> e.g. "<tool_"
	delta, tools := parser.ParseChunk("Some leading content <tool_")
	if delta != "Some leading content " || len(tools) != 0 {
		t.Fatalf("unexpected delta: %q, tools: %v", delta, tools)
	}

	// Flush should release the held prefix
	flushDelta, flushTools := parser.Flush()
	if flushDelta != "<tool_" || len(flushTools) != 0 {
		t.Fatalf("expected flushed delta '<tool_', got %q", flushDelta)
	}
}

func TestStreamParser_FlushUnclosedToolCall(t *testing.T) {
	parser := NewStreamParser("tool_call")

	// Model emits tool call opening and JSON body, but stream ends before </tool_call>
	delta, tools := parser.ParseChunk(`<tool_call>{"id": "call_flush_1", "name": "run_command", "arguments": {"command": "ls -la"}}`)
	if delta != "" || len(tools) != 0 {
		t.Fatalf("unexpected delta: %q, tools: %v", delta, tools)
	}

	flushDelta, flushTools := parser.Flush()
	if flushDelta != "" {
		t.Fatalf("unexpected flush delta: %q", flushDelta)
	}
	if len(flushTools) != 1 {
		t.Fatalf("expected 1 flushed tool call, got %d", len(flushTools))
	}
	if flushTools[0].Function.Name != "run_command" {
		t.Fatalf("expected function 'run_command', got %q", flushTools[0].Function.Name)
	}
	if flushTools[0].Function.Arguments != `{"command":"ls -la"}` {
		t.Fatalf("expected arguments '{\"command\":\"ls -la\"}', got %q", flushTools[0].Function.Arguments)
	}
}

func TestStreamParser_StringifiedArguments(t *testing.T) {
	parser := NewStreamParser("tool_call")

	// Model emits arguments as a stringified JSON string instead of an object
	chunk := `<tool_call>{"id": "call_str_1", "name": "run_bash", "arguments": "{\"flag\": true}"}</tool_call>`
	_, tools := parser.ParseChunk(chunk)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tools))
	}
	if tools[0].Function.Arguments != `{"flag": true}` {
		t.Fatalf("expected raw string arguments to be preserved, got %q", tools[0].Function.Arguments)
	}
}
