package updater

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agy-gateway/internal/config"
)

type mockPoolRecycler struct {
	recycles atomic.Int32
}

func (m *mockPoolRecycler) MarkAllForRecycle() {
	m.recycles.Add(1)
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		v1       string
		v2       string
		expected int
	}{
		{"1.5.17", "1.5.16", 1},
		{"v1.5.17", "1.5.16", 1},
		{"1.5.16", "1.5.16", 0},
		{"v0.5.3", "0.5.3", 0},
		{"0.5.2", "0.5.3", -1},
		{"1.6.0", "1.5.99", 1},
		{"2.0.0", "1.99.99", 1},
		{"", "1.0.0", -1},
		{"1.0.0", "", 1},
	}

	for _, tt := range tests {
		got := compareVersions(tt.v1, tt.v2)
		if got != tt.expected {
			t.Errorf("compareVersions(%q, %q) = %d, expected %d", tt.v1, tt.v2, got, tt.expected)
		}
	}
}

func TestAgyClient_CheckLatest_And_Install(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Create a dummy valid agy executable
	dummyScript := "#!/bin/sh\necho 1.5.17\n"

	// Create dummy tar.gz archive
	archivePath := filepath.Join(tmpDir, "cli_linux_x64.tar.gz")
	tarFile, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("failed to create tar file: %v", err)
	}
	gzw := gzip.NewWriter(tarFile)
	tw := tar.NewWriter(gzw)

	hdr := &tar.Header{
		Name: "agy",
		Mode: 0755,
		Size: int64(len(dummyScript)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("failed to write tar header: %v", err)
	}
	if _, err := tw.Write([]byte(dummyScript)); err != nil {
		t.Fatalf("failed to write tar body: %v", err)
	}
	_ = tw.Close()
	_ = gzw.Close()
	_ = tarFile.Close()

	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("failed to read created archive: %v", err)
	}
	hasher := sha512.New()
	hasher.Write(archiveBytes)
	sha512Hex := hex.EncodeToString(hasher.Sum(nil))

	// 2. Mock HTTP server
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if filepath.Ext(r.URL.Path) == ".json" {
			manifest := AgyManifest{
				Version: "1.5.17",
				URL:     "http://" + r.Host + "/download/cli_linux_x64.tar.gz",
				SHA512:  sha512Hex,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(manifest)
			return
		}
		if r.URL.Path == "/download/cli_linux_x64.tar.gz" {
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(archiveBytes)
			return
		}
		http.NotFound(w, r)
	}))
	defer mockServer.Close()

	client := NewAgyClient(mockServer.Client(), mockServer.URL)

	ctx := context.Background()
	manifest, err := client.CheckLatest(ctx)
	if err != nil {
		t.Fatalf("CheckLatest failed: %v", err)
	}
	if manifest.Version != "1.5.17" {
		t.Errorf("expected version 1.5.17, got %s", manifest.Version)
	}

	binDir := filepath.Join(tmpDir, "bin")
	installedPath, err := client.DownloadAndInstall(ctx, manifest, binDir)
	if err != nil {
		t.Fatalf("DownloadAndInstall failed: %v", err)
	}

	ver, err := client.GetCurrentVersion(ctx, installedPath)
	if err != nil {
		t.Fatalf("GetCurrentVersion failed on installed binary: %v", err)
	}
	if ver != "1.5.17" {
		t.Errorf("expected verified version 1.5.17, got %s", ver)
	}
}

func TestAgyClient_ChecksumMismatch(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if filepath.Ext(r.URL.Path) == ".json" {
			manifest := AgyManifest{
				Version: "1.5.17",
				URL:     "http://" + r.Host + "/download/corrupt.tar.gz",
				SHA512:  "bad-checksum-hash-value-12345",
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(manifest)
			return
		}
		w.Write([]byte("not valid tar.gz"))
	}))
	defer mockServer.Close()

	client := NewAgyClient(mockServer.Client(), mockServer.URL)
	manifest, err := client.CheckLatest(context.Background())
	if err != nil {
		t.Fatalf("CheckLatest failed: %v", err)
	}

	_, err = client.DownloadAndInstall(context.Background(), manifest, t.TempDir())
	if err == nil {
		t.Errorf("expected checksum verification error, got nil")
	}
}

