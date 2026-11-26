#!/usr/bin/env python3
"""
End-to-end verification for the agy-gateway YOLO posture.

@purpose Proves that the gateway behaves like a plain provider endpoint: no turn is blocked by a
permission prompt, the consumer system prompt is honoured, and a system-prompt-declared read-only
phase really prevents writes while a write-enabled phase performs them.
@rationale The regression this pins is a gateway that gates work behind approval prompts or that
silently drops the consumer system prompt. Turn 1 declares a read-only plan directive and must
produce zero write tool calls and no file; turn 2 declares a build directive and must create it;
turn 3/4 read it back through the client tool and the model must echo the exact contents
from a follow-up request that carries the tool result.
@preconditions agy-gateway running and reachable; the client key is exported; the worker home has no
stray AGENTS.md (that file is read by agy and would override the consumer prompt).
@invariants Uses one model only (gemini-3.8-flash) at the "low" thinking level on both endpoints, so
a result cannot be explained by a different model or effort tier.
"""

import json
import os
import shutil
import sys
import tempfile
import time
import urllib.error
import urllib.request

HOST_PORT = os.environ.get("AGY_GW_HOST_PORT", "8182")
BASE_URL = os.environ.get("GATEWAY_BASE_URL", f"http://localhost:{HOST_PORT}")
API_KEY = os.environ.get("API_KEY", os.environ.get("AGY_GW_CLIENT_KEY_1", ""))

# Requirement: always gemini-3.8-flash at low effort, never another model.
MODEL = "gemini-3.8-flash"
EFFORT = "low"
THINKING_BUDGET = 1024  # MapThinkingBudget(1024) -> low

# A stall here means a permission prompt is blocking the turn: the exact regression under test.
TURN_TIMEOUT_SECONDS = 180

PLAN_DIRECTIVE = (
    "You are in plan mode. This phase is strictly read-only: do not create, modify, delete, or "
    "rename any file, and do not call any file-writing tool. Analyse the request and reply with a "
    "short plan only."
)
BUILD_DIRECTIVE = (
    "You are in build mode. Use the provided tools to perform the requested change, then confirm "
    "completion in one short sentence."
)

TARGET_NAME = "plan_target.txt"
TARGET_CONTENT = "PROBE"

TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "write_file",
            "description": "Writes or overwrites content to a specified file path.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "Relative file path."},
                    "content": {"type": "string", "description": "Complete text content to write."},
                },
                "required": ["path", "content"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "read_file",
            "description": "Reads the entire content of a specified file.",
            "parameters": {
                "type": "object",
                "properties": {"path": {"type": "string", "description": "Relative file path."}},
                "required": ["path"],
            },
        },
    },
]


def execute_local_tool(name: str, arguments: dict, workdir: str) -> str:
    """Execute a client-side tool against workdir and return its textual result."""
    path = os.path.join(workdir, arguments.get("path", ""))
    if os.path.abspath(path) != os.path.abspath(workdir) and not os.path.abspath(path).startswith(
        os.path.abspath(workdir) + os.sep
    ):
        return "error: path escapes the workspace"
    if name == "write_file":
        os.makedirs(os.path.dirname(path) or workdir, exist_ok=True)
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(arguments.get("content", ""))
        return "ok: written"
    if name == "read_file":
        if not os.path.exists(path):
            return "error: file not found"
        with open(path, "r", encoding="utf-8") as fh:
            return fh.read()
    return f"error: unknown tool {name}"


def _post(url: str, payload: dict, headers: dict) -> urllib.request.Request:
    body = json.dumps(payload).encode("utf-8")
    return urllib.request.Request(url, data=body, headers=headers, method="POST")


def _consume_sse(resp, on_event) -> None:
    for raw in resp:
        line = raw.decode("utf-8").strip()
        if not line.startswith("data:"):
            continue
        data = line[5:].strip()
        if data == "[DONE]":
            break
        try:
            on_event(json.loads(data))
        except json.JSONDecodeError:
            continue


