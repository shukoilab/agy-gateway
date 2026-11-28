package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Materialization failure sentinels.
//
// @purpose Lets callers distinguish an operator-actionable credential problem from a transient fault.
// @rationale An account that cannot be materialised (wrong identity, corrupt export, revoked refresh
// token) needs a human to re-run `agy-auth login`; retrying it forever would silently burn turns.
// @preconditions None.
// @param None.
// @returns Shared sentinels matched with errors.Is.
// @throws None.
// @invariants Immutable; safe for concurrent use.
var (
	ErrCredentialExportFailed = errors.New("agy-auth export failed")
	// ErrCredentialTransient marks a materialisation failure that says nothing about the stored
	// credential itself: the export subprocess timed out or the caller's context was cancelled. Such a
	// failure must NOT expire the account, because the vault entry is still valid and the next attempt
	// usually succeeds; expiring it would strand a healthy account until an operator intervenes.
	ErrCredentialTransient      = errors.New("credential materialisation failed transiently")
	ErrCredentialPayloadInvalid = errors.New("exported credential payload is invalid")
	ErrCredentialIdentityMatch  = errors.New("exported credential identity does not match the requested account")
)

// exportedCredential is the on-disk credential shape written by `agy-auth export`.
//
// @purpose Validates a credential before it is installed into a worker HOME.
// @rationale The export format is byte-compatible with the file agy itself consumes, so it can be
// installed verbatim; parsing it first is what lets the gateway refuse a wrong or incomplete
// credential instead of handing agy something it will discover is broken hours later.
// @preconditions None.
// @param None.
// @returns None.
// @throws None.
// @invariants Wire-only; never mutated after decode.
type exportedCredential struct {
	Token struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
		Expiry       string `json:"expiry"`
	} `json:"token"`
	AuthMethod   string `json:"auth_method"`
	IDToken      string `json:"id_token"`
	IDTokenEmail string `json:"id_token_email"`
}

// exportAccount is the seam used to obtain one account's decrypted credential.
//
// @purpose Keeps materialisation testable without a real vault or subprocess.
// @rationale The behaviour under test is identity validation, atomic installation, and rollback; none
// of that needs `agy-auth`. One function field is enough — no interface hierarchy.
// @preconditions dir is an empty directory owned by the caller.
// @param ctx Cancellation root.
// @param dir Destination directory for the exported JSON.
// @param accountID Account id to export.
// @returns An error when the export fails.
// @throws None; the default implementation kills the process group on timeout.
// @invariants Bounded by CredentialOpTimeout.
type exportAccountFunc func(ctx context.Context, dir, accountID string) error

