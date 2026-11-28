# agy-gateway

<p align="center">
  <a href="https://github.com/shukoilab/agy-gateway/actions/workflows/ci-release.yml"><img src="https://github.com/shukoilab/agy-gateway/actions/workflows/ci-release.yml/badge.svg" alt="CI Pipeline"></a>
  <a href="https://golang.org"><img src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go" alt="Go Version"></a>
  <a href="https://www.docker.com/"><img src="https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker" alt="Docker Ready"></a>
  <a href="https://github.com/shukoilab/agy-gateway/pkgs/container/agy-gateway"><img src="https://img.shields.io/badge/GHCR-Package-blue?logo=github" alt="GHCR"></a>
  <a href="./LICENSE"><img src="https://img.shields.io/badge/License-MIT-green.svg" alt="License: MIT"></a>
  <a href="https://github.com/shukoilab/agy-gateway/releases"><img src="https://img.shields.io/badge/Release-v3.17.0-purple.svg" alt="Release"></a>
</p>

<p align="center">
  <strong>A protocol bridge exposing Google Antigravity CLI (<code>agy</code>) as an OpenAI Chat Completions and Anthropic Messages API, over the Agent Client Protocol.</strong>
</p>

---

## Contents

- [1. What this is](#1-what-this-is)
- [2. How it works](#2-how-it-works)
- [3. Install](#3-install)
- [4. Configure](#4-configure)
- [5. Accounts and quota](#5-accounts-and-quota)
- [6. Connect a client](#6-connect-a-client)
- [7. Develop](#7-develop)
- [8. Credits, licence, disclaimer](#8-credits-licence-disclaimer)

---

## 1. What this is

`agy-gateway` is a research bridge. It speaks two well-documented HTTP APIs on the north side
(OpenAI Chat Completions, Anthropic Messages) and the Agent Client Protocol over stdio JSON-RPC 2.0
on the south side, where it drives `agy acp` as a child process.

The design constraint is that **the ACP side must behave exactly like a real editor client**. That
is not cosmetic: `agy` branches on the client's `clientInfo`, on the capabilities it advertises, and
on the `session/new` payload. A bridge that sends a different shape receives different behaviour,
including tools the caller never asked for. [`docs/acp-conformance.md`](docs/acp-conformance.md) is
the pinned reference captured from Zed ↔ `agy` and from direct protocol probes;
`internal/acp/conformance_test.go` asserts the payload against it, so the document cannot drift from
the code.

Two consequences worth stating up front, because they surprise people:

- **ACP exposes only three model knobs**: `model`, `thought_level`, `mode`. Everything else in a
  provider request (`temperature`, `top_p`, `max_tokens`, …) is structurally unexpressible. The
  gateway never silently pretends otherwise — see
  [`unsupported_parameter_policy`](#unsupported_parameter_policy).
- **Images work.** `agy` accepts image content blocks natively; the gateway decodes provider image
  payloads, applies an SSRF policy, and forwards them as real ACP blocks.

---

## 2. How it works

```
OpenAI / Anthropic client
        │  HTTP (SSE or JSON)
        ▼
  agy-gateway                        ┌── internal/acp   typed ACP client, v1
        │  stdio JSON-RPC 2.0        │   initialize / session/* / prompt
        ▼                            │
      agy acp  ──────────────────────┴── fs + terminal handlers (agent → gateway)
        │
        └── client-tools MCP server (gateway → agent), one Unix socket per session
```

Request path, per HTTP call:

1. Translate the provider body into ACP `PromptBlock`s. History is bounded by
   [`max_history_tokens`](#max_history_tokens) — compaction of the oldest tool results first, never
   by deleting whole turns.
2. Acquire a worker from the pool, reconcile its bound account against live quota, materialise the
   credential into that worker's private `HOME`.
3. `session/new` with `cwd`, the caller's `additionalDirectories`, the caller's `mcpServers`
   verbatim, plus one `client-tools` stdio server whose transport is a Unix socket inside the
   worker's `TMPDIR` (dir `0700`, socket `0600`). The socket path *is* the credential; session end
   is delivered as EOF, so a call in flight when the turn ends drains instead of failing auth.
4. `session/prompt`, forwarding every `session/update` to the provider stream, then `session/close`.

State is per-request by default: every HTTP call is a fresh ACP session. That is what makes the
gateway usable from any harness that resends a full transcript, which is all of them.

**Tool calling.** The primary path is the MCP socket bridge: the agent calls
`mcp_client-tools_<name>`, the gateway relays it to the caller as a provider tool call. A
delimiter-tagged text parser exists as a fallback for agents that emit tool calls as prose; it is not
the normal path.

**Delegation directive.** When the request declares client tools and
`execution.strict_client_tool_delegation` is on (default), the gateway prepends a short precedence
block telling the model its workspace is empty and that only `mcp_client-tools_*` tools exist. This
is deliberate: `agy` otherwise begins reaching for built-in container tools that cannot work here. It
is the only text the gateway adds, and it is configurable off.

**The workspace is durable, and that is load-bearing.** Every worker runs in the same configured
directory (`storage.workspace_dir`, default `<workspaces_root_dir>/shared`), and nothing deletes it
between requests. Previously each worker had its own directory and it was wiped on every release,
so any file the agent wrote vanished before it could use it — the agent rewrote it forever, which is
an infinite loop with extra steps. Only the account-owned caches
(`~/.gemini/antigravity-cli/{conversations,brain,annotations}` and
`conversation_summaries.db*`) are cleared, and only when a worker is rebound to a different account.
Clearing the workspace itself is an explicit operator action: `agy-accounts clean --workspace`.

**Per-conversation directories.** If the caller sends a conversation id in a header
(`x-session-affinity`, `X-Session-Id`, `Session-Id`, `x-opencode-session`, `x-opencode-session-id` —
any of which OpenCode, LiteLLM, LangChain-Fireworks, pi and openclaw already send) or as the body field
`prompt_cache_key`, the turn runs in `<workspace_dir>/<sha256 of that id>` instead of the root. The
hash keeps caller input out of the path, two conversations cannot collide, and a harness that sends
nothing behaves exactly as before. The id never influences which account serves the turn.

**Observability.** `/metrics` exposes 17 series: request and stream counts, `acp_sessions_created_total`,
`acp_sessions_closed_total`, `acp_sessions_live` (created − closed, i.e. the leak detector),
prompt started/completed/failed, mean TTFT and turn duration in ms, MCP bridge connects/calls/
rejections, and worker recycles split by reason (tainted / request ceiling / uptime).

---

## 3. Install

Minimum: Go 1.25 (source build only), `agy` ≥ 1.5.16 (the gateway refuses to start below it), and
`agy-auth` (`npm install -g @fikrilal/agy-auth`) for credential management.

### Docker Compose

```bash
git clone https://github.com/shukoilab/agy-gateway.git
cd agy-gateway
cp .env.example .env          # then set AGY_GW_ADMIN_KEY and AGY_GW_CLIENT_KEY_1
docker compose up -d
docker exec -it agy-gateway agy-accounts   # [2] Add New Account → follow the OAuth URL
```

A fresh install boots with an **empty vault** and stays up: HTTP, admin API and `/healthz` are live,
`/readyz` reports `not_ready`, inference answers `429 no_accounts`. That is intentional — the
onboarding CLI runs inside this same container, so a container that exited could never be onboarded.
Workers are provisioned and model discovery re-runs on the first request after an account appears;
`/readyz` flips to `ready` with no restart.

### Prebuilt image

```bash
docker pull ghcr.io/shukoilab/agy-gateway:latest
docker run -d --name agy-gateway -p 8080:8080 \
  -e AGY_GW_ADMIN_KEY=... -e AGY_GW_CLIENT_KEY_1=... \
  -v agy-vault-data:/var/lib/agy-gateway/vault-xdg \
  -v agy-workspaces:/var/lib/agy-gateway/workspaces \
  -v agy-workers:/var/lib/agy-gateway/workers \
  --restart unless-stopped ghcr.io/shukoilab/agy-gateway:latest
docker exec -it agy-gateway agy-accounts add
```

### From source

```bash
go build -trimpath -ldflags="-s -w" -o bin/agy-gateway ./cmd/agy-gateway
go build -trimpath -ldflags="-s -w" -o bin/agy-accounts ./cmd/agy-accounts
./bin/agy-gateway --config config.yaml
```

### Verify

```bash
curl -f localhost:8080/healthz    # {"status":"healthy"}
curl -f localhost:8080/readyz     # {"status":"ready",...} once an account is onboarded
```

---

## 4. Configure

Configuration is layered: `config.yaml` first, then environment variables (`.env` is loaded unless
`AGY_GW_ENV_FILE` points elsewhere), then validation. **Environment always wins.** Every value in
`.env.example` is the actual code default, so the file doubles as the reference table.

A stale key — one removed by an earlier refactor — is never fatal: it logs one `WARN` naming the key
and the reason, and boot continues.

### Endpoints

| Path | Auth | Purpose |
| :--- | :--- | :--- |
| `POST /v1/chat/completions` | client key | OpenAI Chat Completions (JSON or SSE) |
| `GET /v1/models`, `GET /v1/models/{id}` | client key | Model catalogue, including aliases |
| `POST /v1/messages` | client key | Anthropic Messages (JSON or SSE) |
| `GET /healthz`, `GET /readyz` | none | Liveness, and readiness with worker counts |
| `GET /metrics` | none | Prometheus exposition (path configurable) |
| `/admin/v1/*` | admin key | Accounts, quota, vault, Anthropic aliases, updater |
| `/swagger/index.html` | none | Interactive OpenAPI schema |

### `max_history_tokens`

Estimated-token budget for the history forwarded per turn (default `60000`, `0` disables). Before
this was compaction-first, the budget deleted whole turns: a real 156-turn session lost 94 of them,
and the model — having forgotten its own analysis — re-read the same files until the client gave up.
Tool results are now shortened head-and-tail with an omission marker, the opening turn is pinned, and
the four newest results stay verbatim. Turns are dropped only when the prose alone exceeds the budget.

### `unsupported_parameter_policy`

ACP can set `model`, `thought_level` and `mode`. Anything else the caller sends cannot reach the
agent. `ignore` (default) accepts the request and names the offending fields in
`X-Agy-Ignored-Params`; `reject` answers a provider-native `invalid_request_error` naming the first
one. `max_tokens`, `stop`/`stop_sequences` and `thinking` are never reported: they are either
mandatory in the provider schema or enforced inside the gateway.

### Identity and capabilities

The `initialize` payload is configuration, not a literal.

| Setting | Default | Meaning |
| :--- | :--- | :--- |
| `identity.profile` | `zed` | `zed` sends the reference editor fingerprint; `gateway` reports this process instead. |
| `identity.name` / `title` / `version` | `zed` / `Zed` / `1.30.2+…` | Used verbatim. The version is data. |
| `capabilities.profile` | `zed` | `zed` advertises filesystem + terminal; `sandbox` advertises neither. |
| `capabilities.fs` / `terminal` / `elicitation` / `session_metadata` | `true` | Fine-grained overrides. |

Advertising a capability without a matching handler makes the agent issue requests that never get
answered and the turn hangs, so startup validates advertisement against the handler registry and
refuses to boot on a mismatch.

### Images and media

`vision_mode: auto` (default) forwards image blocks; `reject` refuses them with a provider-native
400. Provider payloads (`data:` URIs, `http(s)` URLs, Anthropic base64 or URL sources) are resolved
by one policy layer: scheme allow-list, size cap, fetch timeout, content-type allow-list, redirect
re-validation, and refusal of loopback / private / link-local targets unless
`media.allow_private_hosts` is explicitly enabled.

### Workspace

| Setting | Env | Default | Meaning |
| :--- | :--- | :--- | :--- |
| `storage.workspace_dir` | `AGY_GW_WORKSPACE_DIR` | `<workspaces_root_dir>/shared` | The directory every worker runs in. Durable: never cleared on a request path. Mount it on the host if a harness outside the container must edit the same files. |
| `accounts.allocation_strategy` | `AGY_GW_ALLOCATION_STRATEGY` | `round_robin` | `round_robin` \| `least_used` \| `sticky` — all three now filter by eligibility. |

Bind-mount it in compose so opencode, Hermes, or any editor can do real file CRUD:

```yaml
volumes:
  - ./workspace:/var/lib/agy-gateway/workspaces/shared
```

The directory must lie under `workspaces_root_dir` and must not be inside `workers_root_dir`; startup
validates both, because a workspace inside the workers tree would be destroyed by the purge.

### Model catalog

`/v1/models` is discovered from one borrowed credential (`agy models`, ~2 s, cached in process). Every
account returns the identical catalog, so one is enough; the credential comes from an existing worker
home when one holds a token, otherwise from a scratch home holding the vault's active account. The
gateway does **not** infer tier from a model id: a Free account was measured serving
`claude-opus-4-6-thinking`, so entitlement is whatever upstream accepts. If accounts exist but nothing
could be enumerated, chat answers `503 model_discovery_failed` instead of a misleading
`model_not_found`.

### Plan labels

`plan` is read by a separate command from the vault read and cached. When upstream omits it — it omits
`plan` and `usage` together whenever quota is unavailable — it is rendered `Unknown`, never `Free`.

### Proxy

Set `AGY_GW_PROXY_ENABLED` plus `AGY_GW_PROXY_HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` (or the bare
`HTTP_PROXY` family) and both worker subprocesses inherit it. Loopback addresses are always forced
into `NO_PROXY` so healthchecks and OAuth callbacks never traverse the proxy. Proxy URLs are
validated for `http`/`https` scheme at startup and are never written to logs.

### Auto-updater

Keeps `agy` and `agy-auth` current: staged download, checksum and `--version` verification, atomic
rename, and `pool.MarkAllForRecycle()` so running turns finish while workers reload. Off with
`auto_update.enabled: false`. Status and trigger:

```bash
curl -s -H "Authorization: Bearer $AGY_GW_ADMIN_KEY" localhost:8080/admin/v1/updater/status
curl -s -X POST -H "Authorization: Bearer $AGY_GW_ADMIN_KEY" localhost:8080/admin/v1/updater/check
agy-gateway update status [--json]
```

### Response headers

Facts the agent produces that neither provider schema can express:

| Header | When | Meaning |
| :--- | :--- | :--- |
| `X-Agy-Model` | every response | Upstream model actually used, after alias resolution. |
| `X-Agy-Thought-Level` | reasoning models | Level actually in force. May differ from the request when the model does not offer it; the gateway applies the model default and reports that here instead of failing. |
| `X-Agy-Session-Title` | sync responses | The agent's own conversation title. Streaming cannot carry it — the status line is committed before the agent names the conversation. |
| `X-Agy-Ignored-Params` | when applicable | Parameters the request carried that ACP cannot express. |

### Error surface

Provider-native shapes only; there are no gateway-invented codes.

| Condition | Status | Code |
| :--- | :--- | :--- |
| Vault holds no accounts | `429` | `no_accounts` (`rate_limit_error` for Anthropic) |
| Every account cooling or ineligible | `429` | `all_accounts_cooling`, `Retry-After` set to the earliest reset |
| Unknown model | `404` | `model_not_found` (`not_found_error`) |
| Caller-fixable ACP rejection (`-32602`) | `400` | `invalid_request_error`, `param` set when the fault is model/level-related |
| Agent lacks a required ACP method (`-32601`) | `500` | `api_error` |
| Agent ended the turn with no output | `502` | `empty_completion` |
| Credential needs re-login | `401` | `account_auth_required` |
| Accounts exist but no models could be enumerated | `503` | `model_discovery_failed` |

`empty_completion` matters: `agy` reports an internally exhausted quota as a successful `end_turn`
with no content. Returning `200` there would claim work that did not happen.

---

## 5. Accounts and quota

Each stored account is an independent quota source, and exactly one account is bound per worker.
Keep `pool.worker_count ≤ number of accounts`: two workers on one account refresh the same token
concurrently and Google invalidates all but the first. The condition is logged as a `WARN` at
startup.

**Selection.** `accounts.allocation_strategy` = `round_robin` (default, spread by index),
`least_used` (fewest current claims, ties by highest headroom), or `sticky` (per-worker affinity
persisted in `<base_data_dir>/state/worker-bindings.json`; honoured only while the account still
exists, is usable, and is not already claimed by an earlier worker — a corrupt file is a warning,
never a boot failure).

**Per lease** the bound account is re-judged against live quota; an ineligible one is replaced from
the vault and the new credential installed before the turn runs. **Per poll**
(`accounts.quota_poll_interval`, default `30s`) each account's buckets refresh, and the account set is
resynced from the vault — so an account added or removed through the CLI is picked up without a
restart. A bucket under `min_remaining_quota_pct` cools that account until the bucket's own reset
time, and recovery is automatic.

**Quota families are independent.** Gemini and Claude/GPT buckets are metered separately, so an
account at 1% on the third-party bucket still serves Gemini at full capacity.

**Removing an account** purges the vault entry, the live mirror credential (`agy-auth remove
--logout`) and every worker-HOME credential copy, then resyncs — so rotation cannot lease a deleted
account even before the next poll.

**Refusal guard.** A `429` from upstream on a turn that produced zero tokens does not cool an
account whose measured quota is still healthy. The refusal is content-level, not quota-level.

### `agy-accounts`

Interactive (`docker exec -it agy-gateway agy-accounts`) or scriptable:

```bash
agy-accounts list [-json]
agy-accounts add
agy-accounts remove <id|alias|email>
agy-accounts switch <id|alias|email>
agy-accounts rotate
agy-accounts alias set|clear <query> [alias]
agy-accounts clean [--dry-run] [--workspace]
```

`switch` and `rotate` are honest about their scope: run from the CLI they change the vault and say so,
because that process owns no workers. Through `POST /admin/v1/accounts/{switch,rotate}` the daemon
additionally rebinds every worker, respawns them, and returns the per-worker `from` → `to` transitions
plus an `unchanged` flag.

`clean` removes orphan worker homes (any `w<N>` beyond `worker_count`, including surplus homes left by a
previously larger configuration) and, with `--dry-run`, lists exactly what it would remove. Account
deletion removes the whole worker home that held the deleted credential — token, conversations,
summaries and brain artifacts — regardless of `worker_count`. Only `agy-auth`'s own plaintext
`backups/registry-*.json` metadata is beyond the gateway's reach; the CLI says so rather than pretending
otherwise.

Menu options: `[1]` list + live quotas, `[2]` add (OAuth), `[3]` switch active, `[4]` force rotation,
`[5]` set alias, `[7]` remove, `[8]` clean vault cache, `[10]` Anthropic model aliases and ranking,
`[11]` auto-updater and binary versions.

---

## 6. Connect a client

**OpenAI-compatible** (Cursor, Cline, Continue, LiteLLM, any SDK):

```
Base URL: http://localhost:8080/v1
API key:  $AGY_GW_CLIENT_KEY_1
Model:    gemini-3.8-flash        (or an explicit variant such as gemini-3.8-flash-high)
```

**Anthropic-compatible** (Roo Code, Claude SDKs):

```
Base URL: http://localhost:8080
x-api-key: $AGY_GW_CLIENT_KEY_1
Model:    claude-3-5-sonnet
```

Both accept streaming and non-streaming. Model aliases are managed from the admin API or
`agy-gateway models`. Live schema: `http://localhost:8080/swagger/index.html`.

---

## 7. Develop

```bash
gofmt -l .                                   # must print nothing
go vet ./...
go test -race -count=1 ./...
go build -trimpath -o /dev/null ./cmd/agy-gateway
scripts/version.sh verify                    # version drift is a CI gate
```

Regenerate the OpenAPI schema after changing endpoint contracts:

```bash
swag init -g cmd/agy-gateway/main.go -o docs
```

**Versioning.** The version lives once, in `internal/version/version.go::Version`.
`scripts/version.sh` propagates it to every managed location (swag annotation, generated docs,
`deploy/Dockerfile`, both compose files, README badge) and `verify` proves consistency. Releases are
automated by `CI & Release Pipeline`; keep a clone in sync with `git config pull.rebase true` then
`git pull && scripts/version.sh verify`.

**Invariants we hold the code to:** every shared-state access is synchronised (`RWMutex`, atomics) and
`-race` is clean; every ticker, timer, watcher and drainer stops on all exit paths and goroutine count
is stable across repeated turns; every subprocess is reaped by process group; no hardcoded secrets,
paths or operational values — all configuration is externalised and validated at startup.

---

## 8. Credits, licence, disclaimer

Built on the work of others: **Google Antigravity CLI** (`agy`), **[@fikrilal/agy-auth](https://github.com/fikrilal/agy-auth)** for credential vault and OAuth flow, the **Agent Client Protocol** from the Zed team, and **[models.dev](https://models.dev)** for model capability data.

MIT licensed — see [LICENSE](./LICENSE).

This is an independent research proof-of-concept for protocol interoperability. It is not affiliated
with, endorsed by, or supported by Google LLC, Anthropic PBC, or OpenAI Inc.; all trademarks and model
names belong to their owners. Provided as-is, without warranty: you are responsible for your own
configuration, network environment, and compliance with any third-party terms of service.
