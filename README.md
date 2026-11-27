# agy-gateway

<p align="center">
  <a href="https://github.com/shukoilab/agy-gateway/actions/workflows/ci-release.yml"><img src="https://github.com/shukoilab/agy-gateway/actions/workflows/ci-release.yml/badge.svg" alt="CI Pipeline"></a>
  <a href="https://golang.org"><img src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go" alt="Go Version"></a>
  <a href="https://www.docker.com/"><img src="https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker" alt="Docker Ready"></a>
  <a href="https://github.com/shukoilab/agy-gateway/pkgs/container/agy-gateway"><img src="https://img.shields.io/badge/GHCR-Package-blue?logo=github" alt="GHCR"></a>
  <a href="./LICENSE"><img src="https://img.shields.io/badge/License-MIT-green.svg" alt="License: MIT"></a>
  <a href="https://github.com/shukoilab/agy-gateway/releases"><img src="https://img.shields.io/badge/Release-v3.16.8-purple.svg" alt="Release"></a>
</p>

<p align="center">
  <strong>An experimental research gateway and protocol bridge connecting OpenAI and Anthropic client applications to Google Antigravity CLI via standard Agent Client Protocol (ACP).</strong>
</p>

---

## Table of Contents

- [1. Introduction & Research Motivation](#1-introduction--research-motivation)
  - [The Problem with Conventional Bridges](#the-problem-with-conventional-bridges)
  - [The Architectural Solution](#the-architectural-solution)
- [2. Key Features](#2-key-features)
- [3. Installation & Getting Started](#3-installation--getting-started)
  - [Method 1: Docker Compose (Recommended)](#method-1-docker-compose-recommended)
  - [Method 2: Prebuilt Container Image (GHCR)](#method-2-prebuilt-container-image-ghcr)
  - [Method 3: Build from Source](#method-3-build-from-source)
- [4. Configuration & Environment Variables](#4-configuration--environment-variables)
  - [Configuration File (`config.yaml`)](#configuration-file-configyaml)
  - [Environment Variables (`.env`)](#environment-variables-env)
  - [Optional Outbound HTTP/HTTPS Proxy](#optional-outbound-httphttps-proxy)
  - [Automated Background Auto-Updater](#automated-background-auto-updater)
  - [Interactive Account Management CLI](#interactive-account-management-cli)
  - [Connecting Client Applications](#connecting-client-applications)
- [5. Contributing & Development Guidelines](#5-contributing--development-guidelines)
- [6. Credits & Acknowledgements](#6-credits--acknowledgements)
- [7. License](#7-license)
- [8. Disclaimer](#8-disclaimer)

---

## 1. Introduction & Research Motivation

`agy-gateway` is an open-source research project and Proof-of-Concept (PoC) exploring non-invasive protocol translation between standard LLM client interfaces—namely the **OpenAI Chat Completions API** and the **Anthropic Messages API**—and Google's developer tools through the **Agent Client Protocol (ACP)** over stdio JSON-RPC 2.0.

### The Problem with Conventional Bridges
A common challenge when bridging developer IDEs and autonomous coding agents (such as Cursor, Roo Code, Cline, Continue.dev, or LiteLLM) to developer ecosystems is integration fragility:
- **Aggressive File Injection**: Many unofficial tools attempt to overwrite internal harness files (such as `GEMINI.md` or `AGENTS.md`) or inject unauthorized instructions into the agent runtime.
- **Undocumented Endpoint Scraping**: Conventional reverse-engineering approaches frequently rely on private web session cookies and fragile web scraping, leading to session invalidations, security triggers, and service disruptions.
- **Account Policy Violations**: Manipulating closed internal endpoints often violates upstream Terms of Service, putting developer credentials at risk.

### The Architectural Solution
This project explores a sustainable, standards-compliant alternative:
1. **Standard ACP Emulation**: The gateway emulates a native code editor client (such as Zed) communicating directly with the official Google Antigravity CLI (`agy acp`) using standard JSON-RPC 2.0. The underlying CLI runtime operates in its natural state without file injection or binary tampering.
2. **Native ACP System Prompt Delivery**: Rather than altering core system files or scraping undocumented endpoints, developer instructions travel upstream as a standard ACP prompt block in `session/prompt`, prepended as the first block of the turn. The gateway deliberately does not use any vendor-specific side channel: `agy` inspects those with a policy guard that answers certain instruction text with a synthetic `429 ResourceExhausted` while emitting zero updates, which hangs the turn for any harness whose persona trips it (observed with Hermes). The block carrier has no such guard, so one code path behaves identically for every client.

The gateway is a **provider endpoint, not an agent harness**: the agent always runs as `agy acp --dangerously-skip-permissions`, so no turn is ever blocked behind an approval prompt, and this posture is not configurable. The consumer system prompt is forwarded verbatim and prepended as the first prompt block — the gateway injects no persona, no default protocol, and no precedence banner, and prepends nothing when the consumer supplies no prompt. Whether a turn is read-only (plan) or write-enabled (build) is therefore decided purely by the consumer system prompt, exactly as with any other model provider.
3. **Multi-Account Resilience**: A centralized vault monitors live quota limits directly via official usage reports (`agy-auth usage --all`), managing 5-hour rolling windows and weekly quota ceilings with proactive standby rotation.
4. **Drop-in Compatibility**: Exposes standards-compliant REST endpoints for both `/v1/chat/completions` and `/v1/messages`, enabling developers to benchmark models like Gemini 3.8 Flash (supporting up to 1M+ token context windows) using existing tooling. Parallel tool calls stay parallel: every streamed call delta carries its own index, so clients that fold same-index deltas into one call (opencode, and anything else following the OpenAI streaming contract) receive N distinct calls instead of one concatenated, unparseable argument blob.
5. **Fidelity-Safe History Rendering**: Conversational history is flattened into that single prompt block because ACP has no structured transcript field, so the rendering must preserve the semantics the model relies on. Every historical assistant tool call carries the same `id` as its matching tool result, which is what lets the agent tell an already-executed call from a proposed one. The rendered history is also bounded by `execution.max_history_tokens` (default `60000` estimated tokens): beyond ~100K tokens `agy` stops converging and re-issues the same tool calls indefinitely. The budget is enforced by compacting the oldest tool results first (head and tail kept, the omitted span marked with its size), so every assistant turn and both halves of every tool call/result pair survive; the four newest results stay verbatim, and whole turns are dropped only when the non-result prose alone exceeds the budget while the opening turn (the task statement) stays pinned. A tenth of the budget is held back because the four-chars-per-token estimate is optimistic for tool output. Dropping turns outright was itself a loop source: in a real 156-turn session it removed 94 assistant turns, including the operating instructions, and the agent re-read the same files indefinitely.

---

## 2. Key Features

- **Dual Wire Protocol Parity**:
  - **OpenAI 2026 Compatible**: Standard `/v1/chat/completions`, `/v1/models`, and `/v1/models/{model}` endpoints with streaming support.
  - **Anthropic Messages Compatible**: Complete `/v1/messages` endpoint supporting dual authentication headers (`x-api-key` and `Authorization: Bearer`), polymorphic message payloads (plain strings and typed content blocks), and full Server-Sent Events (SSE) streaming (`message_start`, `content_block_start`/`delta`/`stop`, `message_delta`, `message_stop`, plus `ping` and `error`).
- **Reasoning & Extended Thinking Streams**:
  - Captures internal reasoning tokens from upstream thinking models and formats them cleanly as `reasoning_content` (OpenAI format) or `thinking` blocks with content-derived cryptographic signatures (Anthropic format), so no two distinct reasoning blocks share a signature.
- **Dynamic Model Discovery & Live Capability Ranking**:
  - Queries available models dynamically across authenticated accounts.
  - Integrates with [models.dev](https://models.dev) to score and rank models by capability, automatically electing "Family Champions" (Sonnet, Haiku, Opus, and Flash) with resilient offline disk caching.
  - Supports administrative runtime model aliasing and manual overrides through dedicated REST endpoints (`/admin/v1/models/anthropic/*`).
- **Client-Side Tool Calling (Function Calling) & Autonomous Anti-Stall**:
  - Tool schemas and execution directives travel in the same prepended prompt block as the rest of the consumer system prompt, so no vendor side channel is involved and no safety filter conflicts arise.
  - Every client parameter is accepted: fields `agy` understands are mapped, and fields with no ACP equivalent are ignored without failing the request.
  - Delimiter-tagged tool calls are parsed incrementally as the turn streams (`StreamTextToolCalls`), with closing-tag prefix buffering and resilient flush recovery, guaranteeing zero unclosed or fragmented tool calls across network chunk boundaries.
- **Live Quota Tracking & Zero-Downtime Failover**:
  - Real-time per-account quota polling via `agy-auth usage --all --json`, tracking remaining percentages across rolling 5-hour and weekly bucket limits for **every** stored account independently.
  - Model-aware eligibility: quota families are tracked separately, so an account with a nearly spent *Claude and GPT models* bucket is still selected for pure Gemini traffic (and vice versa).
  - Deterministic one-account-per-worker credential materialisation: each worker receives its own account's credential, exported per account and installed atomically (`0600`, `fsync` + `rename`), so no two workers ever share a refresh token.
  - Proactive cooldown scheduling derived from the limiting bucket's own reset time, with automatic recovery once the window reopens and seamless standby worker rotation upon rate-limit detection (`429`).
- **Headless OAuth Onboarding & Terminal Dashboard**:
  - Browser-free authentication flow for headless servers and containerized deployments.
  - Interactive terminal dashboard (`agy-accounts` CLI) for simple account switching, quota inspection, and vault maintenance.
- **Optional Outbound HTTP/HTTPS Proxy Support**:
  - Full compatibility with Docker client proxy configurations (`~/.docker/config.json`) and standard `.env` variables (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`).
  - Strict proxy routing for all external egress traffic (`agy acp`, `agy-auth`, and `models.dev`), combined with automatic loopback protection for internal container healthchecks.
- **Automated Background Auto-Updater (Cron Engine)**:
  - Continuous background discovery, cryptographic verification (SHA-512 and SHA-256), and non-root atomic installation for both the official Google Antigravity CLI (`agy`) and `@fikrilal/agy-auth`.
  - Zero-downtime rolling worker pool reload without interrupting active inference streams or client requests.
  - Complete control plane integration via REST endpoints (`/admin/v1/updater/*`), CLI subcommands (`agy-gateway update`), and interactive dashboard.
- **Resource-Safe Architecture & Zero-Garbage Janitor**:
  - Built with Go 1.25, supervised process group lifecycles preventing zombie processes, zero data races, and JIT ACP session teardown (`session/close`) with automatic purging of SQLite databases, WAL journals (`.db-wal`), shared memory (`.db-shm`), and brain artifacts between requests.
  - The per-turn MCP bridge is bound to its loopback session token; once the gateway ends that session the bridge receives `401` and exits immediately instead of retrying forever.
  - Streaming responses commit their HTTP status exactly once, on the first byte actually written, so a pre-content failure still reports a truthful status instead of a `200` followed by a malformed body.

---

## 3. Installation & Getting Started

### Method 1: Docker Compose (Recommended)

The easiest way to deploy `agy-gateway` is using Docker Compose.

#### Step 1: Clone the Repository
```bash
git clone https://github.com/shukoilab/agy-gateway.git
cd agy-gateway
```

#### Step 2: Configure Environment Variables
Copy the sample environment file:
```bash
cp .env.example .env
```
Edit `.env` to configure your API keys, ports, and optional proxy settings:
```bash
# Set your desired gateway authorization keys
AGY_GW_ADMIN_KEY=your-admin-secret-key
AGY_GW_CLIENT_KEY_1=sk-antigravity-dev-test
```

#### Step 3: Start the Gateway
```bash
docker compose up -d
```
A fresh install boots with an **empty vault**, so the container starts and stays up in
onboarding-only mode: the HTTP server, admin API, and `/healthz` are live, `/readyz` reports
`not_ready` with zero workers, and inference answers `429 no_accounts` until an account is added.
That is deliberate — the onboarding CLI runs *inside* this container, so a container that exited
could never be onboarded.

Verify that the process is alive:
```bash
curl -f http://localhost:8080/healthz
# Returns: {"status":"healthy"}
```

#### Step 4: Authenticate Your First Account
Launch the interactive account manager inside the container:
```bash
docker exec -it agy-gateway agy-accounts
```
1. Select option `[2] Add New Account (OAuth Login)`.
2. Open the displayed Google OAuth authorization URL in your browser.
3. Sign in and paste the resulting redirect URL or code back into the terminal.
4. Your account is saved to the encrypted vault and immediately available for inference.

No restart is needed: the pool provisions its workers on the first request after an account
appears, and model discovery re-runs at the same moment. `curl http://localhost:8080/readyz`
flips to `ready` once the workers are warm.

---

### Method 2: Prebuilt Container Image (GHCR)

If you prefer using prebuilt container images without cloning the full repository:

#### Step 1: Pull the Image
```bash
docker pull ghcr.io/shukoilab/agy-gateway:latest
```

#### Step 2: Run the Container
```bash
docker run -d \
  --name agy-gateway \
  -p 8080:8080 \
  -e AGY_GW_ADMIN_KEY="your-admin-secret-key" \
  -e AGY_GW_CLIENT_KEY_1="sk-antigravity-dev-test" \
  -v agy-vault-data:/var/lib/agy-gateway/vault-xdg \
  -v agy-workspaces:/var/lib/agy-gateway/workspaces \
  -v agy-workers:/var/lib/agy-gateway/workers \
  --restart unless-stopped \
  ghcr.io/shukoilab/agy-gateway:latest
```

#### Step 3: Onboard Your Account
```bash
docker exec -it agy-gateway agy-accounts add
```

---

### Method 3: Build from Source

For local development or bare-metal execution:

#### Prerequisites
- **Go**: Version 1.25 or higher
- **Google Antigravity CLI**: `agy` (v1.5.16+ required; the gateway refuses to start below this) installed and accessible in `$PATH`
- **Node.js & Auth Helper**: `@fikrilal/agy-auth` installed globally (`npm install -g @fikrilal/agy-auth`)

#### Step 1: Build the Binaries
```bash
git clone https://github.com/shukoilab/agy-gateway.git
cd agy-gateway

go build -trimpath -ldflags="-s -w" -o bin/agy-gateway ./cmd/agy-gateway
go build -trimpath -ldflags="-s -w" -o bin/agy-accounts ./cmd/agy-accounts
```

#### Step 2: Initialize Configuration
```bash
cp config.yaml config.local.yaml
export AGY_GW_ADMIN_KEY="your-admin-secret-key"
export AGY_GW_CLIENT_KEY_1="sk-antigravity-dev-test"
```

#### Step 3: Run the Server
```bash
./bin/agy-gateway --config config.local.yaml
```

---

## 4. Configuration & Environment Variables

### Configuration File (`config.yaml`)
`config.yaml` manages system defaults, resource allocations, timeout thresholds, and proxy settings. All values can be overridden dynamically using environment variables without modifying the file.

Key sections include:
- `server`: Host address, port, and timeout durations.
- `security`: API key lists, rate limits, and CORS rules.
- `pool`: Worker concurrency limits, TTFT timeouts, and buffer ceilings.
- `accounts`: Quota polling frequency, cooldown intervals, and failover retries.
- `proxy`: Outbound HTTP/HTTPS proxy rules and exclusions.

Settings that were removed from the gateway are never fatal: a stale key in `config.yaml` or a stale `AGY_GW_*` variable logs a single `WARN` naming the key and the reason, then boot continues. Request parameters follow the same philosophy — every field a client sends is accepted, the ones `agy` can honour are mapped, and the rest are ignored.

### Environment Variables (`.env`)
The gateway follows standard 12-factor configuration principles. Key variables:

| Variable | Default | Description |
|---|---|---|
| `AGY_GW_HOST` | `0.0.0.0` | Server listen address |
| `AGY_GW_PORT` | `8080` | Server listen port |
| `AGY_GW_ADMIN_KEY` | - | Bearer token for administrative endpoints |
| `AGY_GW_CLIENT_KEY_1` | - | Client API key for inference requests |
| `AGY_GW_POOL_WORKER_COUNT` | `2` | Number of concurrent Antigravity worker processes |
| `AGY_GW_DEFAULT_MODEL` | `gemini-3.8-flash` | Default model when unspecified by OpenAI clients |
| `AGY_GW_ANTHROPIC_DEFAULT_MODEL` | `claude-3-5-sonnet` | Default model when unspecified by Anthropic clients |
| `HTTP_PROXY` / `HTTPS_PROXY` | - | Optional outbound forward proxy URL |
| `NO_PROXY` | `localhost,127.0.0.1` | Addresses exempt from proxying |
| `AGY_GW_AUTO_UPDATE_ENABLED` | `true` | Master switch for background cron auto-updater |
| `AGY_GW_AUTO_UPDATE_INTERVAL` | `6h` | Cron check duration (`1h`, `6h`, `24h`) |
| `AGY_GW_AUTO_UPDATE_ON_STARTUP` | `true` | Run initial update check immediately on boot |
| `AGY_GW_AUTO_UPDATE_BIN_DIR` | `/var/lib/agy-gateway/bin` | Destination directory for non-root binary installation |
| `AGY_GW_MAX_HISTORY_TOKENS` | `60000` | Estimated-token budget for the conversation history forwarded to `agy` per turn (`0` disables it). Beyond the budget the middle turns are dropped while the opening turn and the newest turns are kept, so long agentic sessions keep converging instead of repeating tool calls. |

### Optional Outbound HTTP/HTTPS Proxy

`agy-gateway` provides complete support for environments requiring egress traffic to traverse an HTTP or HTTPS forward proxy (with optional Basic Authentication).

#### Option A: Docker Client Layer (`~/.docker/config.json`)
You can configure your proxy globally at the Docker level. Add the `proxies` block to your host machine's `~/.docker/config.json`:
```json
{
  "proxies": {
    "default": {
      "httpProxy": "http://user:password@proxy-host:3128",
      "httpsProxy": "http://user:password@proxy-host:3128",
      "noProxy": "localhost,127.0.0.1,::1,agy-net,agy-gateway"
    }
  }
}
```
Docker CLI automatically propagates these proxy variables into the container during startup.

#### Option B: Environment Configuration (`.env`)
Alternatively, define standard proxy variables directly in your deployment `.env`:
```bash
HTTP_PROXY=http://user:password@proxy-host:3128
HTTPS_PROXY=http://user:password@proxy-host:3128
NO_PROXY=localhost,127.0.0.1,::1,agy-net,agy-gateway,.local
```

**Operational Safeguards:**
- **Guaranteed Worker Egress**: Worker subprocesses (`agy acp`, `agy-auth`) automatically inherit the proxy settings, routing external Google API and OAuth traffic via HTTP `CONNECT` tunnels.
- **Loopback Safety**: The gateway automatically ensures `localhost`, `127.0.0.1`, and `::1` are preserved in `NO_PROXY`, preventing internal healthchecks (`/healthz`) and OAuth callbacks from being sent to the proxy.
- **Password Masking**: Credentials inside proxy URLs are masked as `http://user:*****@host:port` in all system logs.

---

### Automated Background Auto-Updater

The gateway includes an autonomous cron engine that keeps the underlying Google Antigravity CLI and `@fikrilal/agy-auth` updated to their latest upstream releases.

#### Key Invariants & Safety Measures:
- **Non-Root Execution**: Downloads are staged into `/var/lib/agy-gateway/bin` (owned by `agygw` UID `10001`), which takes precedence on `PATH` before `/usr/local/bin` while preserving baked binaries as immutable recovery fallbacks.
- **Atomic Swap (`ETXTBSY` Prevention)**: Binaries are downloaded to temporary staging files, verified with SHA-512/SHA-256 and `--version` smoke tests, and atomically swapped via `os.Rename`.
- **Zero-Downtime Worker Recycling**: When a new binary is installed, `pool.MarkAllForRecycle()` gracefully flags existing workers. Active streaming requests run to completion uninterrupted, and recycled workers reload the new executable upon return.

#### Administrative REST Endpoints:
```bash
# Check updater lifecycle, component versions, and next scheduled check
curl -s http://localhost:8080/admin/v1/updater/status \
  -H "Authorization: Bearer your-admin-secret-key"

# Trigger an immediate on-demand check and upgrade
curl -s -X POST http://localhost:8080/admin/v1/updater/check \
  -H "Authorization: Bearer your-admin-secret-key"
```

#### CLI Subcommand:
```bash
# Display aligned status table
docker exec agy-gateway agy-gateway update status

# Output status in JSON format
docker exec agy-gateway agy-gateway update status --json

# Trigger manual update check
docker exec agy-gateway agy-gateway update check
```

---

### Interactive Account Management CLI

The gateway includes an interactive Terminal User Interface (TUI) for account CRUD and health tracking.

Launch the menu:
```bash
# In Docker:
docker exec -it agy-gateway agy-accounts

# Or via host binary:
./bin/agy-accounts
```

**Available Options:**
- `[1] List Accounts & Live Quotas`: Inspect all account IDs, tier types, and 5-hour/weekly limits.
- `[2] Add New Account`: Run the headless Google OAuth login wizard.
- `[3] Switch Active Account`: Change the primary account in the vault.
- `[4] Force Account Rotation`: Manually cycle to the next healthy account.
- `[5] Set Account Alias`: Assign a human-readable name to an account.
- `[7] Remove Account`: Delete an account and purge what the gateway installed for it: the vault entry, the live mirror credential (`agy-auth remove --logout`), and every worker-HOME credential copy. The account set is resynced immediately, so rotation cannot lease the deleted account even before the next poll, and no restart is required.
- `[8] Clean Vault Cache`: Purge orphaned tokens and temporary files.
- `[10] Anthropic Model Aliases & Ranking`: View active model mappings, pin manual overrides, and trigger capability recalculations.
- `[11] Auto-Updater & Binary Versions`: Inspect active binary paths, check upstream releases, and trigger manual upgrades.

**Non-Interactive Scripting Example:**
```bash
# List accounts in aligned table or JSON
docker exec agy-gateway agy-accounts list
docker exec agy-gateway agy-accounts list -json

# Manage model aliases
docker exec agy-gateway agy-gateway models list
docker exec agy-gateway agy-gateway models set claude-3-7-sonnet claude-sonnet-4-6
```

---

### Multi-Account Rotation & Quota Families

The gateway treats each stored account as an independent quota source and binds exactly one account per worker.

**How selection works**

| Stage | Behaviour |
| :--- | :--- |
| Allocation | `allocation_strategy` assigns accounts to workers at startup: `round_robin` (default, spread by index), `least_used` (greedy over live quota — fewest current claims first, ties broken by highest remaining headroom), or `sticky` (persist per-worker affinity in `<base_data_dir>/state/worker-bindings.json`, honouring a binding only while the account still exists, is usable, and is not already claimed by an earlier worker). A corrupt or missing bindings file is a warning and never blocks startup. |
| Credential install | The assigned account's credential is exported per account and installed atomically into that worker's isolated `HOME` (`0600`, `fsync` + `rename`). A failed export leaves the previous credential untouched. |
| Lease-time reconciliation | On every lease the worker's account is re-judged against live quota. An ineligible account is replaced from the vault and the new credential is installed before the turn runs. |
| Polling | `quota_poll_interval` refreshes every account's buckets. A bucket below `min_remaining_quota_pct` moves that account into cooldown until the bucket's **own** reset time. |
| Recovery | A cooldown that has elapsed returns the account to `HEALTHY` on the next poll, with no external trigger. |
| Refusal guard | A `429` from upstream on a turn that produced zero tokens does **not** cool an account whose measured quota is still above `min_remaining_quota_pct`. The refusal is content-level, not quota-level, so cooling would bench a healthy account for nothing. The account stays in service and the turn fails with a server error (`inference_error`) instead of a misleading `all_accounts_cooling`. |

**Quota families are independent.** Google meters *Gemini Models* and *Claude and GPT models* as separate buckets, so eligibility is evaluated only against the family the requested model uses. An account at 1% on the third-party bucket still serves Gemini models at full capacity.

**Capacity guidance.** Keep `pool.worker_count` less than or equal to the number of stored accounts. Two workers sharing one account means two `agy` processes refreshing the same refresh token concurrently, and Google invalidates all but the first — the sharing condition is logged as a `WARN` at startup.

**Key settings** (`config.yaml` → `accounts:` or `AGY_GW_*` environment variables):

| Setting | Env var | Default | Purpose |
| :--- | :--- | :--- | :--- |
| `allocation_strategy` | `AGY_GW_ALLOCATION_STRATEGY` | `round_robin` | Worker-to-account assignment policy: `round_robin`, `least_used`, or `sticky`. |
| `min_remaining_quota_pct` | `AGY_GW_MIN_REMAINING_QUOTA_PCT` | `5.0` | Floor below which a bucket counts as depleted. An account whose quota could not be read ranks **below** a known-good one but stays usable (rank 1), so one failed quota call never takes the pool offline. |
| `auto_rotate_enabled` | `AGY_GW_AUTO_ROTATE_ENABLED` | `true` | Derive cooldowns from live quota. When `false`, quota data is still refreshed but statuses never change. |
| `quota_poll_interval` | `AGY_GW_QUOTA_POLL_INTERVAL` | `30s` | Background poll cadence (minimum `10s`). Each poll also resyncs the account set from the vault, so an account added or removed through the CLI is picked up without a restart. |
| `default_cooldown_5h` | `AGY_GW_DEFAULT_COOLDOWN_5H` | `30m` | Fallback cooldown when a 5-hour bucket reports no reset time. |
| `default_cooldown_weekly` | `AGY_GW_DEFAULT_COOLDOWN_WEEKLY` | `24h` | Fallback cooldown when a weekly bucket reports no reset time. |
| `credential_op_timeout` | `AGY_GW_CREDENTIAL_OP_TIMEOUT` | `20s` | Deadline for one credential export/install, including process-group kill on expiry. |
| `auth_expired_retry_interval` | `AGY_GW_AUTH_EXPIRED_RETRY_INTERVAL` | `5m` | How long a runtime credential fault keeps an account out of service before it is retried. A vault-reported expiry is permanent and needs `agy-auth login`. |

**Client-visible failure modes**

| Condition | HTTP status | Error code | Retry-After |
| :--- | :--- | :--- | :--- |
| Every account cooling or ineligible | `429` | `all_accounts_cooling` | Earliest cooldown expiry |
| Vault holds no accounts | `429` | `no_accounts` | omitted |
| Agent ended the turn with no output | `502` | `empty_completion` | omitted |

The `no_accounts` row is what a fresh install returns: the gateway boots and serves HTTP with an
empty vault instead of exiting, so the container stays up long enough to run `agy-accounts` inside
it. Workers are provisioned and model discovery re-runs on the first request after an account is
added, so no restart is needed.

The last row matters: `agy` reports an internally exhausted quota as a successful `end_turn` with no content. Reporting `200` there would claim that work happened when it did not, so it is surfaced as a retryable `502` instead.

---

### Connecting Client Applications

Configure your favorite development tools and agent frameworks to connect to `agy-gateway`:

#### Cursor / Continue.dev / Cline (OpenAI Mode)
- **Base URL**: `http://localhost:8080/v1`
- **API Key**: Value of your `AGY_GW_CLIENT_KEY_1`
- **Model**: `gemini-3.8-flash` (or `gemini-3.8-flash-high`)

#### Roo Code / Continue.dev / SDKs (Anthropic Mode)
- **Base URL**: `http://localhost:8080` (or `http://localhost:8080/v1`)
- **API Key (`x-api-key`)**: Value of your `AGY_GW_CLIENT_KEY_1`
- **Model**: `claude-3-5-sonnet` (or `claude-3-7-sonnet`)

#### Interactive OpenAPI / Swagger Documentation
Explore live endpoints and schemas in your browser:
```text
http://localhost:8080/swagger/index.html
```

---

## 5. Contributing & Development Guidelines

Contributions, bug reports, and research discussions are welcome. This project is maintained as an open community research effort.

### Development Workflow
1. Fork and clone the repository.
2. Ensure you have **Go 1.25+** installed.
3. Run tests with race detection before submitting changes:
   ```bash
   go test -v -count=1 -race ./...
   ```
4. Verify code formatting and static analysis:
   ```bash
   go fmt ./...
   go vet ./...
   ```
5. Regenerate Swagger schemas if endpoint contracts or docs are modified:
   ```bash
   swag init -g cmd/agy-gateway/main.go -o docs
   ```

### Version Synchronization

The release version lives in exactly one place: `var Version` in `internal/version/version.go`.
`scripts/version.sh` propagates it to every managed location (swag annotation, generated docs,
`deploy/Dockerfile`, both compose files, README badge) and proves consistency.

Releases are automatic: the `CI & Release Pipeline` workflow bumps, commits, tags, and pushes the
image. To keep a local clone in sync after a release, configure rebase-on-pull once per clone and
then verify:

```bash
git config pull.rebase true   # once per clone
git pull
scripts/version.sh verify     # exit 0 means the tree matches the released version
```

`scripts/version.sh current` prints the source-of-truth version; `bump X.Y.Z` rewrites all managed
locations (used by CI, rarely by hand); `verify` is also a blocking CI gate so drift cannot land.

### Contribution Rules
- **No Hardcoded Secrets or Paths**: All configuration must remain 12-factor and environment-driven.
- **Thread Safety & Zero Race Invariant**: All shared state must be protected with appropriate synchronization primitives (`sync.RWMutex`, atomic variables).
- **Graceful Lifecycle Teardown**: Sockets, streams, and subprocess handles must terminate cleanly upon `SIGTERM` or context cancellation.

---

## 6. Credits & Acknowledgements

`agy-gateway` is built upon concepts and protocol standards developed across the developer community:
- **[Google Antigravity CLI](https://antigravity.google)**: The core desktop and CLI developer environment providing ACP support.
- **[@fikrilal/agy-auth](https://github.com/fikrilal/agy-auth)** by [@fikrilal](https://github.com/fikrilal): Essential research foundation for credential vault management, OAuth token flow, and usage telemetry.
- **[Agent Client Protocol (ACP)](https://zed.dev)**: The standardized editor-to-agent protocol developed by the Zed team.
- **[models.dev](https://models.dev)**: Real-time model intelligence and capability indexing catalog.

---

## 7. License

Distributed under the terms of the **MIT License**. See [LICENSE](./LICENSE) for full details.

---

## 8. Disclaimer

This software is an independent open-source research Proof-of-Concept (PoC) developed strictly for educational, experimental, and protocol interoperability research. 

It is **not** an official product of, maintained by, affiliated with, sponsored by, or endorsed by Google LLC, Anthropic PBC, OpenAI Inc., or any of their respective affiliates or subsidiaries. All trademarks, service marks, model names, and registered product references belong exclusively to their respective owners.

The software is provided **"as is"**, without warranty of any kind, express or implied. Users assume sole responsibility for their local configurations, network environments, and compliance with all applicable third-party terms of service and acceptable use policies.
