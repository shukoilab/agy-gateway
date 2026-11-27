package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agy-gateway/internal/acp"
	"agy-gateway/internal/anthropic"
	"agy-gateway/internal/config"
	"agy-gateway/internal/mcp"
	"agy-gateway/internal/openai"
)

// MapStopReason converts an ACP stop reason into the OpenAI finish_reason and Anthropic stop_reason.
//
// @purpose Stops collapsing every terminal state into "stop", which hid truncation and refusals.
// @rationale agy reports end_turn, cancelled, max_tokens and refusal; each maps to a distinct
// provider value.
// @param acpStopReason Stop reason observed on the prompt result.
// @returns finish_reason for OpenAI and stop_reason for Anthropic.
// @invariants Pure; unknown values degrade to the non-error terminal state.
func MapStopReason(acpStopReason string) (openAIFinish, anthropicStop string) {
	switch strings.ToLower(strings.TrimSpace(acpStopReason)) {
	case "max_tokens":
		return "length", "max_tokens"
	case "max_turn_requests":
		return "length", "max_tokens"
	case "refusal":
		return "content_filter", "refusal"
	case "cancelled", "canceled":
		return "stop", "end_turn"
	case "error":
		return "stop", "end_turn"
	default:
		return "stop", "end_turn"
	}
}

// UsageView is the provider-neutral token accounting for one finished turn.
//
// @purpose Single mapping point from ACP usage to both provider shapes.
// @rationale agy reports authoritative counts on the prompt result and via usage_update; the gateway
// previously ignored both and re-tokenized locally.
// @invariants All counters are non-negative once normalised.
type UsageView struct {
	InputTokens       int64
	OutputTokens      int64
	CachedReadTokens  int64
	CachedWriteTokens int64
	ThoughtTokens     int64
	TotalTokens       int64
}

// FromPromptUsage builds a UsageView from the prompt result, if the agent reported one.
//
// @purpose Prefers authoritative upstream accounting over local estimation.
// @param usage Prompt-result usage, possibly nil.
// @returns A normalised view and whether upstream data was available.
// @invariants Pure; nil input yields a zero view and false.
func FromPromptUsage(usage *acp.PromptUsage) (UsageView, bool) {
	if usage == nil {
		return UsageView{}, false
	}
	v := UsageView{
		InputTokens:       nonNegative(usage.InputTokens),
		OutputTokens:      nonNegative(usage.OutputTokens),
		CachedReadTokens:  nonNegative(usage.CachedReadTokens),
		CachedWriteTokens: nonNegative(usage.CachedWriteTokens),
		ThoughtTokens:     nonNegative(usage.ThoughtTokens),
		TotalTokens:       nonNegative(usage.TotalTokens),
	}
	if v.TotalTokens == 0 {
		v.TotalTokens = v.InputTokens + v.OutputTokens
	}
	return v, true
}

// nonNegative clamps a counter that a vendor extension could report as negative.
//
// @purpose Keeps usage arithmetic safe against malformed upstream payloads.
// @param v Candidate counter.
// @returns v when non-negative, otherwise zero.
// @invariants Pure.
func nonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// toolCollector accumulates intercepted client tool calls for one turn.
//
// @purpose Collects multiple tool calls per assistant message, deduplicated by id, with arguments
// sanitised against the caller's declared schema.
// @rationale The previous implementation returned after the first call, silently discarding the rest;
// and it forwarded vendor keys such as `toolSummary` that are absent from the tool schema.
// @invariants Safe for concurrent use; ordering is first-seen order; at most maxCalls entries.
type toolCollector struct {
	mu    sync.Mutex
	order []string
	byID  map[string]toolCollectorEntry
	// bySig maps "name\x00sanitized-args" to a per-origin observation count. Two transports may each
	// report the same logical invocation, so a call is dropped only when the *other* origin holds an
	// unconsumed surplus for that signature. A last-writer-wins slot would lose a distinct call the
	// moment the 1:1 channel pairing broke (dropped channel send, FR-17 session filter).
	// nil disables cross-origin deduplication.
	bySig       map[string]map[ToolCallOrigin]int
	maxCalls    int
	maxArgBytes int64
	strict      bool
	onLimit     func()
	onInvalid   func(id, name, reason string)
	limitHit    bool
}

