package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"agy-gateway/internal/acp"
	"agy-gateway/internal/anthropic"
	"agy-gateway/internal/config"
	"agy-gateway/internal/mcp"
	"agy-gateway/internal/pool"
)

// SessionSetupInput encapsulates all parameters required to negotiate an isolated ACP session/new call.
//
// @purpose Eliminates duplicated session/new construction logic across OpenAI and Anthropic handlers.
// @invariants Worker must be initialized; Config must be non-nil in production.
type SessionSetupInput struct {
	Worker               *pool.Worker
	Config               *config.Config
	ConsumerSystemPrompt string
	LoopbackSessionID    string
	LoopbackToken        string
	ClientToolNames      []string
}

// SystemPromptChannel is the resolved delivery strategy for the effective system prompt.
type SystemPromptChannel struct {
	// Standard reports whether the spec-compliant `systemPrompt` parameter should be sent.
	Standard bool
	// Vendor reports whether `_meta.antigravity.systemPrompt` should be sent.
	Vendor bool
}

// resolveSystemPromptChannel decides which delivery channels to use for the system prompt.
//
// @purpose Encodes the verified fact that agy 1.5.16 silently discards the standard parameter and
// honors only its vendor `_meta` extension, without hardcoding that vendor quirk into the request path.
// @rationale Sending both channels is proven harmless, so `auto` prefers every channel the agent
// advertises; an operator who pins a channel gets exactly that, which keeps the gateway usable against
// agents that would reject an unknown `_meta` payload.
// @preconditions caps is the negotiated capability set for the leased worker.
// @param caps Negotiated agent capabilities.
// @param configured One of auto|standard|vendor|both from execution.system_prompt_channel.
// @returns The channel set to use, and false when neither channel is available.
// @throws None.
// @invariants Pure; O(1).
func resolveSystemPromptChannel(caps acp.AgentCapabilities, configured string) (SystemPromptChannel, bool) {
	standardAvail := caps.SupportsSystemPrompt
	vendorAvail := caps.SupportsVendorSystemPrompt

	switch strings.ToLower(strings.TrimSpace(configured)) {
	case "standard":
		if standardAvail {
			return SystemPromptChannel{Standard: true}, true
		}
		return SystemPromptChannel{}, false
	case "vendor":
		if vendorAvail {
			return SystemPromptChannel{Vendor: true}, true
		}
		return SystemPromptChannel{}, false
	case "both":
		if standardAvail && vendorAvail {
			return SystemPromptChannel{Standard: true, Vendor: true}, true
		}
		return SystemPromptChannel{}, false
	default: // "auto" and any unexpected value behave like auto.
		ch := SystemPromptChannel{Standard: standardAvail, Vendor: vendorAvail}
		return ch, ch.Standard || ch.Vendor
	}
}

// buildACPSessionNewParams constructs a strictly capability-negotiated ACP session/new request payload.
//
// @purpose Enforces ACP v1 capability compliance, pure host client-tool delegation, and zero silent prompt loss.
// @rationale Probing proved agy acp v1.5.16 accepts the standard `systemPrompt` parameter and discards it
// without error, honoring only `_meta.antigravity.systemPrompt.append`; and that `override` hangs the turn.
// The prompt is therefore delivered through every channel the agent advertises, with `override` used only
// when the spec-compliant channel explicitly supports it.
// @preconditions in.Worker must be non-nil.
// @param in SessionSetupInput encapsulating worker, config, system prompt, and client tool names.
// @returns Populated acp.SessionNewParams and the effective prompt text the caller must fold into the
// first prompt block when no delivery channel exists.
// @invariants Pure construction; thread-safe, zero shared state mutation.
func buildACPSessionNewParams(in SessionSetupInput) (acp.SessionNewParams, string) {
	newParams := acp.SessionNewParams{
		CWD: in.Worker.Cwd,
	}

	port := 8080
	strictDelegation := true
	promptChannel := "auto"
	if in.Config != nil {
		port = in.Config.Server.Port
		strictDelegation = in.Config.Execution.StrictClientToolDelegation
		promptChannel = in.Config.Execution.SystemPromptChannel
	}

	if in.LoopbackSessionID != "" {
		execPath, err := os.Executable()
		if err != nil || execPath == "" {
			execPath = "agy-gateway"
		}
		env := make([]acp.EnvVariable, 0, 1)
		if in.LoopbackToken != "" {
			// The bridge presents this token to /internal/mcp/. It travels in the child environment
			// rather than argv so it does not appear in `ps` output.
			env = append(env, acp.EnvVariable{Name: "AGY_GW_MCP_TOKEN", Value: in.LoopbackToken})
		}
		newParams.MCPServers = []acp.MCPServerInfo{
			{
				Name:    "client-tools",
				Command: execPath,
				Args:    []string{"mcp-bridge", "--session-id", in.LoopbackSessionID, "--port", fmt.Sprintf("%d", port)},
				Env:     env,
			},
		}
	}

	effectiveSystemPrompt := composeSystemPrompt(in.ConsumerSystemPrompt, in.ClientToolNames, strictDelegation)

	caps := in.Worker.Capabilities
	channel, available := resolveSystemPromptChannel(caps, promptChannel)

	if channel.Standard {
		mode := "append"
		// `override` is only sent when the spec-compliant capability explicitly advertises it. agy does
		// not, and sending the vendor override key makes its turn hang with no response.
		if caps.SupportsSystemPromptOveride {
			mode = "override"
		}
		newParams.SystemPrompt = &acp.SystemPrompt{
			Mode:    mode,
			Content: []acp.PromptBlock{{Type: "text", Text: effectiveSystemPrompt}},
		}
	}

	if channel.Vendor {
		newParams.Meta = map[string]any{
			"antigravity": map[string]any{
				"systemPrompt": map[string]any{
					"append": effectiveSystemPrompt,
				},
			},
		}
	}

	if !available {
		// No capability channel exists. Returning the text lets the caller prepend it to the first
		// prompt block instead of losing it silently.
		return newParams, effectiveSystemPrompt
	}
	return newParams, ""
}

