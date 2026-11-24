#!/usr/bin/env python3
"""
Enterprise-Grade Multi-Adapter Concurrent Stress Testing Suite for agy-gateway.

@purpose Stresses the gateway under sustained concurrent load across all supported protocol adapters
(OpenAI /v1/chat/completions and Anthropic /v1/messages, both streaming and non-streaming modes)
with real multi-turn code editor tool execution (write, read, edit, bash) in parallel isolated workspaces.
@rationale Validates worker pool queuing, failover resilience, JIT session cleanup, thread safety,
polymorphic ACP update deserialization, and absence of prompt/turn leaks under production concurrency.
@preconditions agy-gateway running on http://localhost:8182 with healthcheck healthy.
@invariants Fully thread-safe, deterministic filesystem isolation per worker thread in /tmp/stress-workspaces/.
"""

import concurrent.futures
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

BASE_URL = os.environ.get("GATEWAY_BASE_URL", "http://localhost:8182")
API_KEY = os.environ.get("API_KEY", os.environ.get("AGY_GW_CLIENT_KEY_1", "sk-bf-core"))
OPENAI_MODEL = os.environ.get("OPENAI_TEST_MODEL", "gemini-3.8-flash")
ANTHROPIC_MODEL = os.environ.get("ANTHROPIC_TEST_MODEL", "claude-sonnet-4-6")
ROOT_WORKSPACE = "/tmp/stress-workspaces"

# Tool Schemas for OpenAI
OPENAI_TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "write_file",
            "description": "Writes text to a specified file.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "Relative file path."},
                    "content": {"type": "string", "description": "Full file content."}
                },
                "required": ["path", "content"]
            }
        }
    },
    {
        "type": "function",
        "function": {
            "name": "read_file",
            "description": "Reads text from a specified file.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "Relative file path."}
                },
                "required": ["path"]
            }
        }
    },
    {
        "type": "function",
        "function": {
            "name": "edit_file",
            "description": "Replaces target text with replacement text in a file.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "Relative file path."},
                    "target": {"type": "string", "description": "Target string."},
                    "replacement": {"type": "string", "description": "Replacement string."}
                },
                "required": ["path", "target", "replacement"]
            }
        }
    },
    {
        "type": "function",
        "function": {
            "name": "bash",
            "description": "Executes shell commands in the project directory.",
            "parameters": {
                "type": "object",
                "properties": {
                    "command": {"type": "string", "description": "Shell command line."}
                },
                "required": ["command"]
            }
        }
    }
]

# Tool Schemas for Anthropic
ANTHROPIC_TOOLS = [
    {
        "name": "write_file",
        "description": "Writes text to a specified file.",
        "input_schema": {
            "type": "object",
            "properties": {
                "path": {"type": "string", "description": "Relative file path."},
                "content": {"type": "string", "description": "Full file content."}
            },
            "required": ["path", "content"]
        }
    },
    {
        "name": "read_file",
        "description": "Reads text from a specified file.",
        "input_schema": {
            "type": "object",
            "properties": {
                "path": {"type": "string", "description": "Relative file path."}
            },
            "required": ["path"]
        }
    },
    {
        "name": "edit_file",
        "description": "Replaces target text with replacement text in a file.",
        "input_schema": {
            "type": "object",
            "properties": {
                "path": {"type": "string", "description": "Relative file path."},
                "target": {"type": "string", "description": "Target string."},
                "replacement": {"type": "string", "description": "Replacement string."}
            },
            "required": ["path", "target", "replacement"]
        }
    },
    {
        "name": "bash",
        "description": "Executes shell commands in the project directory.",
        "input_schema": {
            "type": "object",
            "properties": {
                "command": {"type": "string", "description": "Shell command line."}
            },
            "required": ["command"]
        }
    }
]