// toolCollectorEntry is one collected call plus its rendered provider payload.
type toolCollectorEntry struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// NewToolCollector creates a collector bounded by maxCalls.
//
// @purpose Provides the multi-call collection required for real agentic turns.
// @rationale agy emits several tool_call updates in one turn; a single-slot design loses them.
// @preconditions maxCalls > 0.
// @param maxCalls Hard ceiling; values <= 0 fall back to the configured default.
// @param onLimit Invoked once when the ceiling is first exceeded, so the caller can finalise.
// @returns An empty collector.
// @invariants O(1).
func NewToolCollector(cfg *config.Config, onLimit func()) *toolCollector {
	maxCalls, maxBytes, strict := 16, int64(0), false
	if cfg != nil {
		c := cfg.MCP.ClientToolCalling
		maxCalls = c.MaxInterceptedToolCalls
		maxBytes = c.MaxToolArgumentBytes
		strict = c.StrictSchemaValidation
	}
	if maxCalls <= 0 {
		maxCalls = 16
	}
	return &toolCollector{
		byID:        make(map[string]toolCollectorEntry, maxCalls),
		bySig:       make(map[string]map[ToolCallOrigin]int, maxCalls),
		maxCalls:    maxCalls,
		maxArgBytes: maxBytes,
		strict:      strict,
		onLimit:     onLimit,
	}
}

// Add records a tool call, sanitising its arguments and ignoring duplicates.
//
// @purpose Single entry point for both the update listener and the loopback reader.
// @rationale Deduplication by toolCallId is required because agy emits both tool_call and
// tool_call_update for the same logical invocation.
// @preconditions id and name are already canonicalised by the caller.
// @param id Stable tool call id.
// @param name Canonical client tool name.
// @param rawArgs Arguments as reported by the agent; may contain vendor keys.
// @param schema The declared input schema for this tool, used to drop undeclared keys.
// @returns true when the call was newly recorded, false for a duplicate or when the ceiling is hit.
// @invariants O(k) where k is the argument key count; mutex-protected.
func (c *toolCollector) Add(id, name string, rawArgs json.RawMessage, schema json.RawMessage) (toolCollectorEntry, bool) {
	return c.AddFrom(id, name, rawArgs, schema, originNativeUpdate)
}

// ToolCallOrigin identifies which transport delivered an intercepted call. The same logical call can
// arrive twice — once as a native `session/update` tool_call and once through the bridge's `tools/call` —
// with two different ids. Deduplication therefore has to consider origin, not just id.
type ToolCallOrigin string

const (
	originNativeUpdate ToolCallOrigin = "session_update"
	originLoopbackCall ToolCallOrigin = "loopback_tools_call"
)

