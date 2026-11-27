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
	"agy-gateway/internal/anthropic"
	"agy-gateway/internal/config"
	"agy-gateway/internal/mcp"
	"agy-gateway/internal/models"
	"agy-gateway/internal/pool"
	"agy-gateway/internal/retry"
	"agy-gateway/internal/telemetry"
)

// AnthropicHandlers handles public Anthropic Messages API compatible endpoints.
//
// @purpose Coordinates POST /v1/messages request lifecycle, translation, streaming, and failover.
// @rationale Provides full protocol parity with Anthropic Claude clients using the agy backend.
// @preconditions Dependencies must be non-nil.
type AnthropicHandlers struct {
	cfg            *config.Config
	pool           *pool.Pool
	failover       *pool.FailoverCoordinator
	modelRegistry  *models.ModelRegistry
	loopbackServer *mcp.LoopbackServer
	translator     *anthropic.Translator
	logger         *slog.Logger
}

// NewAnthropicHandlers creates an initialized handler suite.
func NewAnthropicHandlers(
	cfg *config.Config,
	p *pool.Pool,
	failover *pool.FailoverCoordinator,
	modelRegistry *models.ModelRegistry,
	loopbackServer *mcp.LoopbackServer,
	logger *slog.Logger,
) *AnthropicHandlers {
	if logger == nil {
		logger = slog.Default()
	}
	if loopbackServer == nil {
		loopbackServer = mcp.NewLoopbackServer()
	}
	return &AnthropicHandlers{
		cfg:            cfg,
		pool:           p,
		failover:       failover,
		modelRegistry:  modelRegistry,
		loopbackServer: loopbackServer,
		translator:     anthropic.NewTranslatorWithBudget(cfg.Execution.MaxHistoryTokens),
		logger:         logger.With("component", "anthropic-handlers"),
	}
}

// ModelRegistry returns the underlying model registry.
func (h *AnthropicHandlers) ModelRegistry() *models.ModelRegistry {
	return h.modelRegistry
}

// HandleMessages serves POST /v1/messages.
//
//	@Summary		Create message
//	@Description	Creates a structured message response for conversational turns. 100% compliant with Anthropic Messages API (2026 standard). Supports streaming SSE (stream: true), multi-turn conversation history, real-time reasoning (extended thinking), client-side tool calling with JIT prompt envelope translation, and automated multi-account failover.
//	@Tags			Anthropic - Messages
//	@Accept			json
//	@Produce		json
//	@Produce		text/event-stream
//	@Security		ClientBearerAuth
//	@Param			request	body		anthropic.MessageRequest	true	"Anthropic Messages request parameters including model, messages, tools, and thinking preferences"
//	@Success		200		{object}	anthropic.MessageResponse	"Message generated successfully (non-streaming mode)"
//	@Failure		400		{object}	anthropic.ErrorResponse		"Invalid JSON body, missing parameters, or validation violation"
//	@Failure		401		{object}	anthropic.ErrorResponse		"Missing, expired, or invalid API key"
//	@Failure		404		{object}	anthropic.ErrorResponse		"Requested model identifier was not found in catalog"
//	@Failure		429		{object}	anthropic.ErrorResponse		"Client rate limit exceeded (RPM) or all accounts in cooldown"
//	@Failure		500		{object}	anthropic.ErrorResponse		"Internal gateway error or failure executing turn across workers"
//	@Router			/v1/messages [post]
func (h *AnthropicHandlers) HandleMessages(w http.ResponseWriter, r *http.Request) {
	telemetry.DefaultMetrics.RequestsTotal.Add(1)

	var req anthropic.MessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON payload: "+err.Error())
		return
	}

	if req.Model == "" {
		req.Model = h.modelRegistry.AnthropicDefaultModel()
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
		h.writeAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error",
			"no Antigravity accounts are configured (code: no_accounts); an administrator must add one")
		return
	}

	modelEntry, found := h.modelRegistry.FindModel(req.Model)
	if !found {
		h.writeAnthropicError(w, http.StatusNotFound, "not_found_error", fmt.Sprintf("The model %q does not exist", req.Model))
		return
	}

	if req.MaxTokens <= 0 {
		req.MaxTokens = h.cfg.Anthropic.DefaultMaxTokens
		if req.MaxTokens <= 0 {
			req.MaxTokens = 4096
		}
	}

	if err := anthropic.ValidateMessageRequest(&req); err != nil {
		h.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// FR-07: agy advertises image/embedded context but the model rejects both with INVALID_ARGUMENT.
	if where := anthropicVisionRequest(&req); where != "" && visionModeRejects(h.cfg) {
		h.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("Image content is not supported by the upstream agent (%s). Send a textual description instead.", where))
		return
	}

	delimiter := h.cfg.MCP.ClientToolCalling.DelimiterTag
	if delimiter == "" {
		delimiter = "tool_call"
	}
	systemPrompt, promptBlocks := h.translator.Translate(&req, delimiter)

	h.logger.Debug("anthropic message request received",
		"model", req.Model,
		"stream", req.Stream,
		"messagesCount", len(req.Messages),
		"toolsCount", len(req.Tools),
		"systemPromptLen", len(systemPrompt),
		"promptBlocksCount", len(promptBlocks),
	)

	// Map Anthropic thinking config to ACP thought level
	effort := ""
	if req.Thinking != nil {
		switch req.Thinking.Type {
		case "adaptive":
			effort = "high"
		case "enabled":
			effort = config.MapThinkingBudget(req.Thinking.BudgetTokens)
		}
	}
	if effort == "" && modelEntry.PresetEffort != "" {
		effort = modelEntry.PresetEffort
	}
	thoughtLevel := config.MapReasoningEffort(effort, "medium")

	completionID := fmt.Sprintf("msg_%s", uuid.New().String()[:24])

	if req.Stream {
		h.handleStreaming(w, r, &req, modelEntry, thoughtLevel, systemPrompt, promptBlocks, completionID)
	} else {
		h.handleSync(w, r, &req, modelEntry, thoughtLevel, systemPrompt, promptBlocks, completionID)
	}
}

