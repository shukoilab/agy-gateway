package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrPathOutsideWorkspace is returned when an fs request targets a path outside the session roots.
var ErrPathOutsideWorkspace = errors.New("path is outside the session workspace")

// ClientHandlers answers the agent→client requests that ACP v1 defines for an editor client.
//
// @purpose Makes the gateway a truthful ACP client: every capability it advertises has a handler.
// @rationale agy turns advertised client capabilities into its own tools (`read_file`, `write_file`,
// `run_command`). Advertising fs or terminal without answering the corresponding requests makes the
// agent stall on a request no one replies to. The conformance reference
// (docs/acp-conformance.md §3.1) is the shape mirrored here.
//
// @invariants A handler is only reachable for a session whose roots were registered; unknown
// sessions are refused rather than served from process-wide state.
type ClientHandlers struct {
	// MaxReadBytes bounds one fs/read_text_file response.
	MaxReadBytes int64
	// MaxWriteBytes bounds one fs/write_text_file payload.
	MaxWriteBytes int64
	// TerminalOutputLimit bounds the retained output per terminal.
	TerminalOutputLimit int
	// TerminalIdleTimeout kills a terminal that produced no output for this long.
	TerminalIdleTimeout time.Duration
	// TerminalMaxLifetime bounds a single terminal process regardless of activity.
	TerminalMaxLifetime time.Duration

	mu    sync.RWMutex
	roots map[string][]string
	terms *terminalRegistry
}

// NewClientHandlers creates a handler set with production bounds.
//
// @purpose Single construction site so every limit has a deliberate value.
// @rationale Unbounded reads/writes/terminals are a resource-exhaustion vector driven by the model,
// which is not a trusted input. The limits here are generous enough for real work but finite.
//
// @preconditions None.
// @param maxReadBytes Bound for fs/read_text_file; <=0 selects 8 MiB.
// @param maxWriteBytes Bound for fs/write_text_file; <=0 selects 8 MiB.
// @param terminalOutputLimit Retained terminal bytes; <=0 selects 1 MiB.
// @returns A handler set with an empty registry.
// @throws None.
// @invariants The terminal registry is closed by Close, which also kills every child.
func NewClientHandlers(maxReadBytes, maxWriteBytes int64, terminalOutputLimit int) *ClientHandlers {
	if maxReadBytes <= 0 {
		maxReadBytes = 8 << 20
	}
	if maxWriteBytes <= 0 {
		maxWriteBytes = 8 << 20
	}
	if terminalOutputLimit <= 0 {
		terminalOutputLimit = 1 << 20
	}
	return &ClientHandlers{
		MaxReadBytes:        maxReadBytes,
		MaxWriteBytes:       maxWriteBytes,
		TerminalOutputLimit: terminalOutputLimit,
		TerminalIdleTimeout: 10 * time.Minute,
		TerminalMaxLifetime: 30 * time.Minute,
		roots:               make(map[string][]string),
		terms:               newTerminalRegistry(terminalOutputLimit),
	}
}

// RegisterSession declares the directories a session may touch.
//
// @purpose Confines file access to the workspace the caller actually granted.
// @rationale The agent composes absolute paths from its own idea of the workspace. Serving any path
// it names would turn the gateway into an arbitrary file writer for a remote harness.
//
// @preconditions roots entries are absolute paths; relative entries are ignored.
// @param sessionID Session the roots apply to.
// @param roots Absolute directories the session may read and write.
// @returns None.
// @throws None.
// @invariants Replaces any previous roots for the session; O(len(roots)).
func (h *ClientHandlers) RegisterSession(sessionID string, roots []string) {
	clean := make([]string, 0, len(roots))
	for _, r := range roots {
		if r == "" || !filepath.IsAbs(r) {
			continue
		}
		clean = append(clean, filepath.Clean(r))
	}
	h.mu.Lock()
	h.roots[sessionID] = clean
	h.mu.Unlock()
}

// UnregisterSession drops a session's roots and kills its terminals.
//
// @purpose Releases every resource a finished session owns.
// @rationale A terminal that outlives its session is a zombie process plus a file-descriptor leak;
// the registry kill is what makes end-of-session deterministic.
//
// @preconditions None.
// @param sessionID Session to forget.
// @returns None.
// @throws None.
// @invariants Idempotent; kills children before returning.
func (h *ClientHandlers) UnregisterSession(sessionID string) {
	h.mu.Lock()
	delete(h.roots, sessionID)
	h.mu.Unlock()
	h.terms.killSession(sessionID)
}