// AddFrom records a tool call, deduplicating both by id and, across origins, by identical name+arguments.
//
// @purpose Single funnel for every intercepted client tool call, regardless of transport.
// @rationale agy reports one logical invocation on two channels with unrelated ids; without the
// cross-origin signature check the client receives the same call twice and executes it twice.
// @preconditions id and name non-empty; rawArgs may be nil/null; schema may be nil.
// @param id Agent- or bridge-provided call identifier; unique per transport.
// @param name Canonical client tool name (already normalised).
// @param rawArgs Argument payload as reported upstream (may contain vendor keys).
// @param schema Declared inputSchema of that tool, used to strip undeclared keys.
// @param origin Transport that produced this observation.
// @returns The stored entry and true when newly collected; zero entry and false when duplicate,
// rejected, or over the configured ceiling.
// @throws None.
// @invariants O(1) map lookups under c.mu; bounded by maxCalls; sanitisation runs before storage.
func (c *toolCollector) AddFrom(id, name string, rawArgs json.RawMessage, schema json.RawMessage, origin ToolCallOrigin) (toolCollectorEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.byID[id]; exists {
		return toolCollectorEntry{}, false
	}
	if len(c.order) >= c.maxCalls {
		if !c.limitHit {
			c.limitHit = true
			if c.onLimit != nil {
				go c.onLimit()
			}
		}
		return toolCollectorEntry{}, false
	}

	sanitized := acp.SanitizeToolArguments(rawArgs, schema, acp.VendorToolArgumentKeys...)

	// Cross-origin duplicate: the same tool with byte-identical arguments arriving through the other
	// transport is one logical invocation, not two. Surplus counting keeps this correct under any
	// interleaving: an observation is dropped only when the other origin has an unconsumed claim on
	// that exact signature, so a distinct call is never lost when the pairing breaks.
	sig := name + "\x00" + string(sanitized)
	if c.bySig != nil {
		if seen := c.bySig[sig]; seen != nil {
			other := originLoopbackCall
			if origin == originLoopbackCall {
				other = originNativeUpdate
			}
			if seen[other] > 0 {
				seen[other]--
				return toolCollectorEntry{}, false
			}
		}
	}

	// Enforce the configured argument ceiling and, when strict schema validation is on, reject any
	// argument payload that is not a well-formed JSON object or does not satisfy the declared
	// inputSchema. Both are client-visible correctness guarantees: a tool call the consumer cannot
	// parse or validate must never be emitted.
	if c.maxArgBytes > 0 && int64(len(sanitized)) > c.maxArgBytes {
		c.rejectLocked(id, name, "arguments exceed max_tool_argument_bytes")
		return toolCollectorEntry{}, false
	}
	if c.strict {
		if reason := validateAgainstSchema(sanitized, schema); reason != "" {
			c.rejectLocked(id, name, reason)
			return toolCollectorEntry{}, false
		}
	}

	entry := toolCollectorEntry{
		ID:        id,
		Name:      name,
		Arguments: sanitized,
	}
	c.byID[id] = entry
	c.order = append(c.order, id)
	if c.bySig != nil {
		if c.bySig[sig] == nil {
			c.bySig[sig] = make(map[ToolCallOrigin]int, 2)
		}
		c.bySig[sig][origin]++
	}
	return entry, true
}

// Len reports how many calls have been collected.
//
// @purpose Lets handlers short-circuit provider-specific emission logic.
// @returns The collected count.
// @invariants Mutex-protected read.
func (c *toolCollector) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.order)
}

// Snapshot returns the collected calls in first-seen order.
//
// @purpose Provides a stable slice for final response construction.
// @returns A copy of the collected entries; empty when none.
// @invariants Mutex-protected; the returned slice is independent of collector state.
func (c *toolCollector) Snapshot() []toolCollectorEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]toolCollectorEntry, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, c.byID[id])
	}
	return out
}

// SchemaFor returns the declared input schema for a canonical tool name.
//
// @purpose Supplies sanitisation with the authoritative property list.
// @rationale Without the schema the sanitiser can only strip known vendor keys.
// @param state Loopback session state carrying the declared tools.
// @param name Canonical tool name.
// @returns The schema, or nil when the tool is unknown.
// @invariants O(n) over declared tools; safe on nil state.
func SchemaFor(state *mcp.SessionState, name string) json.RawMessage {
	if state == nil {
		return nil
	}
	for _, tool := range state.ToolDefinitionSnapshot() {
		if tool.Name == name {
			return tool.InputSchema
		}
	}
	return nil
}

// NewTurnTimer creates the single collection-window timer for a turn.
//
// @purpose Replaces eight unstoppable time.AfterFunc timers with one stoppable timer.
// @rationale Every AfterFunc in the previous code leaked its callback until it fired, and could call
// cancelPrompt after the turn had already completed.
// @preconditions window > 0.
// @param window Collection window duration.
// @param fire Callback invoked once when the window elapses.
// @returns The started timer; the caller must always Stop it.
// @invariants O(1); fires at most once.
func NewTurnTimer(window time.Duration, fire func()) *time.Timer {
	if window <= 0 {
		window = time.Second
	}
	if fire == nil {
		fire = func() {}
	}
	return time.AfterFunc(window, fire)
}

