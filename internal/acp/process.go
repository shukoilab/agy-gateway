package acp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Process manages an OS child process isolated in its own process group.
type Process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	logger *slog.Logger

	pgid int
	pid  int

	running atomic.Bool
	doneCh  chan struct{}
	waitErr error
	waitMu  sync.Mutex

	stderrDrained chan struct{}
	stderrMu      sync.Mutex
	stderrTail    []byte
}

// stderrTailLimit bounds retained stderr diagnostics per process.
const stderrTailLimit = 64 * 1024

// maxStderrLineLen bounds a single logged stderr line so a pathological child cannot flood the log.
const maxStderrLineLen = 2048

// SpawnProcess initiates a child process configured with OS process group isolation.
//
// @purpose Creates an isolated agy acp (or other) child with its own process group for clean group-wide teardown.
// @rationale Setpgid+Pgid:0 makes PGID == PID, so signalling -pgid reaches the agent and every MCP child it
// spawns, guaranteeing no orphan or zombie survives a recycle or shutdown. stderr is drained on its own
// goroutine because an unread pipe blocks the child once the OS buffer (~64 KiB) fills, which presents as a
// permanently hung worker with no diagnostic trail.
//
// @preconditions binaryPath must be executable; env should already include HOME/XDG overrides.
// @param ctx Lifetime context; cancelling it signals the child (see Terminate for graceful handling).
// @param binaryPath Absolute path of the agent binary.
// @param args Argument vector excluding argv[0].
// @param env Complete environment slice.
// @param cwd Working directory for the child.
// @param logger Optional structured logger (defaults to slog.Default).
// @returns Live Process handle with running=true.
// @throws Returns a wrapped error when any pipe cannot be created or the process cannot start; all
// partially opened descriptors are closed on every failure path.
// @invariants One reaper goroutine per process awaits cmd.Wait(); one drain goroutine per process reads
// stderr to EOF; doneCh is closed exactly once.
func SpawnProcess(ctx context.Context, binaryPath string, args []string, env []string, cwd string, logger *slog.Logger) (*Process, error) {
	if logger == nil {
		logger = slog.Default()
	}

	cmd := exec.CommandContext(ctx, binaryPath, args...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Pgid:    0,
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to open stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("failed to open stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("failed to open stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("failed to start process: %w", err)
	}

	p := &Process{
		cmd:           cmd,
		stdin:         stdin,
		stdout:        stdout,
		stderr:        stderr,
		logger:        logger,
		pid:           cmd.Process.Pid,
		pgid:          cmd.Process.Pid, // since Pgid: 0 and Setpgid: true, PGID == PID
		doneCh:        make(chan struct{}),
		stderrDrained: make(chan struct{}),
	}
	p.running.Store(true)

	// Supervised cmd.Wait() reaper goroutine to prevent zombie processes
	go func() {
		defer close(p.doneCh)
		p.waitErr = cmd.Wait()
		p.running.Store(false)
		p.logger.Debug("child process exited", "pid", p.pid, "pgid", p.pgid, "err", p.waitErr)
	}()

	go p.drainStderr()

	return p, nil
}

// drainStderr continuously consumes the child's stderr until EOF.
//
// @purpose Prevents the child from blocking on a full stderr pipe and preserves diagnostics.
// @rationale Nothing else in the process tree reads stderr. Without this goroutine, any agent that writes
// more than the OS pipe capacity (~64 KiB, e.g. a stack trace or repeated auth warnings) blocks forever on
// write while holding whatever lock it had, which surfaces as a hung inference turn.
//
// @preconditions p.stderr must be a readable pipe owned by this process.
// @param None.
// @returns None; closes p.stderrDrained on exit.
// @throws None.
// @invariants Retains at most stderrTailLimit bytes; never blocks the writer; O(n) over produced bytes.
func (p *Process) drainStderr() {
	defer close(p.stderrDrained)
	if p.stderr == nil {
		return
	}
	reader := bufio.NewReaderSize(p.stderr, 32*1024)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			p.recordStderr(line)
		}
		if err != nil {
			return
		}
	}
}