func TestAgyAuthClient_CheckLatest_And_Install(t *testing.T) {
	tmpDir := t.TempDir()
	dummyScript := "#!/bin/sh\necho agy-auth 0.5.4\n"
	scriptBytes := []byte(dummyScript)

	hasher := sha256.New()
	hasher.Write(scriptBytes)
	sha256Hex := hex.EncodeToString(hasher.Sum(nil))

	targetAsset, err := (&AgyAuthClient{}).DetectAssetName()
	if err != nil {
		t.Skip("skipping agy-auth platform asset test: unsupported OS/arch")
	}

	checksumsContent := fmt.Sprintf("%s  %s\n", sha256Hex, targetAsset)

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/fikrilal/agy-auth/releases/latest" {
			rel := GitHubRelease{
				TagName: "v0.5.4",
				Assets: []GitHubReleaseAsset{
					{
						Name:               targetAsset,
						BrowserDownloadURL: "http://" + r.Host + "/download/" + targetAsset,
					},
					{
						Name:               "checksums.txt",
						BrowserDownloadURL: "http://" + r.Host + "/download/checksums.txt",
					},
				},
			}
			_ = json.NewEncoder(w).Encode(rel)
			return
		}
		if r.URL.Path == "/download/checksums.txt" {
			_, _ = w.Write([]byte(checksumsContent))
			return
		}
		if r.URL.Path == "/download/"+targetAsset {
			_, _ = w.Write(scriptBytes)
			return
		}
		http.NotFound(w, r)
	}))
	defer mockServer.Close()

	client := NewAgyAuthClient(mockServer.Client(), "fikrilal/agy-auth")
	client.apiBaseURL = mockServer.URL

	version, downloadURL, checksumsURL, err := client.CheckLatest(context.Background())
	if err != nil {
		t.Fatalf("CheckLatest failed: %v", err)
	}
	if version != "0.5.4" {
		t.Errorf("expected version 0.5.4, got %s", version)
	}

	binDir := filepath.Join(tmpDir, "bin")
	installedPath, err := client.DownloadAndInstall(context.Background(), downloadURL, checksumsURL, binDir)
	if err != nil {
		t.Fatalf("DownloadAndInstall failed: %v", err)
	}

	ver, err := client.GetCurrentVersion(context.Background(), installedPath)
	if err != nil {
		t.Fatalf("GetCurrentVersion failed: %v", err)
	}
	if ver != "0.5.4" {
		t.Errorf("expected version 0.5.4, got %s", ver)
	}
}