func (h *AnthropicHandlers) handleSync(
	w http.ResponseWriter,
	r *http.Request,
	req *anthropic.MessageRequest,
	modelEntry *models.ModelInfo,
	thoughtLevel string,
	systemPrompt string,
	promptBlocks []acp.PromptBlock,
	completionID string,
) {
	in := turnInput{
		Ctx:           r.Context(),
		ModelEntry:    modelEntry,
		ThoughtLevel:  thoughtLevel,
		SystemPrompt:  systemPrompt,
		PromptBlocks:  promptBlocks,
		StopSequences: req.StopSequences,
	}

	delimiter := h.cfg.MCP.ClientToolCalling.DelimiterTag
	if delimiter == "" {
		delimiter = "tool_call"
	}
	in.ParseTextToolCalls = func(text string) (string, []toolCollectorEntry) {
		clean, blocks, has := anthropic.ParseCompleteResponse(text, delimiter)
		if !has || len(blocks) == 0 {
			return text, nil
		}
		out := make([]toolCollectorEntry, 0, len(blocks))
		for _, b := range blocks {
			if b.Type != "tool_use" || b.Name == "" {
				continue
			}
			args := json.RawMessage(b.Input)
			if len(args) == 0 || string(args) == "null" {
				args = json.RawMessage("{}")
			}
			out = append(out, toolCollectorEntry{ID: b.ID, Name: b.Name, Arguments: args})
		}
		return clean, out
	}

	if len(req.Tools) > 0 {
		anthropicTools, toolNames := convertAnthropicTools(req.Tools)
		loopbackSessionID := newLoopbackSessionID()
		state, err := h.loopbackServer.RegisterAnthropicSession(loopbackSessionID, anthropicTools)
		if err != nil {
			h.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		defer h.loopbackServer.UnregisterSession(loopbackSessionID)
		in.LoopbackSessionID = loopbackSessionID
		in.LoopbackToken = state.Token
		in.LoopbackState = state
		in.ClientToolNames = toolNames
	}

	maxRetries := h.cfg.Execution.MaxRetries
	if maxRetries <= 0 && h.cfg.Accounts.MaxFailoverRetries > 0 {
		maxRetries = h.cfg.Accounts.MaxFailoverRetries
	}

	var outcome turnResult
	err := h.failover.ExecuteWithFailover(r.Context(), modelEntry.UpstreamModel, modelEntry.RequiresPaidTier, maxRetries, func(ctx context.Context, worker *pool.Worker) error {
		res, attemptErr := executeTurnAttempt(ctx, worker, h.cfg, h.logger, in)
		if attemptErr != nil {
			return attemptErr
		}
		outcome = res
		return nil
	})

	if err != nil {
		h.writeAnthropicTurnError(w, err)
		return
	}

	var contentBlocks []anthropic.ContentBlock
	if (h.cfg.Anthropic.ExposeReasoningContent || req.Thinking != nil) && outcome.Thoughts != "" {
		contentBlocks = append(contentBlocks, anthropic.ContentBlock{
			Type:      "thinking",
			Thinking:  outcome.Thoughts,
			Signature: anthropic.GenerateThinkingSignature(outcome.Thoughts),
		})
	}
	if outcome.Text != "" {
		contentBlocks = append(contentBlocks, anthropic.ContentBlock{Type: "text", Text: outcome.Text})
	}
	contentBlocks = append(contentBlocks, assembleAnthropicTools(outcome)...)

	hasRealContent := false
	for _, block := range contentBlocks {
		if block.Type == "text" || block.Type == "tool_use" {
			hasRealContent = true
			break
		}
	}
	if !hasRealContent {
		contentBlocks = append(contentBlocks, anthropic.ContentBlock{
			Type: "text",
			Text: "I have completed the analysis.",
		})
	}

	stopReason := "end_turn"
	var stopSequence *string
	if len(outcome.Tools) > 0 {
		stopReason = "tool_use"
	} else if outcome.StopMatched {
		stopReason = "stop_sequence"
		if seq := outcome.MatchedSequence; seq != "" {
			stopSequence = &seq
		}
	} else if _, anthropicStop := MapStopReason(outcome.StopReason); anthropicStop != "" {
		stopReason = anthropicStop
	}

	resp := anthropic.MessageResponse{
		ID:           completionID,
		Type:         "message",
		Role:         "assistant",
		Content:      contentBlocks,
		Model:        req.Model,
		StopReason:   &stopReason,
		StopSequence: stopSequence,
		Usage:        buildAnthropicUsage(outcome, promptBlocks),
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("anthropic-version", "2023-06-01")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *AnthropicHandlers) handleStreaming(
	w http.ResponseWriter,
	r *http.Request,
	req *anthropic.MessageRequest,
	modelEntry *models.ModelInfo,
	thoughtLevel string,
	systemPrompt string,
	promptBlocks []acp.PromptBlock,
	completionID string,
) {
	telemetry.DefaultMetrics.RequestsStreaming.Add(1)

	sseWriter, err := anthropic.NewSSEWriter(w)
	if err != nil {
		h.writeAnthropicError(w, http.StatusInternalServerError, "api_error", "Streaming initialization failed: "+err.Error())
		return
	}

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
				_ = sseWriter.WritePing()
			}
		}
	}()

	// One writer for the whole turn: Anthropic requires strictly ordered events, so every emission
	// takes streamMu and block indices are allocated inside it.
	var streamMu sync.Mutex
	blockIndex := 0
	textIndex := -1
	thinkingIndex := -1
	messageStarted := false
	// Accumulates the current thinking block's text so its signature derives from its content: a
	// constant signature (HMAC over "") is identical for every block in every turn, which client SDKs
	// use to detect replayed or forged reasoning.
	var thinkingBuf strings.Builder
	promptTokens := estimateAnthropicPromptTokens(promptBlocks)

	startMessage := func() {
		if messageStarted {
			return
		}
		messageStarted = true
		_ = sseWriter.WriteMessageStart(completionID, req.Model, promptTokens)
	}
	closeThinking := func() {
		if thinkingIndex < 0 {
			return
		}
		sig := anthropic.GenerateThinkingSignature(thinkingBuf.String())
		thinkingBuf.Reset()
		_ = sseWriter.WriteContentBlockDelta(thinkingIndex, anthropic.ContentBlockDelta{Type: "signature_delta", Signature: sig})
		_ = sseWriter.WriteContentBlockStop(thinkingIndex)
		thinkingIndex = -1
	}
	closeText := func() {
		if textIndex < 0 {
			return
		}
		_ = sseWriter.WriteContentBlockStop(textIndex)
		textIndex = -1
	}

	in := turnInput{
		Ctx:           r.Context(),
		ModelEntry:    modelEntry,
		ThoughtLevel:  thoughtLevel,
		SystemPrompt:  systemPrompt,
		PromptBlocks:  promptBlocks,
		StopSequences: req.StopSequences,
	}

	if len(req.Tools) > 0 {
		anthropicTools, toolNames := convertAnthropicTools(req.Tools)
		loopbackSessionID := newLoopbackSessionID()
		state, regErr := h.loopbackServer.RegisterAnthropicSession(loopbackSessionID, anthropicTools)
		if regErr != nil {
			h.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", regErr.Error())
			return
		}
		defer h.loopbackServer.UnregisterSession(loopbackSessionID)
		in.LoopbackSessionID = loopbackSessionID
		in.LoopbackToken = state.Token
		in.LoopbackState = state
		in.ClientToolNames = toolNames
	}

	// Streaming text-tool-call recovery: some models emit `<tool_call>…</tool_call>` inside the text
	// stream instead of native ACP tool_call updates. Parse incrementally so the tool_use block is
	// emitted at the same point the model produced it, not only at end of turn.
	streamDelimiter := h.cfg.MCP.ClientToolCalling.DelimiterTag
	if streamDelimiter == "" {
		streamDelimiter = "tool_call"
	}
	streamTextParser := anthropic.NewStreamParser(streamDelimiter)
	in.StreamTextToolCalls = func(chunk string) (string, []toolCollectorEntry) {
		textDelta, blocks := streamTextParser.ParseChunk(chunk)
		if len(blocks) == 0 {
			return textDelta, nil
		}
		out := make([]toolCollectorEntry, 0, len(blocks))
		for _, b := range blocks {
			if b.Type != "tool_use" || b.Name == "" {
				continue
			}
			args := json.RawMessage(b.Input)
			if len(args) == 0 || string(args) == "null" {
				args = json.RawMessage("{}")
			}
			out = append(out, toolCollectorEntry{ID: b.ID, Name: b.Name, Arguments: args})
		}
		return textDelta, out
	}

	in.OnFirstToken = func() {
		streamMu.Lock()
		defer streamMu.Unlock()
		startMessage()
	}
	in.OnThought = func(chunk string) {
		if !h.cfg.Anthropic.ExposeReasoningContent && req.Thinking == nil {
			return
		}
		streamMu.Lock()
		defer streamMu.Unlock()
		startMessage()
		if thinkingIndex < 0 {
			thinkingIndex = blockIndex
			blockIndex++
			thinkingBuf.Reset()
			_ = sseWriter.WriteContentBlockStart(thinkingIndex, anthropic.ContentBlock{
				Type:     "thinking",
				Thinking: "",
			})
		}
		thinkingBuf.WriteString(chunk)
		_ = sseWriter.WriteContentBlockDelta(thinkingIndex, anthropic.ContentBlockDelta{
			Type:     "thinking_delta",
			Thinking: chunk,
		})
	}
	in.OnText = func(chunk string) {
		streamMu.Lock()
		defer streamMu.Unlock()
		startMessage()
		closeThinking()
		if textIndex < 0 {
			textIndex = blockIndex
			blockIndex++
			_ = sseWriter.WriteContentBlockStart(textIndex, anthropic.ContentBlock{Type: "text", Text: ""})
		}
		_ = sseWriter.WriteContentBlockDelta(textIndex, anthropic.ContentBlockDelta{
			Type: "text_delta",
			Text: chunk,
		})
	}
	in.OnInternalToolProgress = func(text string) {
		if !h.cfg.Anthropic.ExposeReasoningContent && req.Thinking == nil {
			return
		}
		streamMu.Lock()
		defer streamMu.Unlock()
		startMessage()
		_ = sseWriter.WriteContentBlockStart(blockIndex, anthropic.ContentBlock{Type: "text", Text: ""})
		_ = sseWriter.WriteContentBlockDelta(blockIndex, anthropic.ContentBlockDelta{Type: "text_delta", Text: text})
		_ = sseWriter.WriteContentBlockStop(blockIndex)
		blockIndex++
	}
	in.OnTool = func(entry toolCollectorEntry) {
		streamMu.Lock()
		defer streamMu.Unlock()
		startMessage()
		closeThinking()
		closeText()

		idx := blockIndex
		blockIndex++
		args := string(entry.Arguments)
		if args == "" || args == "null" {
			args = "{}"
		}
		_ = sseWriter.WriteContentBlockStart(idx, anthropic.ContentBlock{
			Type:  "tool_use",
			ID:    entry.ID,
			Name:  entry.Name,
			Input: json.RawMessage("{}"),
		})
		_ = sseWriter.WriteContentBlockDelta(idx, anthropic.ContentBlockDelta{
			Type:        "input_json_delta",
			PartialJSON: args,
		})
		_ = sseWriter.WriteContentBlockStop(idx)
	}

	maxRetries := h.cfg.Execution.MaxRetries
	if maxRetries <= 0 && h.cfg.Accounts.MaxFailoverRetries > 0 {
		maxRetries = h.cfg.Accounts.MaxFailoverRetries
	}

	messageStartedPtr := func() bool {
		streamMu.Lock()
		defer streamMu.Unlock()
		return messageStarted
	}

	var outcome turnResult
	err = h.failover.ExecuteWithFailover(r.Context(), modelEntry.UpstreamModel, modelEntry.RequiresPaidTier, maxRetries, func(ctx context.Context, worker *pool.Worker) error {
		res, attemptErr := executeTurnAttempt(ctx, worker, h.cfg, h.logger, in)
		outcome = res
		if attemptErr != nil {
			if messageStartedPtr() {
				return fmt.Errorf("%w: %v", pool.ErrNonRetryableStream, attemptErr)
			}
			return attemptErr
		}
		return nil
	})

	if err != nil {
		if !sseWriter.Committed() {
			h.writeAnthropicTurnError(w, err)
			return
		}
		// See the OpenAI branch: after message_start the only channel left is in-band, so the class
		// is exposed and the full error is logged with the request id (FR-18).
		kind := ClientErrorKind(err)
		h.logger.Error("anthropic stream failed after message start",
			"requestID", requestIDFromContext(r), "class", kind, "err", err)
		_ = sseWriter.WriteError(kind, ClientErrorMessage(kind))
		return
	}

	stopReason := "end_turn"
	var stopSequence *string
	if len(outcome.Tools) > 0 {
		stopReason = "tool_use"
	} else if outcome.StopMatched {
		stopReason = "stop_sequence"
		if seq := outcome.MatchedSequence; seq != "" {
			stopSequence = &seq
		}
	} else if _, anthropicStop := MapStopReason(outcome.StopReason); anthropicStop != "" {
		stopReason = anthropicStop
	}

	streamMu.Lock()
	defer streamMu.Unlock()

	startMessage()
	if outcome.Text == "" && len(outcome.Tools) == 0 {
		textIndex = blockIndex
		blockIndex++
		_ = sseWriter.WriteContentBlockStart(textIndex, anthropic.ContentBlock{Type: "text", Text: ""})
		_ = sseWriter.WriteContentBlockDelta(textIndex, anthropic.ContentBlockDelta{
			Type: "text_delta",
			Text: "I have completed the analysis.",
		})
		_ = sseWriter.WriteContentBlockStop(textIndex)
		textIndex = -1
	}
	closeThinking()
	closeText()

	usage := buildAnthropicUsage(outcome, promptBlocks)
	_ = sseWriter.WriteMessageDelta(stopReason, stopSequence, usage.OutputTokens)
	_ = sseWriter.WriteMessageStop()
}

