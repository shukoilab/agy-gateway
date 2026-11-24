package updater

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// GitHubReleaseAsset represents an asset attached to a GitHub release.
type GitHubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// GitHubRelease represents the GitHub releases API payload.
type GitHubRelease struct {
	TagName string               `json:"tag_name"`
	Assets  []GitHubReleaseAsset `json:"assets"`
}

// NpmPackageResponse represents minimal metadata from the npm registry for fallback version checks.
type NpmPackageResponse struct {
	Version string `json:"version"`
}

// AgyAuthClient coordinates version discovery, downloading, checksum verification, and atomic installation for agy-auth.
type AgyAuthClient struct {
	httpClient  *http.Client
	releaseRepo string
	apiBaseURL  string
	npmBaseURL  string
}

// NewAgyAuthClient constructs an AgyAuthClient.
func NewAgyAuthClient(httpClient *http.Client, releaseRepo string) *AgyAuthClient {
	if releaseRepo == "" {
		releaseRepo = "fikrilal/agy-auth"
	}
	return &AgyAuthClient{
		httpClient:  httpClient,
		releaseRepo: releaseRepo,
		apiBaseURL:  "https://api.github.com",
		npmBaseURL:  "https://registry.npmjs.org",
	}
}

// DetectAssetName resolves the binary release asset name for the active OS and architecture.
func (c *AgyAuthClient) DetectAssetName() (string, error) {
	osName := runtime.GOOS
	arch := runtime.GOARCH

	switch osName {
	case "linux":
		switch arch {
		case "amd64":
			return "agy-auth-linux-amd64", nil
		case "arm64":
			return "agy-auth-linux-arm64", nil
		default:
			return "", fmt.Errorf("unsupported linux arch for agy-auth: %s", arch)
		}
	case "darwin":
		switch arch {
		case "amd64":
			return "agy-auth-darwin-x64", nil
		case "arm64":
			return "agy-auth-darwin-arm64", nil
		default:
			return "", fmt.Errorf("unsupported darwin arch for agy-auth: %s", arch)
		}
	case "windows":
		switch arch {
		case "amd64":
			return "agy-auth-win32-x64.exe", nil
		case "arm64":
			return "agy-auth-win32-arm64.exe", nil
		default:
			return "", fmt.Errorf("unsupported windows arch for agy-auth: %s", arch)
		}
	default:
		return "", fmt.Errorf("unsupported OS for agy-auth: %s", osName)
	}
}

// CheckLatest queries the latest GitHub release or falls back to npm registry.
func (c *AgyAuthClient) CheckLatest(ctx context.Context) (version string, downloadURL string, checksumsURL string, err error) {
	apiBase := c.apiBaseURL
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	apiURL := fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimSuffix(apiBase, "/"), c.releaseRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("User-Agent", "agy-gateway-updater/3.5.0")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err == nil && resp.StatusCode == http.StatusOK {
		defer resp.Body.Close()
		var rel GitHubRelease
		if err := json.NewDecoder(resp.Body).Decode(&rel); err == nil {
			targetAsset, assetErr := c.DetectAssetName()
			if assetErr == nil {
				for _, asset := range rel.Assets {
					if asset.Name == targetAsset {
						downloadURL = asset.BrowserDownloadURL
					}
					if asset.Name == "checksums.txt" {
						checksumsURL = asset.BrowserDownloadURL
					}
				}
				if downloadURL != "" {
					v := strings.TrimPrefix(rel.TagName, "v")
					return v, downloadURL, checksumsURL, nil
				}
			}
		}
	}
	if resp != nil {
		_ = resp.Body.Close()
	}

	// Fallback to npm registry for version check
	npmBase := c.npmBaseURL
	if npmBase == "" {
		npmBase = "https://registry.npmjs.org"
	}
	npmURL := fmt.Sprintf("%s/@fikrilal/agy-auth/latest", strings.TrimSuffix(npmBase, "/"))
	npmReq, err := http.NewRequestWithContext(ctx, http.MethodGet, npmURL, nil)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to construct npm query: %w", err)
	}
	npmReq.Header.Set("User-Agent", "agy-gateway-updater/3.5.0")

	npmResp, err := c.httpClient.Do(npmReq)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to check agy-auth upstream: %w", err)
	}
	defer npmResp.Body.Close()

	if npmResp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("npm query returned HTTP %d", npmResp.StatusCode)
	}

	var npmPkg NpmPackageResponse
	if err := json.NewDecoder(npmResp.Body).Decode(&npmPkg); err != nil {
		return "", "", "", fmt.Errorf("failed to parse npm response: %w", err)
	}

	targetAsset, _ := c.DetectAssetName()
	fallbackDownloadURL := fmt.Sprintf("https://github.com/%s/releases/download/v%s/%s", c.releaseRepo, npmPkg.Version, targetAsset)
	fallbackChecksumURL := fmt.Sprintf("https://github.com/%s/releases/download/v%s/checksums.txt", c.releaseRepo, npmPkg.Version)

	return npmPkg.Version, fallbackDownloadURL, fallbackChecksumURL, nil
}

