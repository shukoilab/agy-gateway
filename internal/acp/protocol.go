// Package acp implements the Agent Client Protocol (JSON-RPC 2.0 over stdio)
// for communication with the Google Antigravity CLI (agy acp) per official ACP v1 specifications.
package acp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// JSON-RPC 2.0 constants
const (
	JSONRPCVersion = "2.0"

	// SupportedProtocolVersion is the ACP protocol generation this gateway implements.
	SupportedProtocolVersion = 1

	// ACP & JSON-RPC 2.0 Canonical Error Codes (RFC 2119 / ACP Schema v1)
	ErrCodeParseError       = -32700
	ErrCodeInvalidRequest   = -32600
	ErrCodeMethodNotFound   = -32601
	ErrCodeInvalidParams    = -32602
	ErrCodeInternalError    = -32603
	ErrCodeRequestCancelled = -32800
	ErrCodeAuthRequired     = -32000
	ErrCodeResourceNotFound = -32002

	// ACP Session Config IDs
	ConfigIDMode         = "mode"
	ConfigIDModel        = "model"
	ConfigIDThoughtLevel = "thought_level"

	// ACP Update Types (Canonical SessionUpdate variants)
	UpdateAgentMessageChunk = "agent_message_chunk"
	UpdateAgentThoughtChunk = "agent_thought_chunk"
	UpdateUserMessageChunk  = "user_message_chunk"
	UpdateToolCall          = "tool_call"
	UpdateToolCallUpdate    = "tool_call_update"
	UpdateUsage             = "usage_update"
	UpdateAvailableCommands = "available_commands_update"
	UpdateCurrentMode       = "current_mode_update"
	UpdateConfigOption      = "config_option_update"
	UpdateNotice            = "notice"
	UpdateCompaction        = "compaction_update"
	UpdateCompactionChunk   = "compaction_summary_chunk"
	UpdatePlan              = "plan"
	UpdatePlanUpdate        = "plan_update"
	UpdatePlanRemoved       = "plan_removed"
	UpdateSubagent          = "subagent_update"
	UpdateSessionMessage    = "session_message"
	UpdateSessionMsgChunk   = "session_message_chunk"
)

// Request is a standard JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a standard JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}

// ResponseError encapsulates JSON-RPC 2.0 errors.
type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *ResponseError) Error() string {
	return e.Message
}

// Notification is an unsolicited JSON-RPC 2.0 notification without an ID.
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// EnvVariable defines an environment variable for stdio MCP servers per ACP v1 specification.
type EnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// HTTPHeader defines an HTTP header for HTTP/SSE MCP servers per ACP v1 specification.
type HTTPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MCPServerInfo describes an MCP server configuration injected during session/new.
// Conforms to official ACP v1 McpServer union (stdio, http, sse).
type MCPServerInfo struct {
	Type    string        `json:"type,omitempty"` // "http", "sse", or empty for stdio
	Name    string        `json:"name"`
	Command string        `json:"command,omitempty"`
	Args    []string      `json:"args,omitempty"`
	Env     []EnvVariable `json:"env,omitempty"`
	URL     string        `json:"url,omitempty"`
	Headers []HTTPHeader  `json:"headers,omitempty"`
}

// DefaultExecutionProtocol is the ultra-smart system prompt applied when the API consumer
// does not specify a custom system prompt. It enforces strict language mirroring,
// first-principles analysis, high-signal directness, and Chain-of-Verification (CoVe).
const DefaultExecutionProtocol = `Apply the following execution protocol to every user request:

1. **Strict Language Mirroring**: Detect the user's input language and write the entire response in that exact language naturally, accurately, and professionally.
2. **First-Principles & Assumption Analysis**: Before concluding, decompose the core objective, underlying first principles, critical variables, key assumptions, and edge cases or counter-arguments. Calibrate depth to task complexity.
3. **High-Signal Directness**: Deliver concrete, actionable, and well-structured outputs (using Markdown headings, lists, or tables where helpful) immediately—omit introductory filler and closing summaries.
4. **Chain-of-Verification (CoVe) Checklist**: Audit every draft claim against these gates before finalizing output:
   - [ ] **Premise & Fact Verification**: Independently verify every claim, calculation, and logical step; state uncertainty or missing data explicitly rather than extrapolating.
   - [ ] **Falsifiability & Edge Cases**: Address boundary conditions, failure modes, and strongest alternative interpretations.
   - [ ] **Logical Coherence**: Ensure conclusions follow strictly from verified premises without internal contradiction.
   - [ ] **Language & Signal Compliance**: Confirm 100% adherence to the user's input language and maximum information density.`

