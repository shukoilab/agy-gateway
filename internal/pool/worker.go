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
	"agy-gateway/internal/version"
)

// Worker represents a warm, isolated agy acp process instance.
type Worker struct {
	ID      int
	HomeDir string
	TmpDir  string
	Cwd     string
	// account holds the *auth.Account this worker authenticates as.
	//
	// @purpose Makes the worker's account binding safe to read while another goroutine rebinds it.
	// @rationale Lease-time reconciliation writes the binding (pool.go) while failover and account
	// sharing read it (failover.go, accountsInUse), so a plain pointer is a data race. Access is
	// confined to GetAccount/SetAccount; never read the field directly.
	// @invariants nil until the first binding; replaced wholesale, never mutated in place.
	account      atomic.Pointer[auth.Account]
	Process      *acp.Process
	Transport    *acp.Transport
	Capabilities acp.AgentCapabilities

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
	w := &Worker{
		ID:         id,
		HomeDir:    homeDir,
		TmpDir:     tmpDir,
		Cwd:        cwd,
		CreatedAt:  time.Now(),
		LastActive: time.Now(),
		cfg:        cfg,
		logger:     logger.With("workerID", id),
	}
	w.account.Store(account)
	return w
}

// GetAccount returns the account this worker currently authenticates as, or nil.
//
// @purpose Gives every reader one synchronised accessor for the worker's account binding.
// @rationale The binding is written by lease-time rotation and failover while other goroutines read
// it; a single accessor is what makes that safe without a mutex on the hot path.
// @preconditions None.
// @param None.
// @returns *auth.Account, or nil when no account is bound yet.
// @throws None.
// @invariants O(1); lock-free.
func (w *Worker) GetAccount() *auth.Account {
	return w.account.Load()
}

// SetAccount replaces the account this worker authenticates as.
//
// @purpose Publishes a new binding after the credential has been installed on disk.
// @rationale Callers install the credential first and publish second, so a concurrent reader never
// observes a binding whose credential is not yet on disk.
// @preconditions The credential for acc has been materialised into this worker's HOME.
// @param acc Account to publish; nil clears the binding.
// @returns None.
// @throws None.
// @invariants O(1); lock-free; safe for concurrent use.
func (w *Worker) SetAccount(acc *auth.Account) {
	w.account.Store(acc)
}

// Start launches the agy acp subprocess and completes the initialize handshake.
func (w *Worker) Start(ctx context.Context) error {
	if w.StartFn != nil {
		return w.StartFn(ctx)
	}

	args := acp.SpawnArgs()

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

	// Perform initialize handshake as a pure AI gateway client
	// Note: We deliberately do NOT advertise filesystem ('fs') or terminal execution ('terminal')
	// capabilities because agy-gateway delegates all tool execution to the external client (e.g. OpenCode)
	// on the host. Advertising phantom capabilities causes Antigravity ACP to declare native container
	// tools (run_command) to the LLM, leading to environment confusion and container tool failures.
	initCtx, cancel := context.WithTimeout(ctx, w.cfg.Pool.HandshakeTimeout)
	defer cancel()

	initParams := map[string]any{
		"protocolVersion": 1,
		"clientInfo": map[string]any{
			"name":    "agy-gateway",
			"title":   "Antigravity Gateway",
			"version": version.Version,
		},
		"clientCapabilities": map[string]any{
			"fs": map[string]any{
				"readTextFile":  false,
				"writeTextFile": false,
			},
			"terminal": false, // Canonical ACP v1 boolean capability
		},
	}

	var initResult map[string]any
	if err := transport.Call(initCtx, "initialize", initParams, &initResult); err != nil {
		w.Close()
		return fmt.Errorf("acp initialize failed: %w", err)
	}
	w.Capabilities = acp.ParseAgentCapabilities(initResult)

	// Reject an agent that negotiated a protocol generation we do not speak. Running on an
	// unknown protocol silently corrupts every downstream field mapping, so it is fatal here
	// and the caller recycles the worker.
	if v := w.Capabilities.ProtocolVersion; v != 0 && v != acp.SupportedProtocolVersion {
		w.Close()
		return fmt.Errorf("unsupported acp protocol version %d (gateway speaks %d)", v, acp.SupportedProtocolVersion)
	}
	w.logger.Info("warm worker process started successfully",
		"pid", proc.PID(),
		"protocolVersion", w.Capabilities.ProtocolVersion,
		"capAddDirs", w.Capabilities.SupportsAdditionalDirs,
		"capClose", w.Capabilities.SupportsSessionClose,
	)
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
	// 1. Purge transient workspace working directory files, but only after proving the root is a
	// disposable scratch directory. A misconfigured root must never destroy user data.
	if err := validateWorkspaceRoot(w.cfg.Storage.WorkspacesRootDir, w.Cwd); err != nil {
		w.logger.Error("refusing to clean workspace: guard rejected the path", "cwd", w.Cwd, "err", err)
	} else if entries, err := os.ReadDir(w.Cwd); err == nil {
		for _, entry := range entries {
			if entry.Name() == ".git" {
				continue
			}
			if err := safeRemoveEntry(w.Cwd, entry.Name()); err != nil {
				w.logger.Warn("failed to remove workspace entry", "entry", entry.Name(), "err", err)
			}
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
