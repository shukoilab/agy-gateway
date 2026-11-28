package acp

import (
	"encoding/json"
	"testing"

	"agy-gateway/internal/config"
)

// referenceZedCapabilities is the exact `initialize.clientCapabilities` block the reference editor
// sends, transcribed from docs/acp-conformance.md §3.1.
//
// @purpose Pins the wire shape so a refactor cannot silently drop a capability block.
// @rationale The gateway previously advertised `fs:{readTextFile:false,writeTextFile:false}` and
// nothing else, so agy never offered its native file or terminal tools. This constant is the
// regression anchor: the reference sends all of it.
// @invariants Kept character-for-character in sync with the captured payload in the doc.
const referenceZedCapabilities = `{
  "fs": {"readTextFile": true, "writeTextFile": true},
  "terminal": true,
  "session": {"compaction": {}, "configOptions": {"boolean": {}}, "notices": {}},
  "auth": {"terminal": true},
  "elicitation": {"form": {}, "url": {}},
  "_meta": {"terminal_output": true, "terminal-auth": true}
}`

// TestBuildInitializeParams_MatchesReferenceWireShape asserts the gateway's own handshake against
// the captured reference payload.
//
// @purpose Makes docs/acp-conformance.md §3 executable instead of prose that can rot.
// @rationale The whole refactor exists because the handshake was an untested anonymous map. A test
// that compares the marshalled bytes to the captured block is the only thing that keeps the two
// from drifting again.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; mismatches are reported through t.Fatalf.
// @invariants O(n) over a fixed-size payload; no I/O.
func TestBuildInitializeParams_MatchesReferenceWireShape(t *testing.T) {
	cfg := config.DefaultConfig()

	params := BuildInitializeParams(
		ClientIdentity{Name: cfg.Identity.Name, Title: cfg.Identity.Title, Version: cfg.Identity.Version},
		ClientCapabilities{
			FS:              cfg.Capabilities.FS,
			Terminal:        cfg.Capabilities.Terminal,
			Elicitation:     cfg.Capabilities.Elicitation,
			SessionMetadata: cfg.Capabilities.SessionMetadata,
		},
	)

	if params.ProtocolVersion != SupportedProtocolVersion {
		t.Fatalf("protocolVersion = %d, want %d", params.ProtocolVersion, SupportedProtocolVersion)
	}

	gotCaps, err := json.Marshal(params.ClientCapabilities)
	if err != nil {
		t.Fatalf("marshal capabilities: %v", err)
	}
	if !jsonEqual(t, gotCaps, []byte(referenceZedCapabilities)) {
		t.Fatalf("clientCapabilities drifted from the reference block\n got: %s\nwant: %s",
			gotCaps, referenceZedCapabilities)
	}

	if params.ClientInfo.Name != "zed" || params.ClientInfo.Title != "Zed" {
		t.Fatalf("clientInfo = %+v, want the Zed fingerprint", params.ClientInfo)
	}
	if params.ClientInfo.Version != config.DefaultZedClientVersion {
		t.Fatalf("clientInfo.version = %q, want %q",
			params.ClientInfo.Version, config.DefaultZedClientVersion)
	}
}

// TestBuildInitializeParams_GatewayProfileRestoresSelfIdentity pins the opt-out profile.
//
// @purpose Proves the reference fingerprint is configuration, not a code literal.
// @rationale An operator may need agy to log the gateway's own name (e.g. to distinguish gateway
// traffic from a real editor). The profile must change every identity field without touching the
// capability block.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; mismatches are reported through t.Fatalf.
// @invariants Deterministic.
func TestBuildInitializeParams_GatewayProfileRestoresSelfIdentity(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Identity.Profile = config.IdentityProfileGateway
	cfg.Identity.Name = "agy-gateway"
	cfg.Identity.Title = "Antigravity Gateway"
	cfg.Identity.Version = "9.9.9"

	params := BuildInitializeParams(
		ClientIdentity{Name: cfg.Identity.Name, Title: cfg.Identity.Title, Version: cfg.Identity.Version},
		ClientCapabilities{FS: true, Terminal: true, SessionMetadata: true, Elicitation: true},
	)

	if params.ClientInfo.Name != "agy-gateway" || params.ClientInfo.Version != "9.9.9" {
		t.Fatalf("gateway profile must use its own identity, got %+v", params.ClientInfo)
	}
}

// TestClientCapabilities_SandboxEmitsNoFsOrTerminal pins the disabled direction.
//
// @purpose Proves a deployment that cannot answer fs/terminal requests never advertises them.
// @rationale Advertising without a handler is what stalled turns; the sandbox profile must be
// unable to claim a capability it will not serve.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; mismatches are reported through t.Fatalf.
// @invariants Deterministic.
func TestClientCapabilities_SandboxEmitsNoFsOrTerminal(t *testing.T) {
	raw, err := json.Marshal(ClientCapabilities{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := decoded["fs"]; present {
		t.Fatalf("a capability set with FS=false must omit the fs block, got %s", raw)
	}
	if _, present := decoded["session"]; present {
		t.Fatalf("a capability set with SessionMetadata=false must omit the session block, got %s", raw)
	}
	if _, present := decoded["elicitation"]; present {
		t.Fatalf("a capability set with Elicitation=false must omit the elicitation block, got %s", raw)
	}
	// `terminal` is always serialised because the field is a plain boolean in the wire shape; for
	// ACP v1 the reference always sends it, and `false` is the honest value.
	var terminal bool
	if err := json.Unmarshal(decoded["terminal"], &terminal); err != nil {
		t.Fatalf("decode terminal: %v", err)
	}
	if terminal {
		t.Fatalf("terminal must be false when not advertised, got %s", raw)
	}
}

// jsonEqual reports whether two JSON documents are semantically equal (key order and whitespace
// irrelevant).
//
// @purpose Compares marshalled payloads without depending on struct field order.
// @rationale A byte comparison would break on a harmless field reorder, which would train
// maintainers to loosen the test.
// @preconditions Both inputs are valid JSON.
// @param t Standard Go test handle, used for failure reporting.
// @param a First document.
// @param b Second document.
// @returns True when both documents decode to deeply equal values.
// @throws None.
// @invariants O(size of the smaller document).
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("decode first document: %v", err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("decode second document: %v", err)
	}
	aj, err := json.Marshal(av)
	if err != nil {
		t.Fatalf("re-encode first document: %v", err)
	}
	bj, err := json.Marshal(bv)
	if err != nil {
		t.Fatalf("re-encode second document: %v", err)
	}
	return string(aj) == string(bj)
}
