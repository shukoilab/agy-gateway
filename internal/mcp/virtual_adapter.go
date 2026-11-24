package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// VirtualAdapter bridges OpenAI function calling definitions into ACP prompt envelopes.
//
// @purpose Maps OpenAI tool schemas and choices into structured directives compatible with ACP.
// @rationale Provides a protocol-agnostic virtual tool calling layer so consumer clients can
// execute functions without native ACP schema lock-in.
// @invariants Stateless and thread-safe.
type VirtualAdapter struct{}

// NewVirtualAdapter creates a new VirtualAdapter.
func NewVirtualAdapter() *VirtualAdapter {
	return &VirtualAdapter{}
}

// BuildToolsPromptEnvelope formats OpenAI tools into a strict prompt instruction block.
//
// @purpose Converts tool definitions and execution rules into machine-parseable prompt instructions.
// @param tools Slice of tool definitions to include.
// @param toolChoice Choice policy ("auto", "required", "none", or specific function name).
// @param parallel Whether parallel tool invocation is permitted.
// @param delimiter Markup tag name for tool calls (e.g. "tool_call").
// @returns Formatted prompt directive string, or empty string if tools is empty.
func (va *VirtualAdapter) BuildToolsPromptEnvelope(tools []OpenAIToolDefinition, toolChoice string, parallel bool, delimiter string) string {
	if len(tools) == 0 {
		return ""
	}
	if delimiter == "" {
		delimiter = "tool_call"
	}

	var sb strings.Builder
	sb.WriteString("# CONSUMER TOOL-CALLING DIRECTIVES\n")
	sb.WriteString("You are operating as the AI engine connected via an API gateway to an external client workspace.\n")
	sb.WriteString("- The client application manages its local project workspace and executes tools directly in its host runtime.\n")
	sb.WriteString("- When calling client tools, you MUST emit your invocation enclosed inside delimiter tags:\n")
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

	return sb.String()
}

// BuildActiveContinuationCue returns a prompt continuation cue to ensure autonomous agent
// loops continue without premature stalls.
//
// @purpose Instructs the LLM to resume execution after tool outputs or primes assistant generation.
// @rationale Autonomous agents often stall when tool outputs resemble static logs.
// A clear directive and 'Assistant: ' turn cue guides the model to invoke the next tool or conclude.
// @param hasToolOutputs True if the conversation ends with tool execution results.
// @param delimiter Tag name for tool call markup.
// @returns Formatted prompt continuation string.
func (va *VirtualAdapter) BuildActiveContinuationCue(hasToolOutputs bool, delimiter string) string {
	if delimiter == "" {
		delimiter = "tool_call"
	}
	if hasToolOutputs {
		return fmt.Sprintf("[SYSTEM DIRECTIVE: You are in an active execution loop. The runtime environment has returned the tool execution results above. Proceed immediately with your next actions by emitting <%s>...</%s>, or provide your final response if the task is complete. Do not echo prompt headers, role markers, or transcript labels.]\n\nAssistant: ", delimiter, delimiter)
	}
	return "Assistant: "
}