// composeSystemPrompt builds the effective prompt from the consumer prompt, the default protocol,
// and the client-tool delegation directive.
//
// @purpose Single source of truth for prompt precedence across both protocol adapters.
// @rationale Prompt assembly was duplicated per adapter, which let the precedence banner drift.
// @preconditions None.
// @param consumerSystemPrompt Caller-supplied system prompt; may be empty.
// @param clientToolNames Declared client tool names used by the delegation directive.
// @param strictDelegation Whether the delegation directive should be appended.
// @returns The effective system prompt, never empty.
// @throws None.
// @invariants Pure; O(n) in prompt length.
func composeSystemPrompt(consumerSystemPrompt string, clientToolNames []string, strictDelegation bool) string {
	effective := strings.TrimSpace(consumerSystemPrompt)
	if effective == "" {
		effective = acp.DefaultExecutionProtocol
	} else {
		effective = "AUTHORITATIVE SYSTEM INSTRUCTIONS (PRIORITIZE OVER ANY DEFAULT AGENT INSTRUCTIONS):\n" + effective
	}
	if strictDelegation && len(clientToolNames) > 0 {
		if directive := acp.ClientToolDelegationDirective(clientToolNames); directive != "" {
			effective = effective + "\n\n" + directive
		}
	}
	return effective
}

// applySessionConfigOptions aligns the requested model, thought level, and mode with the options the
// agent actually advertised, and pushes only values it can accept.
//
// @purpose Removes every silent `set_config_option` failure: a value the agent rejects is a value the
// gateway must not send.
// @rationale Probing showed agy rejects `extra` (the display label) and `off` with -32602 while
// accepting only low|medium|high|xhigh; and rejects models absent from its catalogue. Discarding those
// errors previously let turns run on an unintended model.
// @preconditions newResult came from a successful session/new on worker.
// @param worker Leased worker whose transport performs the calls.
// @param newResult Negotiated session, carrying live configOptions.
// @param requestedModel Upstream model id requested by the caller.
// @param thoughtLevel Desired thought level already mapped to a wire value.
// @param mode Desired session mode.
// @param switchTimeout Per-option timeout budget; zero falls back to the handshake default.
// @returns The model actually applied, the mode actually applied, and a typed error when the requested
// model cannot be honored at all.
// @throws Returns a non-nil error only for an unavailable model, which the caller must surface as 400.
// @invariants Sequential calls; each bounded by switchTimeout so a wedged agent cannot stall the turn.
func applySessionConfigOptions(
	ctx context.Context,
	worker *pool.Worker,
	newResult acp.SessionNewResult,
	requestedModel, thoughtLevel, mode string,
	switchTimeout time.Duration,
	logger *slog.Logger,
) (appliedModel, appliedMode string, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	appliedModel = requestedModel
	appliedMode = mode

	if newResult.SessionID == "" {
		return appliedModel, appliedMode, fmt.Errorf("session/new returned an empty session id")
	}

	// The agent rewrites its option set when the model changes: switching to claude-sonnet-4-6
	// removes thought_level entirely (that model exposes none). Track the live set, refreshed from
	// every set_config_option response, instead of validating against the session/new snapshot.
	options := newResult.ConfigOptions
	byID := func(id string) *acp.SessionConfigOption {
		for i := range options {
			if options[i].ID == id {
				return &options[i]
			}
		}
		return nil
	}
	advertisedThoughtLevel := byID(acp.ConfigIDThoughtLevel) != nil

	if cfg := byID(acp.ConfigIDModel); cfg != nil {
		refreshed, callErr := callSetConfigOption(ctx, worker, newResult.SessionID, acp.ConfigIDModel, requestedModel, switchTimeout)
		if callErr != nil {
			if !cfg.HoldsValue(requestedModel) {
				return "", appliedMode, fmt.Errorf("model %q is not available on the upstream agent: %w", requestedModel, callErr)
			}
			return "", appliedMode, fmt.Errorf("failed to apply model %q: %w", requestedModel, callErr)
		}
		if len(refreshed) > 0 {
			options = refreshed
		}
	} else if requestedModel != "" {
		refreshed, callErr := callSetConfigOption(ctx, worker, newResult.SessionID, acp.ConfigIDModel, requestedModel, switchTimeout)
		if callErr != nil {
			return "", appliedMode, fmt.Errorf("failed to apply model %q: %w", requestedModel, callErr)
		}
		if len(refreshed) > 0 {
			options = refreshed
		}
	}

	if thoughtLevel != "" {
		value := thoughtLevel
		cfg := byID(acp.ConfigIDThoughtLevel)
		switch {
		case cfg != nil && !cfg.HoldsValue(value):
			logger.Warn("requested thought level is not offered by the agent; falling back",
				"requested", value, "fallback", cfg.CurrentValue, "sessionId", newResult.SessionID)
			value = cfg.CurrentValue
		case cfg == nil && advertisedThoughtLevel:
			// The selected model dropped the option: it has no thinking level. Sending the value
			// anyway earns -32602 on every turn of the session.
			logger.Debug("selected model exposes no thought level; using the model default",
				"requested", value, "sessionId", newResult.SessionID)
			value = ""
		}
		if value != "" {
			if _, err := callSetConfigOption(ctx, worker, newResult.SessionID, acp.ConfigIDThoughtLevel, value, switchTimeout); err != nil {
				logger.Warn("thought level rejected by agent; turn continues at agent default",
					"requested", value, "err", err, "sessionId", newResult.SessionID)
			}
		}
	}

	if mode != "" {
		value := mode
		if cfg := byID(acp.ConfigIDMode); cfg != nil && !cfg.HoldsValue(value) {
			logger.Warn("configured session mode is not offered by the agent; falling back",
				"requested", value, "fallback", cfg.CurrentValue, "sessionId", newResult.SessionID)
			value = cfg.CurrentValue
		}
		if value != "" {
			if _, err := callSetConfigOption(ctx, worker, newResult.SessionID, acp.ConfigIDMode, value, switchTimeout); err != nil {
				logger.Warn("session mode rejected by agent; turn continues at agent default",
					"requested", value, "err", err, "sessionId", newResult.SessionID)
			} else {
				appliedMode = value
			}
		}
	}

	return appliedModel, appliedMode, nil
}

