// Package mcp implements OpenAI function tool schemas and JIT prompt adapters for agy-gateway.
package mcp

import "encoding/json"

// OpenAIToolDefinition mirrors the OpenAI 2026 tool schema.
type OpenAIToolDefinition struct {
	Type     string         `json:"type" example:"function"` // "function"
	Function FunctionSchema `json:"function"`
}

// FunctionSchema represents the inner function metadata for a tool.
type FunctionSchema struct {
	Name        string          `json:"name" example:"get_weather"`
	Description string          `json:"description,omitempty" example:"Retrieve current temperature and weather conditions"`
	Parameters  json.RawMessage `json:"parameters,omitempty" swaggertype:"object"`
	Strict      bool            `json:"strict,omitempty" example:"true"`
}