// GetCurrentVersion runs agy-auth --version and extracts the semantic version.
func (c *AgyAuthClient) GetCurrentVersion(ctx context.Context, binaryPath string) (string, error) {
	if binaryPath == "" {
		return "", fmt.Errorf("empty agy-auth binary path")
	}
	if _, err := os.Stat(binaryPath); err != nil {
		return "", fmt.Errorf("binary not found at %s: %w", binaryPath, err)
	}

	cmd := exec.CommandContext(ctx, binaryPath, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to execute %s --version: %w (%s)", binaryPath, err, string(out))
	}

	matches := semverRegex.FindStringSubmatch(string(out))
	if len(matches) > 1 {
		return matches[1], nil
	}
	v := strings.TrimSpace(string(out))
	if v != "" {
		return v, nil
	}
	return "", fmt.Errorf("failed to parse version from %s output: %q", binaryPath, string(out))
}

// DownloadAndInstall downloads the agy-auth standalone binary, verifies SHA-256, and atomically installs it.
func (c *AgyAuthClient) DownloadAndInstall(ctx context.Context, downloadURL string, checksumsURL string, binDir string) (string, error) {
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create binDir %s: %w", binDir, err)
	}

	expectedSHA256 := ""
	targetAsset, err := c.DetectAssetName()
	if err != nil {
		return "", err
	}

	// Fetch checksums.txt if available
	if checksumsURL != "" {
		chkReq, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumsURL, nil)
		if err == nil {
			chkReq.Header.Set("User-Agent", "agy-gateway-updater/3.5.0")
			if chkResp, err := c.httpClient.Do(chkReq); err == nil {
				if chkResp.StatusCode == http.StatusOK {
					scanner := bufio.NewScanner(chkResp.Body)
					for scanner.Scan() {
						line := strings.TrimSpace(scanner.Text())
						parts := strings.Fields(line)
						if len(parts) >= 2 && filepath.Base(parts[1]) == targetAsset {
							expectedSHA256 = parts[0]
							break
						}
					}
					if scanErr := scanner.Err(); scanErr != nil {
						_ = chkResp.Body.Close()
						return "", fmt.Errorf("error reading checksums from %s: %w", checksumsURL, scanErr)
					}
				}
				_ = chkResp.Body.Close()
			}
		}
	}

	// Download binary
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create download request for %s: %w", downloadURL, err)
	}
	req.Header.Set("User-Agent", "agy-gateway-updater/3.5.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to download agy-auth binary: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("agy-auth download returned HTTP %d", resp.StatusCode)
	}

	targetFilename := "agy-auth"
	if runtime.GOOS == "windows" {
		targetFilename = "agy-auth.exe"
	}
	tmpBinaryPath := filepath.Join(binDir, fmt.Sprintf("agy-auth.tmp.%d.%d", os.Getpid(), os.Getuid()))
	defer func() {
		_ = os.Remove(tmpBinaryPath)
	}()

	outBinary, err := os.OpenFile(tmpBinaryPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return "", fmt.Errorf("failed to create temporary binary %s: %w", tmpBinaryPath, err)
	}

	hasher := sha256.New()
	writer := io.MultiWriter(outBinary, hasher)

	// Guard against unbounded payload stream with 500MB safety ceiling
	if _, err := io.Copy(writer, io.LimitReader(resp.Body, 500*1024*1024)); err != nil {
		_ = outBinary.Close()
		return "", fmt.Errorf("failed to stream agy-auth binary: %w", err)
	}
	_ = outBinary.Close()

	// Verify SHA-256 checksum if available
	if expectedSHA256 != "" {
		computedHash := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(computedHash, expectedSHA256) {
			return "", fmt.Errorf("agy-auth SHA-256 checksum mismatch: expected %s, got %s", expectedSHA256, computedHash)
		}
	}

	// Smoke test: verify execution before replacing active binary
	testCmd := exec.CommandContext(ctx, tmpBinaryPath, "--version")
	if out, err := testCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("downloaded agy-auth binary failed verification test: %w (%s)", err, string(out))
	}

	// Atomic rename to final path
	finalPath := filepath.Join(binDir, targetFilename)
	if err := os.Rename(tmpBinaryPath, finalPath); err != nil {
		return "", fmt.Errorf("failed to atomically install agy-auth binary to %s: %w", finalPath, err)
	}

	return finalPath, nil
}
