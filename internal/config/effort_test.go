package config

import (
	"os"
	"testing"
)

// TestMapReasoningEffort_OnlyEmitsAcceptedWireValues is the regression test for the silent effort
// downgrade: the mapper used to return "off" and "extra", both of which agy rejects with -32602.
func TestMapReasoningEffort_OnlyEmitsAcceptedWireValues(t *testing.T) {
	cases := []struct {
		effort   string
		def      string
		expected string
	}{
		// Client-supplied values, canonical.
		{"low", "medium", ThoughtLevelLow},
		{"medium", "low", ThoughtLevelMedium},
		{"high", "low", ThoughtLevelHigh},
		{"xhigh", "low", ThoughtLevelXHigh},

		// Values agy rejects must be normalised onto an accepted level.
		{"extra", "low", ThoughtLevelXHigh},
		{"none", "high", ThoughtLevelLow},
		{"off", "high", ThoughtLevelLow},
		{"minimal", "high", ThoughtLevelLow},
		{"max", "low", ThoughtLevelXHigh},

		// Case/space tolerance.
		{"  HIGH  ", "low", ThoughtLevelHigh},
		{"XHigh", "low", ThoughtLevelXHigh},

		// Empty falls back to the configured default.
		{"", "high", ThoughtLevelHigh},
		{"", "xhigh", ThoughtLevelXHigh},
		{"", "", ThoughtLevelMedium},

		// Unknown tokens degrade to the documented default, never to a rejected value.
		{"banana", "low", ThoughtLevelMedium},
	}

	for _, tc := range cases {
		got := MapReasoningEffort(tc.effort, tc.def)
		if got != tc.expected {
			t.Errorf("MapReasoningEffort(%q, %q) = %q, want %q", tc.effort, tc.def, got, tc.expected)
		}
		if !IsThoughtLevel(got) {
			t.Errorf("MapReasoningEffort(%q, %q) produced %q which agy rejects", tc.effort, tc.def, got)
		}
	}

	// The two historically broken outputs must be unreachable.
	for _, in := range []string{"extra", "off", "none", "minimal", "max"} {
		if out := MapReasoningEffort(in, "medium"); out == "extra" || out == "off" {
			t.Fatalf("MapReasoningEffort(%q) still emits a rejected wire value %q", in, out)
		}
	}
}

func TestMapThinkingBudget(t *testing.T) {
	cases := map[int]string{
		0:     ThoughtLevelMedium,
		1:     ThoughtLevelLow,
		1024:  ThoughtLevelLow,
		1025:  ThoughtLevelMedium,
		4096:  ThoughtLevelMedium,
		4097:  ThoughtLevelHigh,
		16384: ThoughtLevelHigh,
		16385: ThoughtLevelXHigh,
		99999: ThoughtLevelXHigh,
		-5:    ThoughtLevelMedium,
	}
	for budget, want := range cases {
		if got := MapThinkingBudget(budget); got != want {
			t.Errorf("MapThinkingBudget(%d) = %q, want %q", budget, got, want)
		}
	}
}

func TestIsThoughtLevel(t *testing.T) {
	for _, v := range []string{"low", "medium", "high", "xhigh", "HIGH", " XHigh "} {
		if !IsThoughtLevel(v) {
			t.Errorf("expected %q to be accepted", v)
		}
	}
	for _, v := range []string{"", "off", "extra", "minimal", "max", "banana"} {
		if IsThoughtLevel(v) {
			t.Errorf("expected %q to be rejected", v)
		}
	}
}

