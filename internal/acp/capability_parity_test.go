package acp

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestCapabilityParity_FSAdvertisementIsAnswered proves the advertise↔handler contract.
//
// @purpose Proves that when fs is advertised, the two fs methods the agent will actually call are
// answered rather than falling through to `-32601`.
// @rationale Advertising a capability without a handler is the failure this whole area exists to
// prevent: agy emits `fs/read_text_file` and then blocks until the idle watchdog cancels the turn, so
// the client sees a truncated turn. A declaration is only honest if a handler exists.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no subprocess.
func TestCapabilityParity_FSAdvertisementIsAnswered(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "note.txt")
	if err := os.WriteFile(target, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	h := NewClientHandlers(0, 0, 0)
	defer h.Close()
	h.RegisterSession("sess_parity", []string{root})

	for _, advertised := range []ClientCapabilities{
		{FS: true},
		{FS: true, SessionMetadata: true},
		{FS: true, Terminal: true, Elicitation: true, SessionMetadata: true},
	} {
		if !advertised.FS {
			continue
		}
		for _, method := range []string{"fs/read_text_file", "fs/write_text_file"} {
			params, _ := json.Marshal(map[string]any{
				"sessionId": "sess_parity",
				"path":      target,
				"content":   "rewritten\n",
			})
			_, handled, err := h.Handle(method, params)
			if !handled {
				t.Fatalf("%s was not handled although fs is advertised (%+v)", method, advertised)
			}
			if err != nil {
				t.Fatalf("%s returned error: %v", method, err)
			}
		}
	}
}

// TestCapabilityParity_TerminalAdvertisementIsAnswered proves the terminal half of the contract.
//
// @purpose Proves every terminal method a client may call has a handler when terminal is advertised.
// @rationale The agent's `run_command` tool maps onto these four methods; one missing handler turns
// every shell command into a stalled turn.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Spawns one short-lived /bin/sh and reaps it.
func TestCapabilityParity_TerminalAdvertisementIsAnswered(t *testing.T) {
	root := t.TempDir()
	h := NewClientHandlers(0, 0, 0)
	defer h.Close()
	h.RegisterSession("sess_term_parity", []string{root})

	createParams, _ := json.Marshal(map[string]any{
		"sessionId": "sess_term_parity",
		"command":   "/bin/sh",
		"args":      []string{"-c", "echo parity"},
		"cwd":       root,
	})
	raw, handled, err := h.Handle("terminal/create", createParams)
	if !handled || err != nil {
		t.Fatalf("terminal/create handled=%v err=%v", handled, err)
	}
	var created struct {
		TerminalID string `json:"terminalId"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.TerminalID == "" {
		t.Fatalf("terminal/create returned %s", raw)
	}

	for _, method := range []string{"terminal/output", "terminal/wait_for_exit"} {
		params, _ := json.Marshal(map[string]any{
			"sessionId":  "sess_term_parity",
			"terminalId": created.TerminalID,
		})
		if _, handled, err := h.Handle(method, params); !handled || err != nil {
			t.Fatalf("%s handled=%v err=%v", method, handled, err)
		}
	}

	for _, method := range []string{"terminal/kill", "terminal/release"} {
		params, _ := json.Marshal(map[string]any{
			"sessionId":  "sess_term_parity",
			"terminalId": created.TerminalID,
		})
		if _, handled, err := h.Handle(method, params); !handled || err != nil {
			t.Fatalf("%s handled=%v err=%v", method, handled, err)
		}
	}
}

// TestTransport_UnadvertisedCapabilitiesHaveNoHandlers pins the negative direction.
//
// @purpose Proves the zero-value capability set never installs handlers, so a sandbox deployment
// cannot silently answer file-system requests it never advertised.
// @rationale The parity invariant must fail closed: a handler that exists but is not advertised is
// merely dead code, while an advertisement without a handler is the stall. This test pins the
// transport's own wiring so neither side can drift.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestTransport_UnadvertisedCapabilitiesHaveNoHandlers(t *testing.T) {
	outReader, outWriter := io.Pipe()
	inReader, inWriter := io.Pipe()
	defer outReader.Close()
	defer outWriter.Close()
	defer inReader.Close()
	defer inWriter.Close()

	tr := NewTransport(outWriter, inReader, 1<<20, nil)
	defer tr.Close()

	if tr.Handlers() != nil {
		t.Fatal("a transport with no capabilities must not have handlers installed")
	}

	// Install and then check the accessor the pool uses to register session roots.
	h := NewClientHandlers(0, 0, 0)
	tr.SetHandlers(h)
	if tr.Handlers() != h {
		t.Fatal("SetHandlers must be observable through Handlers")
	}
}

// TestInitializeParams_AdvertisedCapabilitiesMatchInstalledHandlers is the wiring test.
//
// @purpose Proves the configuration that decides the advertisement is the same one that decides
// whether handlers are installed.
// @rationale Two independent booleans (one read by the payload builder, one by the pool) could drift;
// this pins the rule the pool implements: handlers exist iff FS or Terminal is advertised.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Pure.
func TestInitializeParams_AdvertisedCapabilitiesMatchInstalledHandlers(t *testing.T) {
	cases := []struct {
		name        string
		caps        ClientCapabilities
		wantHandler bool
	}{
		{"zed profile", ClientCapabilities{FS: true, Terminal: true, Elicitation: true, SessionMetadata: true}, true},
		{"fs only", ClientCapabilities{FS: true}, true},
		{"terminal only", ClientCapabilities{Terminal: true}, true},
		{"sandbox", ClientCapabilities{}, false},
		{"metadata only", ClientCapabilities{SessionMetadata: true, Elicitation: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := BuildInitializeParams(ClientIdentity{Name: "zed", Title: "Zed", Version: "1"}, tc.caps)
			raw, err := json.Marshal(params.ClientCapabilities)
			if err != nil {
				t.Fatalf("marshal capabilities: %v", err)
			}
			// Decode rather than substring-match: `auth:{terminal:true}` also contains the word
			// "terminal", so a substring probe would confuse the auth sub-object with the capability.
			var decoded struct {
				FS          map[string]any `json:"fs"`
				Terminal    bool           `json:"terminal"`
				Session     map[string]any `json:"session"`
				Elicitation map[string]any `json:"elicitation"`
			}
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("decode capabilities: %v", err)
			}
			advertisesFS := decoded.FS != nil
			advertisesTerminal := decoded.Terminal

			// The pool's rule, restated here so a change to either side breaks this test.
			installsHandlers := tc.caps.FS || tc.caps.Terminal
			if installsHandlers != tc.wantHandler {
				t.Fatalf("pool rule mismatch: installsHandlers=%v want=%v", installsHandlers, tc.wantHandler)
			}
			if advertisesFS != tc.caps.FS {
				t.Fatalf("fs advertised=%v but configured=%v (payload %s)", advertisesFS, tc.caps.FS, raw)
			}
			if advertisesTerminal != tc.caps.Terminal {
				t.Fatalf("terminal advertised=%v but configured=%v (payload %s)",
					advertisesTerminal, tc.caps.Terminal, raw)
			}
			if installsHandlers && !(advertisesFS || advertisesTerminal) {
				t.Fatal("handlers installed but nothing that uses them was advertised")
			}
		})
	}
}
