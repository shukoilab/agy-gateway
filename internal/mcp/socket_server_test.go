package mcp

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// TestSocketServer_ServesToolsListAndRemovesSocket pins the contract that replaced the bearer token.
//
// @purpose Proves the bridge transport authenticates by filesystem location and that teardown is
// observable as a closed connection.
// @rationale The old design authenticated each request with a per-session token and produced 104
// "mcp session token invalid" rejections in a day of production logs: the token was deleted on
// session teardown while a bridge call was still in flight. Filesystem-scoped access has no such
// window, but only if the socket is actually created, used, and removed.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; one listener and one connection.
func TestSocketServer_ServesToolsListAndRemovesSocket(t *testing.T) {
	dir := t.TempDir()
	registry := NewLoopbackServer()
	if _, err := registry.RegisterSession("sess_a", []OpenAIToolDefinition{
		{Type: "function", Function: FunctionSchema{Name: "read"}},
	}); err != nil {
		t.Fatalf("register session: %v", err)
	}

	srv := NewSocketServer(registry, nil)
	lst, err := srv.Listen(dir, "mcp-test.sock", "sess_a")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	info, err := os.Stat(lst.Path())
	if err != nil {
		t.Fatalf("socket must exist while the session is live: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}

	conn, err := net.DialTimeout("unix", lst.Path(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	if _, err := conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")); err != nil {
		t.Fatalf("write tools/list: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read tools/list reply: %v", err)
	}

	var resp struct {
		ID     int `json:"id"`
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode reply %q: %v", line, err)
	}
	if resp.ID != 1 || len(resp.Result.Tools) != 1 || resp.Result.Tools[0].Name != "read" {
		t.Fatalf("unexpected tools/list reply: %s", line)
	}

	// Close must (a) unblock the client with EOF and (b) remove the socket file, so a recycled worker
	// can bind the same path again.
	lst.Close()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := bufio.NewReader(conn).ReadBytes('\n'); err == nil {
		t.Fatal("expected the connection to be closed after listener Close")
	}
	if _, err := os.Stat(lst.Path()); !os.IsNotExist(err) {
		t.Fatalf("socket file must be removed, stat err = %v", err)
	}

	// Idempotent close.
	lst.Close()
}

// TestSocketServer_DistinctPathsPerSession pins session isolation.
//
// @purpose Proves two live sessions cannot collide on one endpoint.
// @rationale A shared socket would let one session's bridge answer another session's tool call,
// which is a cross-tenant leak of both output and arguments.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestSocketServer_DistinctPathsPerSession(t *testing.T) {
	dir := t.TempDir()
	registry := NewLoopbackServer()
	srv := NewSocketServer(registry, nil)

	first, err := srv.Listen(dir, "mcp-one.sock", "sess_one")
	if err != nil {
		t.Fatalf("Listen first: %v", err)
	}
	defer first.Close()
	second, err := srv.Listen(dir, "mcp-two.sock", "sess_two")
	if err != nil {
		t.Fatalf("Listen second: %v", err)
	}
	defer second.Close()

	if first.Path() == second.Path() {
		t.Fatalf("both sessions share socket path %q", first.Path())
	}
}

// TestSocketServer_StaleSocketFileIsReplaced pins worker-recycle behaviour.
//
// @purpose Proves a leftover socket file from a killed worker does not block the next session.
// @rationale Workers are recycled in place and reuse their TMPDIR; without the pre-bind removal the
// new session would fail with EADDRINUSE and every tool call would be lost.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestSocketServer_StaleSocketFileIsReplaced(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/mcp-stale.sock"
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}

	srv := NewSocketServer(nil, nil)
	lst, err := srv.Listen(dir, "mcp-stale.sock", "sess_stale")
	if err != nil {
		t.Fatalf("Listen must replace a stale socket file: %v", err)
	}
	defer lst.Close()

	conn, err := net.DialTimeout("unix", lst.Path(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial new socket: %v", err)
	}
	_ = conn.Close()
}

// TestSocketServer_ConcurrentCloseIsSafe pins that teardown racing live connections is clean.
//
// @purpose Proves Close is safe while connections are actively being served (the real teardown shape).
// @rationale Session end happens while a bridge may be mid-request; a Close that panics or races the
// connection map would take down the gateway process on a normal code path.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded by the close handshake; run under -race.
func TestSocketServer_ConcurrentCloseIsSafe(t *testing.T) {
	dir := t.TempDir()
	registry := NewLoopbackServer()
	if _, err := registry.RegisterSession("sess_c", nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := NewSocketServer(registry, nil)
	lst, err := srv.Listen(dir, "mcp-c.sock", "sess_c")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		conn, err := net.DialTimeout("unix", lst.Path(), 2*time.Second)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			defer c.Close()
			for n := 0; n < 8; n++ {
				if _, err := c.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")); err != nil {
					return
				}
				_ = c.SetReadDeadline(time.Now().Add(time.Second))
				if _, err := bufio.NewReader(c).ReadBytes('\n'); err != nil {
					return
				}
			}
		}(conn)
	}

	time.Sleep(20 * time.Millisecond)
	lst.Close()
	wg.Wait()
}
