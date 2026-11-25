package acp

import "syscall"

// killGroupProbe sends signal 0 to the process group to observe whether it still exists.
//
// @purpose Test-only liveness probe for a process group after teardown.
// @rationale Signal 0 performs permission and existence checks without delivering any signal, so it is the
// only reliable way to prove the group (agent + spawned MCP children) is truly gone.
//
// @param pgid Target process group id.
// @returns nil when at least one member still exists; ESRCH when the group is empty.
// @invariants Does not modify process state.
func killGroupProbe(pgid int) error {
	if pgid <= 0 {
		return syscall.ESRCH
	}
	return syscall.Kill(-pgid, syscall.Signal(0))
}
