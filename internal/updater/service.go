package updater

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"agy-gateway/internal/config"
)

// Service coordinates automated background checks, downloads, and rolling pool refreshes.
type Service struct {
	cfg           *config.Config
	pool          WorkerPoolRecycler
	logger        *slog.Logger
	httpClient    *http.Client
	agyClient     *AgyClient
	agyAuthClient *AgyAuthClient

	mu          sync.RWMutex
	checkMu     sync.Mutex // enforces single-flight execution
	status      UpdateStatus
	lastChecked time.Time
	nextCheck   time.Time
	components  map[string]ComponentVersionInfo
	cancelFunc  context.CancelFunc
	doneChan    chan struct{}
}

// NewService constructs an auto-updater Service instance.
func NewService(cfg *config.Config, pool WorkerPoolRecycler, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}

	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
	}

	if cfg.Proxy.Enabled {
		proxyTarget := cfg.Proxy.HTTPSProxy
		if proxyTarget == "" {
			proxyTarget = cfg.Proxy.HTTPProxy
		}
		if proxyTarget != "" {
			if pu, err := url.Parse(proxyTarget); err == nil && pu.Scheme != "" {
				transport.Proxy = http.ProxyURL(pu)
			}
		}
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   300 * time.Second, // ample timeout for multi-megabyte binary downloads
	}

	s := &Service{
		cfg:           cfg,
		pool:          pool,
		logger:        logger.With("module", "auto-updater"),
		httpClient:    httpClient,
		agyClient:     NewAgyClient(httpClient, cfg.AutoUpdate.AgyCLI.ManifestURL),
		agyAuthClient: NewAgyAuthClient(httpClient, cfg.AutoUpdate.AgyAuth.ReleaseRepo),
		status:        StatusIdle,
		components:    make(map[string]ComponentVersionInfo),
		doneChan:      make(chan struct{}),
	}

	// Initialize baseline component state
	s.components[string(BinaryAgy)] = ComponentVersionInfo{
		ActiveBinaryPath: cfg.GetAgyPath(),
	}
	s.components[string(BinaryAgyAuth)] = ComponentVersionInfo{
		ActiveBinaryPath: cfg.GetAgyAuthPath(),
	}

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer probeCancel()
	s.RefreshCurrentVersions(probeCtx)

	return s
}

// Start launches the background scheduler goroutine.
func (s *Service) Start(parentCtx context.Context) {
	if !s.cfg.AutoUpdate.Enabled {
		s.logger.Info("auto-updater is disabled in configuration")
		return
	}

	ctx, cancel := context.WithCancel(parentCtx)
	s.cancelFunc = cancel

	go s.runLoop(ctx)
}

// Stop cleanly terminates the background updater loop.
func (s *Service) Stop() {
	if s.cancelFunc != nil {
		s.cancelFunc()
		<-s.doneChan
	}
}

func (s *Service) runLoop(ctx context.Context) {
	defer close(s.doneChan)

	s.logger.Info("starting auto-updater background service",
		"interval", s.cfg.AutoUpdate.Interval,
		"binDir", s.cfg.AutoUpdate.BinDir,
		"checkOnStartup", s.cfg.AutoUpdate.CheckOnStartup,
	)

	interval := s.cfg.AutoUpdate.Interval
	if interval < 1*time.Minute {
		interval = 6 * time.Hour
	}

	s.mu.Lock()
	if s.cfg.AutoUpdate.CheckOnStartup {
		s.nextCheck = time.Now().Add(3 * time.Second)
	} else {
		s.nextCheck = time.Now().Add(interval)
	}
	s.mu.Unlock()

	// Perform optional initial check after short bootstrapping delay
	if s.cfg.AutoUpdate.CheckOnStartup {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
			s.logger.Info("triggering startup auto-update check")
			if _, err := s.CheckAndUpdateAll(ctx); err != nil {
				s.logger.Warn("startup auto-update check reported issues", "err", err)
			}
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		s.mu.Lock()
		s.nextCheck = time.Now().Add(interval)
		s.mu.Unlock()

		select {
		case <-ctx.Done():
			s.logger.Info("auto-updater loop stopping on context cancel")
			return
		case <-ticker.C:
			s.logger.Info("periodic auto-update cron tick triggered")
			if _, err := s.CheckAndUpdateAll(ctx); err != nil {
				s.logger.Warn("periodic auto-update check reported issues", "err", err)
			}
		}
	}
}

