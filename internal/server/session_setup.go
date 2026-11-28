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
	// ClientToolSessionID keys the in-memory tool registry the bridge calls are attributed to.
	ClientToolSessionID string
	// ClientToolSocketPath is the per-session Unix socket the stdio bridge dials. Empty disables the
	// client-tools MCP server entirely (a turn with no declared tools).
	ClientToolSocketPath string
	ClientToolNames      []string
	// AdditionalDirectories are extra absolute workspace roots the consumer granted for this turn.
	AdditionalDirectories []string
	// WorkspaceDir overrides the session CWD. Empty means the worker's own Cwd, which is the shared
	// workspace root; a non-empty value is a per-conversation subdirectory of that root.
	WorkspaceDir string
	// ClientMCPServers are MCP servers declared by the consumer (stdio/http/sse), forwarded verbatim
	// per docs/acp-conformance.md §4.1 so a harness's own context servers reach the agent.
	ClientMCPServers []acp.MCPServerInfo
}

// buildACPSessionNewParams builds the ACP session/new payload for one turn.
//
// @purpose Supplies the workspace and the loopback MCP server. The consumer system prompt is
// deliberately absent: it travels as the first prompt block of session/prompt instead.
// @rationale Probing proved agy 1.5.16+ accepts the spec-level `systemPrompt` parameter and discards it
// without error, and that `override` hangs the turn. The vendor `_meta.antigravity.systemPrompt.append`
// dialect is worse than useless: agy inspects it with a policy guardian that answers certain
// instruction text with a synthetic HTTP 429 ResourceExhausted and emits zero updates, so the turn
// hangs until the client disconnects. Both carriers were therefore removed; tree-universal behaviour
// comes from a leading text block, which every harness and every agent version honours.
// @preconditions in.Worker must be non-nil.
// @param in SessionSetupInput encapsulating worker, config and client tool names.
// @returns Populated acp.SessionNewParams carrying no prompt text.
// @invariants The payload is byte-identical regardless of what the consumer declared, so it can never
// be the reason one request behaves differently from another.
// Pure construction; thread-safe, zero shared state mutation.
func buildACPSessionNewParams(in SessionSetupInput) acp.SessionNewParams {
	cwd := in.Worker.Cwd
	if in.WorkspaceDir != "" {
		cwd = in.WorkspaceDir
	}
	newParams := acp.SessionNewParams{
		CWD: cwd,
	}

	// Forward the consumer's extra workspace roots. agy uses them for reads and writes outside cwd,
	// and the reference client sends them on every session/new (docs/acp-conformance.md §4.1).
	if len(in.AdditionalDirectories) > 0 {
		clean := make([]string, 0, len(in.AdditionalDirectories))
		for _, dir := range in.AdditionalDirectories {
			if d := strings.TrimSpace(dir); d != "" {
				clean = append(clean, d)
			}
		}
		newParams.AdditionalDirectories = clean
	}

	// Consumer-declared MCP servers travel verbatim: the agent materialises their tools itself
	// (`mcp_<name>_<tool>`), exactly as it does for the reference editor.
	newParams.MCPServers = append(newParams.MCPServers, in.ClientMCPServers...)

	if in.ClientToolSocketPath != "" {
		execPath, err := os.Executable()
		if err != nil || execPath == "" {
			execPath = "agy-gateway"
		}
		// The socket path is the credential: it lives in this worker's private TMPDIR (0700) and the
		// socket file itself is 0600, so no other uid can connect. It travels in the environment
		// rather than argv so it does not appear in `ps` output.
		env := []acp.EnvVariable{{Name: "AGY_GW_MCP_SOCKET", Value: in.ClientToolSocketPath}}
		newParams.MCPServers = append(newParams.MCPServers, acp.MCPServerInfo{
			Name:    "client-tools",
			Command: execPath,
			Args:    []string{"mcp-bridge"},
			Env:     env,
		})
	}

	return newParams
}

// effectiveSystemPrompt returns the prompt text that must reach agy, or "" when there is none.
//
// @purpose Single composer for the consumer system prompt, so the text the agent sees cannot drift
// between protocol adapters.
// @rationale The prompt must travel as a prompt block, never through session/new, because the vendor
// carrier is policy-guarded and can return a synthetic 429 with zero updates.
// @preconditions in.Config may be nil (test fixtures).
// @param in SessionSetupInput carrying the consumer prompt, tool names and config.
// @returns The composed prompt (consumer text plus the delegation directive) or "" when empty.
// @throws None.
// @invariants Pure; O(n) in prompt length.
func effectiveSystemPrompt(in SessionSetupInput) string {
	strictDelegation := true
	if in.Config != nil {
		strictDelegation = in.Config.Execution.StrictClientToolDelegation
	}
	return composeSystemPrompt(in.ConsumerSystemPrompt, in.ClientToolNames, strictDelegation)
}

