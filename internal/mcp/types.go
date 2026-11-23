// Package mcp implements OpenAI function tool schemas and JIT prompt adapters for agy-gateway.
package mcp

import "encoding/json"

// OpenAIToolDefinition mirrors the OpenAI 2026 tool schema.
type OpenAIToolDefinition struct {
	Type     string         `json:"type"` // "function"
	Function FunctionSchema `json:"function"`
}

// FunctionSchema represents the inner function metadata for a tool.
type FunctionSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}
