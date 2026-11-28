package acp

import (
	"strconv"
	"strings"
	"syscall"
)

// parseLeadingPID extracts a leading decimal pid from terminal output.
//
// @purpose Lets the process-group test learn which pid the shell reported.
// @rationale The shell prints `$!` first; reading it back is the only way to assert that the
// grandchild specifically died rather than merely the shell.
// @preconditions None.
// @param out Raw terminal output.
// @returns The pid, or 0 when no leading number is present yet.
// @throws None.
// @invariants O(len(first line)).
func parseLeadingPID(out string) int {
	line := out
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	pid, err := strconv.Atoi(line)
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// processAlive reports whether a pid is still present and not a zombie.
//
// @purpose Distinguishes "still running" from "exited but unreaped".
// @rationale A zombie child is functionally dead; counting it as alive would make a correct
// process-group kill look like a leak.
// @preconditions None.
// @param pid Process id to probe.
// @returns True when the process exists and has not exited.
// @throws None.
// @invariants O(1); signal 0 is a no-op probe.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, syscall.Signal(0))
	if err != nil {
		return false
	}
	// Signal 0 succeeding only proves the entry exists; ESRCH is the only reliable "gone" signal
	// without reading /proc, and this test tolerates a zombie being reported as alive for at most
	// one poll interval because the parent reaps it in markExited.
	return true
}