// assembleAnthropicTools renders collected tool calls as Anthropic tool_use blocks.
//
// @purpose Single conversion point from sanitised collector entries to provider payloads.
// @param outcome Executor result.
// @returns tool_use blocks in first-seen order, or nil.
// @invariants Pure.
func assembleAnthropicTools(outcome turnResult) []anthropic.ContentBlock {
	if len(outcome.Tools) == 0 {
		return nil
	}
	blocks := make([]anthropic.ContentBlock, 0, len(outcome.Tools))
	for _, entry := range outcome.Tools {
		args := string(entry.Arguments)
		if args == "" || args == "null" {
			args = "{}"
		}
		blocks = append(blocks, anthropic.ContentBlock{
			Type:  "tool_use",
			ID:    entry.ID,
			Name:  entry.Name,
			Input: json.RawMessage(args),
		})
	}
	return blocks
}

// buildAnthropicUsage renders usage from the authoritative agent counters.
//
// @purpose Reports cache and reasoning counters the previous implementation dropped.
// @param outcome Executor result.
// @param promptBlocks Prompt blocks used for the fallback estimate.
// @returns Anthropic usage with exact field names.
// @invariants Pure.
func buildAnthropicUsage(outcome turnResult, promptBlocks []acp.PromptBlock) anthropic.Usage {
	if outcome.UsageFromAgent {
		return anthropic.Usage{
			InputTokens:              int(outcome.Usage.InputTokens),
			OutputTokens:             int(outcome.Usage.OutputTokens),
			CacheReadInputTokens:     int(outcome.Usage.CachedReadTokens),
			CacheCreationInputTokens: int(outcome.Usage.CachedWriteTokens),
		}
	}
	return anthropic.Usage{
		InputTokens:  estimateAnthropicPromptTokens(promptBlocks),
		OutputTokens: anthropic.CountTokens(outcome.Text) + anthropic.CountTokens(outcome.Thoughts),
	}
}

