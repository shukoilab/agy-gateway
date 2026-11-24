package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// StreamParser manages chunk-by-chunk demuxing of text and Anthropic tool_use calls.
//
// @purpose Buffers incoming ACP text chunks to isolate delimited tool calls (<tool_call>)
// without leaking delimiter markup into client text deltas.
// @rationale Ensures zero-semantic-loss and clean streaming for Anthropic clients.
// @preconditions delimiter must be non-empty.
type StreamParser struct {
	delimiter      string
	openTag        string
	closeTag       string
	mu             sync.Mutex
	buffer         strings.Builder
	toolIndex      int
	inTool         bool
	toolJSONBuf    strings.Builder
	activeCallID   string
	started        bool
	hasEmittedTool bool
	terminated     bool
}

// IsTerminated reports whether the stream has encountered an early stop sequence (e.g. hallucinated turn boundary).
func (sp *StreamParser) IsTerminated() bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.terminated
}

// cleanLeadingHallucinatedPrefix strips model-echoed turn markers (e.g. "Assistant:", "User: [Tool Execution Results]", "Tool Output (id: ...)")
// from the beginning of assistant generation.
func cleanLeadingHallucinatedPrefix(s string) (string, bool) {
	modified := false
	for {
		trimmed := strings.TrimLeft(s, " \t\r\n")
		matched := false

		// 1. Fixed turn prefixes
		prefixes := []string{
			"Assistant:",
			"User: [Tool Execution Results]",
			"[Tool Execution Results]",
			"[SYSTEM DIRECTIVE:",
			"User:",
		}
		for _, p := range prefixes {
			if strings.HasPrefix(trimmed, p) {
				s = trimmed[len(p):]
				matched = true
				modified = true
				break
			}
		}
		if matched {
			continue
		}

		// 2. Line-based prefixes: Tool Output / Tool Error
		if strings.HasPrefix(trimmed, "Tool Output") || strings.HasPrefix(trimmed, "Tool Error") {
			nl := strings.IndexByte(trimmed, '\n')
			if nl != -1 {
				s = trimmed[nl+1:]
				matched = true
				modified = true
				continue
			} else {
				s = ""
				modified = true
				break
			}
		}

		break
	}
	return strings.TrimLeft(s, " \t\r\n"), modified
}

// isPotentialPrefix checks if a string is a partial prefix of known hallucinated role markers.
func isPotentialPrefix(s string) bool {
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if len(trimmed) == 0 {
		return true // purely leading whitespace, hold until text arrives
	}
	candidates := []string{
		"Assistant:",
		"User: [Tool Execution Results]",
		"[Tool Execution Results]",
		"[SYSTEM DIRECTIVE:",
		"Tool Output",
		"Tool Error",
		"User:",
	}
	for _, cand := range candidates {
		if len(trimmed) < len(cand) && strings.HasPrefix(cand, trimmed) {
			return true
		}
	}
	return false
}

// findEarliestStopMarker returns the index and matched string of the earliest stop marker in s.
//
// @purpose Identifies boundary markers where model starts hallucinating subsequent turns or tool results.
// @rationale Hallucinated turn markers like "User: [Tool Execution Results]" or "\nUser:" must be truncated
// immediately to preserve conversational integrity and prevent token/turn leaks.
// @preconditions s is a valid string.
// @param s Target string to inspect.
// @param hasEmittedTool Flag indicating whether at least one tool call has been emitted in the current turn.
// @returns Index and matched marker string, or (-1, "") if not found.
// @invariants Pure string search, O(len(markers) * len(s)) worst-case, thread-safe.
func findEarliestStopMarker(s string, hasEmittedTool bool) (int, string) {
	markers := []string{
		"User: [Tool Execution Results]",
		"[Tool Execution Results]",
		"Tool Output (id:",
		"Tool Error (id:",
	}
	if hasEmittedTool {
		markers = append(markers,
			"\nUser:",
			"\nAssistant:",
			"Tool Output:",
			"Tool Error:",
			"\n[SYSTEM DIRECTIVE:",
			"[SYSTEM DIRECTIVE:",
			"\nHuman:",
		)
	}

	earliestIdx := -1
	var earliestMarker string
	for _, m := range markers {
		idx := strings.Index(s, m)
		if idx != -1 {
			if earliestIdx == -1 || idx < earliestIdx {
				earliestIdx = idx
				earliestMarker = m
			}
		}
	}
	return earliestIdx, earliestMarker
}

