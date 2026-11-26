package acp

import "testing"

// TestSpawnArgs_EnforcesYOLOPosture pins the product invariant that makes the gateway a provider
// endpoint: the agent must be launched with --dangerously-skip-permissions so no configuration can
// turn a turn into a blocking approval prompt.
func TestSpawnArgs_EnforcesYOLOPosture(t *testing.T) {
	args := SpawnArgs()
	if len(args) != 2 || args[0] != "acp" || args[1] != "--dangerously-skip-permissions" {
		t.Fatalf("unexpected argv: %v", args)
	}
}

// TestSpawnArgs_ReturnsCallerOwnedSlice proves each call gets its own slice, so one caller mutating
// the argv cannot silently degrade every later worker in the pool.
func TestSpawnArgs_ReturnsCallerOwnedSlice(t *testing.T) {
	first := SpawnArgs()
	first[1] = "--tampered"

	second := SpawnArgs()
	if second[1] != "--dangerously-skip-permissions" {
		t.Fatalf("mutating one argv leaked into the next call: %v", second)
	}
}
