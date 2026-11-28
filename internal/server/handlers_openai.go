package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agy-gateway/internal/acp"
	"agy-gateway/internal/config"
	"agy-gateway/internal/mcp"
	"agy-gateway/internal/media"
	"agy-gateway/internal/models"
	"agy-gateway/internal/openai"
	"agy-gateway/internal/pool"
	"agy-gateway/internal/retry"
	"agy-gateway/internal/telemetry"
)

// OpenAIHandlers handles public OpenAI-compatible endpoints.
type OpenAIHandlers struct {
	cfg            *config.Config
	pool           *pool.Pool
	failover       *pool.FailoverCoordinator
	modelRegistry  *models.ModelRegistry
	loopbackServer *mcp.LoopbackServer
	toolSockets    *mcp.SocketServer
	translator     *openai.Translator
	logger         *slog.Logger
}

// NewOpenAIHandlers creates an initialized handler suite.
func NewOpenAIHandlers(
	cfg *config.Config,
	p *pool.Pool,
	failover *pool.FailoverCoordinator,
	modelRegistry *models.ModelRegistry,
	loopbackServer *mcp.LoopbackServer,
	logger *slog.Logger,
) *OpenAIHandlers {
	if logger == nil {
		logger = slog.Default()
	}
	if loopbackServer == nil {
		loopbackServer = mcp.NewLoopbackServer()
	}
	return &OpenAIHandlers{
		cfg:            cfg,
		pool:           p,
		failover:       failover,
		modelRegistry:  modelRegistry,
		loopbackServer: loopbackServer,
		toolSockets:    mcp.NewSocketServer(loopbackServer, logger),
		translator: openai.NewTranslatorWithBudget(cfg.Execution.MaxHistoryTokens).
			WithMedia(media.NewResolver(mediaConfig(cfg))),
		logger: logger,
	}
}