def call_openai(messages: list, workdir: str) -> dict:
    """Run one OpenAI chat-completions turn, executing client tools, returning the observed result."""
    headers = {"Content-Type": "application/json", "Authorization": f"Bearer {API_KEY}"}
    tool_calls: list = []
    pending: dict = {}
    text_parts: list = []
    finish = {"reason": None}

    def on_event(event: dict) -> None:
        for choice in event.get("choices", []):
            delta = choice.get("delta", {})
            if delta.get("content"):
                text_parts.append(delta["content"])
            for tc in delta.get("tool_calls", []) or []:
                idx = tc.get("index", 0)
                slot = pending.setdefault(idx, {"id": "", "name": "", "arguments": ""})
                if tc.get("id"):
                    slot["id"] = tc["id"]
                fn = tc.get("function") or {}
                if fn.get("name"):
                    slot["name"] += fn["name"]
                if fn.get("arguments"):
                    slot["arguments"] += fn["arguments"]
            if choice.get("finish_reason"):
                finish["reason"] = choice["finish_reason"]

    payload = {"model": MODEL, "messages": messages, "tools": TOOLS, "stream": True,
               "reasoning_effort": EFFORT}
    started = time.time()
    with urllib.request.urlopen(
        _post(f"{BASE_URL}/v1/chat/completions", payload, headers), timeout=TURN_TIMEOUT_SECONDS
    ) as resp:
        _consume_sse(resp, on_event)
    elapsed = time.time() - started

    for idx in sorted(pending):
        slot = pending[idx]
        try:
            args = json.loads(slot["arguments"] or "{}")
        except json.JSONDecodeError:
            args = {}
        result = execute_local_tool(slot["name"], args, workdir)
        tool_calls.append({"name": slot["name"], "arguments": args, "result": result})
        messages.append({"role": "assistant", "content": None,
                         "tool_calls": [{"id": slot["id"] or f"call_{idx}", "type": "function",
                                         "function": {"name": slot["name"],
                                                      "arguments": slot["arguments"]}}]})
        messages.append({"role": "tool", "tool_call_id": slot["id"] or f"call_{idx}",
                         "content": result})

    return {"tool_calls": tool_calls, "text": "".join(text_parts),
            "finish_reason": finish["reason"], "elapsed": elapsed}


def call_anthropic(messages: list, system: str, workdir: str) -> dict:
    """Run one Anthropic messages turn, executing client tools, returning the observed result."""
    headers = {"Content-Type": "application/json", "x-api-key": API_KEY,
               "anthropic-version": "2023-06-01"}
    tool_calls: list = []
    text_parts: list = []
    current_tool: dict = {}
    finish = {"reason": None}

    def on_event(event: dict) -> None:
        etype = event.get("type")
        if etype == "content_block_start":
            block = event.get("content_block", {})
            if block.get("type") == "tool_use":
                current_tool.clear()
                current_tool.update({"id": block.get("id", ""), "name": block.get("name", ""),
                                     "json": ""})
        elif etype == "content_block_delta":
            delta = event.get("delta", {})
            if delta.get("type") == "text_delta":
                text_parts.append(delta.get("text", ""))
            elif delta.get("type") == "input_json_delta":
                current_tool["json"] = current_tool.get("json", "") + delta.get(
                    "partial_json", "")
        elif etype == "content_block_stop":
            if current_tool.get("name"):
                try:
                    args = json.loads(current_tool.get("json") or "{}")
                except json.JSONDecodeError:
                    args = {}
                current_tool["args"] = args
                tool_calls.append(dict(current_tool))
                current_tool.clear()
        elif etype == "message_delta":
            finish["reason"] = event.get("delta", {}).get("stop_reason", finish["reason"])

    payload = {
        "model": MODEL,
        "max_tokens": 2048,
        "system": system,
        "messages": messages,
        "tools": [
            {"name": t["function"]["name"], "description": t["function"]["description"],
             "input_schema": t["function"]["parameters"]}
            for t in TOOLS
        ],
        "thinking": {"type": "enabled", "budget_tokens": THINKING_BUDGET},
        "stream": True,
    }
    started = time.time()
    with urllib.request.urlopen(
        _post(f"{BASE_URL}/v1/messages", payload, headers), timeout=TURN_TIMEOUT_SECONDS
    ) as resp:
        _consume_sse(resp, on_event)
    elapsed = time.time() - started

    executed = []
    for tc in tool_calls:
        result = execute_local_tool(tc["name"], tc.get("args", {}), workdir)
        executed.append({"name": tc["name"], "arguments": tc.get("args", {}), "result": result})
        # Feed results back for the build turn's read-back.
        messages.append({"role": "assistant", "content": [
            {"type": "tool_use", "id": tc["id"], "name": tc["name"], "input": tc.get("args", {})}
        ]})
        messages.append({"role": "user", "content": [
            {"type": "tool_result", "tool_use_id": tc["id"], "content": result}
        ]})

    return {"tool_calls": executed, "text": "".join(text_parts),
            "finish_reason": finish["reason"], "elapsed": elapsed}


def run_scenario(api: str) -> list:
    """Run the plan, build, and read-back scenario against one endpoint and return failures."""
    failures: list = []
    workdir = tempfile.mkdtemp(prefix=f"agy-yolo-{api}-")
    try:
        return _run_turns(api, workdir, failures)
    finally:
        shutil.rmtree(workdir, ignore_errors=True)


