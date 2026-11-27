# Changelog

All notable changes to the `agy-gateway` project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [3.16.4] - 2026-11-27

### Bug Fixes

* **translate:** restore tool-call ids and bound conversation history ([43c9c64](https://github.com/shukoilab/agy-gateway/commit/43c9c64f8f1a2ca36d6a4d083bdcdad08880533f))

## [3.16.3] - 2026-11-27

### Bug Fixes

* **acp:** deliver system prompt as prompt block, drop append carrier ([b52091c](https://github.com/shukoilab/agy-gateway/commit/b52091c466e1c527952d3671cb761b0f602a3d9d))

## [Unreleased]

### Bug Fixes

- **Universal harness compatibility: the consumer system prompt no longer travels through `_meta.antigravity.systemPrompt.append`.** agy inspects that field with a policy guard which answers certain instruction text with a synthetic `429 ResourceExhausted` while emitting zero ACP updates, so the turn hung until the client disconnected. Any harness whose persona tripped the guard appeared to receive no output at all — not merely no reasoning (reproduced with Hermes v0.23.4: 0 data frames in 300s; the same text as a prompt block produces 6-8 chunks plus tool calls in under 14s). The prompt is now always prepended as the first `session/prompt` block, so one code path behaves identically for every client and every model.
- **`execution.system_prompt_channel` removed along with the append carrier.** The knob existed only to choose between a working path and a broken one. A stale `system_prompt_channel` key or `AGY_GW_SYSTEM_PROMPT_CHANNEL` env var now produces a startup warning with no behavioural effect, rather than silently restoring the hang.
- **Vendor system-prompt capability parsing removed.** `AgentCapabilities.SupportsVendorSystemPrompt` and the per-worker warning it drove are gone with the carrier they described.
- **SSE status is written once.** `internal/anthropic/sse.go` no longer calls `WriteHeader` in its constructor, and both writers expose `Committed()`, so a failure before the first byte returns a truthful HTTP status instead of a JSON body after a committed 200 (the `superfluous response.WriteHeader` warnings in the log).
- **Unsupported OpenAI/Anthropic parameters are accepted instead of policed.** The `unsupported_param_policy` setting is gone: every parameter a harness sends is accepted, values agy supports are mapped, and the rest are ignored. Harnesses that send extra fields work unmodified.
- **`mcp-bridge` exits when its session is gone.** A 401 from the loopback server used to log and loop forever; it now terminates, so session teardown no longer leaks a blocked child process.
- **Anthropic thinking signatures are content-derived.** Every thinking block previously carried the same signature (an HMAC over the empty string). They now hash the block's own text.
- **Every parallel tool call keeps its own index, so clients stop merging them into one unparseable call.** The OpenAI streaming path emitted `index: 0` for every tool-call delta. OpenAI clients fold deltas that share an index into a single call and concatenate the argument fragments, so a turn producing several parallel calls (opencode routinely issues four `read` calls at once) arrived as one call whose `arguments` was `{"limit":300,…"}{"limit":300,…"}{…}` — invalid JSON, so the client's parse failed and **every `read` tool invocation failed**. Each delta now carries a monotonically increasing per-turn index (`ToolCallIndexer`), mirroring the block index the Anthropic path already assigned. Verified live: four parallel `read` calls arrive as indexes 0..3, each a well-formed argument object.
- **A synthetic 429 no longer cools a healthy account.** When the agent produces zero tokens and the account's measured quota is still above `accounts.min_remaining_quota_pct`, the failover coordinator refuses the turn without marking the account exhausted for five hours.
- **Historical tool calls now carry their id, so the agent can tell executed calls from proposed ones.** The flattened transcript rendered every assistant tool call as `[Assistant called tool X with arguments: …]` with no id, while the result line was `Tool Output (id: call_…): …`. In a real 447-message Hermes session that produced 222 call lines with 0 ids against 220 distinct result ids with 0 references: no call could be linked to its result, so the model re-issued probes it had already run. Each call now renders as `[Assistant called tool X (id: call_…) with arguments: …]`, matching its result line. Measured on that same history: 222 call ids, 222 result ids, 222 matched, 0 orphans.
- **Conversation history is bounded, which stops the agent from looping on long sessions.** Nothing limited how much history reached `agy`; the 447-message session was flattened into one 625 KB prompt block and forwarded in full (153,793 prompt tokens). Above roughly 100K tokens the agent stops converging and re-issues the same tool call indefinitely. `execution.max_history_tokens` (`AGY_GW_MAX_HISTORY_TOKENS`, default `60000` estimated tokens, `0` disables) now drops the middle turns beyond the budget while keeping the opening turn and the newest instructions and file contents, rendering the same session at 60,008 tokens. A live replay of the bounded render with `gemini-3.8-flash-low` converged in 10.5 s with a single sensible tool call, where the unbounded history looped.

## [3.16.2] - 2026-11-26

### Bug Fixes

* **release:** strip any heading level the tag action emits ([2e9a5b4](https://github.com/shukoilab/agy-gateway/commit/2e9a5b4a955b73da86fff2e339c78bce9bd03a73))

## [3.16.1] - 2026-11-26

### Bug Fixes

* **release:** stop nesting the changelog heading on release ([3567ff3](https://github.com/shukoilab/agy-gateway/commit/3567ff39d0c69d0dbd2257cf16db8b1e15343e42))

## [3.16.0] - 2026-11-26

### Features

* **release:** enforce single-source version synchronization ([8c4b276](https://github.com/shukoilab/agy-gateway/commit/8c4b276d187ab810484c3e40e46d469cff24ca5a))


### Bug Fixes

* **release:** align version with v3.15.1 and let CI validate release commits ([178b321](https://github.com/shukoilab/agy-gateway/commit/178b321fc661cbbee2aad1991d03bfb97ef2dd02))

## [3.13.0] - 2026-11-26

### Changed
- **YOLO Mode Is Now a Product Invariant (`internal/acp`, `internal/pool`)**:
  - The agent is always launched as `agy acp --dangerously-skip-permissions`, sourced from a single
    `acp.SpawnArgs()` function. No environment variable, YAML key, or request field can alter it.
  - Verified against a live agent: the flag alone yields zero permission requests and unblocked file
    CRUD, so auto-approval no longer needs a policy switch.
  - Removed `transport.autoApprove`, `SetAutoApprovePermissions`, `AutoApprovePermissions`, and the
    deny branch that answered `cancelled` + `session/cancel`. A permission request is now always
    answered with the highest-priority `allow*` option, so no configuration can stall a turn.

### Removed
- **Configuration knobs** (stale files still boot; each present key logs one warning):
  `execution.acp_session_mode` / `AGY_GW_ACP_SESSION_MODE`, `execution.auto_approve_permissions` /
  `AGY_GW_AUTO_APPROVE_PERMISSIONS`, `execution.system_prompt_channel` /
  `AGY_GW_SYSTEM_PROMPT_CHANNEL`, and `binaries.agy_acp_args` / `AGY_GW_AGY_ACP_ARGS`.
- **Hardcoded prompt injection**: `DefaultExecutionProtocol` and the literal
  `"AUTHORITATIVE SYSTEM INSTRUCTIONS ..."` banner. An absent consumer prompt now means an absent
  `_meta`, not a gateway-authored persona.
- **Dead delivery and mode surface**: the spec-level `systemPrompt` field (agy accepts and silently
  discards it), the `standard`/`vendor`/`both` channel resolution, the plan/build prompt-fold
  fallback, the session-mode config push, `SessionModeState`/`SessionMode`, and four capability
  flags that no longer have a consumer.
- A duplicate `AGY_GW_CLEAN_WORKSPACE` read that shadowed the effective handler.

### Fixed
- `TestSpawnProcess_TerminateReapsGroup` raced the kernel: `SIGTERM` is delivered asynchronously, so
  the process-group liveness probe could observe a backgrounded child milliseconds after reaping.
  The assertion now polls briefly instead of racing, which removes a pre-existing full-suite flake.

### Added
- `tests/e2e_yolo_mode_test.py`: an end-to-end suite on both `/v1/chat/completions` and
  `/v1/messages` using `gemini-3.8-flash` at low effort. Turn 1 declares a read-only plan directive
  and must emit no write tool call and leave no file; turn 2 declares a build directive and must
  create the file, with a per-turn stall detector; turns 3-4 read the file back through the client
  tool and assert the model echoes the exact contents from a follow-up request that carries the tool
  result.

---

## [3.12.0] - 2026-11-25

### Added
- **Default Execution Protocol & Dynamic System Prompt Mode (`internal/acp`, `internal/server`)**:
  - Engineered native ACP v1 `SystemPrompt` schema with polymorphic `mode` (`"override"` vs `"append"`).
  - Implemented immutable `DefaultExecutionProtocol` enforcing strict language mirroring, first-principles analysis, high-signal directness, and Chain-of-Verification (CoVe) checklist when no consumer system prompt is provided.
  - Automatically preserves consumer-provided system prompts seamlessly using `mode: "append"`.
- **Full Canonical ACP v1 Protocol Alignment (`internal/acp`, `internal/pool`)**:
  - Aligned JSON-RPC error codes with official ACP v1 spec (`-32700`, `-32600`, `-32601`, `-32602`, `-32603`, `-32800`, `-32000`, `-32002`).
  - Added strongly-typed ACP structures: `EnvVariable` (`name`/`value`), multi-transport `MCPServerInfo` (`stdio`, `http`, `sse`), `UsageUpdate`, `UsageCost`, and `PermissionOutcome` (`optionId`).
  - Refactored `Worker` client capabilities handshake: set `terminal: false` boolean, removed non-standard capabilities root.
- **In-Memory Loopback MCP Server Engine (`internal/mcp`, `cmd/agy-gateway`)**:
  - Engineered `LoopbackServer` providing in-memory session registration, JSON-RPC 2.0 lifecycle, and stdio bridging (`mcp-bridge`).
  - Native ACP MCP injection via `mcpServers` parameter in `session/new`, completely eliminating simulated `<tool_call>` prompt injection.
  - Implemented `MatchAndNormalizeToolName` for canonical tool mapping (`mcp_client-tools_write_file` -> `write_file`) and transparent filtering of internal agent scaffolding tools (`update_steps`, `update_focus`).
- **Centralized Semantic Versioning (`internal/version`)**:
  - Introduced `internal/version` package providing a single source of truth for runtime semver (`3.12.0`).
  - Added build-time dynamic injection support via `-ldflags="-X agy-gateway/internal/version.Version=..."`.
  - Harmonized all User-Agent headers, Docker configurations, ACP `initialize` handshakes, and Swagger schemas to `3.12.0`.

### Fixed
- **Graceful ACP Cancellation & Logging Disambiguation (`internal/server`, `internal/acp`)**:
  - Aligned turn cancellation with official ACP v1 (`schema-v1.26.0`): sending `session/cancel` notification allows `session/prompt` to return its standard JSON-RPC response (`stopReason: "cancelled"`).
  - Eliminated premature context cancellation causing misleading `"openai non-streaming turn unblocked cleanly upon early termination"` and `"unmatched response ID received"` logs.
  - Resolved non-reentrant mutex deadlock in Anthropic streaming tool calling.

---

## [3.9.4] - 2026-11-24

### Added
- **Polymorphic ACP Content Chunk Unmarshaling (`internal/acp`)**:
  - Implemented custom `UnmarshalJSON` on `ContentChunk` supporting single JSON objects (`{"type":"text","text":"..."}`), JSON arrays of objects (`[{"type":"text","text":"..."}, ...]`), arrays of raw strings (`["..."]`), and primitive JSON strings.
  - Flattens and deterministically concatenates multi-chunk text payloads, eliminating all `json: cannot unmarshal array into Go struct field SessionUpdate.update.content of type acp.ContentChunk` deserialization failures.
  - Added full test coverage in `internal/acp/protocol_test.go` (`TestSessionUpdate_PolymorphicContentChunk`).
- **Comprehensive Debug Observability (`internal/acp`, `internal/server`)**:
  - Added structured debug logging when `AGY_GW_LOG_LEVEL=debug` for incoming ACP notifications (`session/update`, `session/request_permission`) with raw parameter size and parsed chunk types.
  - Added detailed request metrics logging across OpenAI and Anthropic handlers (translated prompt blocks, system prompt length, time-to-first-token latency, and detected tool calls).
- **Enterprise Concurrent Multi-Adapter Stress Test Suite (`tests/e2e_stress_test.py`)**:
  - Implemented high-concurrency multi-adapter test runner (`ThreadPoolExecutor`) testing parallel autonomous sessions simultaneously across both OpenAI (`/v1/chat/completions`) and Anthropic (`/v1/messages`) protocols in both streaming and non-streaming modes.
  - Validates isolated per-worker project workspaces (`/tmp/stress-workspaces/worker_*`), multi-turn tool calling, assertion execution via bash, zero turn header leakage, and sustained container health under load.
- **Automated End-to-End Code Editor Verification Suite (`tests/e2e_code_editor_test.py`)**:
  - Built an autonomous multi-turn test runner operating on `/tmp/test-project` that exercises basic code editor tools (`write_file`, `read_file`, `edit_file`, and `bash`).
  - Verifies multi-turn tool calling, disk state mutation, assertion execution, absence of turn header leakage, and clean zero-error gateway streaming.

### Fixed
- **Autonomous Multi-Turn Hallucination Elimination & Real-Time Stop Sequence Truncation (`internal/openai`, `internal/anthropic`, `internal/mcp`)**:
  - Eliminated autonomous multi-turn simulation loops where models simulated subsequent user turns and tool outputs (`User: [Tool Execution Results]\nTool Output (id: ...): ...`) within a single API request stream.
  - Hardened system prompt tool envelope directives in `VirtualAdapter.BuildToolsPromptEnvelope` and `BuildActiveContinuationCue` with strict invariants: one turn per request, absolute prohibition on simulating tool execution results or user messages, and forbidden transcript labels.
  - Implemented real-time stop sequence detection and stream truncation in `StreamParser` across both OpenAI and Anthropic adapters: as soon as the model attempts to generate `User: [Tool Execution Results]`, `Tool Output:`, or `\nUser:` after emitting tool calls, the stream is truncated cleanly at the turn boundary and `IsTerminated()` prevents further processing.
  - Added line-level `sanitizeTextContent` filtering to strip any residual internal gateway markers from emitted text deltas.
  - Enhanced `ParseCompleteResponse` for non-streaming mode to truncate at the first hallucinated turn marker after tool calls and sanitize output text.
  - Added regression test suites in both adapters (`TestStreamParser_StripHallucinatedPrefix/Stop_sequence_triggers_on_simulated_turn_after_tool_call`, `TestParseCompleteResponse_TruncateSimulatedTurn`).
- **Multi-Turn Tool Turn Deduplication & Continuation Sanitization (`internal/openai`, `internal/anthropic`, `internal/mcp`)**:
  - Implemented `cleanToolOutputContent` / `cleanAnthropicToolContent` in request translators to strip redundant `[Tool Execution Results]` and `User: [Tool Execution Results]` prefixes from incoming messages.
  - Guarded against consecutive duplicate `User: [Tool Execution Results]` turn headers, completely preventing echo loops.
  - Clarified continuation cue directives in `VirtualAdapter.BuildActiveContinuationCue` to explicitly instruct the model: `Do not echo prompt headers, role markers, or transcript labels.`
- **Defensive Stream Parser Role Prefix Stripping (`internal/openai`, `internal/anthropic`)**:
  - Enhanced `StreamParser` in both OpenAI and Anthropic streaming pipelines to detect and strip leading hallucinated turn markers (`Assistant:`, `User: [Tool Execution Results]`) from the initial text delta, preventing model-simulated turn labels from leaking into client agent harness UIs.
  - Added unit test suites verifying full and chunk-split prefix stripping (`TestStreamParser_StripHallucinatedPrefix`).

---

## [3.9.3] - 2026-11-24

### Added
- **Native ACP `systemPrompt.append` Protocol Refactoring (`internal/openai`, `internal/anthropic`, `internal/server`)**:
  - Leveraged native Google Antigravity ACP capability (`agentCapabilities._meta.antigravity.systemPrompt.append`) by forwarding consumer system instructions and tool schemas via `_meta.antigravity.systemPrompt.append` in `session/new`.
  - Replaced artificial prompt-level `<important>` wrapper injection with native upstream system prompt appending, eliminating Claude Sonnet prompt-injection heuristic false positives and ensuring universal compliance across all agent harnesses.

### Fixed
- **Anthropic Streaming Tool-Calling Wire Specification Compliance (`internal/server`, `internal/anthropic`)**:
  - Ensured `content_block_start` for `tool_use` emits an empty input object (`input: {}`) per Anthropic's official streaming specification, delivering arguments exclusively via `input_json_delta`. Completely resolves duplicated argument concatenation (`{"command":"..."}{"command":"..."}`) in OpenCode and Anthropic SDKs.
  - Implemented polymorphic string/object normalization in `internal/anthropic/parser_stream.go` (`normalizeToolInput`), seamlessly supporting both raw JSON objects and stringified JSON payloads.
- **Docker Compose `.env` Dynamic Configuration Ingestion & Duration Parsing Hardening (`docker-compose.yml`, `internal/config`)**:
  - Added `env_file: .env` to `docker-compose.yml` (and `deploy/docker-compose.yml`), ensuring all 12-factor `.env` variables (`AGY_GW_QUOTA_POLL_INTERVAL`, `AGY_GW_POOL_INFERENCE_TIMEOUT`, `AGY_GW_MIN_REMAINING_QUOTA_PCT`, etc.) are automatically injected into the container.
  - Implemented `parseDurationEnv` supporting both standard duration strings (`1800s`, `30m`) and raw integer/float numeric values (`1800`, `60`).
  - Added missing environment variable bindings for `AGY_GW_UNSUPPORTED_PARAM_POLICY`, `AGY_GW_ALLOW_REMOTE_IMAGE_URLS`, `AGY_GW_CLEAN_WORKSPACE`, `AGY_GW_DEFAULT_COOLDOWN_5H`, and `AGY_GW_DEFAULT_COOLDOWN_WEEKLY`.
- **Universal Streaming Parser Tag Fragmentation Protection (`internal/openai`, `internal/anthropic`)**:
  - Implemented closing tag (`</tool_call>`) prefix buffering in `StreamParser.ParseChunk`, preventing partial closing tags split across network chunks from getting swallowed into the tool JSON buffer.
  - Implemented resilient trailing tag recovery in `Flush()`, preventing valid tool calls from leaking as raw plain text into `textDelta` when streams terminate abruptly.
  - Added full-offset boundary testing (`TestStreamParser_SplitCloseTagAllOffsets`) and byte-by-byte streaming tests across both OpenAI and Anthropic adapters.
- **ACP Session Lifecycle Hardening & Complete Zero-Garbage Purge (`internal/acp`, `internal/pool`, `internal/server`)**:
  - Implemented automatic JIT session cleanup via standard ACP `session/close` across all sync and streaming handlers upon request completion.
  - Extended worker workspace cleanup (`CleanWorkspace`) to comprehensively purge ephemeral internal ACP session SQLite databases (`conversations/*`, including `.db`, `.db-shm`, and `.db-wal` write-ahead logs) and brain artifacts (`brain/*`), completely eliminating disk storage leakage across high-throughput agent runs.
- **Client Workspace & Runtime Host Clarification Directive (`internal/mcp`)**:
  - Hardened virtual envelope prompt instructions to clearly state that the engine is connected via an API gateway to an external client workspace, establishing clear host runtime precedence without triggering safety or prompt-injection heuristics.

---

## [3.9.0] - 2026-11-24

### Added
- **Universal Autonomous Agent Anti-Stall & Continuous Execution Engine (`internal/mcp`, `internal/openai`, `internal/anthropic`, `internal/server`)**:
  - **Top-Level Tool Schema Placement**: Relocated tool calling directives and complete JSON schemas into the system `<important>` XML envelope at the beginning of the prompt context, preventing models (e.g. Gemini 3.8 Flash) from misinterpreting buried schema dumps as static chat logs.
  - **Conversational Turn Symmetry for Tool Outputs**: Reconstituted tool messages under structured user turns (`User: [Tool Execution Results]\nTool Output...`) to ensure standard multi-turn symmetry across consecutive tool execution batches.
  - **Active Continuation Priming (`BuildActiveContinuationCue`)**: Injected an active execution loop directive and primed `Assistant: ` turn continuation whenever conversational history terminates with tool outputs, preventing autonomous agent loops (OpenCode, Claude Code, Cursor, Cline, Roo Code, Continue.dev) from stalling into idle state.
  - **Streaming Parser Buffer Recovery & Flush (`Flush()`)**: Added thread-safe buffer flushing in `StreamParser` across both OpenAI and Anthropic adapters, guaranteeing recovery of unclosed tool tags and zero dropped tokens upon stream termination.
  - **Flexible Whitespace Delimiter Parsing**: Upgraded regex pattern matchers (`<%s\s*>...`) in `ParseCompleteResponse` to tolerate varying whitespace within delimiter tags.
  - **Polymorphic Tool Argument Parsing**: Enhanced OpenAI streaming parser to handle both raw JSON objects and stringified JSON payloads without double-escaping arguments.
  - **Streaming Termination Invariant Enforcement**: Added secondary verification of full collected text in streaming handlers to ensure `finish_reason: "tool_calls"` / `"tool_use"` is guaranteed.

---

## [3.5.0] - 2026-11-24

### Added
- **Autonomous Background Auto-Updater Engine (`internal/updater`)**:
  - Background cron-driven scheduler with configurable check intervals (`AGY_GW_AUTO_UPDATE_INTERVAL`, default: `6h`) and optional startup verification (`AGY_GW_AUTO_UPDATE_ON_STARTUP`).
  - Native release manifest resolution for **Google Antigravity CLI (`agy`)** via Google Cloud Run endpoints, supporting streaming `.tar.gz` extraction, decompression bomb protection (500MB threshold), and SHA-512 cryptographic verification.
  - Native release resolution for **`@fikrilal/agy-auth`** multi-account credential manager via GitHub Releases API with automated fallback to the npm registry (`@fikrilal/agy-auth/latest`) and SHA-256 verification.
  - Non-root atomic binary installation in Docker via volume-backed `/var/lib/agy-gateway/bin` prepended to runtime `$PATH`, preventing Linux `ETXTBSY` ("text file busy") collisions while preserving immutable baked binaries as safe fallbacks.
  - Zero-downtime rolling worker pool reload mechanism via `worker.TaintedForRecycle` and `pool.MarkAllForRecycle()`, allowing in-flight streaming requests to finish uninterrupted while respawning updated subprocesses upon release.
  - Administrative REST endpoints:
    - `GET /admin/v1/updater/status` (telemetry, versions, binary paths, next cron schedule).
    - `POST /admin/v1/updater/check` (on-demand upstream check and binary upgrade).
  - CLI subcommand integration:
    - `agy-gateway update status [--json]`
    - `agy-gateway update check [--json]`
  - Interactive terminal dashboard integration (Option `[11] Auto-Updater & Binary Versions`).

- **Outbound HTTP/HTTPS Forward Proxy Support**:
  - Full compatibility with Docker client proxy configurations (`~/.docker/config.json`) and 12-factor environment variables (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, `AGY_GW_PROXY_*`).
  - Automatic injection of proxy environment configurations into child processes (`agy acp`, `agy-auth`), with automatic credentials masking (`user:*****@host:port`) in logs.
  - Automated loopback protection ensuring `localhost`, `127.0.0.1`, and `::1` remain exempt to preserve local container healthchecks (`/healthz`).

- **Anthropic Wire Protocol Parity (`/v1/messages`)**:
  - Full support for Anthropic Messages API v1 with dual authentication headers (`x-api-key` and `Authorization: Bearer`).
  - Polymorphic content block parsing (text, reasoning, tool calls).
  - 7-stage Server-Sent Events (SSE) streaming engine compliant with official Anthropic SDK specifications.
  - Real-time thinking and reasoning blocks extraction with cryptographic signatures.

- **Dynamic Model Discovery, Ranking & Runtime Aliasing**:
  - Real-time model enumeration dynamically querying connected accounts.
  - Live scoring and capability ranking integrated with [models.dev](https://models.dev) to elect family champions (Sonnet, Haiku, Opus, Flash).
  - Administrative REST API for manual alias overrides (`/admin/v1/models/anthropic/aliases/*`) and on-demand synchronization (`/admin/v1/models/anthropic/sync`).

- **Interactive Account & Model Management CLI**:
  - Terminal User Interface (`agy-accounts` / `agy-gateway accounts`) featuring interactive menus for account onboarding, quota telemetry inspection, switching, rotation, and model aliasing.

- **OpenAPI 3.0 / Swagger UI Integration**:
  - Interactive API documentation available at `/swagger/index.html` and `/docs` with dynamic host detection and bearer authentication support.

### Changed
- Refactored Docker deployment to isolate writable runtime binaries in `/var/lib/agy-gateway/bin` under UID `10001` (`agygw`).
- Enhanced HTTP client connection management to guarantee explicit stream closures on non-200 responses, mitigating socket leakage.

### Fixed
- Fixed Swagger UI authentication header parsing to accept raw API keys without mandatory `Bearer ` prefixes from browser inputs.
- Resolved race conditions in multi-account worker pool turnover during rate-limit (`429`) failovers.

---

## [3.4.0] - 2026-11-20

### Added
- OpenAI Chat Completions API (`/v1/chat/completions`) streaming and non-streaming support.
- Rolling 5-hour and weekly quota bucket monitoring via `agy /usage`.
- Headless Google OAuth device code onboarding flow.
- Process group supervision preventing zombie child processes upon container shutdown (`SIGTERM`).
