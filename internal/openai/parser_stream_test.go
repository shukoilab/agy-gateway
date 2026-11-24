package openai

import (
	"strings"
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

func TestStreamParser_SplitCloseTagAllOffsets(t *testing.T) {
	fullPayload := `Before <tool_call>{"id": "call_split", "name": "read", "arguments": {"path": "main.go"}}</tool_call> After`

	for splitIdx := 0; splitIdx <= len(fullPayload); splitIdx++ {
		parser := NewStreamParser("tool_call")
		chunk1 := fullPayload[:splitIdx]
		chunk2 := fullPayload[splitIdx:]

		d1, tc1 := parser.ParseChunk(chunk1)
		d2, tc2 := parser.ParseChunk(chunk2)
		d3, tc3 := parser.Flush()

		allText := d1 + d2 + d3
		allTools := append(append(tc1, tc2...), tc3...)

		if len(allTools) != 1 {
			t.Fatalf("Split at %d: expected 1 tool call, got %d (text: %q)", splitIdx, len(allTools), allText)
		}
		if allTools[0].Function.Name != "read" {
			t.Fatalf("Split at %d: expected tool name 'read', got %q", splitIdx, allTools[0].Function.Name)
		}
		if allText != "Before  After" {
			t.Fatalf("Split at %d: expected text 'Before  After', got %q", splitIdx, allText)
		}
	}
}

func TestStreamParser_ByteByByteStreaming(t *testing.T) {
	fullPayload := `Intro <tool_call>{"id": "call_byte", "name": "execute", "arguments": {"cmd": "pwd"}}</tool_call> Outro`
	parser := NewStreamParser("tool_call")

	var collectedText string
	var collectedTools []ToolCall

	for i := 0; i < len(fullPayload); i++ {
		d, tc := parser.ParseChunk(string(fullPayload[i]))
		collectedText += d
		collectedTools = append(collectedTools, tc...)
	}
	fd, ftc := parser.Flush()
	collectedText += fd
	collectedTools = append(collectedTools, ftc...)

	if len(collectedTools) != 1 {
		t.Fatalf("Byte-by-byte: expected 1 tool call, got %d", len(collectedTools))
	}
	if collectedTools[0].Function.Name != "execute" {
		t.Fatalf("Byte-by-byte: expected 'execute', got %q", collectedTools[0].Function.Name)
	}
	if collectedText != "Intro  Outro" {
		t.Fatalf("Byte-by-byte: expected text 'Intro  Outro', got %q", collectedText)
	}
}

func TestStreamParser_StripHallucinatedPrefix(t *testing.T) {
	t.Run("Assistant prefix stripped in single chunk", func(t *testing.T) {
		parser := NewStreamParser("tool_call")
		d, tc := parser.ParseChunk("Assistant: Here is the plan.")
		fd, _ := parser.Flush()
		total := d + fd
		if total != "Here is the plan." || len(tc) != 0 {
			t.Fatalf("expected 'Here is the plan.', got %q", total)
		}
	})

	t.Run("User tool results prefix stripped", func(t *testing.T) {
		parser := NewStreamParser("tool_call")
		d, tc := parser.ParseChunk("User: [Tool Execution Results]\n<tool_call>{\"name\":\"read\",\"arguments\":{\"path\":\"foo.txt\"}}</tool_call>")
		fd, ftc := parser.Flush()
		total := d + fd
		allTools := append(tc, ftc...)
		if total != "" {
			t.Fatalf("expected empty text, got %q", total)
		}
		if len(allTools) != 1 || allTools[0].Function.Name != "read" {
			t.Fatalf("expected 1 read tool, got %+v", allTools)
		}
	})

	t.Run("Prefix split across chunks", func(t *testing.T) {
		parser := NewStreamParser("tool_call")
		d1, _ := parser.ParseChunk("Assis")
		if d1 != "" {
			t.Fatalf("expected empty delta while buffering prefix, got %q", d1)
		}
		d2, _ := parser.ParseChunk("tant: Hello world!")
		fd, _ := parser.Flush()
		total := d1 + d2 + fd
		if total != "Hello world!" {
			t.Fatalf("expected 'Hello world!', got %q", total)
		}
	})

	t.Run("Stop sequence triggers on simulated turn after tool call", func(t *testing.T) {
		parser := NewStreamParser("tool_call")
		d1, tc1 := parser.ParseChunk(`<tool_call>{"id":"call_chk_git_01","name":"bash","arguments":{"command":"git status"}}</tool_call>`)
		if len(tc1) != 1 || tc1[0].ID != "call_chk_git_01" {
			t.Fatalf("expected call_chk_git_01, got %+v", tc1)
		}

		// Model attempts to hallucinate next user turn and subsequent fake tool call
		d2, tc2 := parser.ParseChunk("\nUser: [Tool Execution Results]\nTool Output (id: call_chk_git_01): M src/file.js\n<tool_call>{\"id\":\"call_fake_2\",\"name\":\"bash\",\"arguments\":{\"command\":\"yarn test\"}}</tool_call>")
		fd, ftc := parser.Flush()

		totalText := d1 + d2 + fd
		allTools := append(append(tc1, tc2...), ftc...)

		if !parser.IsTerminated() {
			t.Fatalf("expected parser to be terminated after encountering hallucinated turn")
		}
		if len(allTools) != 1 {
			t.Fatalf("expected strictly 1 tool call (fake call should be dropped), got %d: %+v", len(allTools), allTools)
		}
		if totalText != "" {
			t.Fatalf("expected zero leaked text, got %q", totalText)
		}
	})

	t.Run("Strip leading Tool Output lines without tool call", func(t *testing.T) {
		parser := NewStreamParser("tool_call")
		d, tc := parser.ParseChunk("Tool Output (id: call_rd_01): Read file lines 1-100\nHello, task is complete.")
		fd, _ := parser.Flush()
		total := d + fd
		if total != "Hello, task is complete." {
			t.Fatalf("expected 'Hello, task is complete.', got %q", total)
		}
		if len(tc) != 0 {
			t.Fatalf("expected 0 tool calls, got %d", len(tc))
		}
	})
}

func TestParseCompleteResponse_TruncateSimulatedTurn(t *testing.T) {
	raw := `<tool_call>{"id":"call_chk_git_01","name":"bash","arguments":{"command":"git status"}}</tool_call>
User: [Tool Execution Results]
Tool Output (id: call_chk_git_01): M src/modules/free/utils/rate-limit-detector.js
<tool_call>{"id":"call_yarn_test_base","name":"bash","arguments":{"command":"yarn test"}}</tool_call>
User: [Tool Execution Results]
Tool Output (id: call_yarn_test_base): 95 passed`

	text, tools, hasTools := ParseCompleteResponse(raw, "tool_call")
	if !hasTools {
		t.Fatalf("expected hasTools to be true")
	}
	if len(tools) != 1 {
		t.Fatalf("expected exactly 1 tool call, got %d: %+v", len(tools), tools)
	}
	if tools[0].ID != "call_chk_git_01" {
		t.Fatalf("expected call_chk_git_01, got %q", tools[0].ID)
	}
	if text != "" {
		t.Fatalf("expected clean text to be empty, got %q", text)
	}
}

func TestStreamParser_MarkdownCodeFencesAndFallbackKeys(t *testing.T) {
	parser := NewStreamParser("tool_call")
	chunk := "<tool_call>```json\n{\n  \"call_id\": \"call_fenced_1\",\n  \"action\": \"glob\",\n  \"parameters\": {\"pattern\": \"*.go\"}\n}\n```</tool_call>"
	d, tools := parser.ParseChunk(chunk)
	fd, ftools := parser.Flush()
	allTools := append(tools, ftools...)
	if len(allTools) != 1 {
		t.Fatalf("expected 1 tool call, got %d (text: %q)", len(allTools), d+fd)
	}
	if allTools[0].Function.Name != "glob" {
		t.Fatalf("expected tool name 'glob', got %q", allTools[0].Function.Name)
	}
	if allTools[0].ID != "call_fenced_1" {
		t.Fatalf("expected call_fenced_1, got %q", allTools[0].ID)
	}
}

func TestStreamParser_MalformedToolCallEmittedAsText(t *testing.T) {
	parser := NewStreamParser("tool_call")
	chunk := "Looking into this:\n<tool_call>\nAssistant:\nBroken JSON\n</tool_call>\nNext line"
	d, tools := parser.ParseChunk(chunk)
	fd, ftools := parser.Flush()
	allText := d + fd
	allTools := append(tools, ftools...)
	if len(allTools) != 0 {
		t.Fatalf("expected 0 tool calls, got %d", len(allTools))
	}
	if !strings.Contains(allText, "Looking into this:") || !strings.Contains(allText, "Broken JSON") || !strings.Contains(allText, "Next line") {
		t.Fatalf("expected malformed payload to be preserved as text, got %q", allText)
	}
}
