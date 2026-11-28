package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// terminal is one running child process owned by a session.
type terminal struct {
	id        string
	sessionID string
	cmd       *exec.Cmd
	done      chan struct{}

	mu        sync.Mutex
	buf       *bytes.Buffer
	truncated bool
	limit     int
	exited    bool
	exitCode  int
	signal    string
	lastSeen  time.Time
	killed    bool
}

// terminalRegistry owns every terminal process across sessions.
//
// @purpose One place that can enumerate, bound, and reap every child the agent asked for.
// @rationale Terminals are process-tree roots: a shell can spawn grandchildren. Without a registry
// the gateway has no way to kill them at session end, and a leaked shell outlives the request.
//
// @invariants Every entry is removed by exactly one of kill/release/closeAll; seq is monotonic.
type terminalRegistry struct {
	outputLimit int

	mu    sync.Mutex
	terms map[string]*terminal
	seq   uint64
}

func newTerminalRegistry(outputLimit int) *terminalRegistry {
	return &terminalRegistry{outputLimit: outputLimit, terms: make(map[string]*terminal)}
}

type createTerminalParams struct {
	SessionID       string        `json:"sessionId"`
	Command         string        `json:"command"`
	Args            []string      `json:"args,omitempty"`
	Cwd             string        `json:"cwd,omitempty"`
	Env             []EnvVariable `json:"env,omitempty"`
	OutputByteLimit *int          `json:"outputByteLimit,omitempty"`
}

// createTerminal serves terminal/create.
//
// @purpose Runs a command on behalf of the agent with bounded, captured output.
// @rationale The agent owns the command line, so the child must be isolated: its own process group
// (so a kill reaps the whole tree), a bounded output buffer (so a chatty command cannot exhaust
// memory), an idle timeout and a hard lifetime cap (so a hung process cannot hold the slot forever).
// @preconditions Params decode into createTerminalParams and the session has registered roots.
// @param params Raw JSON params.
// @returns `{"terminalId":"…"}`.
// @throws A protocol error when the command is missing, the cwd escapes the workspace, or spawn fails.
// @invariants The child is always waited on by a goroutine, so no zombie survives.
func (h *ClientHandlers) createTerminal(params json.RawMessage) (json.RawMessage, error) {
	var p createTerminalParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ResponseError{Code: ErrCodeInvalidParams, Message: "invalid terminal/create params"}
	}
	if p.Command == "" {
		return nil, &ResponseError{Code: ErrCodeInvalidParams, Message: "terminal/create requires a command"}
	}
	cwd := p.Cwd
	if cwd != "" {
		resolved, err := h.resolvePath(p.SessionID, cwd)
		if err != nil {
			return nil, &ResponseError{Code: ErrCodeInvalidParams, Message: err.Error()}
		}
		cwd = resolved
	} else if roots := h.rootsFor(p.SessionID); len(roots) > 0 {
		cwd = roots[0]
	}

	limit := h.TerminalOutputLimit
	if p.OutputByteLimit != nil && *p.OutputByteLimit > 0 && *p.OutputByteLimit < limit {
		limit = *p.OutputByteLimit
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.TerminalMaxLifetime)
	cmd := exec.CommandContext(ctx, p.Command, p.Args...)
	cmd.Dir = cwd
	if len(p.Env) > 0 {
		env := os.Environ()
		for _, e := range p.Env {
			env = append(env, e.Name+"="+e.Value)
		}
		cmd.Env = env
	}
	// Own process group: a kill must reach grandchildren, not just the direct child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	term := &terminal{
		sessionID: p.SessionID,
		cmd:       cmd,
		done:      make(chan struct{}),
		buf:       bytes.NewBuffer(make([]byte, 0, min(limit, 64<<10))),
		limit:     limit,
		lastSeen:  time.Now(),
	}

	h.terms.mu.Lock()
	h.terms.seq++
	term.id = "term_" + strconv.FormatUint(h.terms.seq, 10)
	h.terms.terms[term.id] = term
	h.terms.mu.Unlock()

	// The pipe must be obtained BEFORE the process starts, and it must be an explicit os.Pipe rather
	// than cmd.StdoutPipe(). os/exec closes the StdoutPipe read side inside Wait(), which discards
	// whatever the child wrote but the reader goroutine has not consumed yet — a short-lived command
	// (`echo hi`) loses its entire output that way under scheduling pressure. Owning both ends means
	// the reader drains to EOF before Wait() is called.
	pr, pw, pipeErr := os.Pipe()
	if pipeErr != nil {
		h.terms.remove(term.id)
		cancel()
		return nil, &ResponseError{Code: ErrCodeInternalError, Message: "cannot capture command output"}
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		h.terms.remove(term.id)
		cancel()
		return nil, &ResponseError{Code: ErrCodeInternalError, Message: "cannot start command"}
	}
	// The child holds the only writer it needs; closing ours is what lets the reader see EOF.
	_ = pw.Close()

	// The reader owns draining; Wait runs only after it finishes so no output can be lost.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		chunk := make([]byte, 32<<10)
		for {
			n, readErr := pr.Read(chunk)
			if n > 0 {
				term.append(chunk[:n])
			}
			if readErr != nil {
				_ = pr.Close()
				return
			}
		}
	}()
	go func() {
		<-readerDone
		waitErr := cmd.Wait()
		term.markExited(cmd, waitErr)
		cancel()
		close(term.done)
	}()

	// Idle watchdog: a terminal that stops producing output while still running is killed, so a
	// hung shell cannot occupy the registry until the lifetime cap.
	go h.watchTerminalIdle(term)

	return json.Marshal(map[string]string{"terminalId": term.id})
}