// SystemPrompt defines system-level instructions passed during session setup per ACP v1.
type SystemPrompt struct {
	Mode    string        `json:"mode"` // "append" or "override"
	Content []PromptBlock `json:"content"`
}

// SessionNewParams represents parameters sent in the session/new request per ACP v1 NewSessionRequest.
type SessionNewParams struct {
	CWD                   string          `json:"cwd"`
	AdditionalDirectories []string        `json:"additionalDirectories,omitempty"`
	MCPServers            []MCPServerInfo `json:"mcpServers,omitempty"`
	SystemPrompt          *SystemPrompt   `json:"systemPrompt,omitempty"`
	Meta                  map[string]any  `json:"_meta,omitempty"`
}

// SessionNewResult represents the result returned by session/new per ACP v1 NewSessionResponse.
//
// @purpose Carries the session handle plus the agent-advertised modes, config options and models.
// @rationale `configOptions` is strongly typed so callers can reject values the agent will refuse
// (verified: agy answers -32602 for unlisted thought levels) instead of discarding the error.
//
// @invariants ConfigOptions entries are keyed by ID and are safe to iterate without nil checks.
type SessionNewResult struct {
	SessionID     string                `json:"sessionId"`
	Modes         *SessionModeState     `json:"modes,omitempty"`
	ConfigOptions []SessionConfigOption `json:"configOptions,omitempty"`
	Models        *SessionModelsState   `json:"models,omitempty"` // vendor extension
	Meta          json.RawMessage       `json:"_meta,omitempty"`
}

// SessionModeState describes the agent's mode set as returned by session/new.
//
// @purpose Exposes the current and available modes for runtime validation.
// @rationale agy advertises four modes (default, accept-edits, auto, plan); a configured mode must be
// checked against this set rather than against a hardcoded list.
// @invariants AvailableModes may be empty; CurrentModeID may be empty.
type SessionModeState struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []SessionMode `json:"availableModes"`
}

// SessionMode is one selectable agent mode.
type SessionMode struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// SessionModelsState mirrors the Antigravity vendor `models` extension on session/new.
//
// @purpose Provides the vendor model list for catalogue fidelity.
// @rationale Not part of ACP v1; parsed only when present and otherwise ignored.
// @invariants AvailableModels may be empty.
type SessionModelsState struct {
	AvailableModels []SessionModelInfo `json:"availableModels"`
	CurrentModelID  string             `json:"currentModelId"`
}

// SessionModelInfo is one vendor-advertised model.
type SessionModelInfo struct {
	ModelID string `json:"modelId"`
	Name    string `json:"name,omitempty"`
}

// SessionConfigOption is a discriminated session config option per ACP v1.
//
// @purpose Lets the gateway validate a requested value before issuing session/set_config_option.
// @rationale Prevents the silent fallback that occurs when the agent rejects a value with -32602.
// @invariants Type is "select" or "boolean"; Options is populated for select options.
type SessionConfigOption struct {
	ID           string               `json:"id"`
	Name         string               `json:"name,omitempty"`
	Category     string               `json:"category,omitempty"` // mode|model|model_config|thought_level
	Type         string               `json:"type,omitempty"`     // select|boolean
	CurrentValue string               `json:"currentValue,omitempty"`
	Options      []SessionConfigValue `json:"options,omitempty"`
}

