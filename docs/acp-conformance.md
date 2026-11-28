# ACP Conformance Reference — Zed ↔ Antigravity ACP (agy)

Status: **evidence-based reference**, not a specification rewrite. Every claim below is
tagged with how it was obtained:

| Tag | Meaning |
| --- | --- |
| `PROVEN-SPY` | Captured on the wire by `acp-spy` between Zed and `agy acp` on this machine |
| `PROVEN-PROBE` | Produced by a direct JSON-RPC probe against `/home/shukoi/.local/bin/agy acp` |
| `PROVEN-ZED-SRC` | Read out of Zed's own source (`crates/agent_servers`, `crates/acp_thread`, `crates/agent_ui`) — this settles **client** behaviour without needing a UI interaction |
| `DOC-DECISION` | Deliberate scope decision for `agy-gateway`, not a protocol fact |
| `UNVERIFIED` | Unused. Every claim in this document is now proven by one of the three methods above. If you add a claim without evidence, tag it and say so expressly. |

Versions captured:

- Zed `1.30.2+stable.398.610ea4b8de1c6e887f03563f067278002f41256e`
- `agy` (Antigravity ACP Server) `1.5.18`
- ACP `protocolVersion: 1`, newline-delimited JSON-RPC 2.0 over stdio

Two evidence lanes are used throughout, and they answer different questions:

- **Agent side** (what `agy` sends/accepts) — settled by direct raw JSON-RPC probes against the
  local `agy acp` binary. No Zed involvement needed.
- **Client side** (what Zed sends and how it renders a response) — settled by reading Zed's
  source. Zed's behaviour is code, not a hidden black box, so no manual UI test is required.

Primary capture: `~/.local/state/acp-spy/acp-20261128T023820Z-58193-41411ac7.jsonl`
(168 records: 1 lifecycle, 23 stdin, 144 stdout) — one session, cases 01–13.
Secondary baseline: `acp-20261128T023820Z`'s earlier 7-record snapshot (Zed → agy direct).

Viewer: `acp-spy-view --from-start --full [--filter-stream stdin|stdout|stderr|lifecycle] [--filter-method SUBSTR] <file>`

---

## 0. Evidence apparatus (how each lane was captured)

### 0.1 Zed → agy (the reference lane)

Zed is configured with `acp-spy` as the agent command:

```json
"agent_servers": {
  "Antigravity": {
    "type": "custom",
    "command": "/home/shukoi/.local/bin/acp-spy",
    "args": ["--", "/home/shukoi/.local/bin/agy", "--dangerously-skip-permissions", "acp"],
    "env": { "ACP_SPY_MAX_LOG_BYTES": "104857600", "ACP_SPY_MAX_BACKUPS": "10",
             "ACP_SPY_FSYNC": "0", "ACP_SPY_RAW_B64": "0",
             "ACP_SPY_REDACT_SECRETS": "0", "ACP_SPY_FAIL_MODE": "open" }
  }
}
```

Logs land in `${XDG_STATE_HOME:-~/.local/state}/acp-spy/` as one JSONL per session plus a
`latest.jsonl` symlink. Read with:

```
acp-spy-view --from-start --full <file>
acp-spy-view --from-start --full --filter-stream stdout --filter-method session/prompt <file>
```

Record shape: line 0 is a `lifecycle/session_start` record (`proxy_pid`, `child_pid`, `argv`,
`resolved_binary`, `cwd`, `config`); every following line is
`{seq, ts_utc, mono_ms, stream: stdin|stdout|stderr, event:"message", byte_len, payload,
rpc:{kind,id,method,acp_session_id,update_type,error_code,error_message}}`.
`rpc.kind ∈ request|response|notification|error|unknown` — this is what makes the file
filterable without parsing `payload`.

### 0.2 agy alone (the agent lane)

No Zed needed. Spawn `agy --dangerously-skip-permissions acp` and write one JSON object per line
to its stdin, reading one per line from stdout. This settled every agent-side schema in this
document, including things Zed never triggers (`session/list` with 170 sessions,
`session/resume`, exact error codes).

Practical notes:

- `agy` never writes to stderr; a silent stream means "no answer", not "crashed".
- Malformed JSON is dropped without a diagnostic, so a probe must always read with a timeout.
- Use a throwaway `cwd` (e.g. `/tmp/acpwork`); agy will happily create files there.

### 0.3 agy-gateway itself (the gap lane)

The gateway spawns `agy` as a child, so its own ACP traffic is invisible unless the child binary
is replaced. The working method:

1. Copy the `acp_spy` package into an image (`FROM agy-gateway:3.16.x`), plus a shim at
   `/spy/agy-spy-shim` that **routes only when an argument equals `acp`** and `exec`s the real
   `/usr/local/bin/agy` otherwise. The pass-through matters: the gateway's version gate runs
   `agy --version`, and proxying that call breaks startup with
   `agent version gate failed … exit status 1`.
2. Point the container at the shim: `AGY_PATH=/spy/agy-spy-shim`, `AGY_SPY_TARGET=/usr/local/bin/agy`,
   `AGY_SPY_LOG_DIR=/spy/logs`. Set `AGY_GW_AUTO_UPDATE_AGY_ENABLED=false` for long captures,
   otherwise the auto-updater can rewrite `agy_path` mid-run.
3. Directory and file modes must be readable by uid 10001 — a `COPY --chmod` on the shim alone
   leaves the Python package unreadable and yields `ModuleNotFoundError: No module named
   'acp_spy.cli'`.
4. `acp_spy` refuses to proxy itself (`MAX_NESTING_DEPTH` default 1, plus a self-resolve check),
   which is what stops spy-inside-spy.

### 0.4 Zed source (the client-behaviour lane)

Zed's client behaviour is readable code, so questions like "does Zed read `models`?" or "what
capabilities does it send?" are answered by grep rather than by a UI experiment. Relevant crates:
`agent_servers` (the v1/v2 dispatcher), `acp_thread` (`acp_thread.rs`, `config_options.rs`),
`agent_ui` (`config_options.rs`, `conversation_view/thread_view.rs`).

---

## 1. Transport

- Single child process spawned by Zed: `agy --dangerously-skip-permissions acp` (`PROVEN-SPY` lifecycle record).
  Without the flag the same command is `agy acp`.
- `argv`, `cwd`, `child_pid`, `proxy_pid` are all recorded in the lifecycle line of the spy log.
- Messages are one JSON object per line, no framing headers.
- Client requests carry `id`; agent notifications do not. Only three shapes occur:
  `request`, `response`, `notification`. Agent→client **requests** occur only for
  `session/request_permission` (`PROVEN-PROBE`); zero appeared in the captured session because
  `--dangerously-skip-permissions` was active.

---