// TestValidate_RejectsInvalidEnums ensures the enums that remain are still enforced.
func TestValidate_RejectsInvalidEnums(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*Config)
	}{
		{"bad vision mode", func(c *Config) { c.Execution.VisionMode = "hallucinate" }},
		{"zero max client tools", func(c *Config) { c.MCP.ClientToolCalling.MaxClientTools = 0 }},
		{"oversized max client tools", func(c *Config) { c.MCP.ClientToolCalling.MaxClientTools = 5000 }},
		{"zero intercepted cap", func(c *Config) { c.MCP.ClientToolCalling.MaxInterceptedToolCalls = 0 }},
		{"zero collect window", func(c *Config) { c.MCP.ClientToolCalling.ToolCallCollectWindow = 0 }},
		{"zero config switch timeout", func(c *Config) { c.Pool.ConfigSwitchTimeout = 0 }},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			cfg := DefaultConfig()
			m.mutate(cfg)
			if err := validate(cfg); err == nil {
				t.Fatalf("%s must be rejected", m.name)
			}
		})
	}
}

// TestDefaults_MatchVerifiedAgentBehaviour pins the defaults that encode hard-won empirical facts.
func TestDefaults_MatchVerifiedAgentBehaviour(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Execution.VisionMode != "reject" {
		t.Errorf("vision_mode must default to reject (agy advertises image support but fails every image turn); got %q", cfg.Execution.VisionMode)
	}
	if cfg.Binaries.AgyMinVersion == "" {
		t.Error("agy_min_version must not be empty")
	}
	if cfg.Pool.ConfigSwitchTimeout <= 0 {
		t.Error("config_switch_timeout must default to a positive duration")
	}
	if cfg.MCP.ClientToolCalling.ToolCallCollectWindow <= 0 {
		t.Error("tool_call_collect_window must default to a positive duration")
	}
	// The YOLO posture lives in acp.SpawnArgs, not in configuration. There is no argv field left to
	// carry it, so a stale config cannot reintroduce a blocking posture.
}