// StopSequenceMatcher detects configured stop sequences across streamed chunks.
//
// @purpose Honors client stop sequences gateway-side, since ACP exposes no equivalent.
// @rationale Truncation must happen in the gateway or the consumer's stop contract is violated.
// @invariants Safe for concurrent use; keeps at most maxSeqLen-1 bytes of overlap.
type StopSequenceMatcher struct {
	mu        sync.Mutex
	sequences []string
	pending   string
	overlap   int
	matched   string
	stopped   bool
}

// NewStopSequenceMatcher builds a matcher for the supplied sequences.
//
// @purpose Normalises and validates the client's stop list.
// @rationale Empty sequences are meaningless and would match immediately.
// @param sequences Raw stop sequences from either provider.
// @returns A matcher; an empty matcher when no usable sequence was supplied.
// @invariants O(n) setup.
func NewStopSequenceMatcher(sequences []string) *StopSequenceMatcher {
	clean := make([]string, 0, len(sequences))
	maxLen := 0
	for _, s := range sequences {
		if s == "" {
			continue
		}
		clean = append(clean, s)
		if len(s) > maxLen {
			maxLen = len(s)
		}
	}
	overlap := maxLen - 1
	if overlap < 0 {
		overlap = 0
	}
	return &StopSequenceMatcher{sequences: clean, overlap: overlap}
}

// Active reports whether the matcher has any usable sequence.
//
// @purpose Avoids buffering work when the client supplied no stop sequences.
// @returns true when at least one sequence is configured.
// @invariants Pure read.
func (m *StopSequenceMatcher) Active() bool {
	return m != nil && len(m.sequences) > 0
}

// Feed pushes a chunk and returns the text that is safe to emit.
//
// @purpose Streams text without ever emitting past a stop sequence, including sequences split across
// chunk boundaries.
// @rationale Emitting first and retracting later is impossible in SSE.
// @param chunk Newly received text.
// @returns Emittable text and whether a stop sequence was matched by this chunk.
// @invariants O(n*m); holds back at most overlap bytes.
func (m *StopSequenceMatcher) Feed(chunk string) (string, bool) {
	if !m.Active() {
		return chunk, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.stopped {
		return "", true
	}

	buf := m.pending + chunk
	earliest := -1
	for _, seq := range m.sequences {
		if idx := strings.Index(buf, seq); idx >= 0 && (earliest == -1 || idx < earliest) {
			earliest = idx
			m.matched = seq
		}
	}
	if earliest >= 0 {
		m.stopped = true
		m.pending = ""
		return buf[:earliest], true
	}

	if len(buf) <= m.overlap {
		m.pending = buf
		return "", false
	}
	cut := len(buf) - m.overlap
	// Slicing by byte can split a multi-byte rune, which would emit invalid UTF-8 to the client and
	// break the stop-sequence match on the far side. Back off to the rune boundary.
	for cut < len(buf) && !utf8.RuneStart(buf[cut]) {
		cut++
	}
	m.pending = buf[cut:]
	return buf[:cut], false
}

// Flush returns any held-back text at end of stream.
//
// @purpose Emits the tail that was withheld for overlap detection.
// @returns Remaining text, or empty when a sequence was matched.
// @invariants O(1).
func (m *StopSequenceMatcher) Flush() string {
	if !m.Active() {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return ""
	}
	out := m.pending
	m.pending = ""
	return out
}

// Stopped reports whether a stop sequence was matched.
//
// @purpose Lets the handler choose the terminal reason.
// @returns true when a sequence matched.
// @invariants Pure read.
func (m *StopSequenceMatcher) Stopped() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopped
}

// MatchedSequence returns the sequence that terminated the stream.
//
// @purpose Populates Anthropic stop_sequence.
// @returns The matched sequence, or empty.
// @invariants Pure read.
func (m *StopSequenceMatcher) MatchedSequence() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.matched
}

// visionModeRejects reports whether the configured policy refuses image and embedded blocks.
//
// @purpose Centralises the FR-07 decision.
// @rationale Two adapters must agree on this or the same request behaves differently per endpoint.
// @param cfg Gateway configuration.
// @returns true when image and embedded content must be rejected.
// @invariants Pure.
func visionModeRejects(cfg *config.Config) bool {
	if cfg == nil {
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(cfg.Execution.VisionMode), "flatten")
}