## 2. Method inventory

### 2.1 Client → Agent (`PROVEN-SPY`, all five observed in one session)

| Method | Kind | Count seen |
| --- | --- | --- |
| `initialize` | request | 1 |
| `session/new` | request | 1 |
| `session/set_config_option` | request | 7 |
| `session/prompt` | request | 14 |
| `session/cancel` | **notification** (no `id`) | 1 |

### 2.2 Agent → Client (`PROVEN-PROBE`)

| Method | Notes |
| --- | --- |
| `session/request_permission` | Only emitted without `--dangerously-skip-permissions`, and in practice only for `run_command` (see §10.5). **Out of scope for the gateway** — see §10.5 |

### 2.3 Session lifecycle methods, and who actually calls them

`PROVEN-PROBE` for the agent side, `PROVEN-ZED-SRC` for the client side.

| Method | Agent | Zed calls it? |
| --- | --- | --- |
| `session/new` | ✅ | always, per conversation |
| `session/load` | ✅ replays history | yes when the agent advertises `load_session` |
| `session/resume` | ✅ no replay | yes when the agent advertises `sessionCapabilities.resume`; v2 sends `replayFrom` |
| `session/list` | ✅ ignores every param | yes, from the history picker |
| `session/close` | ✅ returns `{}` | only when registration fails; v2 does **not** close on view release |
| `authenticate` / `logout` | declared | only via the auth UI |

Zed's gating (from source): `load_session` requires `agentCapabilities.loadSession`, otherwise
the client errors with *"Loading sessions is not supported by this agent."*; `resume` requires
`sessionCapabilities.resume`; `close` requires `sessionCapabilities.close`. So a gateway that
declares only `loadSession` gets no resume path at all.

Note that `agy` sends both camelCase and snake_case spellings (`loadSession`/`load_session`,
`sessionCapabilities`/`session_capabilities`). That is a compatibility shim, not something to
imitate; emit one spelling per field.

**Protocol version negotiation (`PROVEN-ZED-SRC`).** Zed tries **v2 first** and only falls back
to v1 when the handshake fails with `UnsupportedVersion` or `InvalidInitializeResponse` — and the
fallback **relaunches a fresh process**. There is also a dedicated `v1_only_mismatch` path
(error code `-32022`, `data.requested == 2`, `data.supported` containing 1 but not 2). A gateway
answering `protocolVersion: 1` works, but costs one extra process spawn per conversation; that is
the price of not implementing v2.

---

## 3. `initialize`

### 3.1 Client → Agent (`PROVEN-SPY`, verbatim)

```json
{
  "jsonrpc": "2.0",
  "id": "a3f2b7bc-dbb6-4c87-8850-2fa105d1ab64",
  "method": "initialize",
  "params": {
    "protocolVersion": 1,
    "clientCapabilities": {
      "fs": { "readTextFile": true, "writeTextFile": true },
      "terminal": true,
      "session": {
        "compaction": {},
        "configOptions": { "boolean": {} },
        "notices": {}
      },
      "auth": { "terminal": true },
      "elicitation": { "form": {}, "url": {} },
      "_meta": { "terminal_output": true, "terminal-auth": true }
    },
    "clientInfo": {
      "name": "zed",
      "title": "Zed",
      "version": "1.30.2+stable.398.610ea4b8de1c6e887f03563f067278002f41256e"
    }
  }
}
```

Notes that matter for compatibility:

