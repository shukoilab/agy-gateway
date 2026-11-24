# Changelog

All notable changes to the `agy-gateway` project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