// CheckAndUpdateAll executes a complete update inspection and installation turn.
func (s *Service) CheckAndUpdateAll(ctx context.Context) (*CheckResult, error) {
	// Guard single-flight execution
	s.checkMu.Lock()
	defer s.checkMu.Unlock()

	s.mu.Lock()
	s.status = StatusChecking
	s.lastChecked = time.Now()
	s.mu.Unlock()

	result := &CheckResult{}
	var resultErrors []string

	// 1. Check Google Antigravity CLI (agy)
	if s.cfg.AutoUpdate.AgyCLI.Enabled {
		result.AgyChecked = true
		currentPath := s.cfg.GetAgyPath()
		currentVer, _ := s.agyClient.GetCurrentVersion(ctx, currentPath)
		result.AgyOldVersion = currentVer

		manifest, err := s.agyClient.CheckLatest(ctx)
		if err != nil {
			msg := fmt.Sprintf("failed to query agy upstream manifest: %v", err)
			s.logger.Warn(msg)
			resultErrors = append(resultErrors, msg)
			s.updateComponentState(BinaryAgy, currentVer, "", false, err)
		} else {
			updateAvailable := compareVersions(manifest.Version, currentVer) > 0
			s.updateComponentState(BinaryAgy, currentVer, manifest.Version, updateAvailable, nil)

			if updateAvailable {
				s.logger.Info("new agy CLI version available; initiating installation",
					"current", currentVer,
					"latest", manifest.Version,
					"targetDir", s.cfg.AutoUpdate.BinDir,
				)

				s.mu.Lock()
				s.status = StatusUpdating
				s.mu.Unlock()

				newPath, installErr := s.agyClient.DownloadAndInstall(ctx, manifest, s.cfg.AutoUpdate.BinDir)
				if installErr != nil {
					msg := fmt.Sprintf("failed to download and install agy %s: %v", manifest.Version, installErr)
					s.logger.Error(msg)
					resultErrors = append(resultErrors, msg)
					s.updateComponentState(BinaryAgy, currentVer, manifest.Version, true, installErr)
				} else {
					s.logger.Info("successfully upgraded agy CLI", "version", manifest.Version, "path", newPath)
					s.cfg.SetAgyPath(newPath)
					result.AgyUpdated = true
					result.AgyNewVersion = manifest.Version

					s.updateComponentSuccess(BinaryAgy, manifest.Version, newPath)

					// Recycle pool workers to adopt upgraded binary
					if s.pool != nil {
						s.logger.Info("requesting rolling recycle of worker pool following agy CLI upgrade")
						s.pool.MarkAllForRecycle()
					}
				}
			}
		}
	}

	// 2. Check @fikrilal/agy-auth CLI
	if s.cfg.AutoUpdate.AgyAuth.Enabled {
		result.AgyAuthChecked = true
		currentPath := s.cfg.GetAgyAuthPath()
		currentVer, _ := s.agyAuthClient.GetCurrentVersion(ctx, currentPath)
		result.AgyAuthOldVersion = currentVer

		latestVer, downloadURL, checksumsURL, err := s.agyAuthClient.CheckLatest(ctx)
		if err != nil {
			msg := fmt.Sprintf("failed to query agy-auth upstream: %v", err)
			s.logger.Warn(msg)
			resultErrors = append(resultErrors, msg)
			s.updateComponentState(BinaryAgyAuth, currentVer, "", false, err)
		} else {
			updateAvailable := compareVersions(latestVer, currentVer) > 0
			s.updateComponentState(BinaryAgyAuth, currentVer, latestVer, updateAvailable, nil)

			if updateAvailable {
				if downloadURL == "" {
					msg := fmt.Sprintf("agy-auth %s is available upstream, but no release asset was found for platform %s/%s", latestVer, runtime.GOOS, runtime.GOARCH)
					s.logger.Warn(msg)
					resultErrors = append(resultErrors, msg)
				} else {
					s.logger.Info("new agy-auth version available; initiating installation",
						"current", currentVer,
						"latest", latestVer,
						"targetDir", s.cfg.AutoUpdate.BinDir,
					)

					s.mu.Lock()
					s.status = StatusUpdating
					s.mu.Unlock()

					newPath, installErr := s.agyAuthClient.DownloadAndInstall(ctx, downloadURL, checksumsURL, s.cfg.AutoUpdate.BinDir)
					if installErr != nil {
						msg := fmt.Sprintf("failed to download and install agy-auth %s: %v", latestVer, installErr)
						s.logger.Error(msg)
						resultErrors = append(resultErrors, msg)
						s.updateComponentState(BinaryAgyAuth, currentVer, latestVer, true, installErr)
					} else {
						s.logger.Info("successfully upgraded agy-auth", "version", latestVer, "path", newPath)
						s.cfg.SetAgyAuthPath(newPath)
						result.AgyAuthUpdated = true
						result.AgyAuthNewVersion = latestVer

						s.updateComponentSuccess(BinaryAgyAuth, latestVer, newPath)
					}
				}
			}
		}
	}

	s.mu.Lock()
	if len(resultErrors) > 0 {
		s.status = StatusFailed
	} else {
		s.status = StatusSuccess
	}
	s.mu.Unlock()

	result.Errors = resultErrors
	if len(resultErrors) > 0 {
		return result, fmt.Errorf("auto-updater encountered %d error(s)", len(resultErrors))
	}
	return result, nil
}