// watchTerminalIdle kills a terminal that has been silent past the idle timeout.
//
// @purpose Bounds the damage from a command that hangs without exiting.
// @rationale The agent may run an interactive tool by mistake; without this the process would live
// until the lifetime cap and hold its output buffer.
// @preconditions term was started and registered.
// @param term Terminal to watch.
// @returns None.
// @throws None.
// @invariants Exits when the terminal exits or is killed.
func (h *ClientHandlers) watchTerminalIdle(term *terminal) {
	if h.TerminalIdleTimeout <= 0 {
		return
	}
	tick := time.NewTicker(h.TerminalIdleTimeout / 4)
	defer tick.Stop()
	for {
		select {
		case <-term.done:
			return
		case <-tick.C:
			if term.idleFor() > h.TerminalIdleTimeout {
				term.kill()
				return
			}
		}
	}
}

// append records output while enforcing the retained-bytes limit.
//
// @purpose Keeps memory bounded regardless of how much the command prints.
// @rationale A `yes` loop must not grow the gateway's heap. Truncation happens at a rune boundary by
// discarding from the front, which is what ACP specifies.
// @preconditions chunk is the freshly read bytes.
// @param chunk Output bytes.
// @returns None.
// @throws None.
// @invariants O(len(chunk)); the buffer never exceeds limit.
func (t *terminal) append(chunk []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastSeen = time.Now()
	t.buf.Write(chunk)
	if t.buf.Len() > t.limit {
		overflow := t.buf.Len() - t.limit
		keep := append([]byte(nil), t.buf.Bytes()[overflow:]...)
		t.buf.Reset()
		t.buf.Write(keep)
		t.truncated = true
	}
}

func (t *terminal) idleFor() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return time.Since(t.lastSeen)
}

func (t *terminal) markExited(cmd *exec.Cmd, waitErr error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.exited {
		return
	}
	t.exited = true
	if cmd.ProcessState != nil {
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			t.exitCode = code
		}
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			t.signal = ws.Signal().String()
		}
	}
	if waitErr != nil && t.signal == "" && t.exitCode == 0 {
		t.exitCode = 1
	}
}