- `id` is a **UUID string**, not an integer. A server must echo it back unmodified.
- `protocolVersion` is a **number** (`1`), not a string.
- `fs` and `terminal` capabilities are `PROVEN-ZED-SRC` as well as `PROVEN-SPY`: Zed always
  advertises `fs.readTextFile/writeTextFile = true`, `terminal = true`, `auth.terminal = true`,
  `session.{configOptions.boolean, compaction, notices}`, `elicitation.{form,url}`, plus
  `subagents` and `plan` (`client_capabilities_for_agent`). It adds a model-picker `_meta` key
  **only** when the agent id is Cursor, so an `agy` peer never sees it. (Earlier reports of
  `false` values came from the gateway's own `initialize`, not from Zed — see §14.)
- Because Zed advertises `fs` and `terminal`, it will answer `fs/read_text_file` and
  `fs/write_text_file` requests, plus terminal handlers (`PROVEN-ZED-SRC`). A gateway that wants
  the agent to read the real repository can delegate instead of injecting files.
- No `workspaceFolders` / `rootUri` field: workspace roots arrive via `session/new.cwd` and
  `session/new.additionalDirectories` only.

### 3.2 Agent → Client result (`PROVEN-SPY`, verbatim, whitespace preserved)

```json
{
  "jsonrpc": "2.0",
  "result": {
    "protocolVersion": 1,
    "agentCapabilities": {
      "loadSession": true,
      "load_session": true,
      "promptCapabilities": { "image": true, "audio": false, "embeddedContext": true },
      "sessionCapabilities": {
        "resume": {}, "close": {}, "load": {}, "list": {}, "additionalDirectories": {}
      },
      "session_capabilities": {
        "resume": {}, "close": {}, "load": {}, "list": {}, "additionalDirectories": {}
      },
      "auth": { "logout": {} },
      "_meta": {
        "antigravity": { "tier": true, "systemPrompt": { "append": true } }
      }
    },
    "agentInfo": { "name": "Antigravity ACP Server", "version": "1.5.18" },
    "authMethods": [
      { "id": "oauth-personal",   "name": "Log in with Google",
        "description": "Log in with your Google account" },
      { "id": "oauth-business",   "name": "Log in with Gemini Enterprise",
        "description": "Log in with your Gemini Enterprise account" },
      { "id": "gemini-api-key",   "name": "Gemini API key",
        "description": "Use an API key with Gemini Developer API" },
      { "id": "agent-platform",   "name": "Gemini Enterprise Agent Platform",
        "description": "Use Gemini Enterprise Agent Platform (formerly Vertex AI) with Application Default Credentials" },
      { "id": "terminal-login",   "name": "Log in from the terminal",
        "description": "Log in with your Google account by pasting a code in a terminal; works over SSH",
        "type": "terminal", "args": ["--login"] }
    ]
  },
  "id": "a3f2b7bc-dbb6-4c87-8850-2fa105d1ab64"
}
```

Compatibility facts:

- `sessionCapabilities` is emitted **twice**: camelCase keys and snake_case duplicates. This is
  a compatibility shim in `agy` 1.5.18, not a requirement. Zed reads the camelCase form
  (`agentCapabilities.loadSession`, `sessionCapabilities.*`). Emit one spelling; do not imitate
  the duplication.
- `sessionCapabilities.additionalDirectories: {}` is what makes
  `session/new.additionalDirectories` meaningful (§4.1) — and it is proven functional: a prompt
  that asked for a file under an additional directory was answered correctly from it.
- `_meta.antigravity.tier: true` is a booleans-when-declaring / object-when-instantiating
  pattern: the capability is `true`, the `session/new` result carries the actual
  `{name, id, upgradeUri}` object (§4.2). Zed does not read `_meta.antigravity` at all, so a
  gateway may omit it.
- `authMethods[].type: "terminal"` with `args` is how a login is delegated back to the client.

---

## 4. `session/new`

### 4.1 Client → Agent (`PROVEN-SPY`, verbatim)

```json
{
  "jsonrpc": "2.0",
  "id": "ef8c9931-da96-4ad6-90f8-247facae83be",
  "method": "session/new",
  "params": {
    "cwd": "/media/shukoi/DATA/img/mcps/mcp-web-research",
    "additionalDirectories": [
      "/media/shukoi/DATA/img/command-code-gateway",
      "/media/shukoi/DATA/img/go-projects/agy-gateway",
      "/media/shukoi/DATA/img/stsa",
      "/media/shukoi/DATA/img/python-projects/acp-spy"
    ],
    "mcpServers": [
      { "type": "http", "name": "web-research", "url": "http://localhost:3031/mcp", "headers": [] }
    ]
  }
}
```

Field rules:

- `cwd` is the Zed workspace root (absolute).
- `additionalDirectories` are the extra folders open in the same Zed window. `agy` uses them
  for `read_file` outside `cwd` (case 03 read a file under an additional directory using an
  absolute `Path`).
- `mcpServers[].type` is `"http"` here. `"stdio"` is the other form the protocol defines and the
  one Zed uses for custom/extension context servers. Its exact shape, `PROVEN-ZED-SRC`
  (`mcp_servers_for_project`): `{type:"stdio", name, command, args:[…], env:[{name,value}…]}`.
  `env` is an **array of objects** and may be empty `[]` — never a map. An agent must accept
  both `[]` and populated arrays.
- `headers` is an **array of objects**, and Zed sent it empty (`[]`). A server must accept
  both `[]` and `[{name,value}]`. Zed **drops** an HTTP context server's `timeout` and `oauth`
  settings when serialising — they never reach the agent (`PROVEN-ZED-SRC`).
- Every open Zed project's context servers are sent on **every** `session/new`, `session/load`
  and `session/resume` — not once (`PROVEN-ZED-SRC`).
- No `systemPrompt`, no model, no mode in this request: those come from `_meta`, from
  `set_config_option`, or from the agent's own defaults.

### 4.2 Agent → Client result (`PROVEN-SPY`)

Top-level keys, in order: `sessionId`, `models`, `modes`, `configOptions`, `_meta`.

```json
{
  "sessionId": "24c99bcf-c533-4b62-be15-197a37b1bc86",
  "models": {
    "availableModels": [
      { "modelId": "gemini-4-hydrogen-high",       "name": "Gemini 4 Hydrogen (High)" },
      { "modelId": "gemini-4-hydrogen-xhigh",      "name": "Gemini 4 Hydrogen (XHigh)" },
      { "modelId": "gemini-4-hydrogen-medium",     "name": "Gemini 4 Hydrogen (Medium)" },
      { "modelId": "gemini-4-hydrogen-low",        "name": "Gemini 4 Hydrogen (Low)" },
      { "modelId": "gemini-3.8-flash-high",        "name": "Gemini 3.8 Flash (High)" },
      { "modelId": "gemini-3.8-flash-medium",      "name": "Gemini 3.8 Flash (Medium)" },
      { "modelId": "gemini-3.8-flash-low",         "name": "Gemini 3.8 Flash (Low)" },
      { "modelId": "claude-sonnet-4-6",            "name": "Claude Sonnet 4.6 (Thinking)" },
      { "modelId": "claude-opus-4-6-thinking",     "name": "Claude Opus 4.6 (Thinking)" },
      { "modelId": "gpt-oss-120b-medium",          "name": "GPT-OSS 120B (Medium)" }
    ]
  },
  "modes": {
    "currentModeId": "default",
    "availableModes": [
      { "id": "default",      "name": "Default",
        "description": "Ask before edits and commands, per your permission settings." },
      { "id": "accept-edits", "name": "Accept edits",
        "description": "Approve file edits in the workspace without asking." },
      { "id": "plan",         "name": "Plan",
        "description": "Plan carefully before executing a task." }
    ]
  },
  "configOptions": [
    {
      "id": "model", "name": "Model", "category": "model", "type": "select",
      "currentValue": "gemini-4-hydrogen",
      "options": [
        { "value": "gemini-4-hydrogen",        "name": "Gemini 4 Hydrogen" },
        { "value": "gemini-3.8-flash",         "name": "Gemini 3.8 Flash" },
        { "value": "claude-sonnet-4-6",        "name": "Claude Sonnet 4.6 (Thinking)" },
        { "value": "claude-opus-4-6-thinking", "name": "Claude Opus 4.6 (Thinking)" },
        { "value": "gpt-oss-120b-medium",      "name": "GPT-OSS 120B (Medium)" }
      ]
    },
    {
      "id": "thought_level", "name": "Thinking Level", "category": "thought_level",
      "type": "select", "currentValue": "high",
      "options": [
        { "value": "low",    "name": "Low" },
        { "value": "medium", "name": "Medium" },
        { "value": "high",   "name": "High" },
        { "value": "xhigh",  "name": "Extra" }
      ]
    },
    {
      "id": "mode", "name": "Mode", "category": "mode", "type": "select",
      "currentValue": "default",
      "options": [
        { "value": "default",      "name": "Default",
          "description": "Ask before edits and commands, per your permission settings." },
        { "value": "accept-edits", "name": "Accept edits",
          "description": "Approve file edits in the workspace without asking." },
        { "value": "plan",         "name": "Plan",
          "description": "Plan carefully before executing a task." }
      ]
    }
  ],
  "_meta": {
    "antigravity": {
      "tier": {
        "name": "Antigravity Starter Quota",
        "id": "free-tier",
        "upgradeUri": "https://antigravity.google/g1-upgrade"
      }
    }
  }
}
```

**The single most error-prone detail — and the one most easily got backwards.** There are two
different lists here, and Zed does not read the first one at all:

- `models.availableModels` (10 entries, `modelId`, e.g. `gemini-4-hydrogen-high`) is **ignored by
  Zed**. `grep -c models` over Zed's v1 and v2 session code returns **0**; Zed's session
  response struct is `SessionConfigResponse { modes, config_options }`. Only `sessionId`,
  `modes`, `configOptions` and `_meta` are consumed. A gateway therefore does **not** need to
  emit a model/variant list — incorrectly mirroring these two lists is a self-inflicted bug
  that the client never even sees.
- `configOptions[id=model].options` (5 entries, `value`, e.g. `gemini-4-hydrogen`) IS the
  picker. Model selection and reasoning level are two independent select options.

The reasoning variant is **not** produced by string concatenation as a general rule. Measured
per-model behaviour (`PROVEN-PROBE`):

| Selected model | `thought_level` option | Its options | Default |
| --- | --- | --- | --- |
| `gemini-4-hydrogen` | present | `low, medium, high, xhigh` | `high` |
| `gemini-3.8-flash` | present | `low, medium, high` (no `xhigh`) | `medium` after a switch |
| `claude-sonnet-4-6` | **absent** | — | — |
| `claude-opus-4-6-thinking` | **absent** | — | — |
| `gpt-oss-120b-medium` | **absent** | — | — |

Consequences to build against:

- The `configOptions` array is **model-dependent**: for the non-Gemini families it shrinks to
  `[model, mode]`. It must be recomputed and re-sent (in the `set_config_option` **response**
  and the `config_option_update` **notification**) whenever `model` changes.
- Setting `thought_level` while a non-Gemini model is selected is an **error**, not a no-op:
  `{"code":-32602,"message":"Thinking level \"high\" is not offered for model \"Claude Sonnet 4.6 (Thinking)\""}`
  — note it quotes the model's **display name**, not its id.
- Switching from `gemini-4-hydrogen` (level `high`) to `gemini-3.8-flash` silently resets
  `thought_level` to `medium`. Do not cache the previous level across a model change.
- `xhigh` exists only on `gemini-4-hydrogen`; an option list is per model, never global.

Zed's rendering rules for these options (`PROVEN-ZED-SRC`, `crates/acp_thread/src/config_options.rs`
plus `agent_ui/src/config_options.rs`):