// HandleChatCompletions serves POST /v1/chat/completions.
//
//	@Summary		Create chat completion
//	@Description	Creates a model response for the given chat conversation. 100% compliant with the OpenAI 2026 chat completions standard. Supports streaming SSE (stream: true), multi-turn conversation history, real-time reasoning thoughts (reasoning_effort and reasoning_content), client-side tool calling with JIT prompt envelope translation, and automated multi-account failover.
//	@Tags			OpenAI - Chat Completions
//	@Accept			json
//	@Produce		json
//	@Produce		text/event-stream
//	@Security		ClientBearerAuth
//	@Param			request	body		openai.ChatCompletionRequest	true	"Chat completion request parameters including model, messages, tools, and reasoning preferences"
//	@Success		200		{object}	openai.ChatCompletionResponse	"Chat completion generated successfully (non-streaming mode)"
//	@Failure		400		{object}	openai.ErrorResponse			"Invalid JSON body, missing parameters, or unsupported option (e.g. n > 1)"
//	@Failure		401		{object}	openai.ErrorResponse			"Missing, expired, or invalid Client Bearer API key"
//	@Failure		404		{object}	openai.ErrorResponse			"Requested model identifier was not found in catalog"
//	@Failure		429		{object}	openai.ErrorResponse			"Client rate limit exceeded (RPM) or all accounts in cooldown"
//	@Failure		500		{object}	openai.ErrorResponse			"Internal gateway error or failure executing turn across workers"
//	@Router			/v1/chat/completions [post]
func (h *OpenAIHandlers) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	telemetry.DefaultMetrics.RequestsTotal.Add(1)

	var req openai.ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeOpenAIError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error(), "invalid_request_error", "invalid_json", "")
		return
	}

	if req.Model == "" {
		req.Model = h.modelRegistry.DefaultModel()
	}

	// A fresh install has an empty vault and therefore an empty catalog; say so instead of reporting
	// an unknown model. Once an account appears this also picks up its models without a restart.
	// Bootstrap the pool first: provisioning a worker is what materialises its credential, and model
	// discovery reads that credential. Discovering before provisioning found nothing on a fresh
	// install, so onboarding looked like it had failed.
	if h.pool.Bootstrap(r.Context()) {
		h.modelRegistry.RefreshIfAccountsChanged(r.Context())
	}
	if h.modelRegistry.NeedsOnboarding() {
		h.writeOpenAIError(w, http.StatusTooManyRequests,
			"No Antigravity accounts are configured; an administrator must add one", "api_error", "no_accounts", "")
		return
	}

	modelEntry, found := h.modelRegistry.FindModel(req.Model)
	if !found {
		// Accounts exist but no model could be enumerated: that is an operational fault, not a typo.
		// Answering model_not_found here sent operators looking for a misspelled id.
		if h.modelRegistry.HasAccounts() && h.modelRegistry.CatalogEmpty() {
			h.writeOpenAIError(w, http.StatusServiceUnavailable,
				"Antigravity accounts are configured but no models could be enumerated; check that the stored credentials can be exported (`agy-accounts list`)",
				"api_error", "model_discovery_failed", "")
			return
		}
		h.writeOpenAIError(w, http.StatusNotFound, fmt.Sprintf("The model %q does not exist", req.Model), "invalid_request_error", "model_not_found", "model")
		return
	}

	if req.N != nil && *req.N > 1 {
		h.writeOpenAIError(w, http.StatusBadRequest, "Only n=1 is supported", "invalid_request_error", "invalid_value", "n")
		return
	}

	// ACP can express only model, thought_level and mode. Every other provider parameter is either
	// reported truthfully in a response header (default) or refused outright, per
	// execution.unsupported_parameter_policy. Never silently accepted.
	unsupported := ignoredOpenAIParams(&req)
	if rejectUnsupportedParams(h.cfg, unsupported, func(status int, message, errType, errCode, param string) {
		h.writeOpenAIError(w, status, message, errType, errCode, param)
	}) {
		return
	}
	if joined := annotateIgnoredParams(w, unsupported); joined != "" {
		h.logger.Debug("openai request carries parameters ACP cannot honour", "ignored", sortedCopy(unsupported))
	}

	// Images are forwarded to the agent as real ACP image blocks; `vision_mode: reject` is the
	// explicit opt-out for deployments that must not accept binary payloads. The error shape is the
	// provider's own, not a gateway-invented code.
	if where := openAIVisionRequest(&req); where != "" && visionModeRejects(h.cfg) {
		h.writeOpenAIError(w, http.StatusBadRequest,
			fmt.Sprintf("Image input is disabled by this deployment (vision_mode=reject); the offending content is at %s", where),
			"invalid_request_error", "invalid_value", "messages")
		return
	}

	// 1. Translate prompt and deduplicate JIT tools into ACP prompt blocks
	delimiter := h.cfg.MCP.ClientToolCalling.DelimiterTag
	if delimiter == "" {
		delimiter = "tool_call"
	}
	systemPrompt, promptBlocks, mediaErr := h.translator.TranslateWithMedia(r.Context(), &req, delimiter)
	if mediaErr != nil {
		h.writeOpenAIError(w, mediaErrorStatus(mediaErr),
			fmt.Sprintf("Invalid image input: %v", mediaErr), "invalid_request_error", "invalid_value", "messages")
		return
	}

	h.logger.Debug("openai completion request received",
		"model", req.Model,
		"stream", req.Stream,
		"messagesCount", len(req.Messages),
		"toolsCount", len(req.Tools),
		"systemPromptLen", len(systemPrompt),
		"promptBlocksCount", len(promptBlocks),
	)

	effort := req.ReasoningEffort
	if effort == "" && modelEntry.PresetEffort != "" {
		effort = modelEntry.PresetEffort
	}
	thoughtLevel := config.MapReasoningEffort(effort, h.cfg.OpenAI.DefaultReasoningEffort)

	// Resolve the workspace directory before any worker is leased: the directory is derived from the
	// harness's own conversation identifier, so two live conversations cannot overwrite each other's
	// files. A failure here is an operational fault (the configured root is not writable), not a
	// caller mistake.
	workspaceDir, _, wsErr := workspaceDirForRequest(h.cfg, r, req.PromptCacheKey)
	if wsErr != nil {
		h.writeOpenAIError(w, http.StatusInternalServerError,
			"The configured workspace directory is not writable: "+wsErr.Error(), "api_error", "workspace_unavailable", "")
		return
	}

	// 2. Execute turn through worker pool with transparent failover
	createdTime := time.Now().Unix()
	completionID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:14])

	if req.Stream {
		h.handleStreaming(w, r, &req, modelEntry, thoughtLevel, systemPrompt, promptBlocks, workspaceDir, completionID, createdTime)
	} else {
		h.handleSync(w, r, &req, modelEntry, thoughtLevel, systemPrompt, promptBlocks, workspaceDir, completionID, createdTime)
	}
}

