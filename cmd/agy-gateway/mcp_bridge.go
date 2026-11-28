package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// runMCPBridgeSubcommand relays stdio JSON-RPC 2.0 messages from agy acp to the gateway's in-memory
// tool registry over a per-session Unix domain socket.
//
// @purpose Enables native MCP tool execution for the agent process agy spawns.
// @rationale The bridge previously spoke HTTP to the gateway and authenticated with a per-session
// bearer token. That token was deleted on session teardown while a call could still be in flight, so
// production logged 104 `mcp session token invalid` rejections in 24h — one per request — and the
// bridge then exited on 401. A Unix socket whose path lives in this worker's private TMPDIR (0700
// directory, 0600 socket) makes the filesystem the credential: only this worker's uid can connect,
// and session teardown is delivered as EOF instead of a rejected request.
// @preconditions The socket path was injected into the environment by the gateway at session/new.
// @param args Command-line arguments following "mcp-bridge".
// @returns None (exits on stdin EOF, socket EOF, or a connection that cannot be established).
// @throws None; every failure is reported as a JSON-RPC error frame so the agent never waits forever.
// @invariants Safe process termination on stdin EOF or SIGTERM; zero zombie child processes.
func runMCPBridgeSubcommand(args []string) {
	fs := flag.NewFlagSet("mcp-bridge", flag.ContinueOnError)
	socketPath := fs.String("socket", "", "Path to the gateway's per-session Unix domain socket")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "Error parsing flags: %v\n", err)
		os.Exit(1)
	}

	path := *socketPath
	if path == "" {
		// Injected by the gateway when it builds the MCP server entry. Passing the path in the
		// environment keeps it off argv, where it would be visible in `ps`.
		path = os.Getenv("AGY_GW_MCP_SOCKET")
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "Error: no MCP socket path supplied (--socket or AGY_GW_MCP_SOCKET)")
		os.Exit(1)
	}

	if err := runBridge(os.Stdin, os.Stdout, path); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

// runBridge pumps newline-delimited JSON-RPC between stdin and the gateway socket until the session
// ends.
//
// @purpose Separates the relay from process wiring so the transport is testable without spawning.
// @rationale Two independent end conditions exist (stdin EOF, socket EOF) and getting them wrong is
// exactly the "bridge outlives its session" leak this redesign removes. A third rule matters as much:
// stdin EOF must NOT discard a reply for a request already forwarded, or the agent sees silence for a
// tools/call it legitimately issued before closing.
// @preconditions socketPath names an existing Unix socket.
// @param stdin The agent's request stream.
// @param stdout Where replies are written.
// @param socketPath Gateway endpoint.
// @returns nil on a clean close of either side, or an error when the socket cannot be dialled.
// @throws Wraps the dial error only; a mid-stream socket failure is reported as a JSON-RPC error
// frame and ends the loop cleanly.
// @invariants No goroutine outlives the call; stdout writes are serialised; at most one frame is
// written per reply line.
func runBridge(stdin io.Reader, stdout io.Writer, socketPath string) error {
	conn, err := net.DialTimeout("unix", socketPath, 10*time.Second)
	if err != nil {
		// The gateway creates the socket before session/new, so a dial failure means the session is
		// already gone. Exiting is correct: the agent will reap this child.
		return fmt.Errorf("cannot connect to gateway MCP socket: %w", err)
	}
	defer func() { _ = conn.Close() }()

	out := bufio.NewWriter(stdout)
	var writeMu sync.Mutex
	write := func(frame []byte) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_, _ = out.Write(frame)
		_, _ = out.Write([]byte("\n"))
		_ = out.Flush()
	}

	// outstanding counts requests forwarded but not yet answered, so stdin EOF can wait for the last
	// reply instead of dropping it.
	var outstanding atomic.Int64
	drained := make(chan struct{}, 1)
	notifyDrain := func() {
		if outstanding.Load() == 0 {
			select {
			case drained <- struct{}{}:
			default:
			}
		}
	}

	socketDone := make(chan struct{})
	go func() {
		defer close(socketDone)
		reader := bufio.NewReaderSize(conn, 1<<20)
		for {
			line, readErr := reader.ReadBytes('\n')
			if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
				write(trimmed)
				outstanding.Add(-1)
				notifyDrain()
			}
			if readErr != nil {
				return
			}
		}
	}()

	in := bufio.NewReaderSize(stdin, 1<<20)
	for {
		select {
		case <-socketDone:
			// The gateway closed the session. Exit instead of proxying into a dead endpoint, which is
			// what the old 401 path did reactively and what leaked processes when it did not fire.
			return nil
		default:
		}

		line, readErr := in.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			outstanding.Add(1)
			if _, writeErr := conn.Write(append(trimmed, '\n')); writeErr != nil {
				writeBridgeError(stdout, trimmed, -32603, "gateway MCP socket closed: "+writeErr.Error())
				return nil
			}
		}
		if readErr != nil {
			// stdin closed by the agent: deliver any reply still in flight, then stop.
			if outstanding.Load() > 0 {
				select {
				case <-drained:
				case <-socketDone:
				case <-time.After(30 * time.Second):
				}
			}
			return nil
		}
	}
}

// writeBridgeError emits a JSON-RPC 2.0 error frame on stdout for the given request line.
//
// @purpose Prevents the upstream ACP agent from blocking forever when the gateway is unreachable.
// @rationale An unanswered MCP request stalls the whole turn; a typed error lets the agent retry or
// report the failure instead of hanging until the inference timeout.
// @preconditions reqLine must be a complete JSON-RPC request line received from stdin.
// @param w Destination stream.
// @param reqLine Raw request bytes as read from stdin.
// @param code JSON-RPC error code to report.
// @param message Human-readable error detail.
// @returns None.
// @throws None; unparsable ids are reported with a null id per JSON-RPC spec.
// @invariants O(1); single write; no shared state.
func writeBridgeError(w io.Writer, reqLine []byte, code int, message string) {
	var req struct {
		ID any `json:"id"`
	}
	_ = json.Unmarshal(reqLine, &req)
	raw, marshalErr := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      req.ID,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
	if marshalErr != nil {
		return
	}
	_, _ = w.Write(append(raw, '\n'))
}