// TestLoad_RemovedKeysWarnButStillBoot proves backward compatibility: a stale configuration file
// containing the four removed keys must load successfully. This matters most for
// auto_approve_permissions, which used to disable auto-approval.
func TestLoad_RemovedKeysWarnButStillBoot(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := tmpDir + "/legacy.yaml"

	content := `
server:
  port: 9090

binaries:
  agy_path: "/usr/local/bin/agy"
  agy_min_version: "1.5.16"
  agy_acp_args: ["acp", "--dangerously-skip-permissions"]

execution:
  acp_session_mode: "accept-edits"
  auto_approve_permissions: false
  vision_mode: "reject"
`
	if err := os.WriteFile(configFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := Load(configFile)
	if err != nil {
		t.Fatalf("a stale config must still boot, got: %v", err)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("surviving keys must still apply, got port %d", cfg.Server.Port)
	}
	if cfg.Execution.VisionMode != "reject" {
		t.Errorf("surviving execution keys must still apply, got %q", cfg.Execution.VisionMode)
	}
}

// TestCollectRemovedEnvKeys_DisclosesStaleEnvironment proves the env path is covered too: most
// deployments configure the gateway through .env or docker-compose, so a YAML-only check would never
// reach an operator who set auto_approve_permissions=false.
func TestCollectRemovedEnvKeys_DisclosesStaleEnvironment(t *testing.T) {
	env := map[string]string{
		"AGY_GW_AUTO_APPROVE_PERMISSIONS": "false",
		"AGY_GW_UNSUPPORTED_PARAM_POLICY": "reject",
		"AGY_GW_SYSTEM_PROMPT_CHANNEL":    "append",
		"UNRELATED_VAR":                   "keep-me",
	}
	got := collectRemovedEnvKeys(func(k string) string { return env[k] })
	if len(got) != 3 {
		t.Fatalf("expected 3 stale env vars, got %v", got)
	}
	for _, key := range got {
		if RemovedKeyNote(key) == "" {
			t.Errorf("removed key %q has no operator-facing note", key)
		}
	}

	// An empty environment is a no-op; a nil lookup must not panic.
	if stale := collectRemovedEnvKeys(func(string) string { return "" }); len(stale) != 0 {
		t.Errorf("empty environment must yield no stale keys, got %v", stale)
	}
	if stale := collectRemovedEnvKeys(nil); len(stale) != 0 {
		t.Errorf("nil lookup must yield no stale keys, got %v", stale)
	}
}

// TestCollectRemovedKeys_OnlyFiresOnRealKeys ensures a clean or unparseable document yields nothing,
// so a normal config produces no misleading startup warning.
func TestCollectRemovedKeys_OnlyFiresOnRealKeys(t *testing.T) {
	if got := collectRemovedKeys([]byte("server:\n  port: 8080\nexecution:\n  vision_mode: reject\n")); len(got) != 0 {
		t.Errorf("clean config must yield no stale keys, got %v", got)
	}
	if got := collectRemovedKeys(nil); len(got) != 0 {
		t.Errorf("nil document must yield no stale keys, got %v", got)
	}
	if got := collectRemovedKeys([]byte("::not yaml::")); len(got) != 0 {
		t.Errorf("unparseable document must yield no stale keys, got %v", got)
	}

	legacy := []byte("execution:\n  acp_session_mode: auto\n  auto_approve_permissions: false\nbinaries:\n  agy_acp_args: [acp]\n")
	got := collectRemovedKeys(legacy)
	if len(got) != 3 {
		t.Fatalf("expected 3 stale YAML keys, got %v", got)
	}
	for _, key := range got {
		if RemovedKeyNote(key) == "" {
			t.Errorf("removed key %q has no operator-facing note", key)
		}
	}
}

// TestLoad_PopulatesRemovedKeysFromYAMLAndEnv proves the Load-side half of the disclosure pipeline:
// a stale file and a stale environment both surface in Config.RemovedKeys, which is the only input
// config.WarnRemovedKeys reads. Without this, deleting the collector wiring leaves every gate green
// while operators silently lose the warning.
func TestLoad_PopulatesRemovedKeysFromYAMLAndEnv(t *testing.T) {
	// Pin the removed env names so the assertion does not depend on the developer's shell.
	for _, name := range removedEnvKeys {
		t.Setenv(name, "")
	}
	t.Setenv("AGY_GW_AUTO_APPROVE_PERMISSIONS", "false")
	t.Setenv("AGY_GW_UNSUPPORTED_PARAM_POLICY", "reject")

	configFile := t.TempDir() + "/legacy.yaml"
	content := "execution:\n  acp_session_mode: auto\n  auto_approve_permissions: false\n  system_prompt_channel: append\n"
	if err := os.WriteFile(configFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := Load(configFile)
	if err != nil {
		t.Fatalf("a stale config must still boot, got: %v", err)
	}

	want := []string{
		"execution.acp_session_mode",
		"execution.auto_approve_permissions",
		"execution.system_prompt_channel",
		"AGY_GW_AUTO_APPROVE_PERMISSIONS",
		"AGY_GW_UNSUPPORTED_PARAM_POLICY",
	}
	if len(cfg.RemovedKeys) != len(want) {
		t.Fatalf("expected %d removed keys, got %v", len(want), cfg.RemovedKeys)
	}
	for i, key := range want {
		if cfg.RemovedKeys[i] != key {
			t.Errorf("RemovedKeys[%d] = %q, want %q", i, cfg.RemovedKeys[i], key)
		}
		if RemovedKeyNote(key) == "" {
			t.Errorf("removed key %q has no operator-facing note", key)
		}
	}
}

// TestLoad_CleanConfigReportsNoRemovedKeys guards the other direction: a current config must not
// produce a misleading startup warning.
func TestLoad_CleanConfigReportsNoRemovedKeys(t *testing.T) {
	for _, name := range removedEnvKeys {
		t.Setenv(name, "")
	}

	configFile := t.TempDir() + "/clean.yaml"
	if err := os.WriteFile(configFile, []byte("execution:\n  vision_mode: reject\n"), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := Load(configFile)
	if err != nil {
		t.Fatalf("clean config must load, got: %v", err)
	}
	if len(cfg.RemovedKeys) != 0 {
		t.Errorf("clean config must report no removed keys, got %v", cfg.RemovedKeys)
	}
}