// SessionConfigValue is one selectable value of a config option.
//
// @purpose Carries the wire value and its human label.
// @rationale agy names the `xhigh` value "Extra"; using the label as a wire value is rejected with -32602.
// @invariants Value is the wire value and is never empty for a well-formed option.
type SessionConfigValue struct {
	Value       string `json:"value"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// HoldsValue reports whether the option accepts the given wire value.
//
// @purpose Prevents sending a value the agent will reject with -32602.
// @rationale agy advertises value "xhigh" under the display name "Extra"; sending the label fails.
// @param value Candidate wire value.
// @returns true when value equals the current value or any advertised option value.
// @invariants O(n) over the option list; pure read; nil-safe.
func (o *SessionConfigOption) HoldsValue(value string) bool {
	if o == nil {
		return false
	}
	if o.CurrentValue == value {
		return true
	}
	for _, opt := range o.Options {
		if opt.Value == value {
			return true
		}
	}
	return false
}

// ConfigOptionByID returns the option with the given ID, or nil.
//
// @purpose Central lookup for config validation.
// @param id Option identifier such as "model", "mode" or "thought_level".
// @returns Pointer to the matching option, or nil when absent.
// @invariants O(n) over the option list (n <= 3 in practice); pure read.
func (r *SessionNewResult) ConfigOptionByID(id string) *SessionConfigOption {
	if r == nil {
		return nil
	}
	for i := range r.ConfigOptions {
		if r.ConfigOptions[i].ID == id {
			return &r.ConfigOptions[i]
		}
	}
	return nil
}

// PromptResult represents the completion of a session/prompt turn.
//
// @purpose Carries the authoritative stop reason and token usage for the finished turn.
// @rationale Both are needed for faithful finish_reason/stop_reason mapping and usage accounting.
// @invariants StopReason may be empty; Usage is nil when the agent did not report it.
type PromptResult struct {
	StopReason string       `json:"stopReason,omitempty"`
	Usage      *PromptUsage `json:"usage,omitempty"`
}

// PromptUsage is the vendor usage extension returned on the session/prompt result.
//
// @purpose Supplies exact token accounting without local estimation.
// @rationale Verified present on agy 1.5.16 prompt results alongside usage_update notifications.
// @invariants All counters are non-negative; zero means unreported.
type PromptUsage struct {
	InputTokens       int64 `json:"inputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	CachedReadTokens  int64 `json:"cachedReadTokens"`
	CachedWriteTokens int64 `json:"cachedWriteTokens"`
	ThoughtTokens     int64 `json:"thoughtTokens"`
	TotalTokens       int64 `json:"totalTokens"`
}

// SessionCloseParams represents parameters sent in the session/close request.
//
// @purpose Unloads an active ACP session and releases all associated memory state.
// @rationale Enforces true JIT lifecycle for ephemeral agent turns without long-lived session bloat.
// @invariants Thread-safe.
type SessionCloseParams struct {
	SessionID string `json:"sessionId"`
}

// SessionCancelParams represents parameters sent in the session/cancel notification.
//
// @purpose Aborts an active ACP session prompt turn.
// @rationale Conforms to official Agent Client Protocol specification for client-initiated cancellation.
// @invariants Thread-safe.
type SessionCancelParams struct {
	SessionID string `json:"sessionId"`
}

// SetConfigOptionParams represents parameters sent in session/set_config_option.
type SetConfigOptionParams struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Value     string `json:"value"`
}