def execute_tool_in_dir(workdir: str, name: str, args: dict) -> str:
    """Executes code editor tool inside an isolated directory."""
    if name == "write_file":
        rel_path = args.get("path", "").lstrip("/")
        full_path = os.path.join(workdir, rel_path)
        os.makedirs(os.path.dirname(full_path), exist_ok=True)
        with open(full_path, "w", encoding="utf-8") as f:
            f.write(args.get("content", ""))
        return f"Wrote {len(args.get('content', ''))} bytes to {rel_path}"

    elif name == "read_file":
        rel_path = args.get("path", "").lstrip("/")
        full_path = os.path.join(workdir, rel_path)
        if not os.path.exists(full_path):
            return f"Error: File not found: {rel_path}"
        with open(full_path, "r", encoding="utf-8") as f:
            return f.read()

    elif name == "edit_file":
        rel_path = args.get("path", "").lstrip("/")
        full_path = os.path.join(workdir, rel_path)
        if not os.path.exists(full_path):
            return f"Error: File not found: {rel_path}"
        with open(full_path, "r", encoding="utf-8") as f:
            content = f.read()
        target = args.get("target", "")
        replacement = args.get("replacement", "")
        if target not in content:
            return f"Error: Target text not found in {rel_path}"
        new_content = content.replace(target, replacement, 1)
        with open(full_path, "w", encoding="utf-8") as f:
            f.write(new_content)
        return f"Successfully edited {rel_path}"

    elif name == "bash":
        cmd = args.get("command", "")
        res = subprocess.run(cmd, shell=True, cwd=workdir, capture_output=True, text=True, timeout=30)
        out = res.stdout
        if res.stderr:
            out += "\nSTDERR:\n" + res.stderr
        out += f"\n[Exit: {res.returncode}]"
        return out.strip()

    return f"Error: Unknown tool {name}"


# ==============================================================================
# OpenAI Adapter Client (Streaming & Sync)
# ==============================================================================

def call_openai_adapter(messages: list, stream: bool = True) -> tuple:
    """Invokes OpenAI /v1/chat/completions endpoint."""
    url = f"{BASE_URL}/v1/chat/completions"
    payload = {
        "model": OPENAI_MODEL,
        "messages": messages,
        "tools": OPENAI_TOOLS,
        "tool_choice": "auto",
        "stream": stream
    }

    req = urllib.request.Request(
        url,
        data=json.dumps(payload).encode("utf-8"),
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bearer {API_KEY}"
        }
    )

    if not stream:
        with urllib.request.urlopen(req, timeout=300) as resp:
            data = json.loads(resp.read().decode("utf-8"))
            choice = data["choices"][0]
            text = choice["message"].get("content") or ""
            raw_tools = choice["message"].get("tool_calls") or []
            tool_calls = []
            for tc in raw_tools:
                tool_calls.append({
                    "id": tc.get("id"),
                    "name": tc["function"]["name"],
                    "arguments": json.loads(tc["function"]["arguments"]) if isinstance(tc["function"]["arguments"], str) else tc["function"]["arguments"]
                })
            return text, tool_calls

    # Streaming mode
    accumulated_text = ""
    tool_calls_map = {}

    with urllib.request.urlopen(req, timeout=300) as resp:
        for line in resp:
            line_str = line.decode("utf-8").strip()
            if not line_str.startswith("data: "):
                continue
            data_str = line_str[6:].strip()
            if data_str == "[DONE]":
                break
            try:
                chunk = json.loads(data_str)
            except json.JSONDecodeError:
                continue

            choices = chunk.get("choices", [])
            if not choices:
                continue
            delta = choices[0].get("delta", {})

            if "content" in delta and delta["content"]:
                accumulated_text += delta["content"]

            if "tool_calls" in delta and delta["tool_calls"]:
                for tc in delta["tool_calls"]:
                    idx = tc.get("index", 0)
                    if idx not in tool_calls_map:
                        tool_calls_map[idx] = {
                            "id": tc.get("id", f"call_{idx}"),
                            "name": tc.get("function", {}).get("name", ""),
                            "arguments_buf": ""
                        }
                    fn = tc.get("function", {})
                    if "name" in fn and fn["name"]:
                        tool_calls_map[idx]["name"] = fn["name"]
                    if "arguments" in fn and fn["arguments"]:
                        tool_calls_map[idx]["arguments_buf"] += fn["arguments"]

    tool_calls = []
    for idx in sorted(tool_calls_map.keys()):
        item = tool_calls_map[idx]
        raw_args = item["arguments_buf"]
        try:
            parsed_args = json.loads(raw_args) if raw_args else {}
        except json.JSONDecodeError:
            parsed_args = {}
        tool_calls.append({
            "id": item["id"],
            "name": item["name"],
            "arguments": parsed_args
        })

    return accumulated_text, tool_calls