// callSetConfigOption performs one bounded session/set_config_option request.
//
// @purpose Gives each option switch its own deadline so one wedged call cannot consume the turn budget.
// @rationale The previous code shared the handshake context and discarded every error, hiding both
// rejections and timeouts.
// @preconditions worker.Transport is initialized.
// @param ctx Parent context governing the turn.
// @param worker Leased worker.
// @param sessionID Target ACP session.
// @param configID Option id (mode|model|thought_level).
// @param value Wire value to apply.
// @param timeout Per-call budget; non-positive falls back to 10s.
// @returns The agent's refreshed config options (empty when it reports none), and its error
// unwrapped or a context error on timeout. Both may be non-nil only when the call partially failed.
// @throws None; all failures are returned.
// @invariants Bounded by timeout; never panics.
func callSetConfigOption(ctx context.Context, worker *pool.Worker, sessionID, configID, value string, timeout time.Duration) ([]acp.SessionConfigOption, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var result struct {
		ConfigOptions []acp.SessionConfigOption `json:"configOptions"`
	}
	if err := worker.Transport.Call(callCtx, "session/set_config_option", acp.SetConfigOptionParams{
		SessionID: sessionID,
		ConfigID:  configID,
		Value:     value,
	}, &result); err != nil {
		return nil, err
	}
	return result.ConfigOptions, nil
}

// extractOpenAIToolNames collects valid function tool names from an OpenAI request payload.
//
// @purpose Supplies the delegation directive with the canonical tool list.
// @rationale Skips non-function entries and blank names so the directive cannot name an unusable tool.
// @preconditions None.
// @param tools Caller-declared OpenAI tools.
// @returns Canonical names in declaration order, or nil.
// @throws None.
// @invariants O(n).
func extractOpenAIToolNames(tools []mcp.OpenAIToolDefinition) []string {
	if len(tools) == 0 {
		return nil
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if t.Type == "function" && strings.TrimSpace(t.Function.Name) != "" {
			names = append(names, t.Function.Name)
		}
	}
	return names
}

// convertAnthropicTools translates Anthropic tool schemas into MCP tool definitions and extracts tool names.
//
// @purpose Adapts Anthropic `tools[]` into the MCP shape the loopback server serves.
// @rationale Anthropic names the schema `input_schema`; MCP expects `inputSchema`.
// @preconditions None.
// @param tools Caller-declared Anthropic tools.
// @returns MCP definitions and their canonical names.
// @throws None.
// @invariants O(n).
func convertAnthropicTools(tools []anthropic.Tool) ([]mcp.AnthropicToolDefinition, []string) {
	if len(tools) == 0 {
		return nil, nil
	}
	defs := make([]mcp.AnthropicToolDefinition, 0, len(tools))
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, mcp.AnthropicToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
		if strings.TrimSpace(t.Name) != "" {
			names = append(names, t.Name)
		}
	}
	return defs, names
}