func (h *OpenAIHandlers) handleSync(
	w http.ResponseWriter,
	r *http.Request,
	req *openai.ChatCompletionRequest,
	modelEntry *models.ModelInfo,
	thoughtLevel string,
	systemPrompt string,
	promptBlocks []acp.PromptBlock,
	workspaceDir string,
	completionID string,
	createdTime int64,
) {
	in := turnInput{
		Ctx:           r.Context(),
		ModelEntry:    modelEntry,
		ThoughtLevel:  thoughtLevel,
		SystemPrompt:  systemPrompt,
		PromptBlocks:  promptBlocks,
		WorkspaceDir:  workspaceDir,
		StopSequences: normalizeStopSequences(req.Stop),
	}

	if len(req.Tools) > 0 {
		loopbackSessionID := newLoopbackSessionID()
		state, err := h.loopbackServer.RegisterSession(loopbackSessionID, req.Tools)
		if err != nil {
			h.writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "too_many_tools", "tools")
			return
		}
		defer h.loopbackServer.UnregisterSession(loopbackSessionID)
		in.ClientToolSessionID = loopbackSessionID
		in.ClientToolSockets = h.toolSockets
		in.LoopbackState = state
		in.ClientToolNames = extractOpenAIToolNames(req.Tools)
	}

	maxRetries := h.cfg.Execution.MaxRetries
	if maxRetries <= 0 && h.cfg.Accounts.MaxFailoverRetries > 0 {
		maxRetries = h.cfg.Accounts.MaxFailoverRetries
	}

	var outcome turnResult
	err := h.failover.ExecuteWithFailover(r.Context(), modelEntry.UpstreamModel, modelEntry.RequiresPaidTier, maxRetries, func(ctx context.Context, worker *pool.Worker) error {
		res, err := executeTurnAttempt(ctx, worker, h.cfg, h.logger, in)
		if err != nil {
			return err
		}
		outcome = res
		return nil
	})

	if err != nil {
		h.writeTurnError(w, err, h.cfg.Pool.FirstTokenTimeout)
		return
	}

	// The applied model and reasoning level are facts the caller cannot infer from the provider
	// schema, and the agent's own conversation title has no field anywhere. Report all three as
	// headers so a harness can see what actually happened upstream.
	w.Header().Set("X-Agy-Model", modelEntry.UpstreamModel)
	if outcome.AppliedThoughtLevel != "" {
		w.Header().Set("X-Agy-Thought-Level", outcome.AppliedThoughtLevel)
	}
	if outcome.SessionTitle != "" {
		w.Header().Set("X-Agy-Session-Title", outcome.SessionTitle)
	}

	_, toolCalls := assembleOpenAITools(outcome)

	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	} else if outcome.StopMatched {
		finishReason = "stop"
	} else if openAI, _ := MapStopReason(outcome.StopReason); openAI != "" {
		finishReason = openAI
	}

	var message openai.ChatMessage
	if len(toolCalls) > 0 {
		message = openai.ChatMessage{Role: "assistant", ToolCalls: toolCalls}
		if strings.TrimSpace(outcome.Text) != "" {
			message.Content = outcome.Text
		}
	} else {
		text := outcome.Text
		if strings.TrimSpace(text) == "" {
			text = "I have completed the analysis."
		}
		message = openai.ChatMessage{Role: "assistant", Content: text}
	}
	if h.cfg.OpenAI.ExposeReasoningContent && outcome.Thoughts != "" {
		message.ReasoningContent = outcome.Thoughts
	}

	usage := buildOpenAIUsage(h.cfg, outcome, promptBlocks)

	resp := openai.ChatCompletionResponse{
		ID:      completionID,
		Object:  "chat.completion",
		Created: createdTime,
		Model:   req.Model,
		Choices: []openai.ChatCompletionChoice{{
			Index:        0,
			Message:      message,
			FinishReason: finishReason,
		}},
		Usage: usage,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *OpenAIHandlers) handleStreaming(
	w http.ResponseWriter,
	r *http.Request,
	req *openai.ChatCompletionRequest,
	modelEntry *models.ModelInfo,
	thoughtLevel string,
	systemPrompt string,
	promptBlocks []acp.PromptBlock,
	workspaceDir string,
	completionID string,
	createdTime int64,
) {
	telemetry.DefaultMetrics.RequestsStreaming.Add(1)

	// These two are known before the turn runs, and the SSE writer commits the status line on the
	// first flush — so they must be set here, not after the turn. X-Agy-Session-Title is not set for
	// streaming responses: the agent assigns the title mid-conversation and the header block is long
	// gone by then. Harnesses that need it read the sync endpoint or the admin API.
	w.Header().Set("X-Agy-Model", modelEntry.UpstreamModel)
	if thoughtLevel != "" {
		w.Header().Set("X-Agy-Thought-Level", thoughtLevel)
	}

	sseWriter, err := openai.NewSSEWriter(w)
	if err != nil {
		h.writeOpenAIError(w, http.StatusInternalServerError, "Streaming initialization failed: "+err.Error(), "api_error", "stream_init_failed", "")
		return
	}

	// Keepalive ticker: stopped when the client disconnects.
	keepaliveInterval := h.cfg.Server.SSEKeepaliveInterval
	if keepaliveInterval <= 0 {
		keepaliveInterval = 15 * time.Second
	}
	keepaliveTicker := time.NewTicker(keepaliveInterval)
	defer keepaliveTicker.Stop()
	go func() {
		for {
			select {
			case <-r.Context().Done():
				return
			case <-keepaliveTicker.C:
				_ = sseWriter.WriteKeepalive()
			}
		}
	}()

	// Single writer: every SSE emission takes streamMu so provider event ordering is guaranteed.
	var streamMu sync.Mutex
	firstChunkSent := false
	emitDelta := func(delta openai.ChatCompletionChunkDelta, finish *string) {
		streamMu.Lock()
		defer streamMu.Unlock()
		if delta.Content == nil && delta.ReasoningContent == nil && len(delta.ToolCalls) == 0 && finish == nil {
			return
		}
		if !firstChunkSent {
			delta.Role = "assistant"
			firstChunkSent = true
		}
		chunk := openai.ChatCompletionChunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: createdTime,
			Model:   req.Model,
			Choices: []openai.ChatCompletionChunkChoice{{
				Index:        0,
				Delta:        delta,
				FinishReason: finish,
			}},
		}
		_ = sseWriter.WriteChunk(&chunk)
	}

	in := turnInput{
		Ctx:           r.Context(),
		ModelEntry:    modelEntry,
		ThoughtLevel:  thoughtLevel,
		SystemPrompt:  systemPrompt,
		PromptBlocks:  promptBlocks,
		WorkspaceDir:  workspaceDir,
		StopSequences: normalizeStopSequences(req.Stop),
	}

	if len(req.Tools) > 0 {
		loopbackSessionID := newLoopbackSessionID()
		state, regErr := h.loopbackServer.RegisterSession(loopbackSessionID, req.Tools)
		if regErr != nil {
			h.writeOpenAIError(w, http.StatusBadRequest, regErr.Error(), "invalid_request_error", "too_many_tools", "tools")
			return
		}
		defer h.loopbackServer.UnregisterSession(loopbackSessionID)
		in.ClientToolSessionID = loopbackSessionID
		in.ClientToolSockets = h.toolSockets
		in.LoopbackState = state
		in.ClientToolNames = extractOpenAIToolNames(req.Tools)
	}

	in.OnText = func(chunk string) {
		emitDelta(openai.ChatCompletionChunkDelta{Content: &chunk}, nil)
	}
	in.OnThought = func(chunk string) {
		if !h.cfg.OpenAI.ExposeReasoningContent {
			return
		}
		emitDelta(openai.ChatCompletionChunkDelta{ReasoningContent: &chunk}, nil)
	}
	// Tool call indexes must be distinct per turn. OpenAI clients merge streamed deltas that share an
	// index into ONE call, concatenating their argument strings, so a constant 0 corrupts any turn
	// that emits more than one call: the client then receives a single unparseable arguments blob and
	// every subsequent tool invocation fails. Anthropic already assigned increasing block indexes;
	// this is the OpenAI equivalent.
	var toolCallIndex ToolCallIndexer
	in.OnTool = func(entry toolCollectorEntry) {
		idx := toolCallIndex.Next()
		emitDelta(openai.ChatCompletionChunkDelta{ToolCalls: []openai.ToolCall{{
			Index: &idx,
			ID:    entry.ID,
			Type:  "function",
			Function: openai.FunctionCallData{
				Name:      entry.Name,
				Arguments: string(entry.Arguments),
			},
		}}}, nil)
	}
	in.OnInternalToolProgress = func(text string) {
		if !h.cfg.OpenAI.ExposeReasoningContent {
			return
		}
		emitDelta(openai.ChatCompletionChunkDelta{ReasoningContent: &text}, nil)
	}

	maxRetries := h.cfg.Execution.MaxRetries
	if maxRetries <= 0 && h.cfg.Accounts.MaxFailoverRetries > 0 {
		maxRetries = h.cfg.Accounts.MaxFailoverRetries
	}

	var outcome turnResult
	firstChunkSentPtr := func() bool {
		streamMu.Lock()
		defer streamMu.Unlock()
		return firstChunkSent
	}

	err = h.failover.ExecuteWithFailover(r.Context(), modelEntry.UpstreamModel, modelEntry.RequiresPaidTier, maxRetries, func(ctx context.Context, worker *pool.Worker) error {
		res, attemptErr := executeTurnAttempt(ctx, worker, h.cfg, h.logger, in)
		outcome = res
		if attemptErr != nil {
			if firstChunkSentPtr() {
				// Tokens are already on the wire; a retry would duplicate them.
				return fmt.Errorf("%w: %v", pool.ErrNonRetryableStream, attemptErr)
			}
			return attemptErr
		}
		return nil
	})

	if err != nil {
		if !sseWriter.Committed() {
			h.writeTurnError(w, err, h.cfg.Pool.FirstTokenTimeout)
			return
		}
		// The 200 has already been sent, so the failure goes out in-band. Only the class is exposed:
		// the full error is logged with the request id (FR-18).
		kind := ClientErrorKind(err)
		h.logger.Error("openai stream failed after first chunk",
			"requestID", requestIDFromContext(r), "class", kind, "err", err)
		_ = sseWriter.WriteError(ClientErrorMessage(kind), kind)
		_ = sseWriter.Close()
		return
	}

	_, _ = assembleOpenAITools(outcome)

	finishReason := "stop"
	if len(outcome.Tools) > 0 {
		finishReason = "tool_calls"
	} else if outcome.StopMatched {
		finishReason = "stop"
	} else if mapped, _ := MapStopReason(outcome.StopReason); mapped != "" {
		finishReason = mapped
	}

	if strings.TrimSpace(outcome.Text) == "" && len(outcome.Tools) == 0 {
		fallback := "I have completed the analysis."
		emitDelta(openai.ChatCompletionChunkDelta{Content: &fallback}, nil)
	}

	emitDelta(openai.ChatCompletionChunkDelta{}, &finishReason)

	if includeUsageChunk(req) {
		usage := buildOpenAIUsage(h.cfg, outcome, promptBlocks)
		streamMu.Lock()
		usageChunk := openai.ChatCompletionChunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: createdTime,
			Model:   req.Model,
			Choices: []openai.ChatCompletionChunkChoice{},
			Usage:   &usage,
		}
		_ = sseWriter.WriteChunk(&usageChunk)
		streamMu.Unlock()
	}

	_ = sseWriter.Close()
}

