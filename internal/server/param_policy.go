package server

import (
	"net/http"
	"sort"
	"strings"

	"agy-gateway/internal/config"
	"agy-gateway/internal/openai"
)

// IgnoredParamsHeader names the response header that reports provider parameters the gateway could
// not honour.
//
// @purpose Makes the gap between the provider schema and ACP visible to the caller.
// @rationale ACP exposes exactly three settable values (model, thought_level, mode); everything else
// an OpenAI or Anthropic client sends is structurally unexpressible. Silently dropping those fields
// is the failure mode that made callers believe temperature/top_p were taking effect. Naming them per
// response turns an invisible lie into a debuggable fact, without breaking the request.
const IgnoredParamsHeader = "X-Agy-Ignored-Params"

// ignoredOpenAIParams reports which OpenAI request fields cannot be honoured by ACP.
//
// @purpose Single enumeration of the unsupported OpenAI surface, in a stable order.
// @rationale Keeping the list in one place means the header, the reject policy and the tests cannot
// disagree about what "unsupported" means. Fields that ARE honoured are deliberately absent:
// `model`, `messages`, `tools`, `tool_choice`, `stream`, `stream_options`, `reasoning_effort`
// (→ thought_level) and `stop` (enforced gateway-side by the stop-sequence matcher).
// @param req The decoded request; nil is treated as empty.
// @returns Field names in wire order, empty when nothing is unsupported.
// @invariants Pure, O(fields), deterministic ordering.
func ignoredOpenAIParams(req *openai.ChatCompletionRequest) []string {
	if req == nil {
		return nil
	}
	var out []string
	if req.Temperature != nil {
		out = append(out, "temperature")
	}
	if req.TopP != nil {
		out = append(out, "top_p")
	}
	if req.MaxTokens != nil {
		out = append(out, "max_tokens")
	}
	if req.MaxCompletionTokens != nil {
		out = append(out, "max_completion_tokens")
	}
	if req.PresencePenalty != nil {
		out = append(out, "presence_penalty")
	}
	if req.FrequencyPenalty != nil {
		out = append(out, "frequency_penalty")
	}
	if req.Seed != nil {
		out = append(out, "seed")
	}
	if req.LogitBias != nil && len(req.LogitBias) > 0 {
		out = append(out, "logit_bias")
	}
	if req.Logprobs != nil {
		out = append(out, "logprobs")
	}
	if req.TopLogprobs != nil {
		out = append(out, "top_logprobs")
	}
	if req.ParallelToolCalls != nil {
		out = append(out, "parallel_tool_calls")
	}
	return out
}

// ignoredAnthropicParams reports which Anthropic request fields cannot be honoured by ACP.
//
// @purpose Anthropic counterpart of ignoredOpenAIParams.
// @rationale `thinking` maps deterministically onto thought_level and `stop_sequences` is enforced
// gateway-side, so neither is listed. `max_tokens` is also excluded although ACP has no equivalent:
// the Anthropic schema requires it, so listing it would make the `reject` policy fail every valid
// request — a mandatory field cannot be an optional-advice case.
// @param req The decoded request; nil is treated as empty.
// @returns Field names in wire order.
// @invariants Pure, O(fields), deterministic ordering.
func ignoredAnthropicParams(req *anthropicRequestView) []string {
	if req == nil {
		return nil
	}
	var out []string
	if req.Temperature != nil {
		out = append(out, "temperature")
	}
	if req.TopP != nil {
		out = append(out, "top_p")
	}
	if req.TopK != nil {
		out = append(out, "top_k")
	}
	return out
}

// anthropicRequestView is the subset of the Anthropic request the policy inspects.
//
// @purpose Avoids importing the provider types into this file twice and keeps the enumeration
// readable.
// @rationale Only presence matters; values are never interpreted. TopK is an *int while the other
// sampling fields are *float64, so the struct mirrors the provider types rather than unifying them.
type anthropicRequestView struct {
	Temperature   *float64
	TopP          *float64
	TopK          *int
	StopSequences []string
}

// annotateIgnoredParams writes the reporting header.
//
// @purpose Single emission point for the ignored-parameter header.
// @rationale A header must be set before the body is written, so every response path calls this
// before any write.
// @param w Response writer.
// @param params Unsupported field names; empty clears nothing.
// @returns The names, for logging.
// @invariants Sort-free except for a final join; never panics.
func annotateIgnoredParams(w http.ResponseWriter, params []string) string {
	if len(params) == 0 {
		return ""
	}
	joined := strings.Join(params, ",")
	w.Header().Set(IgnoredParamsHeader, joined)
	return joined
}

// rejectUnsupportedParams implements the `reject` policy.
//
// @purpose Refuses a request that carries a parameter the gateway cannot honour.
// @rationale Dedicated parameters must not silently diverge. The error is the provider's own shape so
// a harness sees a normal 400 with `param`.
// @param params Unsupported field names.
// @param write Provider-specific error writer.
// @returns True when the request was refused (response already written).
// @invariants Calls write at most once.
func rejectUnsupportedParams(cfg *config.Config, params []string, write func(status int, message, errType, errCode, param string)) bool {
	if len(params) == 0 {
		return false
	}
	if cfg == nil || !strings.EqualFold(cfg.Execution.UnsupportedParameterPolicy, config.UnsupportedParameterPolicyReject) {
		return false
	}
	write(http.StatusBadRequest,
		"Unsupported parameter: "+params[0]+" cannot be represented by the Antigravity ACP backend",
		"invalid_request_error", "unsupported_parameter", params[0])
	return true
}

// sortedCopy returns a sorted copy so logs are stable regardless of map iteration.
//
// @purpose Deterministic log output.
// @param in Input slice.
// @returns A new sorted slice (nil in, nil out).
// @invariants Pure.
func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
