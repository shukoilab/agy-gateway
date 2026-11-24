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
	"agy-gateway/internal/config"
	"agy-gateway/internal/models"
	"agy-gateway/internal/openai"
	"agy-gateway/internal/pool"
	"agy-gateway/internal/telemetry"
)

// OpenAIHandlers handles public OpenAI-compatible endpoints.
type OpenAIHandlers struct {
	cfg           *config.Config
	pool          *pool.Pool
	failover      *pool.FailoverCoordinator
	modelRegistry *models.ModelRegistry
	translator    *openai.Translator
	logger        *slog.Logger
}

// NewOpenAIHandlers creates an initialized handler suite.
func NewOpenAIHandlers(
	cfg *config.Config,
	p *pool.Pool,
	failover *pool.FailoverCoordinator,
	modelRegistry *models.ModelRegistry,
	logger *slog.Logger,
) *OpenAIHandlers {
	if logger == nil {
		logger = slog.Default()
	}
	return &OpenAIHandlers{
		cfg:           cfg,
		pool:          p,
		failover:      failover,
		modelRegistry: modelRegistry,
		translator:    openai.NewTranslator(),
		logger:        logger,
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

	modelEntry, found := h.modelRegistry.FindModel(req.Model)
	if !found {
		h.writeOpenAIError(w, http.StatusNotFound, fmt.Sprintf("The model %q does not exist", req.Model), "invalid_request_error", "model_not_found", "model")
		return
	}

	if req.N != nil && *req.N > 1 {
		h.writeOpenAIError(w, http.StatusBadRequest, "n > 1 is not supported by this gateway", "invalid_request_error", "unsupported_value", "n")
		return
	}

	// 1. Translate prompt and deduplicate JIT tools into ACP prompt blocks
	delimiter := h.cfg.MCP.ClientToolCalling.DelimiterTag
	if delimiter == "" {
		delimiter = "tool_call"
	}
	systemPrompt, promptBlocks := h.translator.Translate(&req, delimiter)

	effort := req.ReasoningEffort
	if effort == "" && modelEntry.PresetEffort != "" {
		effort = modelEntry.PresetEffort
	}
	thoughtLevel := config.MapReasoningEffort(effort, h.cfg.OpenAI.DefaultReasoningEffort)

	// 2. Execute turn through worker pool with transparent failover
	createdTime := time.Now().Unix()
	completionID := fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:14])

	if req.Stream {
		h.handleStreaming(w, r, &req, modelEntry, thoughtLevel, systemPrompt, promptBlocks, completionID, createdTime)
	} else {
		h.handleSync(w, r, &req, modelEntry, thoughtLevel, systemPrompt, promptBlocks, completionID, createdTime)
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
	completionID string,
	createdTime int64,
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

		// A. Call session/new (pure editor cwd boundary, 100% compliant with standard ACP)
		sessCtx, cancelSess := context.WithTimeout(ctx, h.cfg.Pool.HandshakeTimeout)
		defer cancelSess()

		newParams := acp.SessionNewParams{
			CWD: worker.Cwd,
		}
		if systemPrompt != "" {
			newParams.Meta = map[string]any{
				"antigravity": map[string]any{
					"systemPrompt": map[string]any{
						"append": systemPrompt,
					},
				},
			}
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

		defer func() {
			if newResult.SessionID != "" {
				closeCtx, cancelClose := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancelClose()
				_ = worker.Transport.Call(closeCtx, "session/close", acp.SessionCloseParams{
					SessionID: newResult.SessionID,
				}, nil)
			}
		}()

		// B. Apply session config options
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

		// TTFT Watchdog initialization for this attempt
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
			h.writeOpenAIError(w, http.StatusGatewayTimeout, fmt.Sprintf("Time to first token exceeded %s (model cold-cache or prompt prefill timed out)", ttftDuration), "api_error", "first_token_timeout", "")
			return
		}
		var respErr *acp.ResponseError
		if (errors.As(err, &respErr) && respErr.Code == acp.ErrCodeAuthRequired) ||
			strings.Contains(err.Error(), "Authentication required") ||
			strings.Contains(err.Error(), "transport is closed") {
			h.writeOpenAIError(w, http.StatusServiceUnavailable, "No authenticated Antigravity accounts available. Please login an account via admin endpoint POST /admin/v1/accounts/login/init", "invalid_request_error", "account_auth_required", "")
			return
		}
		h.writeOpenAIError(w, http.StatusInternalServerError, "Inference error: "+err.Error(), "api_error", "inference_error", "")
		return
	}

	// 3. Parse text and JIT consumer tool calls
	delimiter := h.cfg.MCP.ClientToolCalling.DelimiterTag
	if delimiter == "" {
		delimiter = "tool_call"
	}
	cleanContent, toolCalls, hasTools := openai.ParseCompleteResponse(fullText.String(), delimiter)

	finishReason := "stop"
	var message openai.ChatMessage
	if hasTools {
		finishReason = "tool_calls"
		var contentVal any
		if cleanContent != "" {
			contentVal = cleanContent
		}
		message = openai.ChatMessage{
			Role:      "assistant",
			Content:   contentVal,
			ToolCalls: toolCalls,
		}
	} else {
		message = openai.ChatMessage{
			Role:    "assistant",
			Content: cleanContent,
		}
	}

	if h.cfg.OpenAI.ExposeReasoningContent && fullThoughts.Len() > 0 {
		message.ReasoningContent = fullThoughts.String()
	}

	promptTokens := openai.CountTokens(promptBlocks[0].Text)
	compTokens := openai.CountTokens(cleanContent)
	reasoningTokens := openai.CountTokens(fullThoughts.String())

	resp := openai.ChatCompletionResponse{
		ID:      completionID,
		Object:  "chat.completion",
		Created: createdTime,
		Model:   req.Model,
		Choices: []openai.ChatCompletionChoice{
			{
				Index:        0,
				Message:      message,
				FinishReason: finishReason,
			},
		},
		Usage: openai.UsageDetails{
			PromptTokens:     promptTokens,
			CompletionTokens: compTokens + reasoningTokens,
			TotalTokens:      promptTokens + compTokens + reasoningTokens,
			PromptTokensDetails: &openai.PromptTokensDetails{
				CachedTokens: 0,
			},
			CompletionTokensDetails: &openai.CompletionTokensDetails{
				ReasoningTokens: reasoningTokens,
			},
		},
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
	completionID string,
	createdTime int64,
) {
	telemetry.DefaultMetrics.RequestsStreaming.Add(1)

	sseWriter, err := openai.NewSSEWriter(w)
	if err != nil {
		h.writeOpenAIError(w, http.StatusInternalServerError, "Streaming not supported: "+err.Error(), "api_error", "streaming_error", "")
		return
	}

	delimiter := h.cfg.MCP.ClientToolCalling.DelimiterTag
	if delimiter == "" {
		delimiter = "tool_call"
	}
	streamParser := openai.NewStreamParser(delimiter)
	firstChunkSent := false
	hasToolCalls := false

	var totalText strings.Builder
	var totalThoughts strings.Builder

	// Start keepalive ticker
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
		totalText.Reset()
		totalThoughts.Reset()
		streamParser = openai.NewStreamParser(delimiter)
		firstChunkSent = false
		hasToolCalls = false

		sessCtx, cancelSess := context.WithTimeout(ctx, h.cfg.Pool.HandshakeTimeout)
		defer cancelSess()

		newParams := acp.SessionNewParams{
			CWD: worker.Cwd,
		}
		if systemPrompt != "" {
			newParams.Meta = map[string]any{
				"antigravity": map[string]any{
					"systemPrompt": map[string]any{
						"append": systemPrompt,
					},
				},
			}
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

		defer func() {
			if newResult.SessionID != "" {
				closeCtx, cancelClose := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancelClose()
				_ = worker.Transport.Call(closeCtx, "session/close", acp.SessionCloseParams{
					SessionID: newResult.SessionID,
				}, nil)
			}
		}()

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

		// TTFT Watchdog initialization for this streaming attempt
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
			if update.Update.SessionUpdate == acp.UpdateAgentMessageChunk && update.Update.Content != nil {
				textChunk := update.Update.Content.Text
				totalText.WriteString(textChunk)

				textDelta, toolCalls := streamParser.ParseChunk(textChunk)

				if len(toolCalls) > 0 {
					hasToolCalls = true
				}

				if textDelta != "" || len(toolCalls) > 0 {
					var delta openai.ChatCompletionChunkDelta
					if !firstChunkSent {
						delta.Role = "assistant"
						firstChunkSent = true
					}
					if textDelta != "" {
						delta.Content = &textDelta
					}
					if len(toolCalls) > 0 {
						delta.ToolCalls = toolCalls
					}

					chunk := openai.ChatCompletionChunk{
						ID:      completionID,
						Object:  "chat.completion.chunk",
						Created: createdTime,
						Model:   req.Model,
						Choices: []openai.ChatCompletionChunkChoice{
							{
								Index: 0,
								Delta: delta,
							},
						},
					}
					_ = sseWriter.WriteChunk(&chunk)
				}
			} else if update.Update.SessionUpdate == acp.UpdateAgentThoughtChunk && update.Update.Content != nil {
				thoughtChunk := update.Update.Content.Text
				totalThoughts.WriteString(thoughtChunk)

				if h.cfg.OpenAI.ExposeReasoningContent {
					chunk := openai.ChatCompletionChunk{
						ID:      completionID,
						Object:  "chat.completion.chunk",
						Created: createdTime,
						Model:   req.Model,
						Choices: []openai.ChatCompletionChunkChoice{
							{
								Index: 0,
								Delta: openai.ChatCompletionChunkDelta{
									ReasoningContent: &thoughtChunk,
								},
							},
						},
					}
					_ = sseWriter.WriteChunk(&chunk)
				}
			} else if update.Update.SessionUpdate == acp.UpdateToolCall || update.Update.SessionUpdate == acp.UpdateToolCallUpdate {
				telemetry.DefaultMetrics.ToolCallsTotal.Add(1)
			}
		})
		defer worker.Transport.SetUpdateListener(nil)

		var promptResult acp.PromptResult
		if err := worker.Transport.Call(promptCtx, "session/prompt", acp.PromptParams{
			SessionID: newResult.SessionID,
			Prompt:    promptBlocks,
		}, &promptResult); err != nil {
			if firstChunkSent {
				return fmt.Errorf("%w: %v", pool.ErrNonRetryableStream, err)
			}
			return err
		}

		// Flush any remaining buffered tokens or unclosed tool tags from stream parser
		flushDelta, flushTools := streamParser.Flush()
		if len(flushTools) > 0 {
			hasToolCalls = true
		}
		if flushDelta != "" || len(flushTools) > 0 {
			var delta openai.ChatCompletionChunkDelta
			if !firstChunkSent {
				delta.Role = "assistant"
				firstChunkSent = true
			}
			if flushDelta != "" {
				delta.Content = &flushDelta
			}
			if len(flushTools) > 0 {
				delta.ToolCalls = flushTools
			}
			chunk := openai.ChatCompletionChunk{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: createdTime,
				Model:   req.Model,
				Choices: []openai.ChatCompletionChunkChoice{
					{
						Index: 0,
						Delta: delta,
					},
				},
			}
			_ = sseWriter.WriteChunk(&chunk)
		}

		return nil
	})

	if err != nil {
		h.logger.Error("streaming turn failed", "err", err)
		if ttftTimedOut.Load() {
			_ = sseWriter.WriteError(fmt.Sprintf("Time to first token exceeded %s (model cold-cache or prompt prefill timed out)", ttftDuration), "first_token_timeout")
			return
		}
		var respErr *acp.ResponseError
		if (errors.As(err, &respErr) && respErr.Code == acp.ErrCodeAuthRequired) ||
			strings.Contains(err.Error(), "Authentication required") ||
			strings.Contains(err.Error(), "transport is closed") {
			_ = sseWriter.WriteError("No authenticated Antigravity accounts available. Please login an account via admin endpoint POST /admin/v1/accounts/login/init", "account_auth_required")
			return
		}
		_ = sseWriter.WriteError("Inference error: "+err.Error(), "api_error")
		return
	}

	// Double check tool presence across full collected text
	if !hasToolCalls {
		_, parsedTools, hasTools := openai.ParseCompleteResponse(totalText.String(), delimiter)
		if hasTools || len(parsedTools) > 0 {
			hasToolCalls = true
		}
	}

	// Emit finish reason chunk
	finishReason := "stop"
	if hasToolCalls {
		finishReason = "tool_calls"
	}

	finishChunk := openai.ChatCompletionChunk{
		ID:      completionID,
		Object:  "chat.completion.chunk",
		Created: createdTime,
		Model:   req.Model,
		Choices: []openai.ChatCompletionChunkChoice{
			{
				Index:        0,
				Delta:        openai.ChatCompletionChunkDelta{},
				FinishReason: &finishReason,
			},
		},
	}
	_ = sseWriter.WriteChunk(&finishChunk)

	// Emit usage chunk if requested
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		promptTokens := openai.CountTokens(promptBlocks[0].Text)
		compTokens := openai.CountTokens(totalText.String())
		reasoningTokens := openai.CountTokens(totalThoughts.String())

		usageChunk := openai.ChatCompletionChunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: createdTime,
			Model:   req.Model,
			Choices: []openai.ChatCompletionChunkChoice{},
			Usage: &openai.UsageDetails{
				PromptTokens:     promptTokens,
				CompletionTokens: compTokens + reasoningTokens,
				TotalTokens:      promptTokens + compTokens + reasoningTokens,
				PromptTokensDetails: &openai.PromptTokensDetails{
					CachedTokens: 0,
				},
				CompletionTokensDetails: &openai.CompletionTokensDetails{
					ReasoningTokens: reasoningTokens,
				},
			},
		}
		_ = sseWriter.WriteChunk(&usageChunk)
	}

	_ = sseWriter.Close()
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
