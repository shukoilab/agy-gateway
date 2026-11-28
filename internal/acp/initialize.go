package acp

import "encoding/json"

// ClientIdentity is the fingerprint the gateway announces as `initialize.clientInfo`.
//
// @purpose Single source of truth for the client identity sent upstream.
// @rationale agy branches on the client name for a few vendor code paths and its own logs name the
// client, so the gateway must be able to present the reference editor fingerprint (default) rather
// than an invented one. Values are data supplied by configuration, never literals here.
//
// @invariants All three fields are populated by the caller; this type does not default them.
type ClientIdentity struct {
	Name    string
	Title   string
	Version string
}

// ClientCapabilities describes what the gateway tells the agent it can do.
//
// @purpose Drives the `initialize.clientCapabilities` block from one typed value.
// @rationale Every advertised capability must have a registered handler: agy turns fs.readTextFile
// into a native file tool, so advertising it without answering fs/read_text_file stalls the turn.
// Keeping this typed (rather than a map built inline) makes the advertisement auditable and lets
// configuration validation prove advertisement/handler parity before startup.
//
// @invariants FS and Terminal are only true when the transport answers their requests.
type ClientCapabilities struct {
	FS              bool
	Terminal        bool
	Elicitation     bool
	SessionMetadata bool
}

// MarshalJSON projects the capability flags onto the ACP v1 wire shape.
//
// @purpose Emits exactly the block the reference client emits, derived from four booleans.
// @rationale The wire block mixes styles: `fs` and `terminal` are real booleans, while `session`,
// `auth`, `elicitation` and `_meta` are declaration-only and carry empty objects. Serialising that
// from the same four flags keeps the advertisement and the handler set from drifting apart.
// @preconditions None.
// @returns The JSON encoding of the capability block.
// @throws Only the errors json.Marshal can produce for plain structs (none in practice).
// @invariants O(1); no shared state.
func (c ClientCapabilities) MarshalJSON() ([]byte, error) {
	type fsBlock struct {
		ReadTextFile  bool `json:"readTextFile"`
		WriteTextFile bool `json:"writeTextFile"`
	}
	type sessionBlock struct {
		Compaction    map[string]any `json:"compaction"`
		ConfigOptions struct {
			Boolean map[string]any `json:"boolean"`
		} `json:"configOptions"`
		Notices map[string]any `json:"notices"`
	}
	type authBlock struct {
		Terminal bool `json:"terminal"`
	}
	type elicitationBlock struct {
		Form map[string]any `json:"form"`
		URL  map[string]any `json:"url"`
	}
	type metaBlock struct {
		TerminalOutput bool `json:"terminal_output"`
		TerminalAuth   bool `json:"terminal-auth"`
	}

	wire := struct {
		FS       *fsBlock          `json:"fs,omitempty"`
		Terminal bool              `json:"terminal"`
		Session  *sessionBlock     `json:"session,omitempty"`
		Auth     *authBlock        `json:"auth,omitempty"`
		Elicit   *elicitationBlock `json:"elicitation,omitempty"`
		Meta     *metaBlock        `json:"_meta,omitempty"`
	}{Terminal: c.Terminal}

	if c.FS {
		wire.FS = &fsBlock{ReadTextFile: true, WriteTextFile: true}
	}
	if c.SessionMetadata {
		s := &sessionBlock{Compaction: map[string]any{}, Notices: map[string]any{}}
		s.ConfigOptions.Boolean = map[string]any{}
		wire.Session = s
		wire.Auth = &authBlock{Terminal: true}
		wire.Meta = &metaBlock{TerminalOutput: true, TerminalAuth: true}
	}
	if c.Elicitation {
		wire.Elicit = &elicitationBlock{Form: map[string]any{}, URL: map[string]any{}}
	}
	return json.Marshal(wire)
}

// InitializeParams is the complete `initialize` request payload.
//
// @purpose Makes the initialize payload a value the conformance tests can assert on.
// @rationale The block was previously assembled as an anonymous map inside Worker.Start, which is
// both unreadable and untestable, and it carried literals (a hardcoded protocolVersion, hardcoded
// identity strings, and false capabilities) that no configuration could correct.
type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientInfo         ClientInfo         `json:"clientInfo"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
}

// ClientInfo is the wire shape of `initialize.clientInfo`.
type ClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

// BuildInitializeParams assembles the `initialize` payload from configuration.
//
// @purpose Removes every hardcoded value from the handshake and makes the payload testable.
// @rationale See docs/acp-conformance.md §3: the reference client sends a full capability block and
// a real client fingerprint. The gateway was sending a two-key capability map with fs/terminal
// disabled and a self-invented identity, which is the anti-pattern this refactor exists to remove.
//
// @preconditions identity.Name is non-empty (configuration validation enforces it).
// @param identity The client fingerprint to announce.
// @param caps Which capabilities to advertise.
// @returns The complete params struct, ready to marshal.
// @throws None.
// @invariants The result always reports SupportedProtocolVersion; no field is left to a literal at
// the call site.
func BuildInitializeParams(identity ClientIdentity, caps ClientCapabilities) InitializeParams {
	return InitializeParams{
		ProtocolVersion: SupportedProtocolVersion,
		ClientInfo: ClientInfo{
			Name:    identity.Name,
			Title:   identity.Title,
			Version: identity.Version,
		},
		ClientCapabilities: caps,
	}
}
