// Package updater implements the automated background cron engine that checks,
// cryptographically verifies, downloads, and rolls out updates for the Google Antigravity
// CLI (agy) and @fikrilal/agy-auth binaries.
package updater

import (
	"time"
)

// BinaryType identifies a managed external binary component.
type BinaryType string

const (
	// BinaryAgy represents the Google Antigravity CLI flat binary.
	BinaryAgy BinaryType = "agy"
	// BinaryAgyAuth represents the @fikrilal/agy-auth multi-account credential manager.
	BinaryAgyAuth BinaryType = "agy-auth"
)

// UpdateStatus represents the lifecycle operational state of the auto-updater engine.
type UpdateStatus string

const (
	// StatusIdle indicates no active update operation is executing.
	StatusIdle UpdateStatus = "IDLE"
	// StatusChecking indicates upstream manifests and repositories are being queried.
	StatusChecking UpdateStatus = "CHECKING"
	// StatusUpdating indicates new binaries are being downloaded, verified, and installed.
	StatusUpdating UpdateStatus = "UPDATING"
	// StatusSuccess indicates the most recent check or update completed with exit code 0.
	StatusSuccess UpdateStatus = "SUCCESS"
	// StatusFailed indicates the most recent update attempt encountered an error.
	StatusFailed UpdateStatus = "FAILED"
)

// ComponentVersionInfo tracks versioning, inspection timestamps, and binary locations for a component.
type ComponentVersionInfo struct {
	CurrentVersion   string    `json:"current_version"`
	LatestVersion    string    `json:"latest_version"`
	UpdateAvailable  bool      `json:"update_available"`
	LastCheckedAt    time.Time `json:"last_checked_at"`
	LastUpdatedAt    time.Time `json:"last_updated_at"`
	ActiveBinaryPath string    `json:"active_binary_path"`
	LastError        string    `json:"last_error,omitempty"`
}

// AutoUpdateStatusResponse provides a snapshot of the updater engine for administrative telemetry.
type AutoUpdateStatusResponse struct {
	Enabled     bool                            `json:"enabled"`
	Interval    string                          `json:"interval"`
	NextCheckAt time.Time                       `json:"next_check_at"`
	Status      UpdateStatus                    `json:"status"`
	Components  map[string]ComponentVersionInfo `json:"components"`
}

// CheckResult details the granular outcome of a manual or automated check-and-update turn.
type CheckResult struct {
	AgyChecked        bool     `json:"agy_checked"`
	AgyUpdated        bool     `json:"agy_updated"`
	AgyOldVersion     string   `json:"agy_old_version,omitempty"`
	AgyNewVersion     string   `json:"agy_new_version,omitempty"`
	AgyAuthChecked    bool     `json:"agy_auth_checked"`
	AgyAuthUpdated    bool     `json:"agy_auth_updated"`
	AgyAuthOldVersion string   `json:"agy_auth_old_version,omitempty"`
	AgyAuthNewVersion string   `json:"agy_auth_new_version,omitempty"`
	Errors            []string `json:"errors,omitempty"`
}

// WorkerPoolRecycler defines the minimal contract required to trigger rolling worker refreshes.
type WorkerPoolRecycler interface {
	MarkAllForRecycle()
}