// PromptBlock represents rich content in an ACP session/prompt request.
//
// @purpose Carries one ACP ContentBlock of the subset the gateway actually forwards.
// @rationale The ACP v1 ContentBlock union is wider (image/audio/resource); those variants are refused
// by the model on agy 1.5.16, so the gateway never populates their payload fields.
//
// @invariants Type is "text" or "resource_link" in practice; Text is set for text blocks.
type PromptBlock struct {
	Type     string `json:"type"` // "text", "resource_link", "image", "audio", "resource"
	Text     string `json:"text,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// SanitizeToolArguments removes agent-injected keys that are not part of a tool's declared input schema.
//
// @purpose Guarantees that tool arguments forwarded to an API consumer validate against the schema the
// consumer declared, so strict OpenAI/Anthropic SDKs do not reject them.
// @rationale Verified on agy 1.5.16: `session/update.rawInput` carries a vendor `toolSummary` key that the
// real MCP `tools/call` does not contain. Forwarding it pollutes the consumer-visible arguments.
//
// @preconditions raw may be nil, "null", a non-object JSON value, or malformed JSON.
// @param raw The rawInput payload exactly as received from the agent.
// @param inputSchema The tool's declared JSON Schema (may be empty or malformed).
// @param vendorKeys Additional non-schema keys known to be injected by the agent runtime.
// @returns Deterministically re-encoded arguments, or `{}` when nothing survives.
// @throws Never returns an error; malformed input degrades to `{}`.
// @invariants O(k log k) over argument keys where k is the top-level key count; never panics.
func SanitizeToolArguments(raw json.RawMessage, inputSchema json.RawMessage, vendorKeys ...string) json.RawMessage {
	empty := json.RawMessage("{}")
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return empty
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return empty
	}

	// Collect the declared property names, when the schema exposes a usable object.
	declared := make(map[string]struct{})
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if len(inputSchema) > 0 && json.Unmarshal(inputSchema, &schema) == nil {
		for name := range schema.Properties {
			declared[name] = struct{}{}
		}
	}

	drop := make(map[string]struct{}, len(vendorKeys))
	for _, k := range vendorKeys {
		if k != "" {
			drop[k] = struct{}{}
		}
	}

	changed := false
	for name := range obj {
		if _, isVendor := drop[name]; isVendor {
			delete(obj, name)
			changed = true
			continue
		}
		if len(declared) > 0 {
			if _, ok := declared[name]; !ok {
				delete(obj, name)
				changed = true
			}
		}
	}

	if len(obj) == 0 {
		return empty
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return empty
	}
	return out
}

// VendorToolArgumentKeys are agent-injected keys observed in session/update rawInput that never belong
// to a tool's declared input schema.
var VendorToolArgumentKeys = []string{"toolSummary"}

// PromptParams represents parameters sent in session/prompt.
type PromptParams struct {
	SessionID string        `json:"sessionId"`
	Prompt    []PromptBlock `json:"prompt"`
}

// UsageCost defines cumulative monetary or credit cost in an ACP usage_update.
type UsageCost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// UsageUpdate defines context window token usage reported via sessionUpdate: "usage_update".
type UsageUpdate struct {
	Used int64      `json:"used"`
	Size int64      `json:"size"`
	Cost *UsageCost `json:"cost,omitempty"`
}

// SessionUpdateParams represents parameters received in session/update notifications.
type SessionUpdateParams struct {
	SessionID string        `json:"sessionId"`
	Update    SessionUpdate `json:"update"`
}

// ContentChunk represents inner text content within a session update.
//
// @purpose Encapsulates polymorphic content payloads received in ACP session/update notifications.
// @rationale Supports both single object and array representation (e.g. [{"type": "text", "text": "..."}])
// sent by diverse upstream agy acp agent runtimes without unmarshaling failures.
// @preconditions Valid JSON input bytes representing an object, an array of objects/strings, or a plain string.
// @invariants Thread-safe, deterministic concatenation of array chunks.
type ContentChunk struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

// UnmarshalJSON deserializes ContentChunk from either an object, an array of chunks, or a plain string.
func (c *ContentChunk) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}

	// 1. JSON Array: [{"type":"text","text":"..."}, ...] or ["...", ...]
	if trimmed[0] == '[' {
		var rawItems []json.RawMessage
		if err := json.Unmarshal(trimmed, &rawItems); err != nil {
			return err
		}
		var sb strings.Builder
		for _, rawItem := range rawItems {
			itemTrimmed := bytes.TrimSpace(rawItem)
			if len(itemTrimmed) == 0 || bytes.Equal(itemTrimmed, []byte("null")) {
				continue
			}
			if itemTrimmed[0] == '{' {
				var obj struct {
					Type string `json:"type,omitempty"`
					Text string `json:"text,omitempty"`
				}
				if err := json.Unmarshal(itemTrimmed, &obj); err == nil {
					sb.WriteString(obj.Text)
					if c.Type == "" && obj.Type != "" {
						c.Type = obj.Type
					}
					continue
				}
			}
			var str string
			if err := json.Unmarshal(itemTrimmed, &str); err == nil {
				sb.WriteString(str)
				continue
			}
		}
		c.Text = sb.String()
		return nil
	}

	// 2. JSON Object: {"type": "text", "text": "..."} or {"text": "..."}
	if trimmed[0] == '{' {
		var obj struct {
			Type string `json:"type,omitempty"`
			Text string `json:"text,omitempty"`
		}
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return err
		}
		c.Type = obj.Type
		c.Text = obj.Text
		return nil
	}

	// 3. Plain JSON String: "text chunk"
	var str string
	if err := json.Unmarshal(trimmed, &str); err == nil {
		c.Text = str
		return nil
	}

	return nil
}

// SessionUpdate contains details of live turn streaming from the agent per ACP v1 specification.
type SessionUpdate struct {
	SessionUpdate     string           `json:"sessionUpdate"`
	Content           *ContentChunk    `json:"content,omitempty"`
	ToolCallID        string           `json:"toolCallId,omitempty"`
	Name              string           `json:"name,omitempty"`
	Title             string           `json:"title,omitempty"`
	Kind              string           `json:"kind,omitempty"`
	Status            string           `json:"status,omitempty"`
	RawInput          json.RawMessage  `json:"rawInput,omitempty"`
	RawOutput         json.RawMessage  `json:"rawOutput,omitempty"`
	Usage             *UsageUpdate     `json:"usage,omitempty"`
	Used              *int64           `json:"used,omitempty"`
	Size              *int64           `json:"size,omitempty"`
	Cost              *UsageCost       `json:"cost,omitempty"`
	AvailableCommands []map[string]any `json:"availableCommands,omitempty"`
}

// PermissionOption describes an option offered during session/request_permission.
//
// @purpose Carries the canonical option identifier, human label and semantic kind.
// @rationale Selection MUST be driven by `kind` (a closed enum), never by substrings of `optionId`,
// because option ids are agent-defined and may be localised or randomised.
//
// @invariants Kind is one of allow_once, allow_always, reject_once, reject_always, or empty when malformed.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

// PermissionToolCall is the tool-call descriptor nested inside a real session/request_permission payload.
//
// @purpose Carries the correlation id and arguments of the tool call awaiting permission.
// @rationale Verified against agy 1.5.16: the payload nests the call under `toolCall`, so a flat
// `toolCallId` field on the params object is always empty. Both shapes are accepted for compatibility.
//
// @invariants ToolCallID is non-empty when the agent supplies it; Args may be any JSON value.
type PermissionToolCall struct {
	ToolCallID string `json:"toolCallId"`
	Name       string `json:"name,omitempty"`
	Args       any    `json:"args,omitempty"`
}

// RequestPermissionParams represents incoming permission requests from agy acp.
//
// @purpose Decodes the agent→client permission request.
// @rationale Supports the nested `toolCall` shape used by agy alongside the flat `toolCallId` fallback.
//
// @invariants ToolCallID() resolves to the first non-empty of ToolCall.ToolCallID and the flat field.
type RequestPermissionParams struct {
	SessionID  string              `json:"sessionId"`
	ToolCallID string              `json:"toolCallId,omitempty"`
	ToolCall   *PermissionToolCall `json:"toolCall,omitempty"`
	Options    []PermissionOption  `json:"options"`
}

// ResolvedToolCallID returns the correlation id from whichever shape the agent used.
//
// @purpose Normalises the two accepted permission payload shapes into one identifier.
// @rationale The nested `toolCall.toolCallId` is authoritative on agy; the flat field is a fallback.
// @returns Nested id when present, otherwise the flat id, otherwise "".
// @invariants Pure read, O(1), nil-safe.
func (p *RequestPermissionParams) ResolvedToolCallID() string {
	if p == nil {
		return ""
	}
	if p.ToolCall != nil && p.ToolCall.ToolCallID != "" {
		return p.ToolCall.ToolCallID
	}
	return p.ToolCallID
}

// PermissionOutcome defines the chosen permission response per ACP v1.
type PermissionOutcome struct {
	Outcome  string `json:"outcome"` // "selected" or "cancelled"
	OptionID string `json:"optionId,omitempty"`
}

// RequestPermissionResult is sent as the JSON-RPC response to session/request_permission.
type RequestPermissionResult struct {
	Outcome PermissionOutcome `json:"outcome"`
}

// AgentCapabilities describes negotiated server capabilities parsed from the ACP initialize response.
//
// @purpose Drives deterministic ACP feature negotiation without assuming unadvertised protocol extensions.
// @rationale ACP v1 prohibits sending unadvertised modes (such as systemPrompt override when only append is supported).
//
//	The official schema expresses `sessionCapabilities.systemPrompt.mode.override` as an object (`{}`), and
//	`append` as an implicit capability carrying NO entry at all. The Antigravity vendor dialect instead emits
//	booleans under `_meta.antigravity.systemPrompt`. Both dialects are parsed separately so that a vendor-only
//	key is never mistaken for evidence about the standard key.
//
// @invariants All boolean flags default cleanly to false when omitted or malformed; never panics on wrong JSON types.
type AgentCapabilities struct {
	ProtocolVersion             int
	LoadSession                 bool
	SupportsSessionClose        bool
	SupportsAdditionalDirs      bool
	SupportsSystemPrompt        bool
	SupportsSystemPromptAppend  bool
	SupportsSystemPromptOveride bool
	SupportsVendorSystemPrompt  bool
	SupportsVendorPromptOveride bool
	SupportsMCPHTTP             bool
	SupportsMCPSSE              bool
	PromptImage                 bool
	PromptEmbeddedContext       bool
}

// ParseAgentCapabilities safely extracts canonical and vendor-extension capabilities from an ACP initialize result.
//
// @purpose Parses initialize result payloads into strongly typed capability flags.
// @rationale Prevents nil-map panics and guarantees strict compliance with ACP v1 capability gating, while
// supporting both the official schema dialect (implicit `append`, object `override`) and the Antigravity
// vendor dialect (booleans under `_meta.antigravity.systemPrompt`).
//
// @preconditions initResult may be nil or partially populated; keys may hold unexpected JSON types.
// @param initResult Raw decoded initialize JSON-RPC result map.
// @returns Populated AgentCapabilities struct.
// @throws None; malformed segments are ignored.
// @invariants O(1) map lookups, never panics, no allocation beyond type assertions.
func ParseAgentCapabilities(initResult map[string]any) AgentCapabilities {
	var caps AgentCapabilities
	if initResult == nil {
		return caps
	}
	// protocolVersion is an integer per ACP v1 InitializeResponse.
	switch v := initResult["protocolVersion"].(type) {
	case float64:
		caps.ProtocolVersion = int(v)
	case int:
		caps.ProtocolVersion = v
	case int64:
		caps.ProtocolVersion = int(v)
	}

	agentCaps, ok := initResult["agentCapabilities"].(map[string]any)
	if !ok {
		return caps
	}

	// loadSession is top-level per schema; `load_session` is the vendor snake_case duplicate.
	if v, ok := agentCaps["loadSession"].(bool); ok {
		caps.LoadSession = v
	} else if v, ok := agentCaps["load_session"].(bool); ok {
		caps.LoadSession = v
	}

	if v, ok := agentCaps["promptCapabilities"].(map[string]any); ok {
		if b, ok := v["image"].(bool); ok {
			caps.PromptImage = b
		}
		if b, ok := v["embeddedContext"].(bool); ok {
			caps.PromptEmbeddedContext = b
		}
	}

	// sessionCapabilities (schema) with session_capabilities (vendor) fallback.
	sessCaps, ok := agentCaps["sessionCapabilities"].(map[string]any)
	if !ok {
		sessCaps, _ = agentCaps["session_capabilities"].(map[string]any)
	}
	if sessCaps != nil {
		// Schema keys are advertised as empty objects, never as booleans/numbers. Accepting a falsy
		// value as "advertised" would send a standard systemPrompt to an agent that said no.
		if v, exists := sessCaps["additionalDirectories"]; exists {
			if _, ok := v.(map[string]any); ok {
				caps.SupportsAdditionalDirs = true
			}
		}
		if v, exists := sessCaps["close"]; exists {
			if _, ok := v.(map[string]any); ok {
				caps.SupportsSessionClose = true
			}
		}
		// Must be an object (present, possibly `{}`). A falsy scalar is not an advertisement, and
		// presence of the object means append is supported: it carries no entry in the schema.
		if spMap, ok := sessCaps["systemPrompt"].(map[string]any); ok && spMap != nil {
			caps.SupportsSystemPrompt = true
			caps.SupportsSystemPromptAppend = true
			if modeMap, ok := spMap["mode"].(map[string]any); ok {
				// Schema dialect: override is an object. Vendor dialect: a boolean.
				if ovRaw, exists := modeMap["override"]; exists {
					switch ov := ovRaw.(type) {
					case map[string]any:
						caps.SupportsSystemPromptOveride = true
					case bool:
						caps.SupportsSystemPromptOveride = ov
					}
				}
				if apRaw, exists := modeMap["append"]; exists {
					switch ap := apRaw.(type) {
					case map[string]any:
						caps.SupportsSystemPromptAppend = true
					case bool:
						caps.SupportsSystemPromptAppend = ap
					}
				}
			}
		}
	}

	if mcpCaps, ok := agentCaps["mcpCapabilities"].(map[string]any); ok {
		if v, ok := mcpCaps["http"].(bool); ok {
			caps.SupportsMCPHTTP = v
		}
		if v, ok := mcpCaps["sse"].(bool); ok {
			caps.SupportsMCPSSE = v
		}
	}

	// Vendor dialect: _meta.antigravity.systemPrompt.{append,override} booleans.
	// A vendor key must not be read as evidence about the standard key (FR-01): it only sets the
	// vendor flag. Delivery-channel selection consults SupportsSystemPrompt, which stays false here,
	// so resolveSystemPromptChannel picks the vendor channel and nothing else.
	if meta, ok := agentCaps["_meta"].(map[string]any); ok {
		if agy, ok := meta["antigravity"].(map[string]any); ok {
			if sp, ok := agy["systemPrompt"].(map[string]any); ok {
				if v, ok := sp["append"].(bool); ok && v {
					caps.SupportsVendorSystemPrompt = true
				}
				if v, ok := sp["override"].(bool); ok && v {
					caps.SupportsVendorSystemPrompt = true
					caps.SupportsVendorPromptOveride = true
				}
			}
		}
	}
	return caps
}

// builtInContainerTools enumerates native ACP agent tools whose execution runs inside the isolated
// gateway worker container rather than the external API client host.
var builtInContainerTools = map[string]struct{}{
	"read_file":        {},
	"write_file":       {},
	"edit_file":        {},
	"run_command":      {},
	"manage_task":      {},
	"send_message":     {},
	"schedule":         {},
	"invoke_subagent":  {},
	"define_subagent":  {},
	"manage_subagents": {},
	"search_web":       {},
	"run_workflow":     {},
	"swarm":            {},
	"ask_question":     {},
	"call_mcp_tool":    {},
	"list_resources":   {},
	"read_resource":    {},
}

// IsBuiltInContainerTool reports whether toolName corresponds to an internal ACP runtime tool
// confined to the gateway container rather than a delegated client MCP tool.
//
// @purpose Enables observability and policy guards to identify container-local tool execution.
// @rationale Distinguishes internal scaffolding/filesystem tools from external mcp_client-tools_* invocations.
// @param name Raw tool name emitted by session/update.
// @returns True if name matches a built-in container tool; false otherwise.
// @invariants O(1) hash lookup.
func IsBuiltInContainerTool(name string) bool {
	_, ok := builtInContainerTools[strings.TrimSpace(name)]
	return ok
}

// ClientToolDelegationDirective builds an authoritative precedence directive instructing the model
// to use only the client's MCP tools (mcp_client-tools_*) when client tool definitions are supplied.
//
// @purpose Eliminates split-brain tool execution where the model inspects empty container directories instead of the host workspace.
// @rationale Antigravity ACP injects built-in container tools even when fs/terminal capabilities are false. Without explicit
// precedence guidance listing the mcp_client-tools_* surface, models invoke internal container tools and miss host project files.
// @preconditions toolNames must contain normalized tool names provided by the API consumer.
// @param toolNames List of OpenAI/Anthropic client tool names registered on the LoopbackServer.
// @returns Directive text suitable for systemPrompt append injection, or empty string if toolNames is empty.
// @invariants Zero XML tag injection; pure semantic role/tool boundary instructions.
func ClientToolDelegationDirective(toolNames []string) string {
	if len(toolNames) == 0 {
		return ""
	}
	prefixed := make([]string, 0, len(toolNames))
	for _, n := range toolNames {
		clean := strings.TrimSpace(n)
		if clean == "" {
			continue
		}
		prefixed = append(prefixed, "mcp_client-tools_"+clean)
	}
	if len(prefixed) == 0 {
		return ""
	}
	return fmt.Sprintf(`CRITICAL EXECUTION ENVIRONMENT & TOOL PRECEDENCE OVERRIDE:
1. You are operating via an API gateway inside an isolated, EMPTY remote container workspace. The user's real repository, files, git history, and runtime environment reside EXCLUSIVELY on the caller's host machine.
2. NEVER call any built-in container tools (such as read_file, write_file, edit_file, run_command, invoke_subagent, define_subagent, manage_subagents, run_workflow, swarm, call_mcp_tool, list_resources, read_resource). Using built-in tools will always see an empty container and FAIL.
3. For ALL file reading, writing, searching, shell commands, git operations, or environment actions, you MUST EXCLUSIVELY invoke the client host MCP tools: [%s].`, strings.Join(prefixed, ", "))
}
