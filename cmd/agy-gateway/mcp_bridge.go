package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// runMCPBridgeSubcommand relays stdio JSON-RPC 2.0 messages from agy acp to the gateway's in-memory LoopbackServer over HTTP.
//
// @purpose Enables native MCP execution for child processes spawned by agy acp.
// @rationale ACP agents launch stdio MCP servers via command & args. This bridge connects agy acp to the gateway's
// centralized in-memory loopback server without writing temporary socket files or managing separate daemons.
// @preconditions Target URL must be reachable.
// @param args Command-line arguments following "mcp-bridge".
// @returns None (exits on EOF or fatal error).
// @throws None.
// @invariants Safe process termination on stdin EOF or SIGTERM; zero zombie child processes.
func runMCPBridgeSubcommand(args []string) {
	fs := flag.NewFlagSet("mcp-bridge", flag.ContinueOnError)
	sessionID := fs.String("session-id", "", "Ephemeral session correlation ID")
	url := fs.String("url", "", "Target LoopbackServer HTTP endpoint URL")
	port := fs.Int("port", 8080, "Fallback port if url is omitted")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "Error parsing flags: %v\n", err)
		os.Exit(1)
	}

	targetURL := *url
	if targetURL == "" {
		if *sessionID == "" {
			fmt.Fprintf(os.Stderr, "Error: --session-id or --url must be specified\n")
			os.Exit(1)
		}
		// Precedence: explicit --port flag > ambient env. The parent gateway always passes --port
		// derived from its own configured listen port, so an inherited AGY_GW_PORT/PORT from an
		// unrelated process must never redirect the bridge to a different instance.
		activePort := *port
		portExplicit := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "port" {
				portExplicit = true
			}
		})
		if !portExplicit {
			if envPort := os.Getenv("AGY_GW_PORT"); envPort != "" {
				var p int
				if _, err := fmt.Sscanf(envPort, "%d", &p); err == nil && p > 0 {
					activePort = p
				}
			} else if envPort := os.Getenv("PORT"); envPort != "" {
				var p int
				if _, err := fmt.Sscanf(envPort, "%d", &p); err == nil && p > 0 {
					activePort = p
				}
			}
		}
		targetURL = fmt.Sprintf("http://127.0.0.1:%d/internal/mcp/%s", activePort, *sessionID)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// AGY_GW_MCP_TOKEN is injected by the gateway into this process's environment at
	// session/new time. It authenticates this bridge against POST /internal/mcp/<session>.
	mcpToken := os.Getenv("AGY_GW_MCP_TOKEN")

	reader := bufio.NewReaderSize(os.Stdin, 1024*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				req, reqErr := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(trimmed))
				if reqErr != nil {
					fmt.Fprintf(os.Stderr, "Failed to create request: %v\n", reqErr)
					continue
				}
				req.Header.Set("Content-Type", "application/json")
				if mcpToken != "" {
					req.Header.Set("Authorization", "Bearer "+mcpToken)
				}

				resp, respErr := client.Do(req)
				if respErr != nil {
					fmt.Fprintf(os.Stderr, "HTTP request failed: %v\n", respErr)
					writeBridgeError(trimmed, -32603, "gateway unreachable: "+respErr.Error())
					continue
				}

				respBody, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					fmt.Fprintf(os.Stderr, "Failed to read response: %v\n", readErr)
					writeBridgeError(trimmed, -32603, "gateway response unreadable: "+readErr.Error())
					continue
				}

				respBody = bytes.TrimSpace(respBody)
				// 204 No Content is the correct reply for JSON-RPC notifications (no id): there is
				// nothing to forward. Treating it as a failure injects a spurious error frame that
				// corrupts the MCP handshake and makes the agent silently drop the tool registry.
				if resp.StatusCode == http.StatusNoContent {
					continue
				}
				if resp.StatusCode != http.StatusOK {
					fmt.Fprintf(os.Stderr, "gateway returned HTTP %d: %s\n", resp.StatusCode, string(respBody))
					if bridgeShouldExit(resp.StatusCode) {
						fmt.Fprintln(os.Stderr, "bridge session ended by the gateway; exiting")
						return
					}
					// A transport-level failure must still reach the agent as a JSON-RPC error frame,
					// otherwise the agent waits forever for a response that will never come.
					writeBridgeError(trimmed, -32603, fmt.Sprintf("gateway HTTP %d: %s", resp.StatusCode, string(respBody)))
					continue
				}

				if len(respBody) > 0 {
					_, _ = os.Stdout.Write(respBody)
					_, _ = os.Stdout.Write([]byte("\n"))
					_ = os.Stdout.Sync()
				}
			}
		}

		if err != nil {
			break // EOF or stdin pipe closed
		}
	}
}

// bridgeShouldExit reports whether a non-200 gateway response is unrecoverable.
//
// @purpose Centralises the one status that must terminate the bridge instead of retrying.
// @rationale The loopback token is minted per gateway session and the gateway deletes it on
// unregister. Once the gateway answers 401 `mcp session token invalid`, every later frame gets the
// same answer, so staying alive only leaks a process for the lifetime of the agent. Any other status
// (502, 503, 500) may be a transient gateway restart and must remain retryable.
// @preconditions None.
// @param statusCode The status the gateway returned.
// @returns true only for http.StatusUnauthorized.
// @throws None.
// @invariants Pure; O(1).
func bridgeShouldExit(statusCode int) bool {
	return statusCode == http.StatusUnauthorized
}

// writeBridgeError emits a JSON-RPC 2.0 error frame on stdout for the given request line.
//
// @purpose Prevents the upstream ACP agent from blocking forever when the gateway is unreachable.
// @rationale An unanswered MCP request stalls the whole turn; a typed error lets the agent retry or
// report the failure instead of hanging until the inference timeout.
// @preconditions reqLine must be a complete JSON-RPC request line received from stdin.
// @param reqLine Raw request bytes as read from stdin.
// @param code JSON-RPC error code to report.
// @param message Human-readable error detail.
// @returns None; writes to stdout and syncs.
// @throws None; unparsable ids are reported with a null id per JSON-RPC spec.
// @invariants O(1); single stdout write; no shared state.
func writeBridgeError(reqLine []byte, code int, message string) {
	var req struct {
		ID any `json:"id"`
	}
	_ = json.Unmarshal(reqLine, &req)
	errFrame := map[string]any{
		"jsonrpc": "2.0",
		"id":      req.ID,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}
	raw, marshalErr := json.Marshal(errFrame)
	if marshalErr != nil {
		return
	}
	_, _ = os.Stdout.Write(raw)
	_, _ = os.Stdout.Write([]byte("\n"))
	_ = os.Stdout.Sync()
}