// longestPartialStopMarkerSuffix returns the length of the longest suffix of s matching a stop marker prefix.
func longestPartialStopMarkerSuffix(s string, hasEmittedTool bool) int {
	markers := []string{
		"User: [Tool Execution Results]",
		"[Tool Execution Results]",
		"Tool Output (id:",
		"Tool Error (id:",
	}
	if hasEmittedTool {
		markers = append(markers,
			"\nUser:",
			"\nAssistant:",
			"Tool Output:",
			"Tool Error:",
			"\n[SYSTEM DIRECTIVE:",
			"[SYSTEM DIRECTIVE:",
			"\nHuman:",
		)
	}

	maxKeep := 0
	for _, m := range markers {
		for k := len(m) - 1; k > 0; k-- {
			if strings.HasSuffix(s, m[:k]) {
				if k > maxKeep {
					maxKeep = k
				}
				break
			}
		}
	}
	return maxKeep
}

// cleanToolJSON cleans and normalizes raw tool call JSON payloads emitted inside delimiter tags.
//
// @purpose Strips markdown code fences, leading/trailing commentary, and extracts the outermost JSON object { ... }.
// @rationale LLMs frequently wrap function calls inside ```json ... ``` blocks or add conversational framing,
// causing strict JSON unmarshalers to fail and silently drop tool calls.
// @preconditions raw is the raw text extracted between opening and closing delimiter tags.
// @param raw Uncleaned string payload.
// @returns Sanitized JSON string suitable for unmarshaling.
// @invariants Idempotent, zero mutation of external state.
func cleanToolJSON(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	// 1. Strip markdown code fences if present: ```json ... ``` or ``` ... ```
	if strings.HasPrefix(trimmed, "```") {
		if newlineIdx := strings.Index(trimmed, "\n"); newlineIdx != -1 {
			trimmed = strings.TrimSpace(trimmed[newlineIdx+1:])
		} else {
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
		}
		if strings.HasSuffix(trimmed, "```") {
			trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, "```"))
		}
	}

	// 2. Isolate outermost JSON object bounds if surrounded by stray text
	firstBrace := strings.Index(trimmed, "{")
	lastBrace := strings.LastIndex(trimmed, "}")
	if firstBrace != -1 && lastBrace > firstBrace {
		trimmed = strings.TrimSpace(trimmed[firstBrace : lastBrace+1])
	}

	return trimmed
}

// sanitizeTextContent strips any residual internal gateway markers from emitted text lines.
func sanitizeTextContent(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	var kept []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "User: [Tool Execution Results]") ||
			strings.HasPrefix(trimmed, "[Tool Execution Results]") ||
			strings.HasPrefix(trimmed, "Tool Output (id:") ||
			strings.HasPrefix(trimmed, "Tool Output:") ||
			strings.HasPrefix(trimmed, "Tool Error (id:") ||
			strings.HasPrefix(trimmed, "Tool Error:") ||
			strings.HasPrefix(trimmed, "[SYSTEM DIRECTIVE:") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// NewStreamParser creates an initialized streaming parser.
func NewStreamParser(delimiter string) *StreamParser {
	if delimiter == "" {
		delimiter = "tool_call"
	}
	return &StreamParser{
		delimiter: delimiter,
		openTag:   fmt.Sprintf("<%s>", delimiter),
		closeTag:  fmt.Sprintf("</%s>", delimiter),
	}
}

