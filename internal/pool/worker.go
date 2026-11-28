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
	// Install the fs/terminal handlers whenever either capability is advertised. Advertising without
	// a handler makes the agent's read_file/write_file/run_command requests go unanswered, which
	// stalls the turn until the idle watchdog cancels it.
	if w.cfg.Capabilities.FS || w.cfg.Capabilities.Terminal {
		transport.SetHandlers(acp.NewClientHandlers(0, 0, 0))
	}

	// Perform initialize handshake as a pure AI gateway client. The payload mirrors the reference
	// client (docs/acp-conformance.md §3): a real client fingerprint plus the full capability
	// block. Both come from configuration so an operator can select the gateway profile or the
	// sandbox profile without a rebuild, and so no literal lives at the call site.
	initCtx, cancel := context.WithTimeout(ctx, w.cfg.Pool.HandshakeTimeout)
	defer cancel()

	initParams := acp.BuildInitializeParams(
		acp.ClientIdentity{
			Name:    w.cfg.Identity.Name,
			Title:   w.cfg.Identity.Title,
			Version: w.cfg.Identity.Version,
		},
		acp.ClientCapabilities{
			FS:              w.cfg.Capabilities.FS,
			Terminal:        w.cfg.Capabilities.Terminal,
			Elicitation:     w.cfg.Capabilities.Elicitation,
			SessionMetadata: w.cfg.Capabilities.SessionMetadata,
		},
	)

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

// PurgeAccountCaches removes the agent caches that belong to the account this worker HOME held.
//
// @purpose Gives the next account a clean agent cache when a worker is rebound, without touching the
// workspace (which must persist) or the credential (which is being replaced).
// @rationale These paths are per-account, not per-workspace: `conversations/` holds the agent's own
// session databases, `brain/` its scratch artifacts, and `conversation_summaries.db*` plus
// `annotations/` live beside them. Leaving them means the new account inherits the previous account's
// conversation state, which is both a privacy problem and a source of stale-context confusion.
// @preconditions None; missing directories are a no-op.
// @param None.
// @returns True when at least one entry was removed; failures are logged.
// @throws None.
// @invariants O(entries in four directories); never deletes the token file or anything in Cwd.
func (w *Worker) PurgeAccountCaches() bool {
	if w.HomeDir == "" {
		return false
	}
	removed := false
	cliDir := filepath.Join(w.HomeDir, ".gemini", "antigravity-cli")
	for _, sub := range []string{"conversations", "brain", "annotations"} {
		dir := filepath.Join(cliDir, sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
				w.logger.Warn("failed to purge account cache entry", "dir", sub, "entry", entry.Name(), "err", err)
				continue
			}
			removed = true
		}
	}
	// SQLite databases and their WAL/SHM companions live directly under the CLI directory.
	for _, name := range []string{"conversation_summaries.db", "conversation_summaries.db-wal", "conversation_summaries.db-shm"} {
		if err := os.Remove(filepath.Join(cliDir, name)); err == nil {
			removed = true
		}
	}
	return removed
}

// PurgeWorkspace removes every entry in the shared workspace except `.git`.
//
// @purpose Implements the operator's explicit `agy-accounts clean --workspace`, and nothing else.
// @rationale This used to run on every Release, which deleted the agent's own output between turns and
// caused an unbounded rewrite loop. It is now reachable only from a deliberate command, so the durable
// workspace stays durable by default.
// @preconditions The configured workspace root must pass the guard; otherwise nothing is deleted.
// @param None.
// @returns The number of top-level entries removed.
// @throws None; failures are logged.
// @invariants O(entries); never removes `.git`; refuses any path the guard rejects.
func (w *Worker) PurgeWorkspace() int {
	// Validate against the PARENT of the configured root: with the shared workspace the target IS the
	// root, which the guard refuses by design (it exists to stop a recursive delete of a root). Its
	// parent check keeps every other protection: refuses filesystem root, home, symlinks, and any path
	// that escapes.
	root := w.cfg.Storage.EffectiveWorkspaceDir()
	guardRoot := root
	if parent := filepath.Dir(filepath.Clean(root)); parent != "" {
		guardRoot = parent
	}
	if err := validateWorkspaceRoot(guardRoot, w.Cwd); err != nil {
		w.logger.Error("refusing to clean workspace: guard rejected the path", "cwd", w.Cwd, "err", err)
		return 0
	}
	entries, err := os.ReadDir(w.Cwd)
	if err != nil {
		return 0
	}
	removed := 0
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		if err := safeRemoveEntry(w.Cwd, entry.Name()); err != nil {
			w.logger.Warn("failed to remove workspace entry", "entry", entry.Name(), "err", err)
			continue
		}
		removed++
	}
	return removed
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