- `category` drives the toolbar slot. Recognised: `mode`, `model`, `model_config`, `speed`,
  `thought_level`, `other`. Toolbar order is `[speed, thought_level, mode, model]`.
- **An option with no `category` never appears in the toolbar.** `from_v1` hard-fails on an
  unknown category ("Unsupported legacy configuration category") and on an unknown select shape
  ("Unsupported legacy configuration choices") — so only the six known categories may be sent.
- An option with **zero** options is dropped entirely; an option with **5 or more** options is
  rendered as a searchable picker, fewer as a simple menu (`PICKER_THRESHOLD = 5`).

### 4.3 Notifications emitted around a turn (`PROVEN-SPY` + `PROVEN-PROBE`)

Actual ordering, established by probing a fresh session and a full turn:

1. `available_commands_update` — the command list (§6), sent once after `session/new`.
2. `usage_update` with `{ "used": 0, "size": 1048576 }` — the context meter's initial value.
3. `session_info_update` with `{ "title": "…" }` — auto-generated conversation title. It often
   trails the first prompt rather than arriving immediately; treat it as optional and late.
4. `agent_message_chunk` / `agent_thought_chunk` / `tool_call` … during the turn.
5. `usage_update` with `{ "used": N, … }` — at the end of the turn, and sometimes mid-turn after
   tool calls.
6. The `session/prompt` **response**.

Nothing is guaranteed except that the response comes last; the client must tolerate any of these
arriving zero or more times.

---

## 5. `session/set_config_option` and its two mirrors

### 5.1 Request (`PROVEN-SPY`, verbatim)

```json
{"jsonrpc":"2.0","id":"9a33f7d4-2e2c-40ee-9870-af99cb3f0beb",
 "method":"session/set_config_option",
 "params":{"sessionId":"24c99bcf-…","configId":"model","value":"gemini-3.8-flash"}}
```

### 5.2 Response (`PROVEN-SPY`)

```json
{"jsonrpc":"2.0","id":"9a33f7d4-…","result":{"configOptions":[ …all three options… ]}}
```

The response carries the **complete** `configOptions` array with updated `currentValue`s, not
just the changed one. Observed transitions:

| `configId` | `value` | Resulting `currentValue`s |
| --- | --- | --- |
| `model` | `gemini-3.8-flash` | model=`gemini-3.8-flash`, thought_level=`medium` (reset), mode=`default` |
| `thought_level` | `low` | model=`gemini-3.8-flash`, thought_level=`low`, mode=`default` |
| `model` | `gemini-4-hydrogen` | model=`gemini-4-hydrogen`, thought_level=`low`, mode=`default` |
| `model` | `claude-sonnet-4-6` | model=`claude-sonnet-4-6`, **no thought_level entry**, mode=`default` |
| `mode` | `plan` | …, mode=`plan` |
| `thought_level` | `high` | …, thought_level=`high` |

The array length therefore changes between calls (3 options for Gemini families, 2 for
Claude/GPT-OSS). A server must return the current array, not a cached one.

### 5.3 Notifications fired alongside every set (`PROVEN-SPY`)

Each `set_config_option` produces **three** extra notifications after its response, in order:

1. `config_option_update` — full `configOptions` array (same shape as the response body).
2. `available_commands_update` — the command list is re-sent on every config change.
3. For `configId=mode`: `current_mode_update` with `{ "currentModeId": "plan" }`.
   `configId=model` and `configId=thought_level` do **not** produce `current_mode_update`.

Consequence for any re-implementation: a config change must be answered with the response
**and** the notifications, or the client's UI silently desynchronises.

---

## 6. `available_commands_update` — dynamic command list (`PROVEN-SPY`)

```json
{"sessionUpdate":"available_commands_update","availableCommands":[
 {"name":"goal","description":"Run until the specified goal is completely finished."},
 {"name":"compact","description":"Compact the conversation now (summarize history to free up the context window)."},
 {"name":"schedule","description":"Run an instruction on a recurring schedule or as a one-time timer."},
 {"name":"browser","description":"Invoke a browser agent for web tasks."},
 {"name":"plan","description":"Plan carefully before executing a task."},
 {"name":"grill-me","description":"Interview me to align on a plan."},
 {"name":"learn","description":"Reflect on recent successes or corrections to capture reusable skills or rules."},
 {"name":"agy-customizations","description":"Comprehensive guide and reference for the Antigravity Customization System. …"},
 {"name":"antigravity-guide","description":"Provides a comprehensive guide, quick reference, and sitemap for Google Antigravity (AGY) …"},
 {"name":"copywriting","description":"When the user wants to write or rewrite page copy …"},
 {"name":"frontend-design","description":"Guidance for distinctive, intentional visual design …"},
 {"name":"modern-web-guidance","description":"Search tool for modern web development best practices. MANDATORY: Execute FIRST …"},
 {"name":"ponytail","description":"Forces the laziest solution that actually works …"},
 {"name":"ponytail-audit","description":"Whole-repo audit for over-engineering …"},
 {"name":"ponytail-debt","description":"Harvest every `ponytail:` comment in the codebase …"},
 {"name":"ponytail-help","description":"Quick-reference card for all ponytail modes …"},
 {"name":"ponytail-review","description":"Code review focused exclusively on over-engineering …"},
 {"name":"run-workflow","description":"How to write and run a dynamic workflow script with the `run_workflow` tool …"},
 {"name":"swarm","description":"(Preview & Quota Heavy) Run a swarm: several agents working in parallel on one task …"}
]}
```

