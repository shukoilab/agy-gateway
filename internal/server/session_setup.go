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

// buildACPSessionNewParams builds the ACP session/new payload for one turn.
//
// @purpose Supplies the workspace, the loopback MCP server and the single append-only system-prompt
// carrier.
// @rationale Probing proved agy 1.5.16+ accepts the standard `systemPrompt` parameter and discards it
// without error, honouring only `_meta.antigravity.systemPrompt.append`; and that `override` hangs the
// turn. The gateway therefore has exactly one carrier. When the consumer supplies no prompt the `_meta`
// object is omitted entirely rather than sent empty, so the gateway injects nothing of its own.
// @preconditions in.Worker must be non-nil.
// @param in SessionSetupInput encapsulating worker, config, system prompt, and client tool names.
// @returns Populated acp.SessionNewParams; Meta is nil when there is no prompt to deliver.
// @invariants The payload never depends on advertised capabilities: an agent that cannot honour
// `append` is reported once per worker by the startup warning, not silently downgraded per request.
// Pure construction; thread-safe, zero shared state mutation.
func buildACPSessionNewParams(in SessionSetupInput) acp.SessionNewParams {
	newParams := acp.SessionNewParams{
		CWD: in.Worker.Cwd,
	}

	port := 8080
	strictDelegation := true
	if in.Config != nil {
		port = in.Config.Server.Port
		strictDelegation = in.Config.Execution.StrictClientToolDelegation
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

	if effective := composeSystemPrompt(in.ConsumerSystemPrompt, in.ClientToolNames, strictDelegation); effective != "" {
		newParams.Meta = map[string]any{
			"antigravity": map[string]any{
				"systemPrompt": map[string]any{
					"append": effective,
				},
			},
		}
	}

	return newParams
}

// composeSystemPrompt builds the effective prompt from the consumer prompt and the client-tool
// delegation directive.
//
// @purpose Single source of truth for prompt precedence across both protocol adapters.
// @rationale The gateway is a provider endpoint: it must not invent a persona or a preamble. An
// earlier version injected a hardcoded default protocol when the consumer prompt was empty and a
// literal "AUTHORITATIVE SYSTEM INSTRUCTIONS" banner otherwise, which altered consumer intent on
// every request. Only the delegation directive remains, because it targets tools rather than voice
// and is emitted solely when the consumer declared tools.
// @preconditions None.
// @param consumerSystemPrompt Caller-supplied system prompt; may be empty.
// @param clientToolNames Declared client tool names used by the delegation directive.
// @param strictDelegation Whether the delegation directive should be appended.
// @returns The effective system prompt; empty when the consumer supplied none and declared no tools.
// @throws None.
// @invariants Pure; O(n) in prompt length.
func composeSystemPrompt(consumerSystemPrompt string, clientToolNames []string, strictDelegation bool) string {
	effective := strings.TrimSpace(consumerSystemPrompt)
	if strictDelegation && len(clientToolNames) > 0 {
		if directive := acp.ClientToolDelegationDirective(clientToolNames); directive != "" {
			if effective == "" {
				return directive
			}
			effective = effective + "\n\n" + directive
		}
	}
	return effective
}

// applySessionConfigOptions aligns the requested model and thought level with the options the agent
// actually advertised, and pushes only values it can accept.
//
// @purpose Removes every silent `set_config_option` failure: a value the agent rejects is a value the
// gateway must not send.
// @rationale Probing showed agy rejects `extra` (the display label) and `off` with -32602 while
// accepting only low|medium|high|xhigh; and rejects models absent from its catalogue. Discarding those
// errors previously let turns run on an unintended model. Session mode is deliberately absent: it is
// the consumer's system prompt that decides plan vs build, not a gateway knob (probe E3/E5).
// @preconditions newResult came from a successful session/new on worker.
// @param ctx Parent context governing the turn.
// @param worker Leased worker whose transport performs the calls.
// @param newResult Negotiated session, carrying live configOptions.
// @param requestedModel Upstream model id requested by the caller.
// @param thoughtLevel Desired thought level already mapped to a wire value.
// @param switchTimeout Per-option timeout budget; zero falls back to the handshake default.
// @param logger Destination for non-fatal fallback warnings; nil falls back to slog.Default.
// @returns The model actually applied (the requested one when no model option is advertised).
// @throws An error when session/new returned an empty session id, when the requested model is absent
// from the agent catalogue or rejected by it (the caller maps this to 400), or when set_config_option
// failed for a model the agent does advertise (an upstream fault).
// @invariants Sequential calls; each bounded by switchTimeout so a wedged agent cannot stall the turn.
func applySessionConfigOptions(
	ctx context.Context,
	worker *pool.Worker,
	newResult acp.SessionNewResult,
	requestedModel, thoughtLevel string,
	switchTimeout time.Duration,
	logger *slog.Logger,
) (appliedModel string, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	appliedModel = requestedModel

	if newResult.SessionID == "" {
		return appliedModel, fmt.Errorf("session/new returned an empty session id")
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
				return "", fmt.Errorf("model %q is not available on the upstream agent: %w", requestedModel, callErr)
			}
			return "", fmt.Errorf("failed to apply model %q: %w", requestedModel, callErr)
		}
		if len(refreshed) > 0 {
			options = refreshed
		}
	} else if requestedModel != "" {
		refreshed, callErr := callSetConfigOption(ctx, worker, newResult.SessionID, acp.ConfigIDModel, requestedModel, switchTimeout)
		if callErr != nil {
			return "", fmt.Errorf("failed to apply model %q: %w", requestedModel, callErr)
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

	return appliedModel, nil
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
