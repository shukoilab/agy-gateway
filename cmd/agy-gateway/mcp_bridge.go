package main

import (
	"bufio"
	"bytes"
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
		activePort := *port
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
		targetURL = fmt.Sprintf("http://127.0.0.1:%d/internal/mcp/%s", activePort, *sessionID)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

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

				resp, respErr := client.Do(req)
				if respErr != nil {
					fmt.Fprintf(os.Stderr, "HTTP request failed: %v\n", respErr)
					continue
				}

				respBody, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					fmt.Fprintf(os.Stderr, "Failed to read response: %v\n", readErr)
					continue
				}

				if len(respBody) > 0 && resp.StatusCode == http.StatusOK {
					respBody = bytes.TrimSpace(respBody)
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
