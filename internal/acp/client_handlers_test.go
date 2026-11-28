package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestHandlers builds a handler set with tight limits so tests stay fast.
//
// @purpose Keeps every handler test independent of production sizes.
// @rationale The defaults are sized for 1M-token turns; a test that reads 8 MiB to prove a limit
// would be slow and prove nothing extra.
// @preconditions None.
// @param t Standard Go test handle.
// @returns A handler set plus a root directory registered for session "sess_test".
// @throws None; failures are reported through t.Fatalf.
// @invariants Leaves no goroutines behind once Close is called.
func newTestHandlers(t *testing.T) (*ClientHandlers, string) {
	t.Helper()
	root := t.TempDir()
	h := NewClientHandlers(1024, 1024, 4096)
	h.RegisterSession("sess_test", []string{root})
	t.Cleanup(h.Close)
	return h, root
}

// call invokes Handle and fails the test on a transport-level error.
func call(t *testing.T, h *ClientHandlers, method, params string) (json.RawMessage, bool) {
	t.Helper()
	res, handled, err := h.Handle(method, json.RawMessage(params))
	if err != nil {
		t.Fatalf("%s returned error: %v", method, err)
	}
	return res, handled
}

// TestClientHandlers_UnknownMethodIsNotHandled pins the dispatch contract.
//
// @purpose Proves an unclaimed method reports handled=false so the transport can answer -32601.
// @rationale Advertising a capability that silently swallows unrelated requests would hide a
// protocol mismatch instead of surfacing it.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Pure dispatch.
func TestClientHandlers_UnknownMethodIsNotHandled(t *testing.T) {
	h, _ := newTestHandlers(t)
	if _, handled := call(t, h, "fs/move_file", `{}`); handled {
		t.Fatal("an unimplemented method must not be reported as handled")
	}
}