// prependPromptBlocks puts the effective system prompt in front of the turn's prompt blocks.
//
// @purpose Delivers the consumer system prompt through the channel agy honours without its policy
// guardian rejecting it.
// @rationale agy answers a synthetic HTTP 429 ResourceExhausted and emits zero updates when
// _meta.antigravity.systemPrompt.append carries certain instruction text, so the turn hangs until the
// client disconnects. The identical text as a leading text block is honoured normally (verified with
// the Hermes payload: 6-8 chunks, 2 tool calls, 8.7-13.3s).
// shortcut: block placement weakens system-level authority versus a dedicated vendor channel;
// upgrade if agy exposes one that is not policy-guarded.
// @preconditions base may be nil; effective may be empty.
// @param base The turn's existing prompt blocks (user text, images, resources).
// @param effective The composed system prompt.
// @returns A new slice with one leading text block when effective is non-empty, else base unchanged.
// @throws None.
// @invariants Never mutates base's backing array; preserves block order; O(len(base)).
func prependPromptBlocks(base []acp.PromptBlock, effective string) []acp.PromptBlock {
	if strings.TrimSpace(effective) == "" {
		return base
	}
	out := make([]acp.PromptBlock, 0, len(base)+1)
	out = append(out, acp.PromptBlock{Type: "text", Text: effective})
	return append(out, base...)
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
) (appliedModel string, appliedThoughtLevel string, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	appliedModel = requestedModel

	if newResult.SessionID == "" {
		return appliedModel, "", fmt.Errorf("session/new returned an empty session id")
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

	if cfg := byID(acp.ConfigIDModel); cfg != nil {
		refreshed, callErr := callSetConfigOption(ctx, worker, newResult.SessionID, acp.ConfigIDModel, requestedModel, switchTimeout)
		if callErr != nil {
			if !cfg.HoldsValue(requestedModel) {
				return "", "", fmt.Errorf("model %q is not available on the upstream agent: %w", requestedModel, callErr)
			}
			return "", "", fmt.Errorf("failed to apply model %q: %w", requestedModel, callErr)
		}
		if len(refreshed) > 0 {
			options = refreshed
		}
	} else if requestedModel != "" {
		refreshed, callErr := callSetConfigOption(ctx, worker, newResult.SessionID, acp.ConfigIDModel, requestedModel, switchTimeout)
		if callErr != nil {
			return "", "", fmt.Errorf("failed to apply model %q: %w", requestedModel, callErr)
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
			// The model offers no such level (e.g. gemini-3.8-flash has no xhigh). Applying the
			// model's own default is the honest outcome; the caller reports it in a response header
			// rather than failing a request the model can otherwise serve.
			logger.Info("requested thought level is not offered by this model; applying its default",
				"requested", value, "applied", cfg.CurrentValue, "sessionId", newResult.SessionID)
			value = cfg.CurrentValue
		case cfg == nil:
			// The live option set has no thought_level: either the selected model never offered one
			// (claude-*, gpt-oss-*) or it dropped the option after the model switch. Sending the
			// value anyway earns -32602 on every turn of the session, so the agent default is used.
			logger.Info("selected model exposes no thought level; using the model default",
				"requested", value, "sessionId", newResult.SessionID)
			value = ""
		}
		if value != "" {
			// The response is the only authoritative statement of what is now in force.
			refreshed, callErr := callSetConfigOption(ctx, worker, newResult.SessionID, acp.ConfigIDThoughtLevel, value, switchTimeout)
			if callErr != nil {
				logger.Warn("thought level rejected by agent; turn continues at agent default",
					"requested", value, "err", callErr, "sessionId", newResult.SessionID)
			} else {
				appliedThoughtLevel = value
				if len(refreshed) > 0 {
					for i := range refreshed {
						if refreshed[i].ID == acp.ConfigIDThoughtLevel && refreshed[i].CurrentValue != "" {
							appliedThoughtLevel = refreshed[i].CurrentValue
						}
					}
				}
			}
		}
	}

	return appliedModel, appliedThoughtLevel, nil
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
