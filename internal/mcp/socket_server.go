package mcp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"agy-gateway/internal/telemetry"
)

// SocketServer exposes one LoopbackServer session over a Unix domain socket.
//
// @purpose Gives the stdio MCP bridge a transport whose access control is the filesystem, not a
// bearer token.
// @rationale The previous design minted a per-session token and authenticated an HTTP request with
// it. That produced 104 `mcp session token invalid` rejections in 24h of production logs: the
// gateway deletes the token on session teardown while a bridge call is still in flight, so the
// child gets 401 for a session that was legitimate a millisecond earlier. A Unix socket removes the
// race by construction — the socket path lives in the worker's private TMPDIR (mode 0700) with the
// socket itself mode 0600, so only that worker's uid can connect, and "session gone" is simply EOF.
// @preconditions Each worker owns a distinct private directory.
// @invariants No shared mutable state beyond the listener map; every created socket file is removed
// by Close.
type SocketServer struct {
	loopback *LoopbackServer
	logger   *slog.Logger
}

// NewSocketServer wires a socket front end to an existing tool registry.
//
// @purpose Shares one SessionState registry between the socket transport and the turn executor.
// @rationale The executor reads intercepted calls from the same SessionState the bridge writes into;
// two registries would silently drop every tool call.
// @preconditions loopback is non-nil.
// @param loopback The tool registry.
// @param logger Optional logger; slog.Default when nil.
// @returns A ready SocketServer.
// @throws None.
// @invariants O(1).
func NewSocketServer(loopback *LoopbackServer, logger *slog.Logger) *SocketServer {
	if loopback == nil {
		loopback = NewLoopbackServer()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &SocketServer{loopback: loopback, logger: logger}
}

// Listener is one session's socket endpoint.
//
// @purpose Owns the listener, its connections and the socket file for exactly one ACP session.
// @rationale Scoping lifetime to the session is what makes "session closed" indistinguishable from
// "socket gone", which is the property that removes the token race.
// @invariants Close is idempotent and safe to call concurrently with active connections.
type Listener struct {
	path     string
	ln       net.Listener
	session  string
	srv      *SocketServer
	done     chan struct{}
	once     sync.Once
	connMu   sync.Mutex
	conns    map[net.Conn]struct{}
	connWG   sync.WaitGroup
	acceptWG sync.WaitGroup
}

// Listen creates the session socket and starts accepting bridge connections.
//
// @purpose Publishes the endpoint before session/new is sent, so the child always finds it.
// @rationale agy spawns the bridge while completing session/new; a socket created afterwards would
// race the handshake and surface as a failed MCP initialise.
// @preconditions dir exists or can be created; it must be private to this worker.
// @param dir Directory that will hold the socket (the worker's TMPDIR).
// @param name Socket file name, short enough to stay well under the ~108-byte unix path limit.
// @param sessionID Registry key the bridge's calls are attributed to.
// @returns The listener, or an error when the directory or socket cannot be created.
// @throws Wraps filesystem and net errors.
// @invariants Creates at most one socket file; a pre-existing file at the path is removed first so a
// recycled worker cannot inherit a dead endpoint.
func (s *SocketServer) Listen(dir, name, sessionID string) (*Listener, error) {
	if dir == "" {
		return nil, errors.New("mcp: empty socket directory")
	}
	// 0700: the directory is the access-control boundary, so a world-writable TMPDIR would expose
	// this socket to every uid in the container.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mcp: create socket directory: %w", err)
	}
	// A recycled worker reuses its TMPDIR; a stale socket file from a killed process would make
	// Listen fail with EADDRINUSE.
	path := filepath.Join(dir, name)
	_ = os.Remove(path)

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("mcp: listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("mcp: chmod socket: %w", err)
	}

	l := &Listener{
		path:    path,
		ln:      ln,
		session: sessionID,
		srv:     s,
		done:    make(chan struct{}),
		conns:   make(map[net.Conn]struct{}),
	}
	l.acceptWG.Add(1)
	go l.acceptLoop()
	s.logger.Debug("mcp socket listening", "sessionID", sessionID, "path", path)
	return l, nil
}

// Path reports the socket path the bridge must dial.
//
// @purpose Single source for the value injected into the child environment.
// @returns The absolute socket path.
// @invariants O(1); immutable after construction.
func (l *Listener) Path() string { return l.path }

func (l *Listener) acceptLoop() {
	defer l.acceptWG.Done()
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			l.srv.logger.Warn("mcp socket accept failed", "sessionID", l.session, "err", err)
			return
		}
		l.connMu.Lock()
		select {
		case <-l.done:
			// Accept can return a live connection after Close; registering it would put it in a map
			// nobody ever drains.
			_ = conn.Close()
			l.connMu.Unlock()
			return
		default:
		}
		l.conns[conn] = struct{}{}
		l.connWG.Add(1)
		l.connMu.Unlock()

		go l.serveConn(conn)
	}
}

// serveConn runs the MCP line protocol for one bridge connection.
//
// @purpose Relays newline-delimited JSON-RPC between the bridge and the session registry.
// @rationale Unix stream sockets have no message boundaries, so the same newline framing the bridge
// uses on stdio is reused here; the registry already answers one JSON object per request.
// @preconditions conn is a live accepted connection.
// @param conn The accepted connection; closed by this method.
// @throws None; read/write failures end the connection like a bridge exit.
// @invariants Exactly one goroutine per connection; the connection is removed from the registry and
// closed on every exit path.
func (l *Listener) serveConn(conn net.Conn) {
	telemetry.DefaultMetrics.BridgeConnects.Add(1)
	defer l.connWG.Done()
	defer func() {
		l.connMu.Lock()
		delete(l.conns, conn)
		l.connMu.Unlock()
		_ = conn.Close()
	}()

	reader := bufio.NewReaderSize(conn, 1<<20)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				telemetry.DefaultMetrics.BridgeCalls.Add(1)
				resp, handleErr := l.srv.loopback.HandleJSONRPC(l.session, trimmed)
				if handleErr != nil {
					telemetry.DefaultMetrics.BridgeRejections.Add(1)
					l.srv.logger.Warn("mcp socket request failed", "sessionID", l.session, "err", handleErr)
				} else if resp != nil {
					if _, writeErr := conn.Write(append(resp, '\n')); writeErr != nil {
						return
					}
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// Close tears the endpoint down and disconnects every bridge.
//
// @purpose Makes "session ended" observable to the child as EOF, which is what replaces the old 401.
// @rationale Closing the listener alone would leave already-connected bridges blocked on read and
// leaking a process for the lifetime of the agent; the connections are closed explicitly.
// @preconditions None; idempotent.
// @returns None.
// @throws None; every failure is logged at Debug because teardown must never fail a turn.
// @invariants Blocks until all connection goroutines have exited.
func (l *Listener) Close() {
	l.once.Do(func() {
		close(l.done)
		_ = l.ln.Close()

		l.connMu.Lock()
		for conn := range l.conns {
			_ = conn.Close()
		}
		l.connMu.Unlock()

		l.acceptWG.Wait()
		// Bounded wait: a connection goroutine can only be inside HandleJSONRPC, which never blocks
		// indefinitely, so this is a formality that avoids an unbounded teardown in a pathological case.
		finished := make(chan struct{})
		go func() {
			l.connWG.Wait()
			close(finished)
		}()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			l.srv.logger.Debug("mcp socket connections still open after grace period", "sessionID", l.session)
		}

		if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
			l.srv.logger.Debug("could not remove mcp socket", "path", l.path, "err", err)
		}
	})
}
