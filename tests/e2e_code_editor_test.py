#!/usr/bin/env python3
"""
End-to-End Automated Code Editor Verification Suite for agy-gateway.

@purpose Deterministically verifies multi-turn CRUD file operations (write, read, edit/patch)
and bash execution against the gateway's /v1/chat/completions endpoint using a live workspace
at /tmp/test-project.
@rationale Validates that upstream ACP tool execution loops, turn header sanitization,
polymorphic session update parsing, and stream parsing function flawlessly in an active harness setup.
@preconditions agy-gateway running and accessible on http://localhost:8182.
@invariants Thread-safe, non-destructive to any workspace outside /tmp/test-project.
"""

import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error

GATEWAY_URL = os.environ.get("GATEWAY_URL", "http://localhost:8182/v1/chat/completions")
API_KEY = os.environ.get("API_KEY", os.environ.get("AGY_GW_CLIENT_KEY_1", "sk-bf-core"))
TEST_DIR = "/tmp/test-project"
MODEL = os.environ.get("TEST_MODEL", "gemini-3.8-flash")

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
                    "content": {"type": "string", "description": "Complete text content to write."}
                },
                "required": ["path", "content"]
            }
        }
    },
    {
        "type": "function",
        "function": {
            "name": "read_file",
            "description": "Reads the entire content of a specified file.",
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
            "description": "Edits a file by replacing an exact target string with a replacement string.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "Relative file path."},
                    "target": {"type": "string", "description": "Exact text to replace."},
                    "replacement": {"type": "string", "description": "Replacement text."}
                },
                "required": ["path", "target", "replacement"]
            }
        }
    },
    {
        "type": "function",
        "function": {
            "name": "bash",
            "description": "Executes a shell command in the project directory and returns stdout/stderr.",
            "parameters": {
                "type": "object",
                "properties": {
                    "command": {"type": "string", "description": "Shell command line to execute."}
                },
                "required": ["command"]
            }
        }
    }
]


def get_arg(arguments: dict, *keys: str) -> str:
    """Helper to extract tool argument case-insensitively with fallback aliases."""
    for k in keys:
        if k in arguments and arguments[k] is not None:
            return str(arguments[k])
        for ak in arguments:
            if ak.lower() == k.lower() and arguments[ak] is not None:
                return str(arguments[ak])
    return ""


def execute_local_tool(name: str, arguments: dict) -> str:
    """Executes a requested code editor tool inside TEST_DIR."""
    print(f"  [Tool Exec] Calling {name} with args: {json.dumps(arguments)}")
    
    if name == "write_file":
        rel_path = get_arg(arguments, "path", "Path", "file_path").lstrip("/")
        if not rel_path:
            return "Error: path parameter is required"
        full_path = os.path.join(TEST_DIR, rel_path)
        os.makedirs(os.path.dirname(full_path), exist_ok=True)
        content = get_arg(arguments, "content", "Content")
        with open(full_path, "w", encoding="utf-8") as f:
            f.write(content)
        return f"Successfully wrote {len(content)} bytes to {rel_path}"

    elif name == "read_file":
        rel_path = get_arg(arguments, "path", "Path", "file_path").lstrip("/")
        if not rel_path:
            return "Error: path parameter is required"
        full_path = os.path.join(TEST_DIR, rel_path)
        if not os.path.exists(full_path):
            return f"Error: File not found: {rel_path}"
        with open(full_path, "r", encoding="utf-8") as f:
            return f.read()

    elif name == "edit_file":
        rel_path = get_arg(arguments, "path", "Path", "file_path").lstrip("/")
        if not rel_path:
            return "Error: path parameter is required"
        full_path = os.path.join(TEST_DIR, rel_path)
        if not os.path.exists(full_path):
            return f"Error: File not found: {rel_path}"
        with open(full_path, "r", encoding="utf-8") as f:
            content = f.read()
        target = get_arg(arguments, "target", "Target")
        replacement = get_arg(arguments, "replacement", "Replacement")
        if target not in content:
            return f"Error: Target text not found in {rel_path}"
        new_content = content.replace(target, replacement, 1)
        with open(full_path, "w", encoding="utf-8") as f:
            f.write(new_content)
        return f"Successfully replaced target in {rel_path}"

    elif name == "bash":
        cmd = get_arg(arguments, "command", "Command", "cmd")
        res = subprocess.run(cmd, shell=True, cwd=TEST_DIR, capture_output=True, text=True, timeout=30)
        output = res.stdout
        if res.stderr:
            output += "\nSTDERR:\n" + res.stderr
        output += f"\n[Exit Code: {res.returncode}]"
        return output.strip()

    else:
        return f"Error: Unknown tool {name}"