def _run_turns(api: str, workdir: str, failures: list) -> list:
    target = os.path.join(workdir, TARGET_NAME)
    request_text = f"Create the file {TARGET_NAME} containing exactly {TARGET_CONTENT}."

    if api == "openai":
        messages = [{"role": "system", "content": PLAN_DIRECTIVE},
                    {"role": "user", "content": request_text}]
        turn1 = call_openai(messages, workdir)
        # Snapshot before turn 2 can create anything, otherwise turn 2's file would be blamed on turn 1.
        target_after_turn1 = os.path.exists(target)
        # Turn 2: swap the system directive to build mode and repeat the request.
        messages = [{"role": "system", "content": BUILD_DIRECTIVE},
                    {"role": "user", "content": request_text}]
        turn2 = call_openai(messages, workdir)
    else:
        turn1 = call_anthropic([{"role": "user", "content": request_text}], PLAN_DIRECTIVE, workdir)
        target_after_turn1 = os.path.exists(target)
        turn2 = call_anthropic([{"role": "user", "content": request_text}], BUILD_DIRECTIVE, workdir)

    writes1 = [c for c in turn1["tool_calls"] if c["name"] == "write_file"]
    if writes1:
        failures.append(f"{api}: turn 1 (plan) emitted write_file: {writes1}")
    if target_after_turn1:
        failures.append(f"{api}: turn 1 (plan) created {TARGET_NAME} on disk")
    if turn1["finish_reason"] is None:
        failures.append(f"{api}: turn 1 produced no finish/stop reason (possible stall): {turn1}")
    if turn1["elapsed"] >= TURN_TIMEOUT_SECONDS:
        failures.append(f"{api}: turn 1 took {turn1['elapsed']:.1f}s (stall detector)")

    writes2 = [c for c in turn2["tool_calls"] if c["name"] == "write_file"]
    if not writes2:
        failures.append(
            f"{api}: turn 2 (build) emitted no write_file; text={turn2['text'][:200]!r} "
            f"tools={turn2['tool_calls']}")
    if not os.path.exists(target):
        failures.append(f"{api}: turn 2 (build) did not create {TARGET_NAME}")
    else:
        with open(target, "r", encoding="utf-8") as fh:
            actual = fh.read().strip()
        if TARGET_CONTENT not in actual:
            failures.append(f"{api}: {TARGET_NAME} content {actual!r} lacks {TARGET_CONTENT!r}")
    if turn2["finish_reason"] is None:
        failures.append(f"{api}: turn 2 produced no finish/stop reason (possible stall)")
    if turn2["elapsed"] >= TURN_TIMEOUT_SECONDS:
        failures.append(f"{api}: turn 2 took {turn2['elapsed']:.1f}s (stall detector)")

    # Turn 3: prove the client-tool round trip reaches the model, not just the disk. The response that
    # chooses read_file cannot echo anything (the result does not exist yet), so a follow-up request
    # carrying the tool result is required to observe the model actually reading it.
    readback = f"Read {TARGET_NAME} using read_file."
    if api == "openai":
        messages = [{"role": "system", "content": BUILD_DIRECTIVE},
                    {"role": "user", "content": readback}]
        turn3 = call_openai(messages, workdir)
        turn4 = call_openai(
            messages + [{"role": "user", "content": "State the exact contents you just read."}],
            workdir)
    else:
        messages = [{"role": "user", "content": readback}]
        turn3 = call_anthropic(messages, BUILD_DIRECTIVE, workdir)
        turn4 = call_anthropic(
            messages + [{"role": "user", "content": "State the exact contents you just read."}],
            BUILD_DIRECTIVE, workdir)

    reads = [c for c in turn3["tool_calls"] if c["name"] == "read_file"]
    if not reads:
        failures.append(
            f"{api}: read-back emitted no read_file; text={turn3['text'][:200]!r} "
            f"tools={turn3['tool_calls']}")
    elif TARGET_CONTENT not in str(reads[0]["result"]):
        failures.append(f"{api}: read_file returned {reads[0]['result']!r}, lacks {TARGET_CONTENT!r}")
    if TARGET_CONTENT not in turn4["text"]:
        failures.append(f"{api}: model did not echo the file content: {turn4['text'][:200]!r}")

    return failures


def main() -> int:
    if not API_KEY:
        print("FATAL: no client key (set API_KEY or AGY_GW_CLIENT_KEY_1)", file=sys.stderr)
        return 2

    all_failures: list = []
    for api in ("openai", "anthropic"):
        try:
            failures = run_scenario(api)
        except urllib.error.HTTPError as exc:
            detail = exc.read()[:400].decode("utf-8", "replace")
            all_failures.append(f"{api}: HTTP {exc.code}: {detail}")
            continue
        except Exception as exc:  # noqa: BLE001 - report, never mask
            all_failures.append(f"{api}: {type(exc).__name__}: {exc}")
            continue
        if failures:
            all_failures.extend(failures)
        else:
            print(f"PASS {api}: plan turn wrote nothing; build turn created {TARGET_NAME}")

    if all_failures:
        print("\nFAILURES:", file=sys.stderr)
        for failure in all_failures:
            print(f"  - {failure}", file=sys.stderr)
        return 1

    print(f"\nALL PASS (model={MODEL}, effort={EFFORT})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