// @purpose: Probes active executables on disk to refresh their current version strings.
// @rationale: Guarantees that status reporting and telemetry reflect actual installed versions even before the first remote check runs.
// @preconditions: ctx must not be nil.
// - @param: ctx (context.Context) - Context controlling execution timeout.
// - @returns: None (mutates internal component telemetry).
// - @throws: None (failures to parse version result in retained or empty strings).
// - @invariants: Probes execute without holding locks to prevent latency contention; mu locked strictly during state assignment.
func (s *Service) RefreshCurrentVersions(ctx context.Context) {
	agyPath := s.cfg.GetAgyPath()
	agyVer, _ := s.agyClient.GetCurrentVersion(ctx, agyPath)

	authPath := s.cfg.GetAgyAuthPath()
	authVer, _ := s.agyAuthClient.GetCurrentVersion(ctx, authPath)

	s.mu.Lock()
	defer s.mu.Unlock()

	infoAgy := s.components[string(BinaryAgy)]
	infoAgy.ActiveBinaryPath = agyPath
	if agyVer != "" {
		infoAgy.CurrentVersion = agyVer
	}
	s.components[string(BinaryAgy)] = infoAgy

	infoAuth := s.components[string(BinaryAgyAuth)]
	infoAuth.ActiveBinaryPath = authPath
	if authVer != "" {
		infoAuth.CurrentVersion = authVer
	}
	s.components[string(BinaryAgyAuth)] = infoAuth
}

// GetStatus returns an instantaneous, thread-safe telemetry snapshot of the auto-updater.
func (s *Service) GetStatus() AutoUpdateStatusResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()

	comps := make(map[string]ComponentVersionInfo, len(s.components))
	for k, v := range s.components {
		comps[k] = v
	}

	return AutoUpdateStatusResponse{
		Enabled:     s.cfg.AutoUpdate.Enabled,
		Interval:    s.cfg.AutoUpdate.Interval.String(),
		NextCheckAt: s.nextCheck,
		Status:      s.status,
		Components:  comps,
	}
}

func (s *Service) updateComponentState(bin BinaryType, current, latest string, updateAvailable bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	info := s.components[string(bin)]
	if current != "" {
		info.CurrentVersion = current
	}
	if latest != "" {
		info.LatestVersion = latest
	}
	info.UpdateAvailable = updateAvailable
	info.LastCheckedAt = time.Now()
	if err != nil {
		info.LastError = err.Error()
	} else {
		info.LastError = ""
	}
	s.components[string(bin)] = info
}

func (s *Service) updateComponentSuccess(bin BinaryType, newVersion, newPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	info := s.components[string(bin)]
	info.CurrentVersion = newVersion
	info.UpdateAvailable = false
	info.LastUpdatedAt = now
	info.LastCheckedAt = now
	info.ActiveBinaryPath = newPath
	info.LastError = ""
	s.components[string(bin)] = info
}

// compareVersions returns 1 if v1 > v2, -1 if v1 < v2, and 0 if v1 == v2.
func compareVersions(v1, v2 string) int {
	v1 = cleanVersion(v1)
	v2 = cleanVersion(v2)

	if v1 == v2 {
		return 0
	}
	if v1 == "" {
		return -1
	}
	if v2 == "" {
		return 1
	}

	parts1 := strings.Split(v1, ".")
	parts2 := strings.Split(v2, ".")

	maxLen := len(parts1)
	if len(parts2) > maxLen {
		maxLen = len(parts2)
	}

	for i := 0; i < maxLen; i++ {
		var n1, n2 int
		if i < len(parts1) {
			n1, _ = strconv.Atoi(parts1[i])
		}
		if i < len(parts2) {
			n2, _ = strconv.Atoi(parts2[i])
		}
		if n1 > n2 {
			return 1
		}
		if n1 < n2 {
			return -1
		}
	}

	return 0
}

func cleanVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	idx := strings.IndexAny(v, "-+")
	if idx >= 0 {
		v = v[:idx]
	}
	return v
}