# ==============================================================================
# Anthropic Adapter Client (Streaming & Sync)
# ==============================================================================

def call_anthropic_adapter(messages: list, stream: bool = True) -> tuple:
    """Invokes Anthropic /v1/messages endpoint."""
    url = f"{BASE_URL}/v1/messages"
    payload = {
        "model": ANTHROPIC_MODEL,
        "max_tokens": 4096,
        "messages": messages,
        "tools": ANTHROPIC_TOOLS,
        "stream": stream
    }

    req = urllib.request.Request(
        url,
        data=json.dumps(payload).encode("utf-8"),
        headers={
            "Content-Type": "application/json",
            "x-api-key": API_KEY,
            "anthropic-version": "2023-06-01"
        }
    )

    if not stream:
        with urllib.request.urlopen(req, timeout=300) as resp:
            data = json.loads(resp.read().decode("utf-8"))
            text = ""
            tool_calls = []
            for block in data.get("content", []):
                if block.get("type") == "text":
                    text += block.get("text", "")
                elif block.get("type") == "tool_use":
                    tool_calls.append({
                        "id": block.get("id"),
                        "name": block.get("name"),
                        "arguments": block.get("input", {})
                    })
            return text, tool_calls

    # Streaming mode
    accumulated_text = ""
    tool_calls_map = {}
    current_block_index = None

    with urllib.request.urlopen(req, timeout=300) as resp:
        for line in resp:
            line_str = line.decode("utf-8").strip()
            if not line_str.startswith("data: "):
                continue
            data_str = line_str[6:].strip()
            try:
                event = json.loads(data_str)
            except json.JSONDecodeError:
                continue

            ev_type = event.get("type")

            if ev_type == "content_block_start":
                current_block_index = event.get("index", 0)
                block = event.get("content_block", {})
                if block.get("type") == "tool_use":
                    tool_calls_map[current_block_index] = {
                        "id": block.get("id"),
                        "name": block.get("name"),
                        "json_buf": ""
                    }

            elif ev_type == "content_block_delta":
                delta = event.get("delta", {})
                delta_type = delta.get("type")
                if delta_type == "text_delta":
                    accumulated_text += delta.get("text", "")
                elif delta_type == "input_json_delta":
                    idx = event.get("index", current_block_index)
                    if idx in tool_calls_map:
                        tool_calls_map[idx]["json_buf"] += delta.get("partial_json", "")

    tool_calls = []
    for idx in sorted(tool_calls_map.keys()):
        item = tool_calls_map[idx]
        raw_json = item["json_buf"]
        try:
            parsed_args = json.loads(raw_json) if raw_json else {}
        except json.JSONDecodeError:
            parsed_args = {}
        tool_calls.append({
            "id": item["id"],
            "name": item["name"],
            "arguments": parsed_args
        })

    return accumulated_text, tool_calls


# ==============================================================================
# Worker Task: Multi-Turn Scenario Execution
# ==============================================================================

