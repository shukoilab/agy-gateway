// Package pool manages a resident pool of agy acp YOLO workers with 1:1 channel leases,
// transparent failover, and proactive process recycling.
package pool

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"agy-gateway/internal/acp"
	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
)

// Worker represents a warm, isolated agy acp process instance.
type Worker struct {
	ID        int
	HomeDir   string
	TmpDir    string
	Cwd       string
	Account   *auth.Account
	Process   *acp.Process
	Transport *acp.Transport

	TaintedForRecycle atomic.Bool
	RequestCount      atomic.Int64
	CreatedAt         time.Time
	LastActive        time.Time

	cfg    *config.Config
	logger *slog.Logger

	// StartFn optionally overrides subprocess spawning for testing or custom process isolation.
	StartFn func(ctx context.Context) error
}

// NewWorker initializes a worker struct with isolated filesystem paths.
func NewWorker(id int, homeDir, tmpDir, cwd string, account *auth.Account, cfg *config.Config, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		ID:         id,
		HomeDir:    homeDir,
		TmpDir:     tmpDir,
		Cwd:        cwd,
		Account:    account,
		CreatedAt:  time.Now(),
		LastActive: time.Now(),
		cfg:        cfg,
		logger:     logger.With("workerID", id),
	}
}

// Start launches the agy acp subprocess and completes the initialize handshake.
func (w *Worker) Start(ctx context.Context) error {
	if w.StartFn != nil {
		return w.StartFn(ctx)
	}

	args := w.cfg.Binaries.AgyAcpArgs
	if len(args) == 0 {
		args = []string{"acp", "--dangerously-skip-permissions"}
	}

	env := append(os.Environ(),
		fmt.Sprintf("HOME=%s", w.HomeDir),
		fmt.Sprintf("XDG_CONFIG_HOME=%s", filepath.Join(w.HomeDir, ".config")),
		fmt.Sprintf("XDG_CACHE_HOME=%s", filepath.Join(w.TmpDir, "cache")),
		fmt.Sprintf("TMPDIR=%s", w.TmpDir),
		"DBUS_SESSION_BUS_ADDRESS=",
		"AGY_AUTH_NO_UPDATE_CHECK=1",
	)
	if proxyEnvs := w.cfg.Proxy.EnvSlice(); len(proxyEnvs) > 0 {
		env = append(env, proxyEnvs...)
	}

	proc, err := acp.SpawnProcess(context.Background(), w.cfg.Binaries.AgyPath, args, env, w.Cwd, w.logger)
	if err != nil {
		return fmt.Errorf("failed to spawn agy acp: %w", err)
	}

	transport := acp.NewTransport(proc.Stdin(), proc.Stdout(), w.cfg.Pool.MaxScannerBufferBytes, w.logger)

	w.Process = proc
	w.Transport = transport

	// Perform initialize handshake mimicking standard ACP editor (Zed)
	initCtx, cancel := context.WithTimeout(ctx, w.cfg.Pool.HandshakeTimeout)
	defer cancel()

	initParams := map[string]any{
		"protocolVersion": 1,
		"clientInfo": map[string]any{
			"name":    "zed",
			"title":   "Zed",
			"version": "0.214.7",
		},
		"clientCapabilities": map[string]any{
			"fs": map[string]any{
				"readTextFile":  true,
				"writeTextFile": true,
			},
			"terminal": map[string]any{
				"create": true,
			},
		},
		"capabilities": map[string]any{
			"fs":       true,
			"terminal": true,
		},
	}

	var initResult any
	if err := transport.Call(initCtx, "initialize", initParams, &initResult); err != nil {
		w.Close()
		return fmt.Errorf("acp initialize failed: %w", err)
	}

	// Warm-up pause to ensure ACP silent keyring authorization completes
	time.Sleep(500 * time.Millisecond)

	w.logger.Info("warm worker process started successfully", "pid", proc.PID())
	return nil
}

// CleanWorkspace removes transient files from the worker workspace and purges
// ephemeral ACP session databases and brain artifacts between requests.
//
// @purpose Eliminates storage and memory leakages across sequential requests on pooled workers.
// @rationale Enforces true JIT isolation and prevents unbounded disk growth from internal ACP SQLite session DBs.
// @invariants Safe against missing directories and concurrent read operations.
func (w *Worker) CleanWorkspace() {
	if !w.cfg.Execution.CleanWorkspaceBetweenRequests {
		return
	}
	// 1. Purge transient workspace working directory files
	entries, err := os.ReadDir(w.Cwd)
	if err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if name == ".git" {
				continue
			}
			_ = os.RemoveAll(filepath.Join(w.Cwd, name))
		}
	}

	// 2. Purge ephemeral ACP session databases, WAL/SHM journals, and brain artifacts in worker home
	if w.HomeDir != "" {
		geminiCliDir := filepath.Join(w.HomeDir, ".gemini", "antigravity-cli")
		convDir := filepath.Join(geminiCliDir, "conversations")
		if convEntries, err := os.ReadDir(convDir); err == nil {
			for _, ce := range convEntries {
				// Purge all ephemeral conversation databases, WAL journals, and SHM shared memory files
				_ = os.RemoveAll(filepath.Join(convDir, ce.Name()))
			}
		}
		brainDir := filepath.Join(geminiCliDir, "brain")
		if brainEntries, err := os.ReadDir(brainDir); err == nil {
			for _, be := range brainEntries {
				_ = os.RemoveAll(filepath.Join(brainDir, be.Name()))
			}
		}
	}
}

// Close gracefully terminates the worker process.
func (w *Worker) Close() {
	if w.Transport != nil {
		_ = w.Transport.Close()
	}
	if w.Process != nil {
		_ = w.Process.Terminate(w.cfg.Pool.ProcessKillGracePeriod)
	}
}
