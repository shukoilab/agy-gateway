package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// VirtualAdapter bridges OpenAI function calling definitions into ACP prompt envelopes.
type VirtualAdapter struct{}

// NewVirtualAdapter creates a new VirtualAdapter.
func NewVirtualAdapter() *VirtualAdapter {
	return &VirtualAdapter{}
}

// BuildToolsPromptEnvelope formats OpenAI tools into a strict prompt instruction block.
func (va *VirtualAdapter) BuildToolsPromptEnvelope(tools []OpenAIToolDefinition, toolChoice string, parallel bool, delimiter string) string {
	if len(tools) == 0 {
		return ""
	}
	if delimiter == "" {
		delimiter = "tool_call"
	}

	var sb strings.Builder
	sb.WriteString("\n\n# CONSUMER TOOL-CALLING DIRECTIVES\n")
	sb.WriteString("You have access to the following consumer-provided tools.\n")
	sb.WriteString("When calling a tool, you MUST emit your invocation enclosed inside delimiter tags:\n")
	fmt.Fprintf(&sb, "<%s>{\"id\": \"call_...\", \"name\": \"<function_name>\", \"arguments\": { ... }}</%s>\n", delimiter, delimiter)

	if !parallel {
		sb.WriteString("CRITICAL: Emit at most ONE tool call per response. Parallel tool execution is DISABLED.\n")
	} else {
		sb.WriteString("You may emit multiple parallel tool calls by repeating the delimiter tag for each call.\n")
	}

	switch strings.ToLower(toolChoice) {
	case "required":
		sb.WriteString("CRITICAL DIRECTIVE: You MUST call at least ONE tool in your response.\n")
	case "none":
		sb.WriteString("CRITICAL DIRECTIVE: You are FORBIDDEN from calling any tools. Answer with pure text only.\n")
	}

	sb.WriteString("\nAvailable Tools (JSON Schema):\n")
	for i, t := range tools {
		if t.Type != "function" {
			continue
		}
		raw, _ := json.Marshal(t.Function)
		fmt.Fprintf(&sb, "[Tool %d] %s: %s\n", i+1, t.Function.Name, string(raw))
	}
	sb.WriteString("\n")

	return sb.String()
}