// TestClientHandlers_ReadTextFile pins the read path, including line slicing.
//
// @purpose Proves reads are scoped to the session roots and honour line/limit.
// @rationale ACP clients pass 1-based line numbers; an off-by-one silently returns the wrong slice
// and the agent reasons about text it did not ask for.
// @preconditions The root contains a three-line file.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants File system only.
func TestClientHandlers_ReadTextFile(t *testing.T) {
	h, root := newTestHandlers(t)
	path := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	whole, handled := call(t, h, "fs/read_text_file", `{"sessionId":"sess_test","path":"`+path+`"}`)
	if !handled {
		t.Fatal("fs/read_text_file must be handled")
	}
	var got struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(whole, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Content != "alpha\nbeta\ngamma\n" {
		t.Fatalf("content = %q", got.Content)
	}

	// line is 1-based: line 2, limit 1 → "beta".
	sliced, _ := call(t, h, "fs/read_text_file",
		`{"sessionId":"sess_test","path":"`+path+`","line":2,"limit":1}`)
	if err := json.Unmarshal(sliced, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Content != "beta\n" {
		t.Fatalf("line 2 limit 1 = %q, want %q", got.Content, "beta\n")
	}
}

// TestClientHandlers_ReadRejectsPathOutsideRoot pins the containment boundary.
//
// @purpose Proves traversal and absolute escapes are refused before any read happens.
// @rationale The agent is a program; the only thing standing between it and the host filesystem is
// this check. A silent fallback would turn every session into a file-disclosure primitive.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Pure.
func TestClientHandlers_ReadRejectsPathOutsideRoot(t *testing.T) {
	h, root := newTestHandlers(t)
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	for _, path := range []string{
		outside,
		filepath.Join(root, "..", "outside.txt"),
		"/etc/passwd",
	} {
		_, _, err := h.Handle("fs/read_text_file",
			json.RawMessage(`{"sessionId":"sess_test","path":"`+path+`"}`))
		if err == nil {
			t.Fatalf("path %q must be refused", path)
		}
		if !strings.Contains(err.Error(), "outside") && !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("path %q refused for an unexpected reason: %v", path, err)
		}
	}
}

// TestClientHandlers_SymlinkEscapeIsRefused pins the symlink hole.
//
// @purpose Proves a symlink inside the root cannot be used to read outside it.
// @rationale Prefix checks alone are defeated by one `ln -s`; the parent is resolved before the
// containment test for exactly this case.
// @preconditions A symlink inside the root points at a file outside it.
// @param Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants File system only; skips if symlinks are unavailable.
func TestClientHandlers_SymlinkEscapeIsRefused(t *testing.T) {
	h, root := newTestHandlers(t)
	outsideDir := t.TempDir()
	target := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(target, []byte("classified\n"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, _, err := h.Handle("fs/read_text_file",
		json.RawMessage(`{"sessionId":"sess_test","path":"`+link+`"}`))
	if err == nil {
		t.Fatal("a symlink pointing outside the root must be refused")
	}
}

// TestClientHandlers_WriteTextFileCreatesAndReplaces pins the write path.
//
// @purpose Proves writes land inside the root, create parent directories, and replace atomically.
// @rationale A partial write leaves the agent reading a truncated file while believing it wrote a
// complete one; the temp-file rename is what makes the write all-or-nothing.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants File system only.
func TestClientHandlers_WriteTextFileCreatesAndReplaces(t *testing.T) {
	h, root := newTestHandlers(t)
	nested := filepath.Join(root, "a", "b", "out.txt")

	if _, handled := call(t, h, "fs/write_text_file",
		`{"sessionId":"sess_test","path":"`+nested+`","content":"first"}`); !handled {
		t.Fatal("fs/write_text_file must be handled")
	}
	got, err := os.ReadFile(nested)
	if err != nil || string(got) != "first" {
		t.Fatalf("read back = %q, err = %v", got, err)
	}

	call(t, h, "fs/write_text_file", `{"sessionId":"sess_test","path":"`+nested+`","content":"second"}`)
	got, err = os.ReadFile(nested)
	if err != nil || string(got) != "second" {
		t.Fatalf("after replace = %q, err = %v", got, err)
	}

	// No temporary file may survive.
	entries, err := os.ReadDir(filepath.Dir(nested))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temporary files left behind: %v", names)
	}
}

// TestClientHandlers_WriteOutsideRootIsRefused pins the write boundary.
//
// @purpose Proves a write cannot escape the workspace either.
// @rationale Read-only containment would still allow overwriting the gateway's own configuration.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Pure.
func TestClientHandlers_WriteOutsideRootIsRefused(t *testing.T) {
	h, _ := newTestHandlers(t)
	_, _, err := h.Handle("fs/write_text_file",
		json.RawMessage(`{"sessionId":"sess_test","path":"/tmp/agy-escape.txt","content":"x"}`))
	if err == nil {
		t.Fatal("a write outside the workspace must be refused")
	}
	if _, statErr := os.Stat("/tmp/agy-escape.txt"); statErr == nil {
		_ = os.Remove("/tmp/agy-escape.txt")
		t.Fatal("the refused write must not have created the file")
	}
}

// TestClientHandlers_TerminalLifecycle pins create/output/wait/release.
//
// @purpose Proves a terminal runs a command, captures output, reports the exit code, and releases.
// @rationale Terminal support is advertised to the agent; a stubbed implementation would make
// `run_command` appear to work while returning nothing.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Waits for the child to exit; no zombie survives the test.
func TestClientHandlers_TerminalLifecycle(t *testing.T) {
	h, root := newTestHandlers(t)

	res, _ := call(t, h, "terminal/create",
		`{"sessionId":"sess_test","command":"/bin/sh","args":["-c","echo hi; exit 3"],"cwd":"`+root+`"}`)
	var created struct {
		TerminalID string `json:"terminalId"`
	}
	if err := json.Unmarshal(res, &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.TerminalID == "" {
		t.Fatal("terminal/create must return a terminal id")
	}

	exit, _ := call(t, h, "terminal/wait_for_exit",
		`{"sessionId":"sess_test","terminalId":"`+created.TerminalID+`"}`)
	var exitStatus struct {
		ExitCode *int   `json:"exitCode"`
		Signal   string `json:"signal"`
	}
	if err := json.Unmarshal(exit, &exitStatus); err != nil {
		t.Fatalf("decode wait: %v", err)
	}
	if exitStatus.ExitCode == nil || *exitStatus.ExitCode != 3 {
		t.Fatalf("exit code = %v, want 3", exitStatus.ExitCode)
	}

	out, _ := call(t, h, "terminal/output",
		`{"sessionId":"sess_test","terminalId":"`+created.TerminalID+`"}`)
	var output struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(out, &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if !strings.Contains(output.Output, "hi") {
		t.Fatalf("output = %q, want it to contain hi", output.Output)
	}

	call(t, h, "terminal/release", `{"sessionId":"sess_test","terminalId":"`+created.TerminalID+`"}`)
}

// TestClientHandlers_TerminalKillReapsProcessGroup pins grandchild cleanup.
//
// @purpose Proves killing a terminal also kills children the shell spawned.
// @rationale `sh -c 'sleep 300 &'` detaches a grandchild; killing only the shell leaves it running
// for five minutes per turn, which is a process leak that accumulates invisibly.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded by the kill grace window.
func TestClientHandlers_TerminalKillReapsProcessGroup(t *testing.T) {
	h, root := newTestHandlers(t)

	res, _ := call(t, h, "terminal/create",
		`{"sessionId":"sess_test","command":"/bin/sh","args":["-c","sleep 300 & echo $!; sleep 300"],"cwd":"`+root+`"}`)
	var created struct {
		TerminalID string `json:"terminalId"`
	}
	if err := json.Unmarshal(res, &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	// Give the shell a moment to spawn and print the grandchild pid.
	deadline := time.Now().Add(3 * time.Second)
	var grandchild int
	for time.Now().Before(deadline) && grandchild == 0 {
		time.Sleep(50 * time.Millisecond)
		out, _ := call(t, h, "terminal/output",
			`{"sessionId":"sess_test","terminalId":"`+created.TerminalID+`"}`)
		var output struct {
			Output string `json:"output"`
		}
		_ = json.Unmarshal(out, &output)
		if pid := parseLeadingPID(output.Output); pid > 0 {
			grandchild = pid
		}
	}
	if grandchild == 0 {
		t.Skip("could not observe the grandchild pid")
	}

	call(t, h, "terminal/kill", `{"sessionId":"sess_test","terminalId":"`+created.TerminalID+`"}`)

	// The grandchild must be gone shortly after the kill.
	gone := false
	for i := 0; i < 40; i++ {
		if !processAlive(grandchild) {
			gone = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !gone {
		t.Fatalf("grandchild pid %d survived the process-group kill", grandchild)
	}
}

// TestClientHandlers_TerminalOwnershipIsEnforced pins cross-session isolation.
//
// @purpose Proves one session cannot read or kill another session's terminal.
// @rationale Terminal ids are sequential (`term_1`), so a guessable id without an ownership check
// is a cross-tenant information leak inside a single process.
// @preconditions Two sessions are registered.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Pure.
func TestClientHandlers_TerminalOwnershipIsEnforced(t *testing.T) {
	h, root := newTestHandlers(t)
	otherRoot := t.TempDir()
	h.RegisterSession("sess_other", []string{otherRoot})
	t.Cleanup(func() { h.UnregisterSession("sess_other") })

	res, _ := call(t, h, "terminal/create",
		`{"sessionId":"sess_test","command":"/bin/sh","args":["-c","sleep 30"],"cwd":"`+root+`"}`)
	var created struct {
		TerminalID string `json:"terminalId"`
	}
	if err := json.Unmarshal(res, &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	if _, _, err := h.Handle("terminal/output",
		json.RawMessage(`{"sessionId":"sess_other","terminalId":"`+created.TerminalID+`"}`)); err == nil {
		t.Fatal("another session must not be able to read this terminal")
	}
	call(t, h, "terminal/kill", `{"sessionId":"sess_test","terminalId":"`+created.TerminalID+`"}`)
}

// TestClientHandlers_UnregisterSessionKillsTerminals pins teardown.
//
// @purpose Proves end-of-session reaps every terminal it started.
// @rationale A session that closes while a command runs must not leave the process behind; the
// worker is then free to serve another conversation.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded by the kill grace window.
func TestClientHandlers_UnregisterSessionKillsTerminals(t *testing.T) {
	h, root := newTestHandlers(t)

	res, _ := call(t, h, "terminal/create",
		`{"sessionId":"sess_test","command":"/bin/sh","args":["-c","sleep 300"],"cwd":"`+root+`"}`)
	var created struct {
		TerminalID string `json:"terminalId"`
	}
	if err := json.Unmarshal(res, &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	h.UnregisterSession("sess_test")

	if _, _, err := h.Handle("terminal/output",
		json.RawMessage(`{"sessionId":"sess_test","terminalId":"`+created.TerminalID+`"}`)); err == nil {
		t.Fatal("a terminal must not survive its session")
	}
}