// estimateAnthropicPromptTokens sums prompt tokens without indexing an empty slice.
//
// @purpose Removes the unchecked `promptBlocks[0]` index expressions.
// @param blocks Prompt blocks sent upstream.
// @returns Token estimate, zero when no blocks exist.
// @invariants Pure; nil-safe.
func estimateAnthropicPromptTokens(blocks []acp.PromptBlock) int {
	total := 0
	for _, block := range blocks {
		total += anthropic.CountTokens(block.Text)
	}
	return total
}

// writeAnthropicTurnError maps an executor error onto the Anthropic error surface.
//
// @purpose Single error-mapping point implementing the FR-18 table for this protocol.
// @param w Response writer.
// @param err Executor error.
// @invariants Writes exactly one response.
func (h *AnthropicHandlers) writeAnthropicTurnError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrFirstTokenTimeout) {
		h.writeAnthropicError(w, http.StatusGatewayTimeout, "api_error", "time to first token exceeded")
		return
	}
	if errors.Is(err, pool.ErrQueueSaturated) {
		h.writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "all workers are busy; retry shortly")
		return
	}
	if errors.Is(err, pool.ErrNoAccounts) || errors.Is(err, pool.ErrNoEligibleAccounts) {
		// Quota exhaustion across every account is retryable, so it maps to 429 with a hint rather
		// than to a 5xx that implies the gateway itself is broken.
		if retryAfter := h.failover.NextAvailableIn(time.Now()); retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
		}
		if errors.Is(err, pool.ErrNoAccounts) {
			h.writeAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error",
				"no Antigravity accounts are configured (code: no_accounts); an administrator must add one")
			return
		}
		h.writeAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error",
			"all Antigravity accounts are temporarily unavailable (code: all_accounts_cooling); retry later")
		return
	}
	if errors.Is(err, retry.ErrEmptyCompletion) {
		h.writeAnthropicError(w, http.StatusBadGateway, "api_error",
			"upstream agent returned an empty completion; retry the request")
		return
	}
	var respErr *acp.ResponseError
	if (errors.As(err, &respErr) && respErr.Code == acp.ErrCodeAuthRequired) ||
		strings.Contains(err.Error(), "Authentication required") ||
		strings.Contains(err.Error(), "transport is closed") {
		h.writeAnthropicError(w, http.StatusServiceUnavailable, "authentication_error", "No authenticated Antigravity accounts available")
		return
	}
	if errors.As(err, &respErr) && respErr.Code == acp.ErrCodeInvalidParams {
		h.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "upstream rejected the request: "+respErr.Message)
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		h.writeAnthropicError(w, http.StatusGatewayTimeout, "api_error", "inference turn was cancelled")
		return
	}
	h.logger.Error("anthropic turn failed", "err", err)
	h.writeAnthropicError(w, http.StatusInternalServerError, "api_error", "Inference error")
}

func (h *AnthropicHandlers) writeAnthropicError(w http.ResponseWriter, statusCode int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("anthropic-version", "2023-06-01")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(anthropic.ErrorResponse{
		Type: "error",
		Error: anthropic.ErrorDetails{
			Type:    errType,
			Message: message,
		},
	})
}
