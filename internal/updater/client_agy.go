package updater

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"agy-gateway/internal/version"
)

var semverRegex = regexp.MustCompile(`\b(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)\b`)

// AgyManifest represents the JSON payload returned by the Google Antigravity release manifest endpoint.
type AgyManifest struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA512  string `json:"sha512"`
}

// AgyClient handles queries, downloads, checksum verifications, and installations for the agy CLI.
type AgyClient struct {
	httpClient  *http.Client
	manifestURL string
}

// NewAgyClient constructs an AgyClient with the specified HTTP client and base manifest URL.
func NewAgyClient(httpClient *http.Client, manifestURL string) *AgyClient {
	if manifestURL == "" {
		manifestURL = "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests"
	}
	manifestURL = strings.TrimSuffix(manifestURL, "/")
	return &AgyClient{
		httpClient:  httpClient,
		manifestURL: manifestURL,
	}
}

// DetectPlatform resolves the Antigravity manifest platform identifier based on GOOS and GOARCH.
func (c *AgyClient) DetectPlatform() (string, error) {
	osName := runtime.GOOS
	arch := runtime.GOARCH

	switch arch {
	case "amd64":
		arch = "amd64"
	case "arm64":
		arch = "arm64"
	default:
		return "", fmt.Errorf("unsupported architecture for agy CLI: %s", arch)
	}

	switch osName {
	case "linux":
		// Detect musl vs glibc on Linux
		if isMusl() {
			return fmt.Sprintf("linux_%s_musl", arch), nil
		}
		return fmt.Sprintf("linux_%s", arch), nil
	case "darwin":
		return fmt.Sprintf("darwin_%s", arch), nil
	case "windows":
		return fmt.Sprintf("windows_%s", arch), nil
	default:
		return "", fmt.Errorf("unsupported operating system for agy CLI: %s", osName)
	}
}

// CheckLatest queries the official Antigravity manifest for the active host platform.
func (c *AgyClient) CheckLatest(ctx context.Context) (*AgyManifest, error) {
	platform, err := c.DetectPlatform()
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/%s.json", c.manifestURL, platform)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create manifest request: %w", err)
	}
	req.Header.Set("User-Agent", version.UserAgent("updater"))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch agy manifest from %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("agy manifest query returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var manifest AgyManifest
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("failed to decode agy manifest JSON: %w", err)
	}

	if manifest.Version == "" || manifest.URL == "" {
		return nil, fmt.Errorf("invalid agy manifest payload: missing version or URL")
	}

	return &manifest, nil
}

// GetCurrentVersion runs the binary at binaryPath with --version and extracts the semantic version.
func (c *AgyClient) GetCurrentVersion(ctx context.Context, binaryPath string) (string, error) {
	if binaryPath == "" {
		return "", fmt.Errorf("empty binary path")
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

// DownloadAndInstall downloads the archive, verifies SHA-512, extracts the binary, and atomically installs it.
func (c *AgyClient) DownloadAndInstall(ctx context.Context, manifest *AgyManifest, binDir string) (string, error) {
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create destination directory %s: %w", binDir, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifest.URL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to construct download request for %s: %w", manifest.URL, err)
	}
	req.Header.Set("User-Agent", version.UserAgent("updater"))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to download agy archive: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("agy download endpoint returned HTTP %d", resp.StatusCode)
	}

	// Stream archive to a temporary file while hashing SHA-512
	tmpArchive, err := os.CreateTemp(binDir, "agy-download-*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("failed to create temporary archive file: %w", err)
	}
	defer func() {
		_ = os.Remove(tmpArchive.Name())
	}()

	hasher := sha512.New()
	writer := io.MultiWriter(tmpArchive, hasher)

	if _, err := io.Copy(writer, resp.Body); err != nil {
		_ = tmpArchive.Close()
		return "", fmt.Errorf("failed to stream agy download: %w", err)
	}
	_ = tmpArchive.Close()

	// Verify SHA-512 checksum if provided in manifest
	if manifest.SHA512 != "" {
		computedHash := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(computedHash, manifest.SHA512) {
			return "", fmt.Errorf("agy SHA-512 checksum mismatch: expected %s, got %s", manifest.SHA512, computedHash)
		}
	}

	// Extract binary from tar.gz
	f, err := os.Open(tmpArchive.Name())
	if err != nil {
		return "", fmt.Errorf("failed to open downloaded archive: %w", err)
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return "", fmt.Errorf("failed to initialize gzip reader: %w", err)
	}

	tr := tar.NewReader(gzr)
	var foundBinary bool
	tmpBinaryPath := filepath.Join(binDir, fmt.Sprintf("agy.tmp.%d.%d", os.Getpid(), os.Getpid()+int(hasher.Sum(nil)[0])))
	defer func() {
		_ = os.Remove(tmpBinaryPath)
	}()

	targetFilename := "agy"
	if runtime.GOOS == "windows" {
		targetFilename = "agy.exe"
	}
	// Upstream renamed the archive member from `agy` to `antigravity` in 1.5.17 while the installed
	// path stayed `agy`. Accept both names and always install under `agy`: the pool spawns that path.
	// The canonical name wins if an archive ever ships both, so tar order cannot decide the result.
	aliasFilename := "antigravity"
	if runtime.GOOS == "windows" {
		aliasFilename = "antigravity.exe"
	}

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = gzr.Close()
			_ = f.Close()
			return "", fmt.Errorf("failed reading tar stream: %w", err)
		}

		base := filepath.Base(header.Name)
		isCanonical := base == targetFilename
		isAlias := base == aliasFilename
		if (!isCanonical && !isAlias) || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) {
			continue
		}
		foundBinary = true
		outBinary, err := os.OpenFile(tmpBinaryPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			_ = gzr.Close()
			_ = f.Close()
			return "", fmt.Errorf("failed to create temporary binary %s: %w", tmpBinaryPath, err)
		}
		// Guard against unbounded decompression bombs with 500MB safety ceiling
		if _, err := io.Copy(outBinary, io.LimitReader(tr, 500*1024*1024)); err != nil {
			_ = outBinary.Close()
			_ = gzr.Close()
			_ = f.Close()
			return "", fmt.Errorf("failed to extract agy binary: %w", err)
		}
		_ = outBinary.Close()
		// A later canonical member overwrites an earlier alias (same truncating path), so keep
		// scanning after an alias hit but stop as soon as the canonical name is found.
		if isCanonical {
			break
		}
	}
	_ = gzr.Close()
	_ = f.Close()

	if !foundBinary {
		return "", fmt.Errorf("binary %q not found in downloaded archive", targetFilename)
	}

	// Smoke test: verify execution before replacing active binary
	testCmd := exec.CommandContext(ctx, tmpBinaryPath, "--version")
	if out, err := testCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("downloaded agy binary failed verification test: %w (%s)", err, string(out))
	}

	// Atomic rename to final path
	finalPath := filepath.Join(binDir, targetFilename)
	if err := os.Rename(tmpBinaryPath, finalPath); err != nil {
		return "", fmt.Errorf("failed to atomically install agy binary to %s: %w", finalPath, err)
	}

	return finalPath, nil
}

func isMusl() bool {
	if _, err := os.Stat("/lib/libc.musl-x86_64.so.1"); err == nil {
		return true
	}
	if _, err := os.Stat("/lib/libc.musl-aarch64.so.1"); err == nil {
		return true
	}
	return false
}
