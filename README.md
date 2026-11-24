# agy-gateway

> High-Performance, Enterprise-Grade OpenAI-Compatible Bridge for Google Antigravity CLI (`agy acp`).
>
> Maintained & Developed by **ShukoiLab** ([@shukoilab](https://github.com/shukoilab))

`agy-gateway` is a production-hardened reverse proxy and protocol translator written in Go. It connects OpenAI API consumers (such as code editors, autonomous agents, and LLM applications) directly to Google Antigravity CLI workers via the **Agent Client Protocol (ACP)** over stdio JSON-RPC 2.0.

---

## 🌟 Key Features

- **100% OpenAI Wire Compatibility**: Standard endpoints for `/v1/chat/completions`, `/v1/models`, and `/v1/models/{model}` matching OpenAI 2026 specifications.
- **Strict Google ToS Compliance**:
  - **Zero Harness Injection**: Eliminates aggressive overrides of `GEMINI.md` or `AGENTS.md`. Antigravity's natural built-in customizations and environment remain untouched.
  - **Safe System Prompt Wrapping**: Automatically wraps consumer `system` / `developer` instructions inside natural `<important>...</important>` XML tags in the conversation context.
- **Just-In-Time (JIT) Tool Calling**:
  - Accepts standard OpenAI `tools` definitions (functions) dynamically per turn.
  - Automatically deduplicates functions by name.
  - Translates model output `<tool_call>` envelopes into standard OpenAI `tool_calls` payloads.
  - Handles multi-turn tool output submissions (`role: "tool"`).
- **Dynamic Account Model Discovery**:
  - Real-time model inspection querying `agy models` on provisioned accounts.
  - Automatic inference of base models and reasoning effort presets (`gemini-3.8-flash`, `gemini-3.8-flash-high/medium/low`, `claude-sonnet-4-6`, `claude-opus-4-6-thinking`, `gpt-oss-120b-medium`).
- **Standard ACP Editor Emulation (Zed Protocol)**:
  - Worker subprocesses execute standard `agy acp --dangerously-skip-permissions`.
  - JSON-RPC `initialize` handshake identifies as standard code editor (`zed v0.214.7`) with `fs` and `terminal` capabilities.
- **Live Multi-Account Quota & Vault Management**:
  - Central credential vault managed via `@fikrilal/agy-auth`.
  - Direct live quota tracking via `agy -p "/usage"` reporting exact percentages remaining, rolling 5-hour and weekly bucket limits, and reset timestamps for both Gemini and third-party models.
  - Dedicated admin endpoints (`GET /admin/v1/accounts/quota` and `GET /admin/v1/accounts/quota/{id}`) for instant quota inspection.
  - Automated cooldown tracking (`COOLDOWN_5H` and `COOLDOWN_WEEKLY`) with zero-downtime failover rotation.
  - Headless OAuth onboarding flow (`/admin/v1/accounts/login/init` & `/admin/v1/accounts/login/callback`).
- **Production-Ready Docker Architecture**:
  - Runs as isolated non-root user (`10001:10001`).
  - Memory-backed `tmpfs` mounts (`/tmp:rw,noexec,nosuid,size=512m`).
  - Docker log rotation (`json-file`, `max-size: 50m`, `max-file: 5`, `compress: true`).
  - Proper OS signal handling (`STOPSIGNAL SIGTERM` with 30s graceful drain).

---

## 🏗️ System Architecture

```
[OpenAI Consumer (e.g. Cline, Cursor, Zed, Python, Curl)]
                        │
                        ▼  HTTP REST / SSE Streaming
              ┌──────────────────┐
              │   agy-gateway    │  (Go 1.25, HTTP ServeMux)
              └─────────┬────────┘
                        │
       ┌────────────────┴────────────────┐
       ▼                                 ▼
┌──────────────┐                 ┌──────────────┐
│ Worker 0     │                 │ Worker 1     │
│ [agy acp]    │                 │ [agy acp]    │
│ (Isolated    │                 │ (Isolated    │
│  HOME / CWD) │                 │  HOME / CWD) │
└──────┬───────┘                 └──────┬───────┘
       │                                 │
       └────────────────┬────────────────┘
                        ▼  OAuth2 / Daily Cloud Code API
        [Google Antigravity Backend (Gemini / Claude / GPT)]
```

---

## 🚀 Quick Start (Docker Compose)

### 1. Prerequisites
- Docker Engine 24.0+ & Docker Compose v2.20+
- At least 1 authenticated Google account for Google Antigravity

### 2. Launch the Gateway
Clone the repository and start the container (the Docker build automatically provisions and verifies the official Google Antigravity CLI binary inside the image):

```bash
docker compose up -d --build
```

Alternatively, pull the prebuilt package published by our automated CI/CD pipeline from GitHub Container Registry:

```bash
docker pull ghcr.io/shukoilab/agy-gateway:latest
```

Verify that the service is running and healthy:

```bash
curl -f http://localhost:8080/healthz
# {"status":"healthy"}
```

---

## ⚙️ Configuration (`config.yaml`)

Configuration is externalized via `config.yaml` and supports environment variable overrides:

```yaml
server:
  host: "0.0.0.0"
  port: 8080
  read_header_timeout: "10s"
  idle_timeout: "120s"
  shutdown_drain_timeout: "30s"
  max_request_body_bytes: 10485760 # 10 MiB
  sse_keepalive_interval: "15s"

security:
  api_keys:
    - id: "prod-client-1"
      key_env: "AGY_GW_CLIENT_KEY_1"
      key: "sk-antigravity-dev-test"
      rate_limit_rpm: 120
  admin_api_key_env: "AGY_GW_ADMIN_KEY"
  admin_api_key: "admin-super-secret-key-change-me"
  allow_remote_image_urls: false

binaries:
  agy_path: "/usr/local/bin/agy"
  agy_min_version: "1.5.5"
  agy_auth_path: "/usr/local/bin/agy-auth"
  agy_acp_args:
    - "acp"
    - "--dangerously-skip-permissions"

execution:
  yolo_mode: true
  acp_session_mode: "accept-edits"
  auto_approve_permissions: true
  clean_workspace_between_requests: true

storage:
  base_data_dir: "/var/lib/agy-gateway"
  vault_xdg_config_dir: "/var/lib/agy-gateway/vault-xdg"
  workers_root_dir: "/var/lib/agy-gateway/workers"
  workers_tmp_dir: "/var/lib/agy-gateway/tmp"
  workspaces_root_dir: "/var/lib/agy-gateway/workspaces"

pool:
  worker_count: 2
  acquire_timeout: "15s"
  handshake_timeout: "15s"
  config_switch_timeout: "10s"
  inference_timeout: "300s"
  first_token_timeout: "45s"
  process_kill_grace_period: "5s"
  max_requests_per_worker: 250
  max_worker_uptime: "2h"
  max_scanner_buffer_bytes: 16777216

accounts:
  auto_rotate_enabled: true
  allocation_strategy: "dedicated_per_worker"
  quota_poll_interval: "30s"
  min_remaining_quota_pct: 5.0
  default_cooldown_5h: "30m"
  default_cooldown_weekly: "6h"
  max_failover_retries: 2

admin:
  command_timeout: "15s"
  allow_credential_export: false
  query_validation_regex: "^[a-zA-Z0-9@._:-]{1,128}$"
  alias_validation_regex: "^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$"
  runtime_overrides_file: "/var/lib/agy-gateway/vault-xdg/gateway-runtime-overrides.json"
  oauth_onboarding:
    enabled: true
    max_concurrent_logins: 3
    url_extract_timeout: "10s"
    login_session_ttl: "5m"
    loopback_replay_timeout: "15s"

openai:
  unsupported_param_policy: "strip_and_warn"
  default_model: "gemini-3.8-flash"
  default_reasoning_effort: "medium"
  expose_reasoning_content: true

mcp:
  client_tool_calling:
    enabled: true
    default_parallel: true
    strict_schema_validation: true
    max_tool_argument_bytes: 1048576
    delimiter_tag: "tool_call"

observability:
  log_level: "info"
  log_format: "json"
  metrics_enabled: true
  metrics_path: "/metrics"
```

---

## 📡 API Usage & Examples

### 📖 Interactive Swagger UI & OpenAPI Specification

`agy-gateway` provides an enterprise-grade, fully interactive OpenAPI 2.0 / Swagger documentation UI with built-in client and administrator authentication support:

- **Swagger UI Interactive Interface**: [http://localhost:8080/swagger/index.html](http://localhost:8080/swagger/index.html) (shortcut: [http://localhost:8080/docs](http://localhost:8080/docs))
- **Raw OpenAPI Specification (JSON)**: [http://localhost:8080/swagger/doc.json](http://localhost:8080/swagger/doc.json)

#### 🔐 Interactive Authentication in Swagger UI
Click the **Authorize 🔓** button in the upper-right corner of Swagger UI:
1. **Client Endpoints (`/v1/*`)**: In **`ClientBearerAuth`**, enter `Bearer <your-client-api-key>` (e.g. `Bearer sk-antigravity-dev-test`).
2. **Admin Control Plane (`/admin/v1/*`)**: In **`AdminBearerAuth`**, enter `Bearer <your-admin-api-key>` (e.g. `Bearer agy-admin-secret-key-12345`).
3. Session tokens persist across browser refreshes automatically (`persistAuthorization: true`).

#### 🏷️ Organized Endpoint Categories
- **`OpenAI - Chat Completions`**: Chat completions with streaming SSE, reasoning thoughts (`reasoning_effort` / `reasoning_content`), and function tool calling.
- **`OpenAI - Models`**: Dynamic model cataloging and detailed metadata queries.
- **`Admin - Accounts`**: Credential management, live quota tracking (`agy /usage`), manual switching, and rotation.
- **`Admin - OAuth Onboarding`**: Out-of-band headless Google OAuth2 flow initialization and loopback completion.
- **`Admin - Vault`**: Credential vault status inspection and hygiene / cleanup.
- **`Operations & Telemetry`**: Liveness probe (`/healthz`), readiness probe (`/readyz`), and Prometheus metrics (`/metrics`).

---

### 1. List Available Models (`GET /v1/models`)

```bash
curl -s -H "Authorization: Bearer sk-antigravity-dev-test" \
  http://localhost:8080/v1/models | jq .
```

**Response**:
```json
{
  "object": "list",
  "data": [
    {
      "id": "gemini-3.8-flash",
      "object": "model",
      "created": 1795458000,
      "owned_by": "google"
    },
    {
      "id": "claude-sonnet-4-6",
      "object": "model",
      "created": 1795458000,
      "owned_by": "anthropic"
    }
  ]
}
```

---

### 2. Basic Chat Completion with System Prompt

```bash
curl -s -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-antigravity-dev-test" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.8-flash",
    "messages": [
      {"role": "system", "content": "You are a concise mathematics assistant."},
      {"role": "user", "content": "What is 12 * 12?"}
    ],
    "reasoning_effort": "low"
  }' | jq .
```

---

### 3. JIT Tool Calling (Function Calling)

#### Turn 1: Client sends tool definitions
```bash
curl -s -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-antigravity-dev-test" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.8-flash",
    "messages": [
      {"role": "user", "content": "What is the weather in Tokyo?"}
    ],
    "tools": [
      {
        "type": "function",
        "function": {
          "name": "get_weather",
          "description": "Get current weather for a city",
          "parameters": {
            "type": "object",
            "properties": {
              "city": {"type": "string"}
            },
            "required": ["city"]
          }
        }
      }
    ],
    "tool_choice": "auto"
  }' | jq .
```

**Model Output**:
```json
{
  "id": "chatcmpl-bd648810-766c-",
  "object": "chat.completion",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": null,
        "tool_calls": [
          {
            "id": "call_get_weather_1",
            "type": "function",
            "function": {
              "name": "get_weather",
              "arguments": "{\"city\":\"Tokyo\"}"
            }
          }
        ]
      },
      "finish_reason": "tool_calls"
    }
  ]
}
```

#### Turn 2: Client returns tool execution result
```bash
curl -s -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-antigravity-dev-test" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.8-flash",
    "messages": [
      {"role": "user", "content": "What is the weather in Tokyo?"},
      {
        "role": "assistant",
        "tool_calls": [
          {
            "id": "call_get_weather_1",
            "type": "function",
            "function": {
              "name": "get_weather",
              "arguments": "{\"city\":\"Tokyo\"}"
            }
          }
        ]
      },
      {
        "role": "tool",
        "tool_call_id": "call_get_weather_1",
        "content": "{\"temperature\": \"22°C\", \"condition\": \"Sunny\"}"
      }
    ]
  }' | jq .
```

---

### 4. Server-Sent Events (SSE) Streaming

```bash
curl -N -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-antigravity-dev-test" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.8-flash",
    "messages": [
      {"role": "user", "content": "Count from 1 to 5."}
    ],
    "stream": true,
    "stream_options": {"include_usage": true}
  }'
```

---

### 5. Admin Quota & Account Inspection

#### Check All Accounts Quota (`GET /admin/v1/accounts/quota`):
```bash
curl -s -H "Authorization: Bearer admin-super-secret-key-change-me" \
  http://localhost:8080/admin/v1/accounts/quota | jq .
```

#### Check Specific Account Quota (`GET /admin/v1/accounts/quota/{id}` or `?account=`):
```bash
curl -s -H "Authorization: Bearer admin-super-secret-key-change-me" \
  http://localhost:8080/admin/v1/accounts/quota/acct_2a71cc7452 | jq .
```

**Real Live Response**:
```json
{
  "timestamp": "2026-11-23T19:04:00.050Z",
  "total": 1,
  "accounts": [
    {
      "id": "acct_2a71cc7452",
      "email": "user@gmail.com",
      "alias": "main-account",
      "plan": "Free",
      "active": true,
      "status": "HEALTHY",
      "quota_available": true,
      "groups": [
        {
          "name": "Gemini Models",
          "description": "Models within this group: Gemini 3.8 Flash",
          "buckets": [
            {
              "id": "gemini-weekly",
              "name": "Weekly Limit Remaining",
              "description": "You have used some of your weekly limit, it will fully refresh in 6 days, 20 hours.",
              "window": "weekly",
              "remaining_fraction": 0.9568,
              "remaining_pct": 95.68,
              "reset_time": "2026-11-30T15:46:50Z",
              "is_exhausted": false
            },
            {
              "id": "gemini-5h",
              "name": "Five Hour Limit Remaining",
              "description": "You have used some of your 5-hour limit, it will fully refresh in 23 minutes.",
              "window": "5h",
              "remaining_fraction": 0.985,
              "remaining_pct": 98.5,
              "reset_time": "2026-11-23T22:47:05Z",
              "is_exhausted": false
            }
          ]
        },
        {
          "name": "Claude and GPT models",
          "description": "Models within this group: Claude Sonnet 4.6, Claude Opus 4.6, GPT-OSS 120B",
          "buckets": [
            {
              "id": "3p-weekly",
              "name": "Weekly Limit Remaining",
              "window": "weekly",
              "remaining_fraction": 1.0,
              "remaining_pct": 100,
              "reset_time": "2026-11-30T19:03:59Z",
              "is_exhausted": false
            }
          ]
        }
      ],
      "quotas": {
        "gemini-weekly": {
          "name": "Gemini Models - Weekly Limit Remaining",
          "remaining_pct": 95.68,
          "reset_time": "2026-11-30T15:46:50Z",
          "is_exhausted": false
        },
        "3p-weekly": {
          "name": "Claude and GPT models - Weekly Limit Remaining",
          "remaining_pct": 100,
          "reset_time": "2026-11-30T19:03:59Z",
          "is_exhausted": false
        }
      },
      "overview": "Gemini Models\tWeekly Limit Remaining\t96%\t2026-11-30T15:46:50Z\nClaude and GPT models\tWeekly Limit Remaining\t100%\t2026-11-30T19:03:59Z",
      "last_synced": "2026-11-23T19:04:00.050Z"
    }
  ]
}
```

---

## 🔐 Google Antigravity Quota Best Practices

- **Live Upstream Quota Synchronization**: Quota limits and remaining fractions are queried directly via Antigravity's internal command (`agy -p "/usage" --output-format json`), ensuring 100% ground-truth accuracy.
- **Dual Rolling Windows**: Google Antigravity manages limits across **5-hour rolling windows** (to smooth aggregate traffic) and **weekly tier limits** for both Gemini and third-party models (Claude / GPT).
- **Automated Failover & Cooldown**: When an account reaches exhausted limits (< 5% remaining), the gateway automatically marks it as `COOLDOWN_5H` or `COOLDOWN_WEEKLY` with exact wakeup scheduled at `reset_time`, rotating seamlessly to healthy standby accounts.
- **Request Pacing & Anti-Abuse**: Maintain natural pacing (2-4 seconds) between automated calls to remain within standard Cloud Code editor heuristics.

---

## 💻 Step-by-Step Development Guide

### 1. Prerequisites
- **Go**: Version 1.25 or higher
- **Docker & Docker Compose**: Docker Engine 24.0+ and Compose v2.20+
- **Google Antigravity CLI**: `agy` binary (v1.5.5+) installed and executable in `$PATH`
- **Antigravity Auth Helper**: `@fikrilal/agy-auth` installed globally or in path

### 2. Local Environment Setup
Clone the repository and inspect the default configuration:

```bash
git clone git@github.com:shukoilab/agy-gateway.git
cd agy-gateway

# Copy default config if creating local overrides
cp config.yaml config.local.yaml
```

Set required environment variables (or keep default values for development):

```bash
export AGY_GW_CLIENT_KEY_1="sk-antigravity-dev-test"
export AGY_GW_ADMIN_KEY="admin-super-secret-key-change-me"
```

### 3. Run Automated Tests & Race Detection
Execute the comprehensive test suite with the race detector enabled:

```bash
go test -v -race ./...
```

Verify formatting and linting:

```bash
go fmt ./...
go vet ./...
```

### 4. Generating & Updating Swagger Documentation
Install the `swag` CLI tool:

```bash
go install github.com/swaggo/swag/cmd/swag@latest
```

Whenever you modify route definitions, handler signatures, or request/response structs, regenerate the OpenAPI assets:

```bash
swag init -d cmd/agy-gateway,internal/server,internal/openai,internal/auth,internal/models,internal/telemetry,internal/mcp -g main.go --parseInternal
```

### 5. Running Locally
Run the gateway service directly on your host machine:

```bash
go run ./cmd/agy-gateway
```

The gateway starts on `http://localhost:8080`, and the interactive Swagger UI is available at `http://localhost:8080/swagger/index.html` or `http://localhost:8080/docs`.

### 6. Running with Docker Compose
To run in the production-mode container environment with non-root security and log rotation:

```bash
docker compose up -d --build
docker compose logs -f
```

---

## 🤖 Automated CI/CD & GitHub Container Registry

The repository is equipped with a fully automated, production-grade GitHub Actions pipeline (`.github/workflows/ci-release.yml`):

1. **Continuous Integration Gate (`ci`)**:
   - Runs on every push and pull request to `main`.
   - Verifies Go module dependencies, runs `go vet`, executes the full test suite with data race detection (`go test -v -race ./...`), and tests binary compilation.
2. **Automated Semantic Version Bumping & Git Tagging**:
   - Analyzes commit messages upon push to `main` (supporting `#major`, `#minor`, `#patch` or Conventional Commits like `feat:` / `fix:`).
   - Generates the next semver tag automatically (e.g. `v3.3.1`), creates an annotated Git tag, and generates automated changelogs.
3. **Automated GitHub Release**:
   - Publishes a formal GitHub Release corresponding to the new version tag.
4. **Automated Docker Package Publishing (GHCR)**:
   - Builds the production multi-stage container with target version branding (`-ldflags="-X main.version=${VERSION}"`).
   - Automatically publishes tagged images and `:latest` to GitHub Container Registry:
     ```bash
     docker pull ghcr.io/shukoilab/agy-gateway:latest
     docker pull ghcr.io/shukoilab/agy-gateway:v3.3.1
     ```

---

## 🤝 Open Contribution & Community

We warmly welcome contributions from the community! `agy-gateway` is actively maintained by **ShukoiLab** and open to bug fixes, feature proposals, and architectural enhancements.

### How to Contribute:
1. **Fork the Repository**: Create your personal fork on GitHub.
2. **Create a Feature Branch**:
   ```bash
   git checkout -b feature/amazing-feature
   ```
3. **Adhere to Core Invariants**:
   - **Google ToS Invariant**: Never add external harness prompt overrides (`GEMINI.md`/`AGENTS.md`). Always encapsulate system prompts in `<important>` XML tags.
   - **Zero Hardcoding**: Pass configurations through `config.yaml` or environment variables.
   - **Concurrency Safety**: Guard all shared mutable states with mutexes; ensure clean goroutine lifecycles.
   - **Documentation**: Document modified functions with clear JSDoc / GoDoc comments.
4. **Run Tests**:
   Ensure all tests pass with zero race conditions:
   ```bash
   go test -v -race ./...
   ```
5. **Open a Pull Request**: Submit your PR with a concise description of your changes and verification logs.

---

## 📄 License & Attribution

This project is licensed under the **MIT License** - see the [LICENSE](LICENSE) file for full details.

Developed & Maintained with ❤️ by **[ShukoiLab](https://github.com/shukoilab)**.