def run_worker_scenario(worker_id: int, adapter_type: str, stream: bool) -> dict:
    """Executes a full multi-turn code development session for a given worker."""
    workdir = os.path.join(ROOT_WORKSPACE, f"worker_{worker_id}_{adapter_type}_{'stream' if stream else 'sync'}")
    os.makedirs(workdir, exist_ok=True)

    tag = f"[Worker {worker_id} | {adapter_type.upper()} | {'STREAM' if stream else 'SYNC'}]"
    print(f"{tag} Started in {workdir}")

    # Build initial scenario instructions
    system_prompt = (
        f"You are Worker {worker_id}, an autonomous software engineer working in your project directory.\n"
        "Your task:\n"
        "1. write_file: create 'app_module.py' with functions compute_sum(a, b) and compute_product(a, b).\n"
        "2. read_file: read 'app_module.py' to verify implementation.\n"
        "3. edit_file: add compute_power(a, b) returning a ** b to 'app_module.py'.\n"
        "4. bash: run python3 -c 'import app_module; assert app_module.compute_power(3, 3) == 27; print(\"WORKER_SUCCESS\")'\n"
        "Execute these actions using available tool calls."
    )

    if adapter_type == "openai":
        messages = [
            {"role": "system", "content": system_prompt},
            {"role": "user", "content": f"Begin development task for worker {worker_id} now."}
        ]
    else:
        # Anthropic format
        messages = [
            {"role": "user", "content": f"{system_prompt}\n\nBegin development task for worker {worker_id} now."}
        ]

    turn = 0
    max_turns = 8
    success = False
    tools_executed = 0
    start_time = time.time()

    while turn < max_turns:
        turn += 1

        if adapter_type == "openai":
            asst_text, tool_calls = call_openai_adapter(messages, stream=stream)
        else:
            asst_text, tool_calls = call_anthropic_adapter(messages, stream=stream)

        # Invariant checks: ensure no turn header leaks
        if "User: [Tool Execution Results]" in asst_text:
            raise AssertionError(f"{tag} Turn {turn}: Leak of 'User: [Tool Execution Results]' in assistant text: {asst_text}")
        if asst_text.strip().startswith("Assistant:"):
            raise AssertionError(f"{tag} Turn {turn}: Leak of 'Assistant:' turn header in assistant text: {asst_text}")

        # Record assistant response in message history
        if adapter_type == "openai":
            asst_msg = {"role": "assistant"}
            if asst_text:
                asst_msg["content"] = asst_text
            if tool_calls:
                asst_msg["tool_calls"] = [
                    {
                        "id": tc["id"],
                        "type": "function",
                        "function": {
                            "name": tc["name"],
                            "arguments": json.dumps(tc["arguments"])
                        }
                    }
                    for tc in tool_calls
                ]
            messages.append(asst_msg)
        else:
            asst_blocks = []
            if asst_text:
                asst_blocks.append({"type": "text", "text": asst_text})
            for tc in tool_calls:
                asst_blocks.append({
                    "type": "tool_use",
                    "id": tc["id"],
                    "name": tc["name"],
                    "input": tc["arguments"]
                })
            messages.append({"role": "assistant", "content": asst_blocks})

        if not tool_calls:
            print(f"{tag} Turn {turn}: Assistant finished tool turns.")
            break

        # Execute tools
        if adapter_type == "openai":
            for tc in tool_calls:
                tools_executed += 1
                out = execute_tool_in_dir(workdir, tc["name"], tc["arguments"])
                if "WORKER_SUCCESS" in out:
                    success = True
                messages.append({
                    "role": "tool",
                    "tool_call_id": tc["id"],
                    "content": out
                })
        else:
            tool_res_blocks = []
            for tc in tool_calls:
                tools_executed += 1
                out = execute_tool_in_dir(workdir, tc["name"], tc["arguments"])
                if "WORKER_SUCCESS" in out:
                    success = True
                tool_res_blocks.append({
                    "type": "tool_result",
                    "tool_use_id": tc["id"],
                    "content": out
                })
            messages.append({"role": "user", "content": tool_res_blocks})

    # Verify disk content
    target_file = os.path.join(workdir, "app_module.py")
    if not os.path.exists(target_file):
        raise AssertionError(f"{tag} Expected file {target_file} does not exist on disk!")

    with open(target_file, "r", encoding="utf-8") as f:
        code = f.read()

    assert "compute_sum" in code, f"{tag} compute_sum missing from code"
    assert "compute_product" in code, f"{tag} compute_product missing from code"
    assert "compute_power" in code, f"{tag} compute_power missing from code"

    # Direct verification on disk
    verify_cmd = "python3 -c 'import app_module; assert app_module.compute_power(2, 4) == 16; print(\"DISK_VERIFIED\")'"
    v_res = subprocess.run(verify_cmd, shell=True, cwd=workdir, capture_output=True, text=True)
    if v_res.returncode != 0 or "DISK_VERIFIED" not in v_res.stdout:
        raise AssertionError(f"{tag} Final disk verification failed: {v_res.stderr}")

    duration = time.time() - start_time
    print(f"{tag} COMPLETED in {duration:.2f}s ({turn} turns, {tools_executed} tools executed)")

    return {
        "worker_id": worker_id,
        "adapter": adapter_type,
        "stream": stream,
        "turns": turn,
        "tools_executed": tools_executed,
        "duration": duration,
        "status": "PASS"
    }