The list is **dynamic**: it reflects the skills/customisations installed for the account
(`ponytail*`, `copywriting`, `frontend-design`, `modern-web-guidance` are local skills, not
built-ins). Earlier captures showed 17 entries; `agy-customizations` and `swarm` appeared
later. Never hardcode it — forward whatever the agent sends, including re-sends.

---

## 7. `session/prompt`

### 7.1 Request (`PROVEN-SPY`, verbatim)

Text only (case 01):

```json
{"jsonrpc":"2.0","id":"80a4a47d-…","method":"session/prompt",
 "params":{"sessionId":"24c99bcf-…","prompt":[{"type":"text","text":"[CASE-01-text] Reply with exactly: ACK-01"}]}}
```

With image (case 08, `data` truncated here to 60 chars):

```json
{"jsonrpc":"2.0","id":"641317de-…","method":"session/prompt",
 "params":{"sessionId":"24c99bcf-…","prompt":[
   {"type":"text","text":"[CASE-08-image] "},
   {"type":"image","data":"iVBORw0KGgoAAAANSUhEUgAABLAAAAJ2CAIAAADAIuwLAAOM9El…",
    "mimeType":"image/png","uri":"zed:///agent/pasted-image?name=Kage+Radar+BG.webp"},
   {"type":"text","text":"  What is in this image?"}
 ]}}
```

Rules observed across 14 prompts:

- `params` has **exactly two keys**: `sessionId` and `prompt`. No `messageId`, no `context`,
  no `mode`, no `model`.
- `prompt` is an **array of content blocks**, order preserved, text split around the image.
- Image block has four fields: `type:"image"`, `data` (base64, **no** `data:` prefix),
  `mimeType`, `uri`. The `uri` is a `zed:///agent/pasted-image?name=…` pseudo-URL; `agy`
  ignores it (it decoded the base64). A note of caution: the file name suggests `.webp`
  while the declared `mimeType` is `image/png` — the declared type is what matters.
- Slash commands are sent as **plain text** in a normal prompt (case 10):
  `prompt:[{"type":"text","text":"/compact"}]`. There is no dedicated command method.
- Reasoning effort is **not** a prompt field; it is `thought_level` (§5).
- Nothing else carries attachments: no `resource`, no `resource_link`, no `blob`.

### 7.2 Response (`PROVEN-SPY`, verbatim)

```json
{"jsonrpc":"2.0","id":"80a4a47d-…","result":{
  "stopReason":"end_turn",
  "usage":{"inputTokens":17592,"outputTokens":4,"cachedReadTokens":0,
           "cachedWriteTokens":0,"thoughtTokens":0,"totalTokens":17596}}}
```

- `stopReason` values observed: `end_turn`, `cancelled`.
- `usage` has six counters — `inputTokens`, `outputTokens`, `cachedReadTokens`,
  `cachedWriteTokens`, `thoughtTokens`, `totalTokens` — and is **optional**. It was absent on a
  successful `end_turn` (a second prompt sent while the first was still in flight) and present
  on a `cancelled` result. Presence cannot be predicted from `stopReason`; always treat `usage`
  as optional and key nothing off its existence.
- `usage.inputTokens` is per-turn (not cumulative); `cachedReadTokens` is a separate counter
  that can exceed `inputTokens` (case 02: input 2144, cachedRead 15541, total 18060).

Prompt → usage samples that show the shape (`PROVEN-SPY`):

| Case | inputTokens | outputTokens | cachedReadTokens | thoughtTokens | totalTokens | stopReason |
| --- | --- | --- | --- | --- | --- | --- |
| 01 text | 17592 | 4 | 0 | 0 | 17596 | end_turn |
| 02 thought (level=low) | 2144 | 375 | 15541 | **0** | 18060 | end_turn |
| 03 read | 4818 | 93 | 31769 | 0 | 36680 | end_turn |
| 04 edit | 2183 | 24 | 16377 | 0 | 18584 | end_turn |
| 05 exec | 4383 | 124 | 33812 | 0 | 38319 | end_turn |
| 06 parallel reads | 4536 | 205 | 34495 | 0 | 39236 | end_turn |
| 07 mcp | 5571 | 67 | 34655 | 0 | 40293 | end_turn |
| 08 image | 3343 | 243 | 18346 | 0 | 21932 | end_turn |
| 09 cancel | — | — | — | — | — | **cancelled** |
| 10 `/compact` | 2330 | 979 | 20597 | 0 | 23906 | end_turn |
| 11 model | 20913 | 39 | 0 | 31 | 20952 | end_turn |
| 12 mode=plan | 23623 | 332 | 19236 | 0 | 43191 | end_turn |
| 13 thought (level=high) | 25112 | 1613 | 21687 | **861** | 48412 | end_turn |

---

## 8. `agent_thought_chunk` — gated by `thought_level`, not by client capability

`PROVEN-SPY`, the decisive pair:

- **Case 02** (`gemini-3.8-flash`, `thought_level: low`): zero `agent_thought_chunk`,
  `thoughtTokens: 0`, although the prompt explicitly asked for step-by-step reasoning.
- **Case 13** (`gemini-3.8-flash`, `thought_level: high`): two `agent_thought_chunk`
  notifications and `thoughtTokens: 861`.

Shape (`PROVEN-SPY`, verbatim):

```json
{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"Computing partial products yields $17 \\times 20 = 340$ and $17 \\times 3 = 51$ …"}}
```

- Same envelope as `agent_message_chunk`: only `sessionUpdate` and `content`, no `_meta`,
  no `id`, no signature field.
- Chunks are arbitrary slices of one logical block; the client is expected to concatenate.
- Consequence: reasoning display cannot be enabled by the client declaring a capability.
  It is a function of `configId:"thought_level"` and the model. Any gateway that wants to
  expose reasoning must (a) forward `agent_thought_chunk` verbatim and (b) preserve
  `thought_level` across turns.

---

## 9. `session/cancel`

`PROVEN-SPY` (case 09), the only agent notification that travels client→agent:

```json
{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"24c99bcf-…"}}
```

- It is a **notification**: no `id`, no response of its own.
- The in-flight `session/prompt` request resolves with:

```json
{"jsonrpc":"2.0","id":"7bf7d3ff-…","result":{"stopReason":"cancelled","usage":{…}}}
```

- `usage` may or may not be present on a cancelled result — it was absent in case 09 (spy) and
  present when cancelling mid-`run_command` (probe). Treat it as optional (§7.2); a shim that
  unconditionally reads `result.usage` will panic on the absent case.
- Cancellation is per-session; the session stays usable, and the next prompt on the same
  `sessionId` succeeded immediately afterwards (case 10 in the same capture).
- `session/cancel` on an **idle** session is silently accepted (`PROVEN-PROBE`): no response,
  no error, and the next prompt works normally.
- Tool calls interrupted by a cancel go `in_progress → pending → in_progress → failed`.
  `status:"cancelled"` **never** appeared — so a client must not depend on it; a failed status
  is what an interrupted tool looks like.

---

## 10. `tool_call` and `tool_call_update`

### 10.1 Envelope (`PROVEN-SPY`)

Notification:

```json
{"jsonrpc":"2.0","method":"session/update",
 "params":{"sessionId":"…","update":{ …sessionUpdate object… }}}
```

`update.sessionUpdate` values actually observed (counts in the capture):
`agent_message_chunk` 64, `usage_update` 21, `available_commands_update` 9,
`config_option_update` 8, `tool_call` 8, `tool_call_update` 8, `current_mode_update` 2,
`agent_thought_chunk` 2, `session_info_update` 1.

Never observed: `user_message_chunk` (only on `session/load`, §12), `plan`.

### 10.2 Common `tool_call` fields (`PROVEN-SPY`)

`sessionUpdate`, `toolCallId`, `name`, `args`, `status`, `kind`, `title`, `locations?`, `rawInput`
— plus, on `tool_call_update`: `rawOutput?`, `content?`.

`kind` values observed: `read`, `edit`, `execute`, `other`.
`status` values observed: `in_progress`, `completed` (`pending` appeared only in the
`PROVEN-PROBE` permission flow, §10.5).

### 10.3 Per-tool detail

`read_file` (`PROVEN-SPY`, verbatim):

```json
{"sessionUpdate":"tool_call","toolCallId":"call_2737784179388677306","name":"read_file",
 "args":{"EndLine":10,"Path":"/media/shukoi/DATA/img/go-projects/agy-gateway/go.mod","StartLine":1},
 "status":"in_progress","kind":"read","title":"File ../../go-projects/agy-gateway/go.mod",
 "locations":[{"path":"/media/shukoi/DATA/img/go-projects/agy-gateway/go.mod"}],
 "rawInput":{"EndLine":10,"Path":"…go.mod","StartLine":1}}
```

- `title` is a **relative** display path (`File ../../go-projects/…`), while `locations[].path`
  is absolute. Both must be forwarded as-is; recomputing one from the other breaks the
  client's file linking.
- The completed update repeats `args`/`rawInput`/`locations` verbatim and adds nothing.

`write_file` (`PROVEN-SPY`, completed, verbatim):

```json
{"sessionUpdate":"tool_call_update","toolCallId":"call_9587217137894174939","name":"write_file",
 "args":{"Content":"hi","Overwrite":true,"Path":"/tmp/probe/hello.txt"},
 "status":"completed","kind":"edit","title":"File ../../../../../../tmp/probe/hello.txt",
 "locations":[{"path":"/tmp/probe/hello.txt"}],
 "rawInput":{"Content":"hi","Overwrite":true,"Path":"/tmp/probe/hello.txt"},
 "rawOutput":{},
 "content":[{"type":"diff","path":"/tmp/probe/hello.txt","newText":"hi"}]}
```

- Edits arrive as `content:[{type:"diff", path, newText}]` for a **new** file. For a
  **modification** the same block additionally carries `oldText` (`PROVEN-PROBE`):
  `{"type":"diff","path":"…","newText":"line1\nline2","oldText":"line1\n"}`. So `oldText` is
  optional and must be forwarded when present.
- `rawOutput` is an empty object, not `null`.

`run_command` (`PROVEN-SPY`, completed, verbatim):

```json
{"sessionUpdate":"tool_call_update","toolCallId":"call_15308777120579027757","name":"run_command",
 "args":{"CommandLine":"uname -a","WaitMsBeforeAsync":5000,"toolSummary":"Run uname -a"},
 "status":"completed","kind":"execute","title":"uname -a",
 "rawInput":{…same three fields…},
 "rawOutput":{"output":"Created At: 2026-11-28T10:31:34+07:00\nCompleted At: …\nCommand exited with code 0.\n\nCommand output:\nLinux shukoi 7.0.0-44-generic …\r\n","exit_code":0},
 "content":[{"type":"content","content":{"type":"text","text":"Created At: …\nCommand exited with code 0.\n\nCommand output:\nLinux shukoi …"}}]}
```

- `content[0].type` is `"content"`, with the payload nested one level deeper at
  `content[0].content` (itself a text block). This double nesting is easy to get wrong.
- The command output is duplicated in `rawOutput.output` and in `content[0].content.text`.
- `rawOutput.output` is the machine-oriented field (`exit_code` sibling); `content` is the
  display-oriented one.

MCP tool (`PROVEN-SPY`, case 07, completed, verbatim):

```json
{"sessionUpdate":"tool_call_update","toolCallId":"call_5423200771919501665",
 "name":"mcp_web-research_googleSearch",
 "args":{"limit":1,"query":"acp protocol","toolSummary":"Search ACP protocol"},
 "status":"completed","kind":"other","title":"Run mcp_web-research_googleSearch",
 "rawInput":{"limit":1,"query":"acp protocol","toolSummary":"Search ACP protocol"},
 "rawOutput":null,"content":null}
```

- Naming convention: `mcp_<serverName>_<toolName>` — the server name comes verbatim from
  `session/new.mcpServers[].name`, so a name containing `-` or `_` is not escaped.
- `kind` is `other`; `rawOutput` and `content` are **`null`** (not `{}`/`[]`) for this server,
  so the result of an MCP call is not visible to the client through `tool_call_update` here.
- `toolSummary` is injected into `args` by the agent for display; it is not part of the MCP
  tool's own schema and must not be forwarded to the MCP server.

Plan artifact (`PROVEN-SPY`, case 12/13):

```json
{"sessionUpdate":"tool_call","name":"write_file","kind":"edit",
 "title":"File ../../../../../../home/shukoi/.gemini/antigravity-cli/brain/24c99bcf-…/plan_case_13_thought.md",
 "args":{"ArtifactMetadata":{"RequestFeedback":true,"UserFacing":true,
         "Summary":"Implementation plan for computing 17 * 23 step-by-step …"},
         "Content":"# Implementation Plan - …","Overwrite":true,"Path":"…/plan_case_13_thought.md"}}
```