// snapshot returns the retained output and exit state.
func (t *terminal) snapshot() (output string, truncated bool, exited bool, exitCode int, signal string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String(), t.truncated, t.exited, t.exitCode, t.signal
}

// kill terminates the process group and waits for the exit to be recorded.
//
// @purpose Deterministic reaping, including grandchildren.
// @rationale Killing only the direct child leaves a shell's children running; the process group is
// what the OS offers for the whole tree.
// @preconditions None.
// @param None.
// @returns None.
// @throws None.
// @invariants Idempotent; returns after the exit is observed or a short grace expires.
func (t *terminal) kill() {
	t.mu.Lock()
	if t.killed {
		t.mu.Unlock()
		return
	}
	t.killed = true
	t.mu.Unlock()

	_ = killProcessGroup(t.cmd)
	select {
	case <-t.done:
	case <-time.After(5 * time.Second):
	}
}

// killProcessGroup sends SIGKILL to the child's process group.
//
// @purpose Reaps the whole tree in one call.
// @rationale Negative pid targets the group created by Setpgid.
// @preconditions cmd was started with Setpgid.
// @param cmd Command to kill.
// @returns The kill error, if any.
// @throws None.
// @invariants Never panics on an already-exited process.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return cmd.Process.Kill()
	}
	return nil
}

type terminalIDParams struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

// lookup returns a terminal only when it belongs to the requesting session.
//
// @purpose Prevents one session from touching another's processes.
// @rationale Terminal ids are predictable ("term_N"); ownership must be checked, not assumed.
// @preconditions None.
// @param sessionID Requester.
// @param terminalID Target.
// @returns The terminal, or nil.
// @throws None.
// @invariants O(1).
func (r *terminalRegistry) lookup(sessionID, terminalID string) *terminal {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.terms[terminalID]
	if !ok || t.sessionID != sessionID {
		return nil
	}
	return t
}

func (r *terminalRegistry) remove(id string) {
	r.mu.Lock()
	delete(r.terms, id)
	r.mu.Unlock()
}

// terminalOutput serves terminal/output.
//
// @purpose Lets the agent read a running command's output incrementally.
// @rationale The reference client returns the captured-so-far text plus the exit status when the
// process has finished; that is the shape implemented here.
// @preconditions Params decode into terminalIDParams.
// @param params Raw JSON params.
// @returns `{"output":…,"truncated":…,"exitStatus":{…}}` when the process exited.
// @throws A protocol error for an unknown terminal.
// @invariants O(retained output).
func (h *ClientHandlers) terminalOutput(params json.RawMessage) (json.RawMessage, error) {
	var p terminalIDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ResponseError{Code: ErrCodeInvalidParams, Message: "invalid terminal/output params"}
	}
	term := h.terms.lookup(p.SessionID, p.TerminalID)
	if term == nil {
		return nil, &ResponseError{Code: ErrCodeResourceNotFound, Message: "unknown terminal"}
	}
	output, truncated, exited, exitCode, signal := term.snapshot()
	resp := map[string]any{"output": output, "truncated": truncated}
	if exited {
		status := map[string]any{}
		if exitCode >= 0 {
			status["exitCode"] = exitCode
		}
		if signal != "" {
			status["signal"] = signal
		}
		resp["exitStatus"] = status
	}
	return json.Marshal(resp)
}

// terminalWaitForExit serves terminal/wait_for_exit.
//
// @purpose Blocks until the command finishes, so the agent can use command output synchronously.
// @rationale A bounded wait is required: an unbounded wait on a hung child would pin the transport
// goroutine that must stay responsive to session/cancel.
// @preconditions Params decode into terminalIDParams.
// @param params Raw JSON params.
// @returns `{"exitCode":…}` or `{"signal":…}`.
// @throws A protocol error for an unknown terminal.
// @invariants Waits at most TerminalMaxLifetime; then returns the current state.
func (h *ClientHandlers) terminalWaitForExit(params json.RawMessage) (json.RawMessage, error) {
	var p terminalIDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ResponseError{Code: ErrCodeInvalidParams, Message: "invalid terminal/wait_for_exit params"}
	}
	term := h.terms.lookup(p.SessionID, p.TerminalID)
	if term == nil {
		return nil, &ResponseError{Code: ErrCodeResourceNotFound, Message: "unknown terminal"}
	}
	select {
	case <-term.done:
	case <-time.After(h.TerminalMaxLifetime):
	}
	_, _, _, exitCode, signal := term.snapshot()
	resp := map[string]any{}
	if signal != "" {
		resp["signal"] = signal
	} else {
		resp["exitCode"] = exitCode
	}
	return json.Marshal(resp)
}