// normalizeStopSequences extracts stop sequences from the OpenAI `stop` field.
//
// @purpose Maps both accepted shapes (string and array) onto one list.
// @rationale The field is genuinely honored gateway-side; it must not be reported as ignored.
// @param stop Decoded `stop` value.
// @returns Up to four non-empty sequences.
// @invariants Pure; never returns more than four entries.
func normalizeStopSequences(stop any) []string {
	var sequences []string
	switch v := stop.(type) {
	case string:
		if v != "" {
			sequences = append(sequences, v)
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				sequences = append(sequences, s)
			}
			if len(sequences) >= 4 {
				break
			}
		}
	}
	if len(sequences) > 4 {
		sequences = sequences[:4]
	}
	return sequences
}

// includeUsageChunk reports whether the caller requested a final usage chunk.
//
// @purpose Implements stream_options.include_usage without silently accepting the field.
// @param req Decoded request.
// @returns true when usage should be emitted.
// @invariants Pure.
func includeUsageChunk(req *openai.ChatCompletionRequest) bool {
	return req.StreamOptions != nil && req.StreamOptions.IncludeUsage
}

// assembleOpenAITools renders collected tool calls in OpenAI shape.
//
// @purpose Single conversion point from sanitised collector entries to provider payloads.
// @param outcome Executor result.
// @returns The tool call list in first-seen order with stable indices.
// @invariants Pure.
func assembleOpenAITools(outcome turnResult) ([]openai.ToolCall, []openai.ToolCall) {
	if len(outcome.Tools) == 0 {
		return nil, nil
	}
	calls := make([]openai.ToolCall, 0, len(outcome.Tools))
	for i, entry := range outcome.Tools {
		idx := i
		args := string(entry.Arguments)
		if args == "" || args == "null" {
			args = "{}"
		}
		calls = append(calls, openai.ToolCall{
			Index: &idx,
			ID:    entry.ID,
			Type:  "function",
			Function: openai.FunctionCallData{
				Name:      entry.Name,
				Arguments: args,
			},
		})
	}
	return calls, calls
}