- In `plan` mode a written plan is an ordinary `write_file` whose `args` carry the extra
  `ArtifactMetadata` object (`RequestFeedback`, `UserFacing`, `Summary`).
- The path is under the agent's own state directory
  (`~/.gemini/antigravity-cli/brain/<sessionId>/`), **outside** the workspace, and the
  completed update's `content[0]` is a diff pointing at that outside path. A sandboxed
  wrapper that rejects writes outside `cwd` will break plan mode.

### 10.4 Tool calls arrive one at a time

Case 06 asked for two reads in one step; the capture shows two separate `tool_call`
notifications with distinct ids, the second emitted after the first `completed`. There is no
batch/aggregate field, and no observable overlap between tool calls of a single turn.

Two prompts sent on one session **do** run concurrently (`PROVEN-PROBE`): both were answered
(`end_turn` each) and their chunks interleaved in one stream. serialization is per-tool, not
per-session. A client that expects `session/prompt` to be a lock will mis-attribute chunks.

### 10.5 `session/request_permission` — `DOC-DECISION`: out of scope

`agy-gateway` is a **strict YOLO-mode** product: it always runs the agent with
`--dangerously-skip-permissions`, so the agent never emits `session/request_permission` for
gateway traffic and no permission bridging will be built.

Recorded for completeness (`PROVEN-PROBE`, `--dangerously-skip-permissions` **removed**):

- Only `run_command` asks. `write_file` and `read_file` never ask, including in `default` mode.
- Options are always exactly `[{optionId:"allow",name:"Allow",kind:"allow_once"},
  {optionId:"deny",name:"Deny",kind:"reject_once"}]`. `allow_always` / `reject_always` are
  **never** offered by `agy` 1.5.18.
- The reply is `{"outcome":{"outcome":"selected","optionId":"allow"}}`; `optionId` is
  authoritative and `kind` is informational (Zed overrides `kind` from the option definition).
- Denying leaves the tool at `status:"failed"`, and the turn still ends `end_turn`.
- Zed's dialog text comes entirely from the server-supplied `options[].name`.

---

## 11. `usage_update`

`PROVEN-SPY`, verbatim:

```json
{"sessionUpdate":"usage_update","used":24067,"size":1048576}
```

- `size` was `1048576` in every one of the 21 occurrences — a fixed 1M-token context window,
  not a measured value.
- `used` grows per turn and is emitted at least once per turn. The **first** one of a turn
  arrives at `used: 0` before any `agent_message_chunk` (`PROVEN-PROBE`), and a later one
  follows after the turn completes; a mid-turn emission can also occur after tool calls.
- It is not derived from `usage.totalTokens` directly (case 12's final `used` was 24345 while
  `totalTokens` was 43191) — treat it as the agent's own context accounting.

---

## 12. Session persistence methods (`PROVEN-PROBE` agent side, `PROVEN-ZED-SRC` client side)

Client-side calls are now settled from Zed's source, not from manual UI interaction.

### 12.1 `session/list`

Request: any of `{}`, `{cwd}`, `{cursor,limit}` — **all parameters are ignored**. Result:

```json
{"sessions":[
  {"sessionId":"55f3a83f-…","cwd":"/tmp/acpwork","title":"Create Hello.txt File","updatedAt":"2026-11-28T03:03:39Z"},
  {"sessionId":"046dc201-…","cwd":"/tmp/acpwork","title":"Fast Environment And System Verification","updatedAt":"0001-01-01T00:00:00Z"}
]}
```

- Always the **full** list (170 entries in the probe). No pagination fields, no filtering, no
  `nextCursor`. `updatedAt` may be the zero-value `0001-01-01T00:00:00Z`.
- `title` is the auto-generated `session_info_update` title.
- Zed **does** send `cwd`, `cursor` and `_meta` (`ListSessionsRequest::new().cwd()…`) and maps
  each returned session's `cwd` + `additional_directories` into work dirs — but the agent's
  ignoring of them means a gateway that filters server-side would be stricter than `agy`.

### 12.2 `session/load`

Request: `{"sessionId":"…","cwd":"…","mcpServers":[]}` (also `additionalDirectories`).
Effect: replays the whole conversation as notifications, in one burst:

`user_message_chunk` → `agent_thought_chunk` → `tool_call` (per tool) →
`agent_message_chunk` → … then returns a **complete `session/new`-shaped result**
(`sessionId`, `models`, `modes`, `configOptions`, `_meta`).

### 12.3 `session/resume`

Request: `{"sessionId":"…"}` alone or with `{sessionId,cwd,mcpServers}`.
Result: the same full `session/new`-shaped payload with the **same** sessionId, and **zero**
notifications (no replay).

### 12.4 `session/close`

Accepted, returns `{}`. **No `session/closed` notification** is emitted.

`session/load` vs `session/resume`: load replays, resume does not — both re-announce models,
modes, and config options, so a client that keeps its own copy must resynchronise from the
result rather than assume its cached state.

`session/close` is not what "closing the tab" does (`PROVEN-ZED-SRC`): Zed v2's
`should_close_sessions_on_release()` returns **false**, so disposing a conversation view only
detaches. Zed sends `session/close` in exactly one place — when a `session/new` response arrived
but registration failed and the agent advertises `sessionCapabilities.close`
(`agent_servers` v1 dispatcher). Closing a session still initialising errors with
*"Session is still initializing"*.

`replayFrom`: Zed v2 sets `replayFrom(ReplayFrom::Start)` on `session/resume` when the caller
asks for history replay, so a gateway implementing resume should honour that field instead of
always replaying (or always not replaying) history.

---

## 13. Error and edge semantics (`PROVEN-PROBE`, all cases exercised)

The error object is minimal and consistent: **only `code` and `message`. No `data` field, ever.**

| Case | Result |
| --- | --- |
| Unknown method (`no/such/method`) | `{"code":-32601,"message":"Method not found: no/such/method"}` |
| `params: null` | `{"code":-32602,"message":"Missing params"}` |
| Unknown session id | `{"code":-32602,"message":"Session not found: <uuid>"}` |
| Unknown `configId` | `{"code":-32602,"message":"Unknown config option: nonexistent"}` |
| `thought_level` not offered by model | `{"code":-32602,"message":"Thinking level \"high\" is not offered for model \"Claude Sonnet 4.6 (Thinking)\""}` |
| Malformed JSON line | **no response at all** — silently ignored; the connection stays alive and the next valid line is processed |
| `session/cancel` on idle session | silently accepted; no output |
| Duplicate prompt on one session | **accepted**; both answer `end_turn`, chunks interleave |
| stderr | never written to (zero bytes across every probe) |