func TestService_Workflow_Lifecycle(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutoUpdate.Interval = 100 * time.Millisecond
	cfg.AutoUpdate.CheckOnStartup = false
	cfg.AutoUpdate.BinDir = t.TempDir()

	pool := &mockPoolRecycler{}
	svc := NewService(cfg, pool, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc.Start(ctx)

	status := svc.GetStatus()
	if !status.Enabled {
		t.Errorf("expected status.Enabled to be true")
	}

	// Verify Stop shuts down cleanly without hanging
	svc.Stop()
}

func TestService_CheckAndUpdateAll_EndToEnd(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Prepare dummy agy tar.gz
	dummyAgyScript := "#!/bin/sh\necho 1.5.18\n"
	archivePath := filepath.Join(tmpDir, "cli_linux_x64.tar.gz")
	tarFile, _ := os.Create(archivePath)
	gzw := gzip.NewWriter(tarFile)
	tw := tar.NewWriter(gzw)
	_ = tw.WriteHeader(&tar.Header{Name: "agy", Mode: 0755, Size: int64(len(dummyAgyScript))})
	_, _ = tw.Write([]byte(dummyAgyScript))
	_ = tw.Close()
	_ = gzw.Close()
	_ = tarFile.Close()

	archiveBytes, _ := os.ReadFile(archivePath)
	hasher512 := sha512.New()
	hasher512.Write(archiveBytes)
	sha512Hex := hex.EncodeToString(hasher512.Sum(nil))

	// 2. Prepare dummy agy-auth binary and checksum
	dummyAuthScript := "#!/bin/sh\necho agy-auth 0.5.5\n"
	targetAsset, err := (&AgyAuthClient{}).DetectAssetName()
	if err != nil {
		t.Skip("skipping test: unsupported platform asset")
	}
	hasher256 := sha256.New()
	hasher256.Write([]byte(dummyAuthScript))
	sha256Hex := hex.EncodeToString(hasher256.Sum(nil))
	checksumsContent := fmt.Sprintf("%s  %s\n", sha256Hex, targetAsset)

	// 3. Mock HTTP Server
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Agy Manifest
		if filepath.Ext(r.URL.Path) == ".json" && !strings.Contains(r.URL.Path, "releases") {
			_ = json.NewEncoder(w).Encode(AgyManifest{
				Version: "1.5.18",
				URL:     "http://" + r.Host + "/download/cli_linux_x64.tar.gz",
				SHA512:  sha512Hex,
			})
			return
		}
		if r.URL.Path == "/download/cli_linux_x64.tar.gz" {
			_, _ = w.Write(archiveBytes)
			return
		}
		// Agy-Auth Release
		if strings.Contains(r.URL.Path, "/releases/latest") {
			_ = json.NewEncoder(w).Encode(GitHubRelease{
				TagName: "v0.5.5",
				Assets: []GitHubReleaseAsset{
					{Name: targetAsset, BrowserDownloadURL: "http://" + r.Host + "/download/" + targetAsset},
					{Name: "checksums.txt", BrowserDownloadURL: "http://" + r.Host + "/download/checksums.txt"},
				},
			})
			return
		}
		if r.URL.Path == "/download/checksums.txt" {
			_, _ = w.Write([]byte(checksumsContent))
			return
		}
		if r.URL.Path == "/download/"+targetAsset {
			_, _ = w.Write([]byte(dummyAuthScript))
			return
		}
		http.NotFound(w, r)
	}))
	defer mockServer.Close()

	// 4. Configure test Service
	cfg := config.DefaultConfig()
	cfg.AutoUpdate.BinDir = filepath.Join(tmpDir, "bin")
	cfg.AutoUpdate.AgyCLI.ManifestURL = mockServer.URL
	cfg.AutoUpdate.AgyAuth.ReleaseRepo = "fikrilal/agy-auth"

	// Create initial dummy binaries (v1.5.16 and v0.5.3) so updater sees upgrade opportunity
	initialAgy := filepath.Join(tmpDir, "initial-agy")
	_ = os.WriteFile(initialAgy, []byte("#!/bin/sh\necho 1.5.16\n"), 0755)
	cfg.SetAgyPath(initialAgy)

	initialAuth := filepath.Join(tmpDir, "initial-auth")
	_ = os.WriteFile(initialAuth, []byte("#!/bin/sh\necho agy-auth 0.5.3\n"), 0755)
	cfg.SetAgyAuthPath(initialAuth)

	pool := &mockPoolRecycler{}
	svc := NewService(cfg, pool, nil)
	svc.agyAuthClient.apiBaseURL = mockServer.URL

	ctx := context.Background()
	result, err := svc.CheckAndUpdateAll(ctx)
	if err != nil {
		t.Fatalf("CheckAndUpdateAll failed: %v", err)
	}

	if !result.AgyUpdated {
		t.Errorf("expected AgyUpdated to be true")
	}
	if result.AgyNewVersion != "1.5.18" {
		t.Errorf("expected AgyNewVersion 1.5.18, got %s", result.AgyNewVersion)
	}
	if !result.AgyAuthUpdated {
		t.Errorf("expected AgyAuthUpdated to be true")
	}
	if result.AgyAuthNewVersion != "0.5.5" {
		t.Errorf("expected AgyAuthNewVersion 0.5.5, got %s", result.AgyAuthNewVersion)
	}

	// Verify worker pool was notified to reload
	if pool.recycles.Load() != 1 {
		t.Errorf("expected pool to be recycled 1 time, got %d", pool.recycles.Load())
	}

	// Verify status telemetry
	status := svc.GetStatus()
	if status.Status != StatusSuccess {
		t.Errorf("expected status SUCCESS, got %s", status.Status)
	}
	agyComp := status.Components[string(BinaryAgy)]
	if agyComp.CurrentVersion != "1.5.18" {
		t.Errorf("expected agy current_version 1.5.18, got %s", agyComp.CurrentVersion)
	}
	authComp := status.Components[string(BinaryAgyAuth)]
	if authComp.CurrentVersion != "0.5.5" {
		t.Errorf("expected agy-auth current_version 0.5.5, got %s", authComp.CurrentVersion)
	}
}