// rejectLocked records an invalid tool call without emitting it.
//
// @purpose Keeps unparseable tool arguments away from the API consumer.
// @rationale A dropped call with a logged reason is recoverable; a malformed call crashes the client.
// @preconditions c.mu must be held by the caller.
// @param id Tool call id.
// @param name Canonical tool name.
// @param reason Human-readable rejection cause.
// @returns None.
// @throws None.
// @invariants O(1); never re-enters the mutex.
// validateAgainstSchema checks a tool-argument object against the tool's declared JSON Schema subset.
//
// @purpose Implements FR-22 `strict_schema_validation`: a tool call whose arguments contradict the
// schema the model was given would be rejected by the consumer's own validator, so it must never be
// forwarded.
// @rationale A full JSON Schema engine is unnecessary risk and dependency weight; the subset that
// gateways actually publish (`type`, `required`, `properties`, `enum`, `items`) covers every tool
// schema observed from real clients. Anything outside that subset is not enforced rather than
// mis-enforced.
// @preconditions arguments is non-nil; schema may be nil/empty (then only the object check runs).
// @param arguments Sanitised argument JSON.
// @param schema Declared inputSchema of the tool, as published to the agent.
// @returns Empty string when the payload is acceptable, otherwise a short machine-readable reason.
// @throws None; malformed input values are reported as reasons, never panics.
// @invariants O(k) in the number of top-level properties; no allocation beyond map decoding.
func validateAgainstSchema(arguments, schema json.RawMessage) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &obj); err != nil {
		return "arguments are not a JSON object"
	}

	trimmed := bytes.TrimSpace(schema)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}

	var spec struct {
		Type       string                     `json:"type"`
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(trimmed, &spec); err != nil {
		// An unparsable schema is a gateway-side data problem, not the client's fault.
		return ""
	}
	if spec.Type != "" && spec.Type != "object" {
		return "arguments must match a non-object schema"
	}

	for _, name := range spec.Required {
		if _, ok := obj[name]; !ok {
			return "missing required argument " + name
		}
	}

	for name, raw := range obj {
		propRaw, declared := spec.Properties[name]
		if !declared {
			continue // extra keys are permitted unless the schema says otherwise
		}
		if reason := validateProperty(raw, propRaw); reason != "" {
			return name + ": " + reason
		}
	}
	return ""
}

