package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"agy-gateway/internal/acp"
	"agy-gateway/internal/anthropic"
	"agy-gateway/internal/config"
	"agy-gateway/internal/models"
	"agy-gateway/internal/pool"
	"agy-gateway/internal/telemetry"
)

// AnthropicHandlers handles public Anthropic Messages API compatible endpoints.
//
// @purpose Coordinates POST /v1/messages request lifecycle, translation, streaming, and failover.
// @rationale Provides full protocol parity with Anthropic Claude clients using the agy backend.
// @preconditions Dependencies must be non-nil.
type AnthropicHandlers struct {
	cfg           *config.Config
	pool          *pool.Pool
	failover      *pool.FailoverCoordinator
	modelRegistry *models.ModelRegistry
	translator    *anthropic.Translator
	logger        *slog.Logger
}

// NewAnthropicHandlers creates an initialized handler suite.
func NewAnthropicHandlers(
	cfg *config.Config,
	p *pool.Pool,
	failover *pool.FailoverCoordinator,
	modelRegistry *models.ModelRegistry,
	logger *slog.Logger,
) *AnthropicHandlers {
	if logger == nil {
		logger = slog.Default()
	}
	return &AnthropicHandlers{
		cfg:           cfg,
		pool:          p,
		failover:      failover,
		modelRegistry: modelRegistry,
		translator:    anthropic.NewTranslator(),
		logger:        logger.With("component", "anthropic-handlers"),
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

	delimiter := h.cfg.MCP.ClientToolCalling.DelimiterTag
	if delimiter == "" {
		delimiter = "tool_call"
	}
	promptBlocks := h.translator.Translate(&req, delimiter)

	// Map Anthropic thinking config to ACP thought level
	effort := ""
	if req.Thinking != nil {
		switch req.Thinking.Type {
		case "adaptive":
			effort = "high"
		case "enabled":
			switch {
			case req.Thinking.BudgetTokens <= 1024:
				effort = "low"
			case req.Thinking.BudgetTokens <= 4096:
				effort = "medium"
			case req.Thinking.BudgetTokens <= 16384:
				effort = "high"
			default:
				effort = "extra"
			}
		}
	}
	if effort == "" && modelEntry.PresetEffort != "" {
		effort = modelEntry.PresetEffort
	}
	thoughtLevel := config.MapReasoningEffort(effort, "medium")

	completionID := fmt.Sprintf("msg_%s", uuid.New().String()[:24])

	if req.Stream {
		h.handleStreaming(w, r, &req, modelEntry, thoughtLevel, promptBlocks, completionID)
	} else {
		h.handleSync(w, r, &req, modelEntry, thoughtLevel, promptBlocks, completionID)
	}
}

func (h *AnthropicHandlers) handleSync(
	w http.ResponseWriter,
	r *http.Request,
	req *anthropic.MessageRequest,
	modelEntry *models.ModelInfo,
	thoughtLevel string,
	promptBlocks []acp.PromptBlock,
	completionID string,
) {
	var fullText strings.Builder
	var fullThoughts strings.Builder
	var turnMu sync.Mutex

	ttftDuration := h.cfg.Pool.FirstTokenTimeout
	if ttftDuration <= 0 {
		ttftDuration = 120 * time.Second
	}
	var ttftTimedOut atomic.Bool

	maxRetries := h.cfg.Execution.MaxRetries
	if maxRetries <= 0 && h.cfg.Accounts.MaxFailoverRetries > 0 {
		maxRetries = h.cfg.Accounts.MaxFailoverRetries
	}

	err := h.failover.ExecuteWithFailover(r.Context(), modelEntry.RequiresPaidTier, maxRetries, func(ctx context.Context, worker *pool.Worker) error {
		turnMu.Lock()
		fullText.Reset()
		fullThoughts.Reset()
		turnMu.Unlock()

		// A. Negotiate ACP session
		sessCtx, cancelSess := context.WithTimeout(ctx, h.cfg.Pool.HandshakeTimeout)
		defer cancelSess()

		newParams := acp.SessionNewParams{
			CWD: worker.Cwd,
		}
		var newResult acp.SessionNewResult
		if err := worker.Transport.Call(sessCtx, "session/new", newParams, &newResult); err != nil {
			var respErr *acp.ResponseError
			if errors.As(err, &respErr) && respErr.Code == acp.ErrCodeAuthRequired {
				time.Sleep(1 * time.Second)
				err = worker.Transport.Call(sessCtx, "session/new", newParams, &newResult)
			}
			if err != nil {
				return err
			}
		}

		_ = worker.Transport.Call(sessCtx, "session/set_config_option", acp.SetConfigOptionParams{
			SessionID: newResult.SessionID,
			ConfigID:  acp.ConfigIDMode,
			Value:     h.cfg.Execution.AcpSessionMode,
		}, nil)
		_ = worker.Transport.Call(sessCtx, "session/set_config_option", acp.SetConfigOptionParams{
			SessionID: newResult.SessionID,
			ConfigID:  acp.ConfigIDModel,
			Value:     modelEntry.UpstreamModel,
		}, nil)
		if thoughtLevel != "" {
			_ = worker.Transport.Call(sessCtx, "session/set_config_option", acp.SetConfigOptionParams{
				SessionID: newResult.SessionID,
				ConfigID:  acp.ConfigIDThoughtLevel,
				Value:     thoughtLevel,
			}, nil)
		}

		// B. TTFT Watchdog initialization
		var ttftDisarmed atomic.Bool
		ttftTimer := time.NewTimer(ttftDuration)
		defer ttftTimer.Stop()

		promptCtx, cancelPrompt := context.WithTimeout(ctx, h.cfg.Pool.InferenceTimeout)
		defer cancelPrompt()

		go func() {
			select {
			case <-promptCtx.Done():
			case <-ttftTimer.C:
				if !ttftDisarmed.Load() {
					ttftTimedOut.Store(true)
					h.logger.Warn("time-to-first-token watchdog triggered", "timeout", ttftDuration)
					cancelPrompt()
				}
			}
		}()

		// C. Register listener for turn updates
		worker.Transport.SetUpdateListener(func(update acp.SessionUpdateParams) {
			if !ttftDisarmed.Load() {
				ttftDisarmed.Store(true)
				ttftTimer.Stop()
			}
			turnMu.Lock()
			defer turnMu.Unlock()

			if update.Update.SessionUpdate == acp.UpdateAgentMessageChunk && update.Update.Content != nil {
				fullText.WriteString(update.Update.Content.Text)
			} else if update.Update.SessionUpdate == acp.UpdateAgentThoughtChunk && update.Update.Content != nil {
				fullThoughts.WriteString(update.Update.Content.Text)
			} else if update.Update.SessionUpdate == acp.UpdateToolCall || update.Update.SessionUpdate == acp.UpdateToolCallUpdate {
				telemetry.DefaultMetrics.ToolCallsTotal.Add(1)
			}
		})
		defer worker.Transport.SetUpdateListener(nil)

		// D. Send session/prompt turn
		var promptResult acp.PromptResult
		if err := worker.Transport.Call(promptCtx, "session/prompt", acp.PromptParams{
			SessionID: newResult.SessionID,
			Prompt:    promptBlocks,
		}, &promptResult); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		if ttftTimedOut.Load() {
			h.writeAnthropicError(w, http.StatusGatewayTimeout, "api_error", fmt.Sprintf("Time to first token exceeded %s", ttftDuration))
			return
		}
		var respErr *acp.ResponseError
		if (errors.As(err, &respErr) && respErr.Code == acp.ErrCodeAuthRequired) ||
			strings.Contains(err.Error(), "Authentication required") ||
			strings.Contains(err.Error(), "transport is closed") {
			h.writeAnthropicError(w, http.StatusServiceUnavailable, "authentication_error", "No authenticated Antigravity accounts available")
			return
		}
		h.writeAnthropicError(w, http.StatusInternalServerError, "api_error", "Inference error: "+err.Error())
		return
	}

	// 3. Parse text and JIT consumer tool calls
	delimiter := h.cfg.MCP.ClientToolCalling.DelimiterTag
	if delimiter == "" {
		delimiter = "tool_call"
	}
	cleanContent, toolBlocks, hasTools := anthropic.ParseCompleteResponse(fullText.String(), delimiter)

	var contentBlocks []anthropic.ContentBlock

	// Include thinking block if reasoning content is exposed or client requested thinking
	if (h.cfg.Anthropic.ExposeReasoningContent || req.Thinking != nil) && fullThoughts.Len() > 0 {
		contentBlocks = append(contentBlocks, anthropic.ContentBlock{
			Type:      "thinking",
			Thinking:  fullThoughts.String(),
			Signature: anthropic.GenerateThinkingSignature(fullThoughts.String()),
		})
	}

	// Append text content if present
	if cleanContent != "" {
		contentBlocks = append(contentBlocks, anthropic.ContentBlock{
			Type: "text",
			Text: cleanContent,
		})
	}

	// Append tool calls
	if hasTools {
		contentBlocks = append(contentBlocks, toolBlocks...)
	}

	stopReason := "end_turn"
	if hasTools {
		stopReason = "tool_use"
	}

	promptTokens := anthropic.CountTokens(promptBlocks[0].Text)
	outputTokens := anthropic.CountTokens(cleanContent) + anthropic.CountTokens(fullThoughts.String())

	resp := anthropic.MessageResponse{
		ID:         completionID,
		Type:       "message",
		Role:       "assistant",
		Content:    contentBlocks,
		Model:      req.Model,
		StopReason: &stopReason,
		Usage: anthropic.Usage{
			InputTokens:  promptTokens,
			OutputTokens: outputTokens,
		},
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
	promptBlocks []acp.PromptBlock,
	completionID string,
) {
	telemetry.DefaultMetrics.RequestsStreaming.Add(1)

	sseWriter, err := anthropic.NewSSEWriter(w)
	if err != nil {
		h.writeAnthropicError(w, http.StatusInternalServerError, "api_error", "Streaming initialization failed: "+err.Error())
		return
	}

	delimiter := h.cfg.MCP.ClientToolCalling.DelimiterTag
	if delimiter == "" {
		delimiter = "tool_call"
	}

	streamParser := anthropic.NewStreamParser(delimiter)
	var firstTokenSent atomic.Bool
	var blockIndex int
	var streamMu sync.Mutex

	var thinkingStarted bool
	var thinkingIndex int
	var textStarted bool
	var textIndex int

	var totalText strings.Builder
	var totalThoughts strings.Builder

	promptTokens := anthropic.CountTokens(promptBlocks[0].Text)

	// Keepalive ticker
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

	ttftDuration := h.cfg.Pool.FirstTokenTimeout
	if ttftDuration <= 0 {
		ttftDuration = 120 * time.Second
	}
	var ttftTimedOut atomic.Bool

	maxRetries := h.cfg.Execution.MaxRetries
	if maxRetries <= 0 && h.cfg.Accounts.MaxFailoverRetries > 0 {
		maxRetries = h.cfg.Accounts.MaxFailoverRetries
	}

	err = h.failover.ExecuteWithFailover(r.Context(), modelEntry.RequiresPaidTier, maxRetries, func(ctx context.Context, worker *pool.Worker) error {
		streamMu.Lock()
		totalText.Reset()
		totalThoughts.Reset()
		streamParser = anthropic.NewStreamParser(delimiter)
		thinkingStarted = false
		textStarted = false
		blockIndex = 0
		streamMu.Unlock()

		sessCtx, cancelSess := context.WithTimeout(ctx, h.cfg.Pool.HandshakeTimeout)
		defer cancelSess()

		newParams := acp.SessionNewParams{
			CWD: worker.Cwd,
		}
		var newResult acp.SessionNewResult
		if err := worker.Transport.Call(sessCtx, "session/new", newParams, &newResult); err != nil {
			var respErr *acp.ResponseError
			if errors.As(err, &respErr) && respErr.Code == acp.ErrCodeAuthRequired {
				time.Sleep(1 * time.Second)
				err = worker.Transport.Call(sessCtx, "session/new", newParams, &newResult)
			}
			if err != nil {
				return err
			}
		}

		_ = worker.Transport.Call(sessCtx, "session/set_config_option", acp.SetConfigOptionParams{
			SessionID: newResult.SessionID,
			ConfigID:  acp.ConfigIDMode,
			Value:     h.cfg.Execution.AcpSessionMode,
		}, nil)
		_ = worker.Transport.Call(sessCtx, "session/set_config_option", acp.SetConfigOptionParams{
			SessionID: newResult.SessionID,
			ConfigID:  acp.ConfigIDModel,
			Value:     modelEntry.UpstreamModel,
		}, nil)
		if thoughtLevel != "" {
			_ = worker.Transport.Call(sessCtx, "session/set_config_option", acp.SetConfigOptionParams{
				SessionID: newResult.SessionID,
				ConfigID:  acp.ConfigIDThoughtLevel,
				Value:     thoughtLevel,
			}, nil)
		}

		var ttftDisarmed atomic.Bool
		ttftTimer := time.NewTimer(ttftDuration)
		defer ttftTimer.Stop()

		promptCtx, cancelPrompt := context.WithTimeout(ctx, h.cfg.Pool.InferenceTimeout)
		defer cancelPrompt()

		go func() {
			select {
			case <-promptCtx.Done():
			case <-ttftTimer.C:
				if !ttftDisarmed.Load() {
					ttftTimedOut.Store(true)
					h.logger.Warn("streaming time-to-first-token watchdog triggered", "timeout", ttftDuration)
					cancelPrompt()
				}
			}
		}()

		worker.Transport.SetUpdateListener(func(update acp.SessionUpdateParams) {
			if !ttftDisarmed.Load() {
				ttftDisarmed.Store(true)
				ttftTimer.Stop()
			}

			streamMu.Lock()
			defer streamMu.Unlock()

			// 1. Handle Thinking Chunk
			if update.Update.SessionUpdate == acp.UpdateAgentThoughtChunk && update.Update.Content != nil {
				thoughtChunk := update.Update.Content.Text
				totalThoughts.WriteString(thoughtChunk)

				if !firstTokenSent.Load() {
					firstTokenSent.Store(true)
					_ = sseWriter.WriteMessageStart(completionID, req.Model, promptTokens)
				}

				if !thinkingStarted {
					thinkingStarted = true
					thinkingIndex = blockIndex
					blockIndex++
					_ = sseWriter.WriteContentBlockStart(thinkingIndex, anthropic.ContentBlock{
						Type:     "thinking",
						Thinking: "",
					})
				}

				_ = sseWriter.WriteContentBlockDelta(thinkingIndex, anthropic.ContentBlockDelta{
					Type:     "thinking_delta",
					Thinking: thoughtChunk,
				})
				return
			}

			// 2. Handle Message Text Chunk
			if update.Update.SessionUpdate == acp.UpdateAgentMessageChunk && update.Update.Content != nil {
				textChunk := update.Update.Content.Text
				totalText.WriteString(textChunk)

				// If thinking was active, close it out before emitting text
				if thinkingStarted {
					sig := anthropic.GenerateThinkingSignature(totalThoughts.String())
					_ = sseWriter.WriteContentBlockDelta(thinkingIndex, anthropic.ContentBlockDelta{
						Type:      "signature_delta",
						Signature: sig,
					})
					_ = sseWriter.WriteContentBlockStop(thinkingIndex)
					thinkingStarted = false
				}

				if !firstTokenSent.Load() {
					firstTokenSent.Store(true)
					_ = sseWriter.WriteMessageStart(completionID, req.Model, promptTokens)
				}

				textDelta, toolCalls := streamParser.ParseChunk(textChunk)

				if textDelta != "" {
					if !textStarted {
						textStarted = true
						textIndex = blockIndex
						blockIndex++
						_ = sseWriter.WriteContentBlockStart(textIndex, anthropic.ContentBlock{
							Type: "text",
							Text: "",
						})
					}
					_ = sseWriter.WriteContentBlockDelta(textIndex, anthropic.ContentBlockDelta{
						Type: "text_delta",
						Text: textDelta,
					})
				}

				if len(toolCalls) > 0 {
					// Close active text block if open
					if textStarted {
						_ = sseWriter.WriteContentBlockStop(textIndex)
						textStarted = false
					}

					for _, tc := range toolCalls {
						toolIdx := blockIndex
						blockIndex++
						_ = sseWriter.WriteContentBlockStart(toolIdx, tc)
						_ = sseWriter.WriteContentBlockDelta(toolIdx, anthropic.ContentBlockDelta{
							Type:        "input_json_delta",
							PartialJSON: string(tc.Input),
						})
						_ = sseWriter.WriteContentBlockStop(toolIdx)
					}
				}
				return
			}

			if update.Update.SessionUpdate == acp.UpdateToolCall || update.Update.SessionUpdate == acp.UpdateToolCallUpdate {
				telemetry.DefaultMetrics.ToolCallsTotal.Add(1)
			}
		})
		defer worker.Transport.SetUpdateListener(nil)

		var promptResult acp.PromptResult
		if err := worker.Transport.Call(promptCtx, "session/prompt", acp.PromptParams{
			SessionID: newResult.SessionID,
			Prompt:    promptBlocks,
		}, &promptResult); err != nil {
			if firstTokenSent.Load() {
				return pool.ErrNonRetryableStream
			}
			return err
		}

		// Flush any remaining buffered tokens or unclosed tool tags from stream parser
		flushDelta, flushTools := streamParser.Flush()
		if flushDelta != "" {
			if !textStarted {
				textStarted = true
				textIndex = blockIndex
				blockIndex++
				_ = sseWriter.WriteContentBlockStart(textIndex, anthropic.ContentBlock{
					Type: "text",
					Text: "",
				})
			}
			_ = sseWriter.WriteContentBlockDelta(textIndex, anthropic.ContentBlockDelta{
				Type: "text_delta",
				Text: flushDelta,
			})
		}
		if len(flushTools) > 0 {
			if textStarted {
				_ = sseWriter.WriteContentBlockStop(textIndex)
				textStarted = false
			}
			for _, tc := range flushTools {
				toolIdx := blockIndex
				blockIndex++
				_ = sseWriter.WriteContentBlockStart(toolIdx, tc)
				_ = sseWriter.WriteContentBlockDelta(toolIdx, anthropic.ContentBlockDelta{
					Type:        "input_json_delta",
					PartialJSON: string(tc.Input),
				})
				_ = sseWriter.WriteContentBlockStop(toolIdx)
			}
		}

		return nil
	})

	if err != nil {
		if firstTokenSent.Load() {
			_ = sseWriter.WriteError("api_error", "Streaming interrupted: "+err.Error())
			return
		}
		if ttftTimedOut.Load() {
			h.writeAnthropicError(w, http.StatusGatewayTimeout, "api_error", fmt.Sprintf("Time to first token exceeded %s", ttftDuration))
			return
		}
		h.writeAnthropicError(w, http.StatusInternalServerError, "api_error", "Inference error: "+err.Error())
		return
	}

	// Final stream closure
	streamMu.Lock()
	defer streamMu.Unlock()

	if thinkingStarted {
		sig := anthropic.GenerateThinkingSignature(totalThoughts.String())
		_ = sseWriter.WriteContentBlockDelta(thinkingIndex, anthropic.ContentBlockDelta{
			Type:      "signature_delta",
			Signature: sig,
		})
		_ = sseWriter.WriteContentBlockStop(thinkingIndex)
		thinkingStarted = false
	}
	if textStarted {
		_ = sseWriter.WriteContentBlockStop(textIndex)
		textStarted = false
	}

	stopReason := "end_turn"
	_, toolBlocks, hasTools := anthropic.ParseCompleteResponse(totalText.String(), delimiter)
	if hasTools || len(toolBlocks) > 0 {
		stopReason = "tool_use"
	}

	outputTokens := anthropic.CountTokens(totalText.String()) + anthropic.CountTokens(totalThoughts.String())

	_ = sseWriter.WriteMessageDelta(stopReason, nil, outputTokens)
	_ = sseWriter.WriteMessageStop()
}

// writeAnthropicError renders an Anthropic RFC-compliant JSON error.
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