// recordStderr retains a bounded tail of stderr and logs notable lines.
func (p *Process) recordStderr(line string) {
	trimmed := strings.TrimRight(line, "\r\n")
	if trimmed == "" {
		return
	}
	if len(trimmed) > maxStderrLineLen {
		trimmed = trimmed[:maxStderrLineLen] + "…[truncated]"
	}

	p.stderrMu.Lock()
	p.stderrTail = append(p.stderrTail, line...)
	if over := len(p.stderrTail) - stderrTailLimit; over > 0 {
		p.stderrTail = append(p.stderrTail[:0], p.stderrTail[over:]...)
	}
	p.stderrMu.Unlock()

	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "error") || strings.Contains(lower, "fatal") ||
		strings.Contains(lower, "panic") || strings.Contains(lower, "denied") {
		p.logger.Warn("agent stderr", "pid", p.pid, "line", trimmed)
		return
	}
	p.logger.Debug("agent stderr", "pid", p.pid, "line", trimmed)
}

// StderrTail returns a copy of the retained stderr tail for diagnostics.
//
// @purpose Exposes recent agent diagnostics without re-reading the closed pipe.
// @returns Up to stderrTailLimit trailing bytes; empty when nothing was written.
// @invariants Snapshot copy under lock; safe after the process exited.
func (p *Process) StderrTail() string {
	p.stderrMu.Lock()
	defer p.stderrMu.Unlock()
	return string(p.stderrTail)
}

// PID returns the OS process ID.
func (p *Process) PID() int {
	return p.pid
}

// PGID returns the OS process group ID.
func (p *Process) PGID() int {
	return p.pgid
}

// IsRunning indicates if the process is currently active.
func (p *Process) IsRunning() bool {
	return p.running.Load()
}

// Stdin returns the process stdin writer.
func (p *Process) Stdin() io.WriteCloser {
	return p.stdin
}

// Stdout returns the process stdout reader.
func (p *Process) Stdout() io.ReadCloser {
	return p.stdout
}

// Stderr returns the process stderr reader.
//
// @purpose Compatibility accessor for callers that want the raw pipe.
// @rationale The pipe is already consumed by the internal drain goroutine; reading it from elsewhere would
// steal bytes and re-introduce the block-forever failure this package now prevents. Prefer StderrTail().
//
// @returns The stderr pipe handle (already being drained internally).
// @invariants Reading from the returned reader races the drain goroutine; use StderrTail instead.
func (p *Process) Stderr() io.ReadCloser {
	return p.stderr
}

// Terminate cleanly shuts down the process group via SIGTERM followed by SIGKILL.
//
// @purpose Guarantees the agent and every MCP child it spawned are gone before returning.
// @rationale Signalling the process group (-pgid) rather than the pid reaches grandchildren (the stdio MCP
// bridge) that would otherwise survive as orphans. The final `<-p.doneCh` is unconditional so a SIGKILLed
// child is always reaped, leaving no zombie.
//
// @preconditions gracePeriod should exceed the agent's own shutdown budget.
// @param gracePeriod Time to wait after SIGTERM before escalating to SIGKILL.
// @returns nil once the process exited and was reaped; safe to call on an already-exited process.
// @throws None; signal errors are logged, not returned.
// @invariants Serialised by waitMu; awaits both the reaper and the stderr drain before returning.
func (p *Process) Terminate(gracePeriod time.Duration) error {
	if !p.running.Load() {
		p.awaitStderrDrain(time.Second)
		return nil
	}

	p.waitMu.Lock()
	defer p.waitMu.Unlock()

	// 1. Close stdin to signal EOF
	_ = p.stdin.Close()

	// 2. Send SIGTERM to entire process group (-pgid)
	if p.pgid > 0 {
		_ = syscall.Kill(-p.pgid, syscall.SIGTERM)
	} else if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	}

	// 3. Wait for exit or timeout
	select {
	case <-p.doneCh:
		_ = p.stdout.Close()
		p.awaitStderrDrain(time.Second)
		_ = p.stderr.Close()
		return nil
	case <-time.After(gracePeriod):
		p.logger.Warn("process group did not terminate within grace period; sending SIGKILL", "pgid", p.pgid)
		// 4. Send SIGKILL to entire process group (-pgid)
		if p.pgid > 0 {
			_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
		} else if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	}

	// 5. Await final reap
	<-p.doneCh
	_ = p.stdout.Close()
	p.awaitStderrDrain(time.Second)
	_ = p.stderr.Close()
	return nil
}

// awaitStderrDrain waits for the stderr drain goroutine, bounded by timeout.
func (p *Process) awaitStderrDrain(timeout time.Duration) {
	if p.stderrDrained == nil {
		return
	}
	select {
	case <-p.stderrDrained:
	case <-time.After(timeout):
	}
}