// validateProperty checks one property value against its schema node.
//
// @purpose Nested half of validateAgainstSchema; kept separate so recursion over items stays readable.
// @rationale Only the published subset is enforced — type, enum, and array items. Unknown keywords
// are intentionally ignored so a richer upstream schema is never rejected for a keyword we cannot read.
// @preconditions value is a valid JSON fragment; node is the schema node for that property.
// @param value Decoded property value as raw JSON.
// @param node Schema node describing it.
// @returns Empty string when acceptable, else a short reason.
// @throws None.
// @invariants O(len(node)); recursion depth follows the schema, which is client-supplied and bounded by
// the request size limit.
func validateProperty(value, node json.RawMessage) string {
	var spec struct {
		Type       string                     `json:"type"`
		Enum       []any                      `json:"enum"`
		Items      json.RawMessage            `json:"items"`
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(node, &spec); err != nil {
		return ""
	}

	if len(spec.Enum) > 0 {
		var got any
		if err := json.Unmarshal(value, &got); err == nil {
			matched := false
			for _, want := range spec.Enum {
				if jsonValuesEqual(got, want) {
					matched = true
					break
				}
			}
			if !matched {
				return "value is not one of the allowed options"
			}
		}
	}

	switch spec.Type {
	case "":
		return ""
	case "string", "number", "integer", "boolean", "object", "array":
		// Decode once into `any` and switch on the concrete Go type. Decoding straight into
		// string/json.Number/bool would accept values that are not of the declared JSON type:
		// encoding/json coerces a JSON string into json.Number when it looks numeric ("5" passed
		// as integer), and it silently stores JSON null into a string as "".
		var decoded any
		if err := json.Unmarshal(value, &decoded); err != nil {
			return "expected a " + spec.Type
		}
		switch spec.Type {
		case "string":
			if _, ok := decoded.(string); !ok {
				return "expected a string"
			}
		case "number":
			if _, ok := decoded.(float64); !ok {
				return "expected a number"
			}
		case "integer":
			f, ok := decoded.(float64)
			if !ok {
				return "expected a number"
			}
			if f != math.Trunc(f) {
				return "expected an integer"
			}
		case "boolean":
			if _, ok := decoded.(bool); !ok {
				return "expected a boolean"
			}
		case "object":
			if _, ok := decoded.(map[string]any); !ok {
				return "expected an object"
			}
		case "array":
			if _, ok := decoded.([]any); !ok {
				return "expected an array"
			}
		}
	}
	if spec.Type == "array" {
		var arr []json.RawMessage
		if json.Unmarshal(value, &arr) != nil {
			return "expected an array"
		}
		if items := bytes.TrimSpace(spec.Items); len(items) > 0 && !bytes.Equal(items, []byte("null")) {
			for _, item := range arr {
				if reason := validateProperty(item, spec.Items); reason != "" {
					return "array item: " + reason
				}
			}
		}
	}
	// Nested objects carry their own required/properties: without recursing here a check of
	// "is an object" alone lets `{"a":{}}` through a schema that declares `a.required`.
	if spec.Type == "object" || len(spec.Required) > 0 || len(spec.Properties) > 0 {
		if reason := validateObject(value, spec.Required, spec.Properties); reason != "" {
			return reason
		}
	}
	return ""
}

// validateObject enforces required/properties on an already-known object value.
//
// @purpose Shared recursion target for the object branch of validateProperty and array items, so
// nested schemas are enforced at any depth rather than only at the top level (NEW-03).
// @rationale Extracted rather than inlined because both the object type branch and the array-items
// path need identical semantics; duplicating them is how the depth-1-only gap appeared.
// @preconditions value may be any JSON fragment; a non-object value yields a short reason.
// @param value Decoded object value as raw JSON.
// @param required Names that must be present.
// @param properties Per-name schema nodes for declared properties.
// @returns Empty string when acceptable, else a short reason.
// @throws None; malformed values are reported, never panic.
// @invariants O(k) in declared properties plus the cost of recursion into each declared child.
func validateObject(value json.RawMessage, required []string, properties map[string]json.RawMessage) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(value, &obj); err != nil || obj == nil {
		return "expected an object"
	}
	for _, name := range required {
		if _, ok := obj[name]; !ok {
			return "missing required argument " + name
		}
	}
	for name, raw := range obj {
		propRaw, declared := properties[name]
		if !declared {
			continue // extra keys are permitted unless the schema says otherwise
		}
		if reason := validateProperty(raw, propRaw); reason != "" {
			return name + ": " + reason
		}
	}
	return ""
}