# ==============================================================================
# Main Concurrency Orchestrator
# ==============================================================================

def main():
    print("=" * 78)
    print("🚀 HIGH-CONCURRENCY MULTI-ADAPTER STRESS TEST SUITE")
    print(f"Target Gateway: {BASE_URL}")
    print(f"OpenAI Model:   {OPENAI_MODEL}")
    print(f"Anthropic Model: {ANTHROPIC_MODEL}")
    print(f"Auth Token:     {API_KEY[:6]}***")
    print("=" * 78)

    # Initialize root workspace
    if os.path.exists(ROOT_WORKSPACE):
        shutil.rmtree(ROOT_WORKSPACE)
    os.makedirs(ROOT_WORKSPACE, exist_ok=True)

    # Define parallel stress workload:
    # 4 concurrent workers covering both OpenAI and Anthropic adapters in streaming & sync modes
    workloads = [
        {"worker_id": 1, "adapter_type": "openai", "stream": True},
        {"worker_id": 2, "adapter_type": "anthropic", "stream": True},
        {"worker_id": 3, "adapter_type": "openai", "stream": False},
        {"worker_id": 4, "adapter_type": "anthropic", "stream": False},
    ]

    print(f"Launching {len(workloads)} concurrent autonomous coding sessions...")
    test_start = time.time()
    results = []

    with concurrent.futures.ThreadPoolExecutor(max_workers=len(workloads)) as executor:
        future_map = {
            executor.submit(
                run_worker_scenario,
                w["worker_id"],
                w["adapter_type"],
                w["stream"]
            ): w
            for w in workloads
        }

        for future in concurrent.futures.as_completed(future_map):
            meta = future_map[future]
            try:
                res = future.result()
                results.append(res)
            except Exception as e:
                print(f"❌ Worker {meta['worker_id']} ({meta['adapter_type']}) FAILED: {e}")
                raise e

    total_time = time.time() - test_start
    print("\n" + "=" * 78)
    print("📊 STRESS TEST EXECUTION SUMMARY")
    print("=" * 78)
    for r in sorted(results, key=lambda x: x["worker_id"]):
        mode = "STREAM" if r["stream"] else "SYNC"
        print(f"Worker {r['worker_id']} | {r['adapter'].upper():<10} | {mode:<6} | Turns: {r['turns']:<2} | Tools: {r['tools_executed']:<2} | Latency: {r['duration']:.2f}s | {r['status']}")

    total_tools = sum(r["tools_executed"] for r in results)
    total_turns = sum(r["turns"] for r in results)
    print("-" * 78)
    print(f"All {len(results)} Concurrent Sessions Passed Successfully!")
    print(f"Total Turns Processed: {total_turns}")
    print(f"Total Tools Executed:  {total_tools}")
    print(f"Total Wall Time:       {total_time:.2f}s")
    print("=" * 78)


if __name__ == "__main__":
    main()