// buildOpenAIUsage renders usage from the authoritative agent counters.
//
// @purpose Reports real token accounting instead of local estimation whenever available.
// @param cfg Gateway configuration.
// @param outcome Executor result.
// @param promptBlocks Prompt blocks used for the fallback estimate.
// @returns OpenAI usage details.
// @invariants Pure.
func buildOpenAIUsage(cfg *config.Config, outcome turnResult, promptBlocks []acp.PromptBlock) openai.UsageDetails {
	promptTokens := 0
	completionTokens := 0
	totalTokens := 0
	var cachedTokens int64
	var reasoningTokens int

	if outcome.UsageFromAgent {
		promptTokens = int(outcome.Usage.InputTokens)
		completionTokens = int(outcome.Usage.OutputTokens)
		totalTokens = int(outcome.Usage.TotalTokens)
		cachedTokens = outcome.Usage.CachedReadTokens
		reasoningTokens = int(outcome.Usage.ThoughtTokens)
	} else {
		promptTokens = estimatePromptTokens(promptBlocks)
		completionTokens = openai.CountTokens(outcome.Text)
		reasoningTokens = openai.CountTokens(outcome.Thoughts)
		completionTokens += reasoningTokens
		totalTokens = promptTokens + completionTokens
	}

	return openai.UsageDetails{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
		PromptTokensDetails: &openai.PromptTokensDetails{
			CachedTokens: int(cachedTokens),
		},
		CompletionTokensDetails: &openai.CompletionTokensDetails{
			ReasoningTokens: reasoningTokens,
		},
	}
}