// defaultExportAccount runs `agy-auth export --dir <dir> <accountID>`.
//
// @purpose Produces the plaintext credential for exactly one account.
// @rationale Exporting a single account (rather than the whole vault) keeps unrelated credentials out
// of the staging directory and makes "exactly one file" a meaningful correctness check.
// @preconditions agy-auth is installed and the vault holds accountID.
// @param ctx Cancellation root; the whole process group is killed when it expires.
// @param dir Destination directory.
// @param accountID Account id to export.
// @returns A wrapped error including stderr on failure.
// @throws None; failures are returned.
// @invariants Exactly one subprocess; never leaves an orphan (Setpgid + group kill).
func (m *Manager) defaultExportAccount(ctx context.Context, dir, accountID string) error {
	timeout := m.cfg.Accounts.CredentialOpTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(opCtx, m.cfg.Binaries.AgyAuthPath, "export", "--dir", dir, accountID)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("XDG_CONFIG_HOME=%s", m.cfg.Storage.VaultXDGConfigDir),
		"AGY_AUTH_NO_UPDATE_CHECK=1",
		"DBUS_SESSION_BUS_ADDRESS=",
	)
	if proxyEnvs := m.cfg.Proxy.EnvSlice(); len(proxyEnvs) > 0 {
		cmd.Env = append(cmd.Env, proxyEnvs...)
	}
	// A dedicated process group is what makes the timeout kill the whole `agy-auth` tree rather than
	// only the shell it may have spawned, so a wedged export cannot leave an orphan behind.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// A timeout or a cancelled caller is transient: the vault entry is untouched, so the account
		// must stay eligible rather than being marked AUTH_EXPIRED for an infrastructure hiccup.
		if opCtx.Err() != nil || ctx.Err() != nil {
			return fmt.Errorf("%w: %v (stderr: %s)", ErrCredentialTransient, err, strings.TrimSpace(stderr.String()))
		}
		return fmt.Errorf("%w: %v: %s", ErrCredentialExportFailed, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// SetExportForTest replaces the credential export implementation for tests.
//
// @purpose Lets tests drive materialisation without a real vault or `agy-auth` subprocess.
// @rationale Materialisation is exercised from two packages (auth and pool); a single exported seam
// keeps both honest about the real installation path instead of duplicating it.
// @preconditions Only call from tests.
// @param fn Replacement export implementation; nil restores the default.
// @returns None.
// @throws None.
// @invariants O(1); not safe to call concurrently with materialisation.
func (m *Manager) SetExportForTest(fn exportAccountFunc) {
	m.exportAccount = fn
}

// MaterializeWorkerCredential installs accountID's credential into workerID's isolated HOME.
//
// @purpose Makes each worker authenticate as its own account, which is the prerequisite for every
// quota, cooldown, and rotation guarantee.
// @rationale Previously all workers copied whichever token happened to exist under $HOME, so the pool
// believed it owned three accounts while every process authenticated as one. Exporting per account and
// installing atomically is the only sequence that both fixes the identity and cannot leave a worker
// holding a half-written token.
// @preconditions workerID addresses an existing or creatable worker HOME; accountID exists in the
// vault. m.cfg.Storage.WorkersRootDir is configured.
// @param ctx Cancellation root for the export subprocess.
// @param workerID Worker index whose HOME receives the credential.
// @param accountID Account to install.
// @returns The absolute path that received the credential.
// @throws ErrAccountNotFound when accountID is unknown; ErrCredentialPayloadInvalid when the export
// yields zero or multiple files or an incomplete payload; ErrCredentialIdentityMatch when the exported
// identity differs from the account; ErrCredentialExportFailed when the subprocess fails for a reason
// that indicates the stored credential is unusable; ErrCredentialTransient when it timed out or the
// caller's context was cancelled, in which case the account must NOT be marked expired. The previous
// credential is left untouched on every error path.
// @invariants O(1) filesystem work plus one subprocess; staging directory always removed; the installed
// file is 0600 and becomes visible only once complete.
func (m *Manager) MaterializeWorkerCredential(ctx context.Context, workerID int, accountID string) (string, error) {
	m.vaultMu.Lock()
	defer m.vaultMu.Unlock()
	return m.materializeLocked(ctx, workerID, accountID)
}

// materializeLocked performs materialisation with vaultMu already held.
//
// @purpose Lets callers that must serialise several vault operations run the install inside their
// existing critical section without re-entering a non-reentrant mutex.
// @rationale ProvisionWorkerHome creates the HOME tree and installs the credential as one atomic
// provisioning step; taking vaultMu twice on the same goroutine would deadlock.
// @preconditions m.vaultMu is held by the caller.
// @param ctx Cancellation root for the export subprocess.
// @param workerID Worker index whose HOME receives the credential.
// @param accountID Account to install.
// @returns The absolute path that received the credential.
// @throws The same typed errors as MaterializeWorkerCredential.
// @invariants Same guarantees as MaterializeWorkerCredential; never acquires vaultMu itself.
func (m *Manager) materializeLocked(ctx context.Context, workerID int, accountID string) (string, error) {
	account, err := m.accountByID(accountID)
	if err != nil {
		return "", err
	}

	stageDir, err := os.MkdirTemp("", fmt.Sprintf("agycred-w%d-", workerID))
	if err != nil {
		return "", fmt.Errorf("failed to create credential staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stageDir) }()

	export := m.exportAccount
	if export == nil {
		export = m.defaultExportAccount
	}
	if err := export(ctx, stageDir, accountID); err != nil {
		return "", err
	}

	payload, err := readSingleCredential(stageDir)
	if err != nil {
		return "", err
	}

	// The identity check is what prevents installing a well-formed credential for the wrong account,
	// which would leave the pool's bookkeeping and reality disagreeing again.
	if account.Email != "" && !strings.EqualFold(payload.IDTokenEmail, account.Email) {
		return "", fmt.Errorf("%w: exported %q, account is %q",
			ErrCredentialIdentityMatch, payload.IDTokenEmail, account.Email)
	}
	if payload.Token.AccessToken == "" || payload.Token.RefreshToken == "" {
		return "", fmt.Errorf("%w: account %s carries no usable access/refresh token",
			ErrCredentialPayloadInvalid, accountID)
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to encode credential for account %s: %w", accountID, err)
	}

	homeDir := filepath.Join(m.cfg.Storage.WorkersRootDir, fmt.Sprintf("w%d", workerID))
	tokenDir := filepath.Join(homeDir, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(tokenDir, 0700); err != nil {
		return "", fmt.Errorf("failed to create token directory %s: %w", tokenDir, err)
	}

	tokenPath := filepath.Join(tokenDir, "antigravity-oauth-token")
	if err := atomicWriteFile(tokenPath, raw, 0600); err != nil {
		return "", err
	}

	m.logger.Info("credential materialized",
		"workerID", workerID, "accountID", account.ID, "email", account.Email)
	return tokenPath, nil
}

// accountByID resolves a stored account by id.
//
// @purpose Gives materialisation the account metadata it needs for the identity check.
// @rationale Reading through GetAccounts keeps the lock discipline in one place and returns a copy, so
// no caller can mutate manager state by accident.
// @preconditions None.
// @param accountID Account id to resolve.
// @returns A copy of the matching account.
// @throws ErrAccountNotFound when the id is unknown.
// @invariants O(accounts); no allocation beyond the copied slice.
func (m *Manager) accountByID(accountID string) (Account, error) {
	for _, acc := range m.GetAccounts() {
		if acc.ID == accountID {
			return acc, nil
		}
	}
	return Account{}, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
}

// AccountByID returns the vault representation of one account.
//
// @purpose Lets callers outside this package install a specific account on a worker.
// @rationale Rebind needs the full Account (tier, quota snapshot) to publish a truthful binding, and
// the unexported lookup was the only accessor.
// @preconditions None.
// @param accountID Account to look up.
// @returns The account and nil, or an empty account and a wrapped ErrAccountNotFound.
// @throws ErrAccountNotFound when the vault has no such id.
// @invariants O(accounts); takes a read lock via GetAccounts.
func (m *Manager) AccountByID(accountID string) (Account, error) {
	return m.accountByID(accountID)
}

// readSingleCredential reads the one credential file an export produced.
//
// @purpose Enforces "exactly one file" so a whole-vault export can never be installed for one account.
// @rationale Zero files means the account could not be exported; several files means the export
// silently ignored the account selector. Both are configuration faults that must not be papered over
// by picking one arbitrarily.
// @preconditions dir exists and was created exclusively for this export.
// @param dir Staging directory.
// @returns The decoded credential.
// @throws ErrCredentialPayloadInvalid when the file count is not exactly one, the JSON is malformed,
// or the file cannot be read.
// @invariants O(files) directory listing; reads exactly one file.
func readSingleCredential(dir string) (exportedCredential, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return exportedCredential{}, fmt.Errorf("failed to read credential staging directory: %w", err)
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	if len(files) != 1 {
		return exportedCredential{}, fmt.Errorf("%w: expected exactly 1 credential file, found %d",
			ErrCredentialPayloadInvalid, len(files))
	}

	raw, err := os.ReadFile(files[0])
	if err != nil {
		return exportedCredential{}, fmt.Errorf("%w: cannot read %s: %v",
			ErrCredentialPayloadInvalid, filepath.Base(files[0]), err)
	}

	var payload exportedCredential
	if err := json.Unmarshal(raw, &payload); err != nil {
		return exportedCredential{}, fmt.Errorf("%w: malformed JSON in %s: %v",
			ErrCredentialPayloadInvalid, filepath.Base(files[0]), err)
	}
	return payload, nil
}