// ParseChunk processes a raw text chunk and yields either delta text or completed tool_use blocks.
func (sp *StreamParser) ParseChunk(chunk string) (textDelta string, toolBlocks []ContentBlock) {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	if sp.terminated {
		return "", nil
	}

	sp.buffer.WriteString(chunk)
	current := sp.buffer.String()

	if !sp.started {
		if isPotentialPrefix(current) && len(current) < 32 {
			return "", nil
		}
		cleaned, wasStripped := cleanLeadingHallucinatedPrefix(current)
		if wasStripped {
			current = cleaned
			sp.buffer.Reset()
			sp.buffer.WriteString(current)
		}
		if len(current) > 0 && !isPotentialPrefix(current) {
			sp.started = true
		}
	}

	for len(current) > 0 && !sp.terminated {
		if !sp.inTool {
			openIdx := strings.Index(current, sp.openTag)
			stopIdx, _ := findEarliestStopMarker(current, sp.hasEmittedTool)

			// If a stop marker appears before any openTag (or openTag doesn't exist):
			if stopIdx != -1 && (openIdx == -1 || stopIdx < openIdx) {
				// We hit a hallucinated turn boundary / stop sequence!
				if stopIdx > 0 {
					textDelta += sanitizeTextContent(current[:stopIdx])
				}
				sp.terminated = true
				sp.buffer.Reset()
				return textDelta, toolBlocks
			}

			if openIdx == -1 {
				// No opening tag; check if trailing part might be a prefix of openTag or a stop marker
				keepLenTag := 0
				for k := len(sp.openTag) - 1; k > 0; k-- {
					if strings.HasSuffix(current, sp.openTag[:k]) {
						keepLenTag = k
						break
					}
				}
				keepLenStop := longestPartialStopMarkerSuffix(current, sp.hasEmittedTool)
				keepLen := keepLenTag
				if keepLenStop > keepLen {
					keepLen = keepLenStop
				}

				emitLen := len(current) - keepLen
				if emitLen > 0 {
					textDelta += sanitizeTextContent(current[:emitLen])
					current = current[emitLen:]
				}
				sp.buffer.Reset()
				sp.buffer.WriteString(current)
				return textDelta, toolBlocks
			}

			// Found opening tag; emit text before tag
			if openIdx > 0 {
				textDelta += sanitizeTextContent(current[:openIdx])
			}
			current = current[openIdx+len(sp.openTag):]
			sp.inTool = true
			sp.toolJSONBuf.Reset()
			sp.activeCallID = fmt.Sprintf("toolu_%s", uuid.New().String()[:12])
		}

		if sp.inTool {
			idx := strings.Index(current, sp.closeTag)
			if idx == -1 {
				// Still accumulating tool JSON; retain partial prefix of closeTag in buffer if split across chunks
				keepLen := 0
				for k := len(sp.closeTag) - 1; k > 0; k-- {
					if strings.HasSuffix(current, sp.closeTag[:k]) {
						keepLen = k
						break
					}
				}
				emitLen := len(current) - keepLen
				if emitLen > 0 {
					sp.toolJSONBuf.WriteString(current[:emitLen])
					current = current[emitLen:]
				}
				sp.buffer.Reset()
				sp.buffer.WriteString(current)
				return textDelta, toolBlocks
			}

			// Tool tag closed
			sp.toolJSONBuf.WriteString(current[:idx])
			current = current[idx+len(sp.closeTag):]
			sp.inTool = false

			fullJSON := sp.toolJSONBuf.String()
			cleanedJSON := cleanToolJSON(fullJSON)
			var parsed struct {
				ID           string          `json:"id"`
				CallID       string          `json:"call_id"`
				Name         string          `json:"name"`
				Function     string          `json:"function"`
				FunctionName string          `json:"function_name"`
				Action       string          `json:"action"`
				Arguments    json.RawMessage `json:"arguments"`
				Parameters   json.RawMessage `json:"parameters"`
				Input        json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal([]byte(cleanedJSON), &parsed); err == nil && (parsed.Name != "" || parsed.Function != "" || parsed.FunctionName != "" || parsed.Action != "") {
				callID := parsed.ID
				if callID == "" {
					callID = parsed.CallID
				}
				if callID == "" {
					callID = sp.activeCallID
				}
				if !strings.HasPrefix(callID, "toolu_") {
					callID = "toolu_" + callID
				}

				name := parsed.Name
				if name == "" {
					name = parsed.Function
				}
				if name == "" {
					name = parsed.FunctionName
				}
				if name == "" {
					name = parsed.Action
				}

				rawInputArgs := parsed.Arguments
				if len(rawInputArgs) == 0 || string(rawInputArgs) == "null" {
					if len(parsed.Parameters) > 0 {
						rawInputArgs = parsed.Parameters
					} else if len(parsed.Input) > 0 {
						rawInputArgs = parsed.Input
					}
				}

				rawInput := normalizeToolInput(rawInputArgs)

				toolBlocks = append(toolBlocks, ContentBlock{
					Type:  "tool_use",
					ID:    callID,
					Name:  name,
					Input: rawInput,
				})
				sp.toolIndex++
				sp.hasEmittedTool = true
			} else {
				// Resilient fallback: emit unparseable tag payload as text rather than silently dropping
				textDelta += sanitizeTextContent(fullJSON)
			}
			sp.buffer.Reset()
			sp.buffer.WriteString(current)
		}
	}

	return textDelta, toolBlocks
}

// Flush forces emission of any pending unparsed buffer text and recovers any unclosed
// tool_use markup before the stream terminates.
func (sp *StreamParser) Flush() (textDelta string, toolBlocks []ContentBlock) {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	if sp.terminated {
		return "", nil
	}

	if !sp.started {
		cleaned, wasStripped := cleanLeadingHallucinatedPrefix(sp.buffer.String())
		if wasStripped {
			sp.buffer.Reset()
			sp.buffer.WriteString(cleaned)
		}
		sp.started = true
	}

	if sp.inTool {
		fullJSON := strings.TrimSpace(sp.toolJSONBuf.String() + sp.buffer.String())
		// Resilient recovery: strip any trailing complete or partial close tag that was buffered or swallowed
		if idx := strings.Index(fullJSON, sp.closeTag); idx != -1 {
			fullJSON = fullJSON[:idx]
		} else {
			for k := len(sp.closeTag) - 1; k > 0; k-- {
				if strings.HasSuffix(fullJSON, sp.closeTag[:k]) {
					fullJSON = strings.TrimSuffix(fullJSON, sp.closeTag[:k])
					break
				}
			}
		}
		fullJSON = strings.TrimSpace(fullJSON)
		cleanedJSON := cleanToolJSON(fullJSON)

		var parsed struct {
			ID           string          `json:"id"`
			CallID       string          `json:"call_id"`
			Name         string          `json:"name"`
			Function     string          `json:"function"`
			FunctionName string          `json:"function_name"`
			Action       string          `json:"action"`
			Arguments    json.RawMessage `json:"arguments"`
			Parameters   json.RawMessage `json:"parameters"`
			Input        json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal([]byte(cleanedJSON), &parsed); err == nil && (parsed.Name != "" || parsed.Function != "" || parsed.FunctionName != "" || parsed.Action != "") {
			callID := parsed.ID
			if callID == "" {
				callID = parsed.CallID
			}
			if callID == "" {
				callID = sp.activeCallID
			}
			if !strings.HasPrefix(callID, "toolu_") {
				callID = "toolu_" + callID
			}

			name := parsed.Name
			if name == "" {
				name = parsed.Function
			}
			if name == "" {
				name = parsed.FunctionName
			}
			if name == "" {
				name = parsed.Action
			}

			rawInputArgs := parsed.Arguments
			if len(rawInputArgs) == 0 || string(rawInputArgs) == "null" {
				if len(parsed.Parameters) > 0 {
					rawInputArgs = parsed.Parameters
				} else if len(parsed.Input) > 0 {
					rawInputArgs = parsed.Input
				}
			}

			rawInput := normalizeToolInput(rawInputArgs)
			toolBlocks = append(toolBlocks, ContentBlock{
				Type:  "tool_use",
				ID:    callID,
				Name:  name,
				Input: rawInput,
			})
			sp.toolIndex++
			sp.hasEmittedTool = true
		} else {
			textDelta = sanitizeTextContent(sp.openTag + fullJSON)
		}
		sp.inTool = false
		sp.toolJSONBuf.Reset()
		sp.buffer.Reset()
		return textDelta, toolBlocks
	}

	if sp.buffer.Len() > 0 {
		bufStr := sp.buffer.String()
		sp.buffer.Reset()
		stopIdx, _ := findEarliestStopMarker(bufStr, sp.hasEmittedTool)
		if stopIdx != -1 {
			bufStr = bufStr[:stopIdx]
		}
		textDelta = sanitizeTextContent(bufStr)
	}
	return textDelta, toolBlocks
}

// ParseCompleteResponse parses a complete completion response for non-streaming mode.
func ParseCompleteResponse(rawText, delimiter string) (string, []ContentBlock, bool) {
	if delimiter == "" {
		delimiter = "tool_call"
	}

	closeTag := fmt.Sprintf("</%s>", delimiter)

	// 1. If tool calls exist, find the first stop marker AFTER any closing tool tag.
	// Any tool calls or text after that stop marker are part of a hallucinated subsequent turn.
	effectiveText := rawText
	firstCloseTagIdx := strings.Index(effectiveText, closeTag)
	if firstCloseTagIdx != -1 {
		searchRegion := effectiveText[firstCloseTagIdx+len(closeTag):]
		stopIdx, _ := findEarliestStopMarker(searchRegion, true)
		if stopIdx != -1 {
			effectiveText = effectiveText[:firstCloseTagIdx+len(closeTag)+stopIdx]
		}
	} else {
		// No tool tag closed; check for invariant stop markers
		stopIdx, _ := findEarliestStopMarker(effectiveText, false)
		if stopIdx != -1 {
			effectiveText = effectiveText[:stopIdx]
		}
	}

	pattern := regexp.MustCompile(fmt.Sprintf(`<%s\s*>([\s\S]*?)</%s\s*>`, regexp.QuoteMeta(delimiter), regexp.QuoteMeta(delimiter)))
	matches := pattern.FindAllStringSubmatch(effectiveText, -1)

	var toolBlocks []ContentBlock
	for _, m := range matches {
		if len(m) > 1 {
			cleanedJSON := cleanToolJSON(m[1])
			var parsed struct {
				ID           string          `json:"id"`
				CallID       string          `json:"call_id"`
				Name         string          `json:"name"`
				Function     string          `json:"function"`
				FunctionName string          `json:"function_name"`
				Action       string          `json:"action"`
				Arguments    json.RawMessage `json:"arguments"`
				Parameters   json.RawMessage `json:"parameters"`
				Input        json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal([]byte(cleanedJSON), &parsed); err == nil && (parsed.Name != "" || parsed.Function != "" || parsed.FunctionName != "" || parsed.Action != "") {
				callID := parsed.ID
				if callID == "" {
					callID = parsed.CallID
				}
				if callID == "" {
					callID = fmt.Sprintf("toolu_%s", uuid.New().String()[:12])
				}
				if !strings.HasPrefix(callID, "toolu_") {
					callID = "toolu_" + callID
				}

				name := parsed.Name
				if name == "" {
					name = parsed.Function
				}
				if name == "" {
					name = parsed.FunctionName
				}
				if name == "" {
					name = parsed.Action
				}

				rawInputArgs := parsed.Arguments
				if len(rawInputArgs) == 0 || string(rawInputArgs) == "null" {
					if len(parsed.Parameters) > 0 {
						rawInputArgs = parsed.Parameters
					} else if len(parsed.Input) > 0 {
						rawInputArgs = parsed.Input
					}
				}

				rawInput := normalizeToolInput(rawInputArgs)

				toolBlocks = append(toolBlocks, ContentBlock{
					Type:  "tool_use",
					ID:    callID,
					Name:  name,
					Input: rawInput,
				})
			}
		}
	}

	cleanText := strings.TrimSpace(pattern.ReplaceAllString(effectiveText, ""))
	cleanText, _ = cleanLeadingHallucinatedPrefix(cleanText)
	cleanText = strings.TrimSpace(sanitizeTextContent(cleanText))
	return cleanText, toolBlocks, len(toolBlocks) > 0
}

// normalizeToolInput normalizes polymorphic tool call arguments into raw JSON object bytes.
func normalizeToolInput(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return json.RawMessage("{}")
	}
	// If the model emitted arguments as a stringified JSON string (e.g. "{\"cmd\":\"pwd\"}")
	if len(trimmed) >= 2 && trimmed[0] == '"' && trimmed[len(trimmed)-1] == '"' {
		var unquoted string
		if err := json.Unmarshal(trimmed, &unquoted); err == nil {
			unquotedTrimmed := strings.TrimSpace(unquoted)
			if strings.HasPrefix(unquotedTrimmed, "{") && strings.HasSuffix(unquotedTrimmed, "}") {
				return json.RawMessage(unquotedTrimmed)
			}
		}
	}
	return raw
}