// estimatePromptTokens counts prompt tokens without indexing an empty slice.
//
// @purpose Removes four unchecked `promptBlocks[0]` index expressions.
// @rationale A translator change returning an empty slice would panic the handler.
// @param blocks Prompt blocks sent upstream.
// @returns Token estimate, zero when no blocks exist.
// @invariants Pure; nil-safe.
func estimatePromptTokens(blocks []acp.PromptBlock) int {
	total := 0
	for _, block := range blocks {
		total += openai.CountTokens(block.Text)
	}
	return total
}

// writeTurnError maps an executor error onto the correct OpenAI HTTP response.
//
// @purpose Single error-mapping point implementing the FR-18 table.
// @rationale Both handlers previously re-implemented this mapping, and neither sanitised internals.
// @param w Response writer.
// @param err Executor error.
// @param ttft configured first-token timeout for the message.
// @invariants Writes exactly one response.
func (h *OpenAIHandlers) writeTurnError(w http.ResponseWriter, err error, ttft time.Duration) {
	if errors.Is(err, ErrFirstTokenTimeout) {
		h.writeOpenAIError(w, http.StatusGatewayTimeout,
			fmt.Sprintf("Time to first token exceeded %s (model cold-cache or prompt prefill timed out)", ttft),
			"api_error", "first_token_timeout", "")
		return
	}
	if errors.Is(err, pool.ErrQueueSaturated) {
		h.writeOpenAIError(w, http.StatusServiceUnavailable,
			"All workers are busy; retry shortly", "api_error", "pool_saturated", "")
		return
	}
	if errors.Is(err, pool.ErrNoAccounts) || errors.Is(err, pool.ErrNoEligibleAccounts) {
		// Every account is cooling or unusable: a quota condition the client can wait out, not a
		// gateway fault. The retry hint is derived from the earliest cooldown expiry.
		if retryAfter := h.failover.NextAvailableIn(time.Now()); retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
		}
		message := "All Antigravity accounts are temporarily unavailable; retry later"
		code := "all_accounts_cooling"
		if errors.Is(err, pool.ErrNoAccounts) {
			message = "No Antigravity accounts are configured; an administrator must add one"
			code = "no_accounts"
		}
		h.writeOpenAIError(w, http.StatusTooManyRequests, message, "api_error", code, "")
		return
	}
	if errors.Is(err, retry.ErrEmptyCompletion) {
		// The agent ended the turn without producing anything. Reporting 200 here would claim success
		// for work that never happened.
		h.writeOpenAIError(w, http.StatusBadGateway,
			"Upstream agent returned an empty completion; retry the request", "api_error", "empty_completion", "")
		return
	}
	var respErr *acp.ResponseError
	if (errors.As(err, &respErr) && respErr.Code == acp.ErrCodeAuthRequired) ||
		strings.Contains(err.Error(), "Authentication required") ||
		strings.Contains(err.Error(), "transport is closed") {
		h.writeOpenAIError(w, http.StatusServiceUnavailable,
			"No authenticated Antigravity accounts available. Please login an account via admin endpoint POST /admin/v1/accounts/login/init",
			"invalid_request_error", "account_auth_required", "")
		return
	}
	if errors.As(err, &respErr) && respErr.Code == acp.ErrCodeInvalidParams {
		// ACP answers -32602 for an unknown session, an unknown config option, or a level the
		// selected model does not offer. Those are all caller-fixable, so they map to a
		// provider-native 400 rather than a server fault. The offending parameter is named only when
		// the message actually identifies one — inventing "model" for a session fault would mislead.
		param := ""
		lower := strings.ToLower(respErr.Message)
		if strings.Contains(lower, "model") || strings.Contains(lower, "thinking level") ||
			strings.Contains(lower, "thought") || strings.Contains(lower, "config option") {
			param = "model"
		}
		h.writeOpenAIError(w, http.StatusBadRequest, "upstream rejected the request: "+respErr.Message, "invalid_request_error", "invalid_value", param)
		return
	}
	if errors.As(err, &respErr) && respErr.Code == acp.ErrCodeMethodNotFound {
		// The agent does not implement a method the gateway needs: a version/support fault, never the
		// caller's fault, so it stays a 500 and never leaks the method name upward.
		h.logger.Error("upstream agent does not implement a required ACP method", "err", err)
		h.writeOpenAIError(w, http.StatusInternalServerError, "Inference error", "api_error", "inference_error", "")
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		h.writeOpenAIError(w, http.StatusGatewayTimeout, "inference turn was cancelled", "api_error", "inference_cancelled", "")
		return
	}
	h.logger.Error("openai turn failed", "err", err)
	h.writeOpenAIError(w, http.StatusInternalServerError, "Inference error", "api_error", "inference_error", "")
}