// Close releases every terminal owned by any session.
//
// @purpose Deterministic shutdown: no child outlives the transport.
// @rationale Process-group kill is the only way to reap a shell that spawned grandchildren.
// @preconditions None.
// @param None.
// @returns None.
// @throws None.
// @invariants Safe to call more than once.
func (h *ClientHandlers) Close() {
	h.terms.closeAll()
}

// rootsFor returns the allowed roots for a session, or nil.
func (h *ClientHandlers) rootsFor(sessionID string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.roots[sessionID]
}

// resolvePath validates that path lies inside one of the session roots and returns the cleaned path.
//
// @purpose Blocks traversal and symlink escapes before any file operation.
// @rationale A lexical prefix check alone is insufficient: a symlink inside the workspace can point
// anywhere. The parent directory is therefore resolved with EvalSymlinks and re-checked.
//
// @preconditions path is absolute.
// @param sessionID Session whose roots apply.
// @param path Absolute path requested by the agent.
// @returns The cleaned, validated path.
// @throws ErrPathOutsideWorkspace when no root contains the path.
// @invariants O(len(roots)); never touches the filesystem beyond symlink resolution.
func (h *ClientHandlers) resolvePath(sessionID, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: %q is not absolute", ErrPathOutsideWorkspace, path)
	}
	clean := filepath.Clean(path)
	roots := h.rootsFor(sessionID)
	if len(roots) == 0 {
		return "", fmt.Errorf("%w: session %q has no registered workspace", ErrPathOutsideWorkspace, sessionID)
	}
	for _, root := range roots {
		if !withinRoot(root, clean) {
			continue
		}
		resolvedRoot, rootErr := filepath.EvalSymlinks(root)
		if rootErr != nil {
			continue
		}

		// An existing path is resolved in full, so a symlink inside the workspace cannot launder a
		// path that lives outside it. This also covers the common case of cwd == root.
		if resolved, err := filepath.EvalSymlinks(clean); err == nil {
			if withinRoot(resolvedRoot, resolved) {
				return resolved, nil
			}
			continue
		}

		// The path does not exist yet (a create request): resolve the nearest existing ancestor and
		// re-check, so the missing segment cannot turn out to be a symlink pointing elsewhere.
		parent := filepath.Dir(clean)
		if resolvedParent, err := filepath.EvalSymlinks(parent); err == nil {
			candidate := filepath.Join(resolvedParent, filepath.Base(clean))
			if withinRoot(resolvedRoot, candidate) {
				return candidate, nil
			}
			continue
		}

		// Neither the path nor its parent exists: a lexical match is the only evidence available, and
		// it is trusted only because the root itself resolved successfully.
		if withinRoot(resolvedRoot, clean) {
			return clean, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrPathOutsideWorkspace, clean)
}

// withinRoot reports whether path is root or a descendant of it.
//
// @purpose One place for the prefix rule so every caller applies the same (segment-aware) check.
// @rationale A plain strings.HasPrefix would accept "/workspace-evil" for root "/workspace".
// @preconditions Neither argument is empty.
// @param root Absolute root directory.
// @param path Absolute candidate path.
// @returns True when path is inside root.
// @throws None.
// @invariants String work only.
func withinRoot(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(root, string(os.PathSeparator))+string(os.PathSeparator))
}

// Handle answers one agent→client request.
//
// @purpose Single dispatch point so the transport stays free of filesystem and process logic.
// @rationale Returning handled=false lets the transport emit the canonical method-not-found error
// without duplicating the switch.
//
// @preconditions req.Params is the raw params payload.
// @param method JSON-RPC method name.
// @param params Raw params.
// @returns result The marshalled result payload, handled Whether the method was answered, err A
// protocol-level failure to report to the agent.
// @throws None; failures are returned as err.
// @invariants Never blocks longer than the configured timeouts.
func (h *ClientHandlers) Handle(method string, params json.RawMessage) (json.RawMessage, bool, error) {
	switch method {
	case "fs/read_text_file":
		out, err := h.readTextFile(params)
		return out, true, err
	case "fs/write_text_file":
		out, err := h.writeTextFile(params)
		return out, true, err
	case "terminal/create":
		out, err := h.createTerminal(params)
		return out, true, err
	case "terminal/output":
		out, err := h.terminalOutput(params)
		return out, true, err
	case "terminal/wait_for_exit":
		out, err := h.terminalWaitForExit(params)
		return out, true, err
	case "terminal/kill":
		out, err := h.terminalKill(params)
		return out, true, err
	case "terminal/release":
		out, err := h.terminalRelease(params)
		return out, true, err
	default:
		return nil, false, nil
	}
}

type readTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Line      *int   `json:"line,omitempty"`
	Limit     *int   `json:"limit,omitempty"`
}

// readTextFile serves fs/read_text_file.
//
// @purpose Lets the agent read workspace files through the client rather than assuming a shared
// filesystem.
// @rationale See docs/acp-conformance.md §3.1: agy emits read_file when the client advertises
// fs.readTextFile. Bounded by MaxReadBytes and confined to the session roots.
// @preconditions Params decode into readTextFileParams.
// @param params Raw JSON params.
// @returns `{"content":"…"}`.
// @throws ErrPathOutsideWorkspace, or a read error, reported as an ACP error.
// @invariants O(file size), capped at MaxReadBytes.
func (h *ClientHandlers) readTextFile(params json.RawMessage) (json.RawMessage, error) {
	var p readTextFileParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ResponseError{Code: ErrCodeInvalidParams, Message: "invalid fs/read_text_file params"}
	}
	path, err := h.resolvePath(p.SessionID, p.Path)
	if err != nil {
		return nil, &ResponseError{Code: ErrCodeInvalidParams, Message: err.Error()}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &ResponseError{Code: ErrCodeResourceNotFound, Message: "cannot read requested file"}
	}
	content := string(raw)
	if p.Line != nil || p.Limit != nil {
		content = sliceLines(content, p.Line, p.Limit)
	}
	if int64(len(content)) > h.MaxReadBytes {
		content = content[:h.MaxReadBytes]
	}
	return json.Marshal(map[string]string{"content": content})
}

// sliceLines applies the 1-based line window ACP defines for read_text_file.
//
// @purpose Honours `line` and `limit` without reading the file twice.
// @rationale A caller that asks for line 500 of a 10 MB log must not receive all 10 MB first.
// @preconditions content is already loaded; line is 1-based when present.
// @param content Full file text.
// @param line Optional 1-based start line.
// @param limit Optional maximum number of lines.
// @returns The selected lines, joined with their original newlines.
// @throws None.
// @invariants O(number of lines) in content.
func sliceLines(content string, line, limit *int) string {
	lines := strings.SplitAfter(content, "\n")
	start := 0
	if line != nil && *line > 0 {
		start = *line - 1
	}
	if start >= len(lines) {
		return ""
	}
	end := len(lines)
	if limit != nil && *limit > 0 && start+*limit < end {
		end = start + *limit
	}
	return strings.Join(lines[start:end], "")
}

type writeTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}

// writeTextFile serves fs/write_text_file.
//
// @purpose Lets the agent create and edit workspace files through the client.
// @rationale Writes are the riskiest client capability, so the path is confined to the session
// roots, the payload is bounded, and the file is written atomically (temp + rename) so a crash
// cannot leave a half-written source file.
// @preconditions Params decode into writeTextFileParams.
// @param params Raw JSON params.
// @returns `{}`.
// @throws ErrPathOutsideWorkspace or an I/O error, reported as an ACP error.
// @invariants O(payload size); creates parent directories inside the root only.
func (h *ClientHandlers) writeTextFile(params json.RawMessage) (json.RawMessage, error) {
	var p writeTextFileParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ResponseError{Code: ErrCodeInvalidParams, Message: "invalid fs/write_text_file params"}
	}
	if int64(len(p.Content)) > h.MaxWriteBytes {
		return nil, &ResponseError{Code: ErrCodeInvalidParams, Message: "content exceeds the write limit"}
	}
	path, err := h.resolvePath(p.SessionID, p.Path)
	if err != nil {
		return nil, &ResponseError{Code: ErrCodeInvalidParams, Message: err.Error()}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, &ResponseError{Code: ErrCodeInternalError, Message: "cannot create parent directory"}
	}
	if err := atomicWriteFile(path, []byte(p.Content), 0o644); err != nil {
		return nil, &ResponseError{Code: ErrCodeInternalError, Message: "cannot write requested file"}
	}
	return json.RawMessage("{}"), nil
}

// atomicWriteFile writes data to path via a temp file in the same directory.
//
// @purpose Prevents a partially written file from being observed.
// @rationale The agent may rewrite a source file mid-turn; a torn write would corrupt it.
// @preconditions dir of path exists.
// @param path Destination file.
// @param data Payload.
// @param mode File mode for a newly created file.
// @returns The write error, if any.
// @throws None.
// @invariants The temp file is removed on every failure path.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".agy-gateway-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = ""
	return nil
}
