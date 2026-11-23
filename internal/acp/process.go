package acp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
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
}

// SpawnProcess initiates a child process configured with OS process group isolation.
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
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		logger: logger,
		pid:    cmd.Process.Pid,
		pgid:   cmd.Process.Pid, // since Pgid: 0 and Setpgid: true, PGID == PID
		doneCh: make(chan struct{}),
	}
	p.running.Store(true)

	// Supervised cmd.Wait() reaper goroutine to prevent zombie processes
	go func() {
		defer close(p.doneCh)
		p.waitErr = cmd.Wait()
		p.running.Store(false)
		p.logger.Debug("child process exited", "pid", p.pid, "pgid", p.pgid, "err", p.waitErr)
	}()

	return p, nil
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
func (p *Process) Stderr() io.ReadCloser {
	return p.stderr
}

// Terminate cleanly shuts down the process group via SIGTERM followed by SIGKILL.
func (p *Process) Terminate(gracePeriod time.Duration) error {
	if !p.running.Load() {
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
	_ = p.stderr.Close()
	return nil
}
