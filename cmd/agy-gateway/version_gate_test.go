package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"agy-gateway/internal/config"
)

// @purpose: Pins the numeric comparison used by the startup version gate, including the
// case lexical comparison gets wrong ("1.5.9" vs "1.5.16").
// @rationale: A string comparison would accept an older agent and silently reinstate every
// defect this task compensates for; the ordering must be proven, not assumed.
// @preconditions: None.
// @param: t (*testing.T) - Go test harness.
// @returns: None.
// @throws: Calls t.Fatalf on any mismatch.
// @invariants: Table-driven, no I/O, deterministic.
func TestCompareVersions_NumericOrdering(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.5.16", "1.5.9", 1},
		{"1.5.9", "1.5.16", -1},
		{"1.5.16", "1.5.16", 0},
		{"v1.5.16", "1.5.16", 0},
		{"1.5", "1.5.0", 0},
		{"2.0.0", "1.99.99", 1},
		{"1.6.0", "1.5.16", 1},
		{"garbage", "0.0.1", -1},
	}
	for _, tc := range cases {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q,%q) = %d; want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// @purpose: Proves the startup gate rejects an agent older than the configured minimum and
// accepts an equal or newer one, driven through a real child process.
// @rationale: The gate's value depends on actually executing the binary, so a fake script is
// used rather than a stubbed comparison (anti-decorative test mandate).
// @preconditions: /bin/sh is available on the host running the tests.
// @param: t (*testing.T) - Go test harness.
// @returns: None.
// @throws: Calls t.Fatalf on any mismatch.
// @invariants: Creates and removes one temporary script; leaves no child process.
func TestCheckAgyVersion_Gate(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-agy")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	writeFake := func(version string) {
		body := "#!/bin/sh\necho '" + version + "'\n"
		if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
			t.Fatalf("write fake agy: %v", err)
		}
	}
	newCfg := func(min string) *config.Config {
		cfg := config.DefaultConfig()
		cfg.Binaries.AgyPath = script
		cfg.Binaries.AgyMinVersion = min
		return cfg
	}

	writeFake("1.5.9")
	if err := checkAgyVersion(newCfg("1.5.16"), logger); err == nil {
		t.Fatal("expected older 1.5.9 to be rejected against 1.5.16")
	}

	writeFake("1.5.16")
	if err := checkAgyVersion(newCfg("1.5.16"), logger); err != nil {
		t.Fatalf("expected equal 1.5.16 to pass: %v", err)
	}

	writeFake("agy 1.6.2 (build abc)")
	if err := checkAgyVersion(newCfg("1.5.16"), logger); err != nil {
		t.Fatalf("expected newer 1.6.2 to pass: %v", err)
	}

	// An unparsable version coerces to 0.0.0 and is therefore rejected, never silently accepted.
	writeFake("not-a-version")
	if err := checkAgyVersion(newCfg("1.5.16"), logger); err == nil {
		t.Fatal("expected unparsable version to fail closed")
	}

	// A missing minimum disables the gate entirely.
	writeFake("0.0.1")
	if err := checkAgyVersion(newCfg(""), logger); err != nil {
		t.Fatalf("expected empty minimum to disable the gate: %v", err)
	}
}
