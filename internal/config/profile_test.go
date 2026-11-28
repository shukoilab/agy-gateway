package config

import (
	"strings"
	"testing"
)

// TestValidate_SandboxProfileAdvertisesNothing pins the capability honesty rule.
//
// @purpose Proves the sandbox profile cannot accidentally advertise fs or terminal.
// @rationale Advertising a capability without a handler makes the agent issue requests nobody
// answers, which stalls the turn; the profile exists so an operator can refuse those features
// outright rather than rely on remembering to unset each flag.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Pure.
func TestValidate_SandboxProfileAdvertisesNothing(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Capabilities.Profile = CapabilitiesProfileSandbox
	// Simulate an operator who set the individual flags; the profile must win.
	cfg.Capabilities.FS = true
	cfg.Capabilities.Terminal = true
	cfg.Capabilities.Elicitation = true

	if err := validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Capabilities.FS || cfg.Capabilities.Terminal ||
		cfg.Capabilities.Elicitation || cfg.Capabilities.SessionMetadata {
		t.Fatalf("sandbox profile must advertise nothing, got %+v", cfg.Capabilities)
	}
}

// TestValidate_GatewayProfileReplacesOnlyDefaultIdentity pins the override precedence.
//
// @purpose Proves switching profile rewrites the reference fingerprint but never clobbers an
// explicit operator value.
// @rationale An operator who set identity.name deliberately must not have it silently replaced;
// an operator who only switched profiles must not keep claiming to be Zed.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Pure.
func TestValidate_GatewayProfileReplacesOnlyDefaultIdentity(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Identity.Profile = IdentityProfileGateway
	if err := validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Identity.Name != "agy-gateway" || cfg.Identity.Title != "Antigravity Gateway" {
		t.Fatalf("gateway profile did not replace the reference identity: %+v", cfg.Identity)
	}
	if cfg.Identity.Version == DefaultZedClientVersion {
		t.Fatal("gateway profile must report the gateway version, not the reference client version")
	}

	// An explicit name survives the profile switch.
	cfg2 := DefaultConfig()
	cfg2.Identity.Profile = IdentityProfileGateway
	cfg2.Identity.Name = "my-harness"
	cfg2.Identity.Title = "My Harness"
	if err := validate(cfg2); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg2.Identity.Name != "my-harness" || cfg2.Identity.Title != "My Harness" {
		t.Fatalf("explicit identity was overwritten: %+v", cfg2.Identity)
	}
}

// TestValidate_RejectsUnknownProfiles pins the fail-fast contract.
//
// @purpose Proves a typo in a profile name is a startup error, not a silent fallback.
// @rationale Silent fallback would leave the gateway claiming an identity or capability set the
// operator did not choose, which is exactly the class of bug this refactor removed.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Pure.
func TestValidate_RejectsUnknownProfiles(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Identity.Profile = "zedd"
	err := validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "identity.profile") {
		t.Fatalf("want an identity.profile error, got %v", err)
	}

	cfg = DefaultConfig()
	cfg.Capabilities.Profile = "sandboxed"
	err = validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "capabilities.profile") {
		t.Fatalf("want a capabilities.profile error, got %v", err)
	}
}

// TestValidate_RejectsEmptyIdentity pins that an empty fingerprint cannot be sent.
//
// @purpose Proves the gateway never emits an initialize with a blank clientInfo field.
// @rationale Some agents key behaviour off clientInfo; an empty string is indistinguishable from a
// broken client and would be a silent regression of D1.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Pure.
func TestValidate_RejectsEmptyIdentity(t *testing.T) {
	for _, field := range []string{"name", "title", "version"} {
		cfg := DefaultConfig()
		switch field {
		case "name":
			cfg.Identity.Name = "  "
		case "title":
			cfg.Identity.Title = ""
		case "version":
			cfg.Identity.Version = "\t"
		}
		if err := validate(cfg); err == nil {
			t.Fatalf("an empty identity.%s must be rejected", field)
		}
	}
}

// TestValidate_UnsupportedParameterPolicy pins the closed policy enum.
//
// @purpose Proves only `ignore` and `reject` are accepted.
// @rationale A third value would silently behave like one of them, which is the guesswork this
// policy exists to remove.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Pure.
func TestValidate_UnsupportedParameterPolicy(t *testing.T) {
	for _, ok := range []string{UnsupportedParameterPolicyIgnore, UnsupportedParameterPolicyReject} {
		cfg := DefaultConfig()
		cfg.Execution.UnsupportedParameterPolicy = ok
		if err := validate(cfg); err != nil {
			t.Fatalf("%q must be accepted: %v", ok, err)
		}
	}
	cfg := DefaultConfig()
	cfg.Execution.UnsupportedParameterPolicy = "warn"
	if err := validate(cfg); err == nil {
		t.Fatal("an unknown unsupported_parameter_policy must be rejected")
	}
}
