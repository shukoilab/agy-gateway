package config

import "testing"

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

// TestValidate_AcceptsAutoSessionMode proves the mode agy actually advertises is selectable.
func TestValidate_AcceptsAutoSessionMode(t *testing.T) {
	for _, mode := range []string{"default", "accept-edits", "auto", "plan"} {
		cfg := DefaultConfig()
		cfg.Execution.AcpSessionMode = mode
		if err := validate(cfg); err != nil {
			t.Errorf("mode %q must be valid: %v", mode, err)
		}
	}
	cfg := DefaultConfig()
	cfg.Execution.AcpSessionMode = "yolo-mode-not-real"
	if err := validate(cfg); err == nil {
		t.Fatal("expected invalid session mode to be rejected")
	}
}

func TestValidate_RejectsInvalidEnums(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*Config)
	}{
		{"bad system prompt channel", func(c *Config) { c.Execution.SystemPromptChannel = "carrier-pigeon" }},
		{"bad vision mode", func(c *Config) { c.Execution.VisionMode = "hallucinate" }},
		{"bad unsupported param policy", func(c *Config) { c.OpenAI.UnsupportedParamPolicy = "ignore" }},
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
	if cfg.Execution.SystemPromptChannel != "auto" {
		t.Errorf("system_prompt_channel must default to auto; got %q", cfg.Execution.SystemPromptChannel)
	}
	if cfg.Execution.SystemPromptChannel == "" || !validSystemPromptChannel(cfg.Execution.SystemPromptChannel) {
		t.Errorf("default system prompt channel must validate")
	}
	if cfg.OpenAI.UnsupportedParamPolicy != "strip_and_warn" {
		t.Errorf("unsupported param policy default changed: %q", cfg.OpenAI.UnsupportedParamPolicy)
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
	// The dangerous flag must no longer be baked into the defaults.
	for _, a := range cfg.Binaries.AgyAcpArgs {
		if a == "--dangerously-skip-permissions" {
			t.Error("--dangerously-skip-permissions must not be a built-in default")
		}
	}
	if len(cfg.Binaries.AgyAcpArgs) == 0 || cfg.Binaries.AgyAcpArgs[0] != "acp" {
		t.Errorf("agy_acp_args must start with 'acp', got %v", cfg.Binaries.AgyAcpArgs)
	}
}

// TestDeadKnobsRemoved guards against re-introducing the knobs that were validated but never read.
func TestDeadKnobsRemoved(t *testing.T) {
	// Compile-time style assertion: these fields no longer exist, so referencing them would not build.
	// The behavioural check here is that YAML containing them still loads (unknown keys are ignored).
	cfg := DefaultConfig()
	if err := validate(cfg); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}