// terminalKill serves terminal/kill.
//
// @purpose Stops a running command at the agent's request.
// @rationale Uses the process group so the whole tree dies, then removes the entry so the slot is
// freed immediately.
// @preconditions Params decode into terminalIDParams.
// @param params Raw JSON params.
// @returns `{}`.
// @throws A protocol error for an unknown terminal.
// @invariants Idempotent.
func (h *ClientHandlers) terminalKill(params json.RawMessage) (json.RawMessage, error) {
	var p terminalIDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ResponseError{Code: ErrCodeInvalidParams, Message: "invalid terminal/kill params"}
	}
	term := h.terms.lookup(p.SessionID, p.TerminalID)
	if term == nil {
		// Killing an already-released terminal is not an error: the caller's intent is satisfied.
		return json.RawMessage("{}"), nil
	}
	term.kill()
	h.terms.remove(p.TerminalID)
	return json.RawMessage("{}"), nil
}

// terminalRelease serves terminal/release.
//
// @purpose Drops the retained output and any process for a terminal the agent is done with.
// @rationale ACP separates kill (stop the process) from release (forget it); releasing must also
// stop a still-running process, otherwise the entry disappears while the child keeps running.
// @preconditions Params decode into terminalIDParams.
// @param params Raw JSON params.
// @returns `{}`.
// @throws None; an unknown terminal is a no-op.
// @invariants Idempotent; never leaves a running child.
func (h *ClientHandlers) terminalRelease(params json.RawMessage) (json.RawMessage, error) {
	var p terminalIDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ResponseError{Code: ErrCodeInvalidParams, Message: "invalid terminal/release params"}
	}
	if term := h.terms.lookup(p.SessionID, p.TerminalID); term != nil {
		term.kill()
	}
	h.terms.remove(p.TerminalID)
	return json.RawMessage("{}"), nil
}

// killSession kills and forgets every terminal belonging to one session.
//
// @purpose Makes end-of-session deterministic for process resources.
// @rationale A turn that ends must not leave a shell running under the gateway.
// @preconditions None.
// @param sessionID Session whose terminals are reaped.
// @returns None.
// @throws None.
// @invariants O(number of terminals); bounded by a per-terminal grace.
func (r *terminalRegistry) killSession(sessionID string) {
	r.mu.Lock()
	victims := make([]*terminal, 0, len(r.terms))
	for id, t := range r.terms {
		if t.sessionID == sessionID {
			victims = append(victims, t)
			delete(r.terms, id)
		}
	}
	r.mu.Unlock()
	for _, t := range victims {
		t.kill()
	}
}

// closeAll kills and forgets every terminal.
//
// @purpose Shutdown path: no child outlives the transport.
// @rationale Called from Transport.Close and from process shutdown.
// @preconditions None.
// @param None.
// @returns None.
// @throws None.
// @invariants Idempotent; parallel kills so one slow child cannot serialise shutdown.
func (r *terminalRegistry) closeAll() {
	r.mu.Lock()
	victims := make([]*terminal, 0, len(r.terms))
	for id, t := range r.terms {
		victims = append(victims, t)
		delete(r.terms, id)
	}
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, t := range victims {
		wg.Add(1)
		go func(t *terminal) {
			defer wg.Done()
			t.kill()
		}(t)
	}
	wg.Wait()
}
