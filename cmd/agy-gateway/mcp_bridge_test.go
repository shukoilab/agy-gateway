package main

import (
	"bytes"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBridgeRelaysAndExitsOnSocketEOF pins S13: the bridge relays frames in both directions and
// terminates when the gateway closes the session socket.
//
// @purpose Proves the replacement transport has the property the token path lacked.
// @rationale The old bridge exited only on a 401, which arrived one request late for a session that
// was already gone and never arrived at all when the gateway restarted. Closing the socket is the
// single unambiguous end-of-session signal, so the bridge must treat EOF as exit, not as a retry.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded by a 5-second deadline; no goroutine survives the test.
func TestBridgeRelaysAndExitsOnSocketEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		n, _ := conn.Read(buf)
		// Echo one reply frame, then close: the close is the session-teardown signal.
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":7,"result":{"ok":true}}` + "\n"))
		_ = n
	}()

	stdin := strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}` + "\n")
	var stdout bytes.Buffer

	done := make(chan error, 1)
	go func() { done <- runBridge(stdin, &stdout, path) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runBridge: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not exit after the gateway closed the socket")
	}

	<-serverDone

	line := strings.TrimSpace(stdout.String())
	if line == "" {
		t.Fatal("bridge forwarded no reply to stdout")
	}
	var frame struct {
		ID     int               `json:"id"`
		Result struct{ OK bool } `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("reply is not valid JSON: %v (%q)", err, line)
	}
	if frame.ID != 7 || !frame.Result.OK {
		t.Fatalf("unexpected reply frame: %s", line)
	}
}

// TestBridgeExitsCleanlyWhenSocketClosesMidStream pins the no-hang rule on the new transport.
//
// @purpose Proves a socket that dies mid-turn ends the bridge instead of blocking forever.
// @rationale The old bridge stayed alive through a gateway restart and only reacted to a 401 that
// never arrived; a bridge that blocks on a dead socket leaks a process per agent session.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded by a 5-second deadline.
func TestBridgeExitsCleanlyWhenSocketClosesMidStream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp2.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr == nil {
			_ = conn.Close()
		}
	}()

	stdin := strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"tools/call"}` + "\n")
	var stdout bytes.Buffer

	done := make(chan error, 1)
	go func() { done <- runBridge(stdin, &stdout, path) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runBridge must end cleanly when the session ends, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bridge blocked after the gateway closed the socket")
	}
	_ = ln.Close()
}

// TestWriteBridgeErrorFramesTheRequestID pins the error framing that keeps the agent from stalling.
//
// @purpose Proves a transport failure is reported as a JSON-RPC error carrying the request's id.
// @rationale An unanswered tools/call stalls the agent until the inference timeout; the echoed id is
// what lets it fail that specific call instead of hanging or mismatching the reply.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Pure; no I/O.
func TestWriteBridgeErrorFramesTheRequestID(t *testing.T) {
	var buf bytes.Buffer
	writeBridgeError(&buf, []byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call"}`), -32603, "socket closed")

	var frame struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	raw := strings.TrimSpace(buf.String())
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		t.Fatalf("error frame is not valid JSON: %v (%q)", err, raw)
	}
	if frame.JSONRPC != "2.0" || frame.ID != 9 || frame.Error.Code != -32603 || frame.Error.Message == "" {
		t.Fatalf("unexpected error frame: %s", raw)
	}
}

// TestBridgeRequiresSocketPath pins the fail-fast configuration rule.
//
// @purpose Proves a bridge without an endpoint never starts half-working.
// @rationale Silently falling back to a default port would reconnect to a different gateway instance,
// which is the class of cross-instance coupling the socket design removes.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Pure; no filesystem or network access.
func TestBridgeRequiresSocketPath(t *testing.T) {
	if err := runBridge(strings.NewReader(""), &bytes.Buffer{}, filepath.Join(t.TempDir(), "absent.sock")); err == nil {
		t.Fatal("expected an error when the socket does not exist")
	}
}
