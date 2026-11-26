package acp

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestSpawnProcess_StderrBurstDoesNotBlock is the regression test for the production hang where an agent
// writing more than the OS pipe capacity to stderr blocks forever because nothing read the pipe.
func TestSpawnProcess_StderrBurstDoesNotBlock(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Emit ~1 MiB to stderr (far beyond the ~64 KiB pipe buffer), then echo a sentinel on stdout.
	script := `i=0; while [ $i -lt 1024 ]; do printf '%01024d\n' $i >&2; i=$((i+1)); done; echo SENTINEL`

	proc, err := SpawnProcess(ctx, "/bin/sh", []string{"-c", script}, []string{}, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	defer func() { _ = proc.Terminate(2 * time.Second) }()

	// The child must reach its own exit within the budget. An undrained stderr pipe would block it on
	// write forever. Poll the process state rather than matching stdout bytes: exec.Cmd closes the
	// stdout read end in Wait, so a starved reader can lose the final buffered bytes and flake.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && proc.IsRunning() {
		time.Sleep(10 * time.Millisecond)
	}
	if proc.IsRunning() {
		t.Fatalf("child never exited; stderr pipe was not drained")
	}

	// The retained tail must be bounded and non-empty.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(proc.StderrTail()) >= stderrTailLimit {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	tail := proc.StderrTail()
	if len(tail) == 0 {
		t.Fatal("expected retained stderr diagnostics, got nothing")
	}
	if len(tail) > stderrTailLimit {
		t.Fatalf("stderr tail exceeds limit: %d > %d", len(tail), stderrTailLimit)
	}
}

// TestSpawnProcess_TerminateReapsGroup proves no zombie or orphan survives teardown.
func TestSpawnProcess_TerminateReapsGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	ctx := context.Background()

	proc, err := SpawnProcess(ctx, "/bin/sh", []string{"-c", "sleep 30 & sleep 30"}, []string{}, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}

	pid := proc.PID()
	if pid <= 0 {
		t.Fatalf("invalid pid %d", pid)
	}

	start := time.Now()
	if err := proc.Terminate(3 * time.Second); err != nil {
		t.Fatalf("terminate returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("terminate took too long: %v", elapsed)
	}
	if proc.IsRunning() {
		t.Fatal("process still reported as running after Terminate")
	}

	// The whole process group must be gone: signalling -pgid must fail with ESRCH.
	// SIGTERM is delivered asynchronously, so a backgrounded child can still be observable for a
	// few milliseconds after the group leader reaps. Poll briefly instead of racing the kernel.
	var probeErr error
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if probeErr = killGroupProbe(proc.PGID()); probeErr != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if probeErr == nil {
		t.Fatalf("process group %d still alive after Terminate", proc.PGID())
	}

	// Terminate must be idempotent.
	if err := proc.Terminate(time.Second); err != nil {
		t.Fatalf("second Terminate returned error: %v", err)
	}
}

// TestSpawnProcess_StderrTailIsBounded verifies the ring buffer ceiling under sustained output.
func TestSpawnProcess_StderrTailIsBounded(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// ~4 MiB to stderr, then exit naturally so nothing truncates the run.
	proc, err := SpawnProcess(ctx, "/bin/sh",
		[]string{"-c", `i=0; while [ $i -lt 4096 ]; do printf '%01024d\n' $i >&2; i=$((i+1)); done`},
		[]string{}, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}

	// Wait for natural exit before inspecting the tail.
	deadline := time.Now().Add(15 * time.Second)
	for proc.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	_ = proc.Terminate(3 * time.Second)

	tail := proc.StderrTail()
	if len(tail) > stderrTailLimit {
		t.Fatalf("stderr tail exceeded bound: %d > %d", len(tail), stderrTailLimit)
	}
	if len(tail) == 0 {
		t.Fatal("expected some retained stderr bytes")
	}
}
