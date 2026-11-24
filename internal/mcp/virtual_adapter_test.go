package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVirtualAdapter_BuildToolsPromptEnvelope(t *testing.T) {
	adapter := NewVirtualAdapter()

	tools := []OpenAIToolDefinition{
		{
			Type: "function",
			Function: FunctionSchema{
				Name:        "get_weather",
				Description: "Get current weather for a given location",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}}}`),
			},
		},
	}

	envelope := adapter.BuildToolsPromptEnvelope(tools, "auto", true, "tool_call")

	if !strings.Contains(envelope, "# CONSUMER TOOL-CALLING DIRECTIVES") {
		t.Fatalf("expected tool calling directives header in envelope")
	}
	if !strings.Contains(envelope, "API gateway") {
		t.Fatalf("expected API gateway directive in envelope")
	}
	if !strings.Contains(envelope, "external client workspace") {
		t.Fatalf("expected external client workspace directive in envelope")
	}
	if !strings.Contains(envelope, "<tool_call>") {
		t.Fatalf("expected tool delimiter tag in envelope")
	}
	if !strings.Contains(envelope, "get_weather") {
		t.Fatalf("expected tool name in envelope")
	}

	// Test required tool choice
	requiredEnv := adapter.BuildToolsPromptEnvelope(tools, "required", false, "tool_call")
	if !strings.Contains(requiredEnv, "You MUST call at least ONE tool") {
		t.Fatalf("expected required directive in envelope")
	}
	if !strings.Contains(requiredEnv, "Parallel tool execution is DISABLED") {
		t.Fatalf("expected parallel disabled directive in envelope")
	}

	// Test none tool choice
	noneEnv := adapter.BuildToolsPromptEnvelope(tools, "none", true, "tool_call")
	if !strings.Contains(noneEnv, "FORBIDDEN from calling any tools") {
		t.Fatalf("expected forbidden directive in envelope")
	}

	// Empty tools should return empty string
	emptyEnv := adapter.BuildToolsPromptEnvelope(nil, "auto", true, "tool_call")
	if emptyEnv != "" {
		t.Fatalf("expected empty string for nil tools")
	}

	// Test BuildActiveContinuationCue
	cueWithTools := adapter.BuildActiveContinuationCue(true, "tool_call")
	if !strings.Contains(cueWithTools, "[SYSTEM DIRECTIVE: You are in an active execution loop.") {
		t.Fatalf("expected active execution loop directive in cue: %s", cueWithTools)
	}
	if !strings.HasSuffix(cueWithTools, "Assistant: ") {
		t.Fatalf("expected cue to end with 'Assistant: ', got: %s", cueWithTools)
	}

	cueWithoutTools := adapter.BuildActiveContinuationCue(false, "tool_call")
	if cueWithoutTools != "Assistant: " {
		t.Fatalf("expected 'Assistant: ', got %q", cueWithoutTools)
	}
}
