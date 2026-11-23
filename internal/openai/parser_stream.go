package openai

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// StreamParser manages chunk-by-chunk demuxing of text and tool calls.
type StreamParser struct {
	delimiter    string
	openTag      string
	closeTag     string
	mu           sync.Mutex
	buffer       strings.Builder
	toolIndex    int
	inTool       bool
	toolJSONBuf  strings.Builder
	activeCallID string
	activeName   string
}

// NewStreamParser creates a parser instance.
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

// ParseChunk processes a raw text chunk and yields either delta text or tool call fragments.
func (sp *StreamParser) ParseChunk(chunk string) (textDelta string, toolCalls []ToolCall) {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	sp.buffer.WriteString(chunk)
	current := sp.buffer.String()

	for len(current) > 0 {
		if !sp.inTool {
			idx := strings.Index(current, sp.openTag)
			if idx == -1 {
				// No opening tag; check if trailing part might be a prefix of openTag
				keepLen := 0
				for k := len(sp.openTag) - 1; k > 0; k-- {
					if strings.HasSuffix(current, sp.openTag[:k]) {
						keepLen = k
						break
					}
				}
				emitLen := len(current) - keepLen
				if emitLen > 0 {
					textDelta += current[:emitLen]
					current = current[emitLen:]
				}
				sp.buffer.Reset()
				sp.buffer.WriteString(current)
				return textDelta, toolCalls
			}

			// Found opening tag; emit text before tag
			if idx > 0 {
				textDelta += current[:idx]
			}
			current = current[idx+len(sp.openTag):]
			sp.inTool = true
			sp.toolJSONBuf.Reset()
			sp.activeCallID = fmt.Sprintf("call_%s", uuid.New().String()[:9])
			sp.activeName = ""
		}

		if sp.inTool {
			idx := strings.Index(current, sp.closeTag)
			if idx == -1 {
				// Still accumulating tool JSON
				sp.toolJSONBuf.WriteString(current)
				sp.buffer.Reset()
				return textDelta, toolCalls
			}

			// Tool tag closed
			sp.toolJSONBuf.WriteString(current[:idx])
			current = current[idx+len(sp.closeTag):]
			sp.inTool = false

			fullJSON := sp.toolJSONBuf.String()
			var parsed struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				Arguments any    `json:"arguments"`
			}
			if err := json.Unmarshal([]byte(fullJSON), &parsed); err == nil {
				callID := parsed.ID
				if callID == "" {
					callID = sp.activeCallID
				}
				name := parsed.Name
				argBytes, _ := json.Marshal(parsed.Arguments)
				argStr := string(argBytes)

				idxVal := sp.toolIndex
				tc := ToolCall{
					Index: &idxVal,
					ID:    callID,
					Type:  "function",
					Function: FunctionCallData{
						Name:      name,
						Arguments: argStr,
					},
				}
				toolCalls = append(toolCalls, tc)
				sp.toolIndex++
			}
			sp.buffer.Reset()
			sp.buffer.WriteString(current)
		}
	}

	return textDelta, toolCalls
}

// ParseCompleteResponse parses a complete completion response for non-streaming mode.
func ParseCompleteResponse(rawText, delimiter string) (string, []ToolCall, bool) {
	if delimiter == "" {
		delimiter = "tool_call"
	}

	openTag := fmt.Sprintf("<%s>", delimiter)
	closeTag := fmt.Sprintf("</%s>", delimiter)

	pattern := regexp.MustCompile(fmt.Sprintf(`%s([\s\S]*?)%s`, regexp.QuoteMeta(openTag), regexp.QuoteMeta(closeTag)))
	matches := pattern.FindAllStringSubmatch(rawText, -1)

	if len(matches) == 0 {
		return rawText, nil, false
	}

	var toolCalls []ToolCall
	for i, m := range matches {
		if len(m) > 1 {
			var parsed struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				Arguments any    `json:"arguments"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(m[1])), &parsed); err == nil {
				callID := parsed.ID
				if callID == "" {
					callID = fmt.Sprintf("call_%s", uuid.New().String()[:9])
				}
				argBytes, _ := json.Marshal(parsed.Arguments)
				idx := i
				toolCalls = append(toolCalls, ToolCall{
					Index: &idx,
					ID:    callID,
					Type:  "function",
					Function: FunctionCallData{
						Name:      parsed.Name,
						Arguments: string(argBytes),
					},
				})
			}
		}
	}

	cleanText := strings.TrimSpace(pattern.ReplaceAllString(rawText, ""))
	return cleanText, toolCalls, len(toolCalls) > 0
}