def call_chat_completions(messages: list) -> tuple:
    """
    Calls /v1/chat/completions with streaming enabled and parses delta tokens and tool calls.
    Returns (assistant_text, tool_calls_list).
    """
    payload = {
        "model": MODEL,
        "messages": messages,
        "tools": TOOLS,
        "tool_choice": "auto",
        "stream": True
    }
    
    req = urllib.request.Request(
        GATEWAY_URL,
        data=json.dumps(payload).encode("utf-8"),
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bearer {API_KEY}"
        }
    )

    accumulated_text = ""
    tool_calls_map = {}

    with urllib.request.urlopen(req, timeout=120) as resp:
        for line in resp:
            line_str = line.decode("utf-8").strip()
            if not line_str.startswith("data: "):
                continue
            data_part = line_str[6:].strip()
            if data_part == "[DONE]":
                break
            
            try:
                chunk = json.loads(data_part)
            except json.JSONDecodeError:
                continue

            choices = chunk.get("choices", [])
            if not choices:
                continue
            delta = choices[0].get("delta", {})

            # Content delta
            if "content" in delta and delta["content"]:
                accumulated_text += delta["content"]

            # Tool call deltas
            if "tool_calls" in delta and delta["tool_calls"]:
                for tc in delta["tool_calls"]:
                    idx = tc.get("index", 0)
                    if idx not in tool_calls_map:
                        tool_calls_map[idx] = {
                            "id": tc.get("id", f"call_{idx}"),
                            "type": "function",
                            "function": {
                                "name": tc.get("function", {}).get("name", ""),
                                "arguments": ""
                            }
                        }
                    fn = tc.get("function", {})
                    if "name" in fn and fn["name"]:
                        tool_calls_map[idx]["function"]["name"] = fn["name"]
                    if "arguments" in fn and fn["arguments"]:
                        tool_calls_map[idx]["function"]["arguments"] += fn["arguments"]

    # Sort tool calls by index
    tool_calls = [tool_calls_map[k] for k in sorted(tool_calls_map.keys())]
    return accumulated_text, tool_calls


def main():
    print(f"=== E2E Code Editor Test Starting on {GATEWAY_URL} ===")
    
    # Setup clean workspace
    if os.path.exists(TEST_DIR):
        shutil.rmtree(TEST_DIR)
    os.makedirs(TEST_DIR, exist_ok=True)
    print(f"Initialized clean test workspace at {TEST_DIR}")

    system_instruction = (
        "You are an autonomous senior software engineer. You have tools to write_file, read_file, edit_file, and bash.\n"
        "Your task:\n"
        "1. Create math_utils.py with add(a, b) and multiply(a, b).\n"
        "2. Read math_utils.py to inspect the created code.\n"
        "3. Edit math_utils.py to add power(base, exponent) function.\n"
        "4. Run a bash command to verify: python3 -c 'import math_utils; assert math_utils.power(2, 3) == 8; print(\"ALL TESTS PASSED\")'\n"
        "Execute these steps systematically using tool calls."
    )

    messages = [
        {"role": "system", "content": system_instruction},
        {"role": "user", "content": "Please start the development workflow in /tmp/test-project now."}
    ]

    max_turns = 10
    turn = 0
    all_tests_passed = False

    while turn < max_turns:
        turn += 1
        print(f"\n--- Turn {turn} ---")
        
        assistant_text, tool_calls = call_chat_completions(messages)
        
        # Check for prohibited leakage of turn headers
        if "User: [Tool Execution Results]" in assistant_text:
            raise AssertionError(f"LEAK DETECTED: 'User: [Tool Execution Results]' leaked into assistant text:\n{assistant_text}")
        if assistant_text.strip().startswith("Assistant:"):
            raise AssertionError(f"LEAK DETECTED: 'Assistant:' turn prefix leaked into assistant text:\n{assistant_text}")

        if assistant_text:
            print(f"[Assistant Text]: {assistant_text.strip()[:200]}...")

        # Construct assistant message for history
        asst_msg = {"role": "assistant"}
        if assistant_text:
            asst_msg["content"] = assistant_text
        if tool_calls:
            asst_msg["tool_calls"] = tool_calls
        messages.append(asst_msg)

        if not tool_calls:
            print("[Turn Complete] No further tool calls emitted by assistant.")
            break

        # Execute each tool call and append tool output message
        for tc in tool_calls:
            tc_id = tc["id"]
            fn_name = tc["function"]["name"]
            raw_args = tc["function"]["arguments"]
            try:
                args = json.loads(raw_args) if raw_args else {}
            except json.JSONDecodeError:
                args = {}

            output = execute_local_tool(fn_name, args)
            print(f"  [Tool Output ({tc_id})]: {output[:150]}")

            if "ALL TESTS PASSED" in output:
                all_tests_passed = True

            messages.append({
                "role": "tool",
                "tool_call_id": tc_id,
                "content": output
            })

        # The workflow is complete once the verification bash command passes. Without this the
        # loop keeps calling the agent after success, and the model replays the task from the
        # top -- overwriting the edits it already made.
        if all_tests_passed:
            print("[Turn Complete] Verification passed; stopping workflow.")
            break

    # Verifications on Disk
    math_utils_path = os.path.join(TEST_DIR, "math_utils.py")
    if not os.path.exists(math_utils_path):
        raise AssertionError(f"Expected file {math_utils_path} does not exist on disk!")

    with open(math_utils_path, "r", encoding="utf-8") as f:
        file_content = f.read()

    print("\n=== Final math_utils.py Content on Disk ===")
    print(file_content)

    assert "def add" in file_content, "Missing 'def add' in math_utils.py"
    assert "def multiply" in file_content, "Missing 'def multiply' in math_utils.py"
    assert "def power" in file_content, "Missing 'def power' in math_utils.py"

    # Run bash verification directly
    verify_cmd = "python3 -c 'import math_utils; assert math_utils.add(2, 3) == 5; assert math_utils.multiply(3, 4) == 12; assert math_utils.power(2, 4) == 16; print(\"VERIFIED DISK FUNCTIONS\")'"
    v_res = subprocess.run(verify_cmd, shell=True, cwd=TEST_DIR, capture_output=True, text=True)
    if v_res.returncode != 0 or "VERIFIED DISK FUNCTIONS" not in v_res.stdout:
        raise AssertionError(f"Disk function verification failed: {v_res.stderr}")

    print("\n[SUCCESS] All CRUD file operations, bash execution, and disk state verified!")
    print(f"[SUCCESS] All Tests Passed flag: {all_tests_passed or 'VERIFIED DISK FUNCTIONS' in v_res.stdout}")


if __name__ == "__main__":
    main()
