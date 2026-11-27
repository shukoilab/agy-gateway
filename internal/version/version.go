// Package version provides the centralized semantic versioning metadata for agy-gateway.
package version

import "fmt"

// Version represents the active semantic version of agy-gateway.
// Can be dynamically overridden at compile time via Go ldflags:
// -ldflags="-X agy-gateway/internal/version.Version=X.Y.Z"
var Version = "3.16.6"

// Get returns the current semantic version string.
//
// @purpose Exposes the single source of truth for runtime versioning.
// @rationale Eliminates hardcoded version strings across packages and enables dynamic build stamping.
// @returns Current semver string (e.g. "3.12.0").
// @invariants Pure read, thread-safe.
func Get() string {
	return Version
}

// UserAgent generates a standardized RFC-compliant HTTP User-Agent header.
//
// @purpose Creates uniform telemetry and client identity headers across outbound requests.
// @rationale Provides upstream services with consistent service and version identifiers.
// @param component Subsystem or module name (e.g. "updater", "modelsdev").
// @returns Standardized User-Agent string.
// @invariants Thread-safe, non-allocating after format string construction.
func UserAgent(component string) string {
	if component == "" {
		return fmt.Sprintf("agy-gateway/%s (+https://github.com/shukoilab/agy-gateway)", Version)
	}
	return fmt.Sprintf("agy-gateway-%s/%s (+https://github.com/shukoilab/agy-gateway)", component, Version)
}
