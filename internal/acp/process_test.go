package acp

import (
	"context"
	"os/exec"
	"strings"
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

	// Reading stdout must complete; if stderr were unread the child would never reach the echo.
	buf := make([]byte, 64)
	deadline := time.Now().Add(15 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		n, readErr := proc.Stdout().Read(buf)
		if n > 0 {
			got += string(buf[:n])
			if strings.Contains(got, "SENTINEL") {
				break
			}
		}
		if readErr != nil {
			break
		}
	}

	if !strings.Contains(got, "SENTINEL") {
		t.Fatalf("child never reached its final write; stderr pipe was not drained (got %q)", got)
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
	if err := killGroupProbe(proc.PGID()); err == nil {
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