JSON-RPC notification with a malformed body is likewise dropped without a reply. A server that
wants strictness must add it; matching `agy` means swallowing it.

---

## 14. Anti-patterns: what `agy-gateway` 3.16.8 does that deviates from this reference

Kept here because this document is the yardstick used to judge the gateway. Each row is
`PROVEN` against the gateway's own captured ACP traffic (via a spy shim inside the gateway).

| # | Deviation | Reference behaviour |
| --- | --- | --- |
| 1 | No ACP server mode at all: the binary exposes only HTTP (`/v1/chat/completions`, `/v1/messages`, `/admin/v1/*`) | Zed needs `agy acp`-style stdio JSON-RPC |
| 2 | `initialize.clientCapabilities` sends `fs:{readTextFile:false,writeTextFile:false}, terminal:false` and nothing else | Zed sends `true/true/true` plus `session`, `auth`, `elicitation`, `_meta` |
| 3 | `session/new` omits `additionalDirectories` | 4 directories were passed in this capture |
| 4 | `session/new` omits `mcpServers` entirely (uses one loopback `client-tools` stdio server) | Zed passes `{type:"http",name,url,headers}`; agent materialises tools as `mcp_<name>_<tool>` |
| 5 | Rejects image blocks (`code:"image_not_supported"`) | `{type:"image",data,mimeType,uri}` accepted, answered correctly |
| 6 | Discards `usage_update`, `session_info_update`, `available_commands_update`, `config_option_update`, `current_mode_update`, `agent_thought_chunk` | all are forwarded to the client |
| 7 | There is no Zed-facing session object at all, so `modes`/`configOptions` are never surfaced to a client | `session/new` returns `sessionId` + `modes` + `configOptions` (+ `_meta`), and the client builds its picker from `configOptions` |
| 8 | Sends exactly one `session/prompt` per HTTP request and closes the session | 14 prompts on one session |
| 9 | Injects a 770-char "CRITICAL EXECUTION ENVIRONMENT & TOOL PRECEDENCE OVERRIDE … EMPTY remote container workspace" text block ahead of the user prompt | the reference sends only the client's own blocks |
| 10 | `set_config_option` returns only a result, no `config_option_update`/`available_commands_update`/`current_mode_update` notifications | all three follow every set |
| 11 | `vision_mode=reject` kills images before they reach the agent | agent supports them natively (`promptCapabilities.image: true`) |
| 12 | Command list dropped, so slash commands never reach the client | the dynamic command list is re-sent on every config change |
| 13 | Fresh `session/new` + `session/close` per HTTP request; no session id is ever reused or exposed | one long-lived session per editor conversation, selectable from history |
| 14 | No `session/list`/`session/load`/`session/resume`, so an interrupted agent run cannot be reopened | all three are advertised, and Zed's history picker drives them |
| 15 | One account/worker is rebound per request with no session affinity | a session keeps its workspace and conversation state |

---

## 15. Conformance checklist for `agy-gateway`

Ordered by what blocks a usable Zed session, hardest dependency first. Each row is testable from
this document alone; no Zed UI interaction required beyond sending a prompt.

| # | Requirement | Evidence |
| --- | --- | --- |
| 1 | Serve ACP over stdio as a JSON-RPC peer (one JSON object per line) | §1 |
| 2 | Reply `protocolVersion: 1` to `initialize`; Zed tries v2 first and relaunches on fallback | §3 |
| 3 | Advertise `loadSession`, `promptCapabilities.image`, `sessionCapabilities.{resume,close,load,list,additionalDirectories}` | §3.2 |
| 4 | `session/new` must honour `cwd` + `additionalDirectories` (proven functional) | §4.1 |
| 5 | Pass `mcpServers` through verbatim (stdio `env:[{name,value}]`, http `headers:[{name,value}]`) | §4.1 |
| 6 | Return `sessionId` + `modes` + `configOptions`; `models` is ignored by Zed | §4.2 |
| 7 | `configOptions` must use only the 6 known categories and recompute per selected model | §4.2 |
| 8 | Forward every `session/update` verbatim: chunks, thought, tool_call, usage, commands, info, mode | §10.1 |
| 9 | Accept image blocks in `session/prompt` and forward them to the agent | §7.1 |
| 10 | Implement `session/load` (replay) and `session/resume` (no replay, honour `replayFrom`) | §12 |
| 11 | Implement `session/list` (title + updatedAt) and `session/close` (returns `{}`, no notification) | §12 |
| 12 | Treat `usage` as optional; never derive control flow from its presence | §7.2, §9 |
| 13 | Relay `session/cancel` as a notification; resolve the pending prompt with `stopReason:"cancelled"` | §9 |
| 14 | Answer errors as `{code,message}` only | §13 |
| 15 | Never emit `session/request_permission` — YOLO mode is the product contract | §10.5 |

### Recommended re-capture (still worth doing)

These verify **the agent against a newer `agy`**, not Zed. They are the only items that a future
`agy` release could invalidate; none of them block implementation today.

1. Save files → `git log --oneline` in this repo as the `agy` version evolves.
2. Re-run `acp-spy-view --from-start --full` on a fresh session and diff for new
   `sessionUpdate` variants (a new model family may add a config option or change `thought_level`
   options).
3. If `agy` starts offering `allow_always`/`reject_always`, note it — it would matter only if
   permissions ever came into scope.

---

## 16. Residual unknowns

There is **one** class of unknown left, and it is not a protocol gap:

- **`agy` version drift.** Every agent-side statement here is `PROVEN-PROBE` against `agy`
  1.5.18. A future release could add a `sessionUpdate` variant, change `thought_level` options,
  or offer more permission kinds. That is not "unverified Zed behaviour" — it is the normal
  forward-compatibility duty to forward unknown `sessionUpdate` values verbatim rather than
  pattern-match a fixed list.

Everything else previously listed here is now settled:

| Formerly unknown | Now |
| --- | --- |
| Client shape of `session/list`/`load`/`resume` | `PROVEN-ZED-SRC` §12 |
| Permission options / `cancelled` outcome | `PROVEN-PROBE`, and out of scope anyway §10.5 |
| `oldText` on modified-file diffs | `PROVEN-PROBE`: present §10.3 |
| stdio `mcpServers` form from Zed | `PROVEN-ZED-SRC` §4.1 |
| JSON-RPC error object shape | `PROVEN-PROBE`: `{code,message}` only §13 |
| Duplicate prompt / cancel on idle session | `PROVEN-PROBE` §13 |
| `thought_level` for claude/gpt-oss | `PROVEN-PROBE`: option absent entirely §4.2 |
| `usage_update` before first chunk | `PROVEN-PROBE`: yes, at `used:0` §4.3 |