// jsonValuesEqual compares two decoded JSON scalars for schema-enum purposes.
//
// @purpose Enum matching without pulling in a deep-equality dependency.
// @rationale Schema enums are scalars in every published tool definition; comparing their rendered
// JSON is exact for that domain and avoids reflect.DeepEqual on arbitrary values.
// @preconditions None.
// @param a (any) - First decoded value (may be nil).
// @param b (any) - Second decoded value (may be nil).
// @returns true when both render to identical JSON.
// @throws None.
// @invariants O(n) in the value size; pure.
func jsonValuesEqual(a, b any) bool {
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// rejectLocked reports a rejected tool call without emitting it.
//
// @purpose Surfaces argument rejections to the caller's logger without breaking the turn.
// @rationale A malformed client tool call must be dropped, not forwarded and not fatal; the operator
// still needs to see it happened.
// @preconditions Called with c.mu held.
// @param id Tool call identifier as reported upstream.
// @param name Canonical client tool name.
// @param reason Short machine-readable cause, e.g. "arguments do not satisfy inputSchema".
// @returns None.
// @throws None.
// @invariants Dispatches asynchronously so the collector lock is never held across user callbacks.
func (c *toolCollector) rejectLocked(id, name, reason string) {
	if c.onInvalid != nil {
		go c.onInvalid(id, name, reason)
	}
}

// ClientErrorKind classifies a failure into the fixed vocabulary a client is allowed to see.
//
// @purpose Implements FR-18 for the mid-stream branches that bypass writeTurnError.
// @rationale Streaming has already sent a 200, so an error can only be delivered in-band; echoing
// err.Error() there leaks upstream paths, model ids and ACP internals (CWE-209). The class is what
// the caller acts on, so it is all they get.
// @preconditions err is non-nil.
// @param err Failure returned by the turn executor.
// @returns A stable transport-level error code, never derived from err.Error().
// @throws None.
// @invariants O(1); no string formatting from upstream data.
func ClientErrorKind(err error) string {
	if err == nil {
		return "api_error"
	}
	if errors.Is(err, ErrFirstTokenTimeout) {
		return "first_token_timeout"
	}
	var respErr *acp.ResponseError
	if errors.As(err, &respErr) {
		switch respErr.Code {
		case acp.ErrCodeAuthRequired:
			return "account_auth_required"
		case acp.ErrCodeInvalidParams:
			return "invalid_params"
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "inference_cancelled"
	}
	return "api_error"
}

// ClientErrorMessage renders the client-visible message for a failure class.
//
// @purpose Companion to ClientErrorKind; keeps the wording in one place for both providers.
// @rationale The message must be a constant per class so that no upstream text can reach the client.
// @preconditions None.
// @param kind Value returned by ClientErrorKind.
// @returns A fixed human-readable sentence.
// @throws None.
// @invariants Pure, O(1).
func ClientErrorMessage(kind string) string {
	switch kind {
	case "first_token_timeout":
		return "The upstream model did not produce a first token in time. Retry the request."
	case "unsupported_parameter":
		return "The request used a parameter the upstream agent cannot honour."
	case "account_auth_required":
		return "No authenticated upstream account is available."
	case "invalid_params":
		return "The upstream agent rejected the session configuration."
	case "inference_cancelled":
		return "The inference turn was cancelled before it completed."
	default:
		return "The upstream agent failed to complete the request."
	}
}

// openAIVisionRequest returns the first image-bearing content part, when present.
//
// @purpose Implements FR-07: agy advertises image support but fails with INVALID_ARGUMENT.
// @rationale Forwarding an image makes the agent answer about something it never received.
// @preconditions req may be nil.
// @param req Decoded OpenAI request.
// @returns A short description of the offending message role, or "" when no image was supplied.
// @throws None.
// @invariants O(messages); no mutation.
func openAIVisionRequest(req *openai.ChatCompletionRequest) string {
	if req == nil {
		return ""
	}
	for i, msg := range req.Messages {
		parts, ok := msg.Content.([]any)
		if !ok {
			continue
		}
		for _, part := range parts {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := pm["type"].(string); t == "image_url" || t == "input_image" {
				return fmt.Sprintf("messages[%d].content (role %q)", i, msg.Role)
			}
		}
	}
	return ""
}

// anthropicVisionRequest returns the first image-bearing block, when present.
//
// @purpose Implements FR-07 for the Anthropic surface.
// @rationale See openAIVisionRequest.
// @preconditions req may be nil.
// @param req Decoded Anthropic request.
// @returns A short description of the offending message index, or "" when no image was supplied.
// @throws None.
// @invariants O(messages*blocks).
func anthropicVisionRequest(req *anthropic.MessageRequest) string {
	if req == nil {
		return ""
	}
	for i, msg := range req.Messages {
		// The text and block representations are independent: an Anthropic message may
		// carry both, and skipping block inspection when Text is set let an image
		// through the reject policy (FR-07 / EC-25).
		for _, block := range msg.Content.Blocks {
			switch block.Type {
			case "image", "document":
				return fmt.Sprintf("messages[%d].content (type %q)", i, block.Type)
			}
		}
	}
	return ""
}