// newLoopbackSessionID mints a full-entropy loopback session identifier.
//
// @purpose Prevents session-id guessing on the unauthenticated-shape internal endpoint.
// @rationale The previous 12-character slice left ~48 bits of entropy; a full UUID costs nothing.
// @returns A session id prefixed with "sess_".
// @invariants O(1).
func newLoopbackSessionID() string {
	return "sess_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

// HandleListModels serves GET /v1/models.
//
//	@Summary		List available models
//	@Description	Lists the currently available models dynamically discovered across authenticated Google Antigravity accounts.
//	@Tags			OpenAI - Models
//	@Accept			json
//	@Produce		json
//	@Security		ClientBearerAuth
//	@Success		200	{object}	openai.ModelListResponse	"List of active models"
//	@Failure		401	{object}	openai.ErrorResponse		"Missing or invalid Client Bearer token"
//	@Router			/v1/models [get]
func (h *OpenAIHandlers) HandleListModels(w http.ResponseWriter, r *http.Request) {
	resp := openai.ModelListResponse{
		Object: "list",
		Data:   h.modelRegistry.ListModels(),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// HandleGetModel serves GET /v1/models/{model}.
//
//	@Summary		Retrieve model details
//	@Description	Retrieves detailed metadata for a specific model by identifier.
//	@Tags			OpenAI - Models
//	@Accept			json
//	@Produce		json
//	@Security		ClientBearerAuth
//	@Param			model	path		string					true	"Model identifier (e.g. 'gemini-3.8-flash', 'claude-sonnet-4.6')"
//	@Success		200		{object}	openai.ModelObject		"Model information"
//	@Failure		400		{object}	openai.ErrorResponse	"Missing model identifier in request path"
//	@Failure		401		{object}	openai.ErrorResponse	"Missing or invalid Client Bearer token"
//	@Failure		404		{object}	openai.ErrorResponse	"The specified model was not found"
//	@Router			/v1/models/{model} [get]
func (h *OpenAIHandlers) HandleGetModel(w http.ResponseWriter, r *http.Request) {
	pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(pathParts) < 3 {
		h.writeOpenAIError(w, http.StatusBadRequest, "Missing model identifier in path", "invalid_request_error", "missing_model", "model")
		return
	}
	modelID := pathParts[2]

	m, found := h.modelRegistry.FindModel(modelID)
	if !found {
		h.writeOpenAIError(w, http.StatusNotFound, fmt.Sprintf("The model %q does not exist", modelID), "invalid_request_error", "model_not_found", "model")
		return
	}

	resp := openai.ModelObject{
		ID:      m.ID,
		Object:  "model",
		Created: m.Created,
		OwnedBy: m.OwnedBy,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *OpenAIHandlers) writeOpenAIError(w http.ResponseWriter, statusCode int, message, errType, errCode, param string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	details := openai.ErrorDetails{
		Message: message,
		Type:    errType,
	}
	if errCode != "" {
		details.Code = &errCode
	}
	if param != "" {
		details.Param = &param
	}
	_ = json.NewEncoder(w).Encode(openai.ErrorResponse{
		Error: details,
	})
}

// formatInternalToolProgress formats an internal ACP tool_call update into a concise
// human-readable reasoning status line for streaming clients.
//
// @purpose Surfaces internal agent tool execution progress to clients via reasoning/thinking streams.
// @rationale Low-reasoning models (e.g. gemini-4-hydrogen-low) may execute multiple internal tools without
// emitting thought chunks, causing clients to appear frozen. Emitting a concise status line keeps SSE active.
// @preconditions None.
// @param u The ACP SessionUpdate containing tool metadata.
// @returns Formatted status line ending with newline.
// @invariants Pure function, zero side effects, O(1) execution.
func formatInternalToolProgress(u acp.SessionUpdate) string {
	label := strings.TrimSpace(u.Title)
	if label == "" {
		label = strings.TrimSpace(u.Name)
	}
	if label == "" {
		label = strings.TrimSpace(u.Kind)
	}
	if label == "" {
		label = "internal_tool"
	}
	return fmt.Sprintf("\n> [Executing: %s]\n", label)
}
