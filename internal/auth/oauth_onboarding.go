package auth

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"

	"agy-gateway/internal/config"
)

var (
	oauthURLRegex = regexp.MustCompile(`https://accounts\.google\.com/o/oauth2/[^\s'"]+`)
	portRegex     = regexp.MustCompile(`redirect_uri=http%3A%2F%2Flocalhost%3A(\d+)`)
)

// OAuthSession represents an in-flight headless login interaction.
type OAuthSession struct {
	ID         string         `json:"id"`
	Alias      string         `json:"alias,omitempty"`
	AuthURL    string         `json:"auth_url"`
	PinnedPort string         `json:"pinned_port,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	Cmd        *exec.Cmd      `json:"-"`
	Stdin      io.WriteCloser `json:"-"`
	ErrBuf     *bytes.Buffer  `json:"-"`
	PGID       int            `json:"pgid,omitempty"`
	DoneCh     chan struct{}  `json:"-"`
	Consumed   atomic.Bool    `json:"-"`
	Completed  atomic.Bool    `json:"-"`
	Failed     atomic.Bool    `json:"-"`
}

// MarshalJSON provides safe JSON serialization without exposing process handles or channels.
func (s *OAuthSession) MarshalJSON() ([]byte, error) {
	type Alias OAuthSession
	return json.Marshal(&struct {
		*Alias
		Completed bool `json:"completed"`
		Failed    bool `json:"failed"`
	}{
		Alias:     (*Alias)(s),
		Completed: s.Completed.Load(),
		Failed:    s.Failed.Load(),
	})
}

// OAuthOnboardingManager manages out-of-band interactive login flows.
type OAuthOnboardingManager struct {
	cfg        *config.Config
	logger     *slog.Logger
	mgr        *Manager
	sessionsMu sync.RWMutex
	sessions   map[string]*OAuthSession
	stopCh     chan struct{}
}

// NewOAuthOnboardingManager creates an initialized manager and starts the TTL sweeper.
func NewOAuthOnboardingManager(cfg *config.Config, mgr *Manager, logger *slog.Logger) *OAuthOnboardingManager {
	if logger == nil {
		logger = slog.Default()
	}
	oom := &OAuthOnboardingManager{
		cfg:      cfg,
		logger:   logger,
		mgr:      mgr,
		sessions: make(map[string]*OAuthSession),
		stopCh:   make(chan struct{}),
	}
	go oom.sweeperLoop()
	return oom
}

// Close stops the sweeper loop and kills any abandoned sessions.
func (oom *OAuthOnboardingManager) Close() {
	close(oom.stopCh)
	oom.sessionsMu.Lock()
	defer oom.sessionsMu.Unlock()
	for _, s := range oom.sessions {
		oom.terminateSession(s)
	}
}

// InitLogin begins an out-of-band login and returns a login ID and the OAuth authorization URL.
func (oom *OAuthOnboardingManager) InitLogin(ctx context.Context, alias string) (string, string, error) {
	oom.sessionsMu.Lock()
	if len(oom.sessions) >= oom.cfg.Admin.OAuthOnboarding.MaxConcurrentLogins {
		oom.sessionsMu.Unlock()
		return "", "", errors.New("maximum concurrent login sessions reached")
	}
	oom.sessionsMu.Unlock()

	loginID := fmt.Sprintf("lgn_%s", uuid.New().String()[:8])

	args := []string{"login", "--no-browser"}
	if alias != "" {
		args = append(args, "--alias", alias)
	}

	cmd := exec.Command(oom.cfg.Binaries.AgyAuthPath, args...)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("XDG_CONFIG_HOME=%s", oom.cfg.Storage.VaultXDGConfigDir),
		"AGY_AUTH_NO_UPDATE_CHECK=1",
		"DBUS_SESSION_BUS_ADDRESS=",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Pgid:    0,
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", "", fmt.Errorf("failed to open stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return "", "", fmt.Errorf("failed to open stdout pipe: %w", err)
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return "", "", fmt.Errorf("failed to start agy-auth login: %w", err)
	}

	session := &OAuthSession{
		ID:        loginID,
		Alias:     alias,
		CreatedAt: time.Now(),
		Cmd:       cmd,
		Stdin:     stdin,
		ErrBuf:    &errBuf,
		PGID:      cmd.Process.Pid,
		DoneCh:    make(chan struct{}),
	}

	go func() {
		defer close(session.DoneCh)
		err := cmd.Wait()
		if err == nil {
			session.Completed.Store(true)
		} else {
			session.Failed.Store(true)
		}
	}()

	// Extract URL from output within timeout
	extractTimeout := oom.cfg.Admin.OAuthOnboarding.URLExtractTimeout
	if extractTimeout <= 0 {
		extractTimeout = 10 * time.Second
	}

	urlCh := make(chan struct {
		authURL string
		port    string
	}, 1)
	errCh := make(chan error, 1)

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			text := scanner.Text()
			if match := oauthURLRegex.FindString(text); match != "" {
				portMatch := portRegex.FindStringSubmatch(match)
				var port string
				if len(portMatch) > 1 {
					port = portMatch[1]
				}
				urlCh <- struct {
					authURL string
					port    string
				}{authURL: match, port: port}
				return
			}
		}
		if err := scanner.Err(); err != nil {
			errCh <- err
		}
	}()

	select {
	case <-time.After(extractTimeout):
		oom.terminateSession(session)
		return "", "", errors.New("timed out extracting OAuth authorization URL from agy-auth output")
	case err := <-errCh:
		oom.terminateSession(session)
		return "", "", fmt.Errorf("scanner error: %w", err)
	case res := <-urlCh:
		session.AuthURL = res.authURL
		session.PinnedPort = res.port

		oom.sessionsMu.Lock()
		oom.sessions[loginID] = session
		oom.sessionsMu.Unlock()

		return loginID, res.authURL, nil
	}
}

// CompleteCallback validates and processes the OAuth authorization code or loopback redirect URL.
func (oom *OAuthOnboardingManager) CompleteCallback(ctx context.Context, loginID, code, redirectURL string) error {
	oom.sessionsMu.RLock()
	session, exists := oom.sessions[loginID]
	oom.sessionsMu.RUnlock()

	if !exists {
		return fmt.Errorf("session %q not found or expired", loginID)
	}

	if !session.Consumed.CompareAndSwap(false, true) {
		return errors.New("login session already consumed")
	}

	// 1. Out-of-band authorization code flow (Antigravity Google OAuth callback)
	if code != "" {
		if session.Stdin == nil {
			return errors.New("login session stdin pipe is not available")
		}
		if _, err := fmt.Fprintln(session.Stdin, strings.TrimSpace(code)); err != nil {
			return fmt.Errorf("failed to write authorization code to agy-auth stdin: %w", err)
		}
		_ = session.Stdin.Close()

		// Await process exit
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-session.DoneCh:
			if session.Failed.Load() {
				errMsg := session.ErrBuf.String()
				if errMsg == "" {
					errMsg = "agy-auth process failed to exchange authorization code"
				}
				return fmt.Errorf("login failed: %s", strings.TrimSpace(errMsg))
			}
			// Synchronize new account into manager
			_ = oom.mgr.Sync(context.Background())
			return nil
		case <-time.After(25 * time.Second):
			return errors.New("timed out waiting for agy-auth process to finalize token exchange")
		}
	}

	// 2. Loopback redirect URL flow
	if redirectURL != "" {
		// Zero-SSRF verification: Scheme == http, Hostname == localhost/127.0.0.1, Port == pinnedPort
		u, err := url.Parse(redirectURL)
		if err != nil {
			return fmt.Errorf("invalid redirect URL: %w", err)
		}

		if u.Scheme != "http" {
			return errors.New("invalid redirect URL scheme (must be http)")
		}
		host := u.Hostname()
		if host != "127.0.0.1" && host != "localhost" {
			return errors.New("invalid redirect URL host (must be localhost or 127.0.0.1)")
		}
		if session.PinnedPort != "" && u.Port() != session.PinnedPort {
			return fmt.Errorf("redirect port %s does not match pinned port %s", u.Port(), session.PinnedPort)
		}

		// Replay callback locally
		loopbackURL := fmt.Sprintf("http://127.0.0.1:%s%s", u.Port(), u.RequestURI())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, loopbackURL, nil)
		if err != nil {
			return fmt.Errorf("failed to construct loopback request: %w", err)
		}

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("failed to replay callback over loopback: %w", err)
		}
		_ = resp.Body.Close()

		// Await process exit
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-session.DoneCh:
			if session.Failed.Load() {
				return errors.New("agy-auth process exited with an error")
			}
			// Synchronize new account into manager
			_ = oom.mgr.Sync(context.Background())
			return nil
		case <-time.After(15 * time.Second):
			return errors.New("timed out waiting for agy-auth process to finalize")
		}
	}

	return errors.New("neither authorization code nor redirect_url was provided")
}

// GetSession returns current session state.
func (oom *OAuthOnboardingManager) GetSession(loginID string) (*OAuthSession, bool) {
	oom.sessionsMu.RLock()
	defer oom.sessionsMu.RUnlock()
	s, exists := oom.sessions[loginID]
	return s, exists
}

func (oom *OAuthOnboardingManager) sweeperLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	ttl := oom.cfg.Admin.OAuthOnboarding.LoginSessionTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}

	for {
		select {
		case <-oom.stopCh:
			return
		case <-ticker.C:
			oom.sweep(ttl)
		}
	}
}

func (oom *OAuthOnboardingManager) sweep(ttl time.Duration) {
	oom.sessionsMu.Lock()
	defer oom.sessionsMu.Unlock()

	now := time.Now()
	for id, s := range oom.sessions {
		if now.Sub(s.CreatedAt) > ttl {
			oom.logger.Info("sweeping expired OAuth onboarding session", "loginID", id)
			oom.terminateSession(s)
			delete(oom.sessions, id)
		}
	}
}

func (oom *OAuthOnboardingManager) terminateSession(s *OAuthSession) {
	if s.Stdin != nil {
		_ = s.Stdin.Close()
	}
	if s.PGID > 0 {
		_ = syscall.Kill(-s.PGID, syscall.SIGTERM)
		time.Sleep(100 * time.Millisecond)
		_ = syscall.Kill(-s.PGID, syscall.SIGKILL)
	}
}
