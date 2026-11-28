package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"agy-gateway/internal/config"
)

// maxConversationKeyBytes bounds the harness identifier the gateway will hash.
//
// @purpose Keeps a caller-controlled header from being hashed at unbounded size.
// @rationale The value arrives in a request header, so its length is attacker-controlled. Anything
// above this is not a real conversation identifier and is treated as absent.
const maxConversationKeyBytes = 64 << 10

// affinityHeaderNames is the ordered list of request headers the gateway treats as a conversation key.
//
// @purpose Gives one place that decides which harness identifiers are honoured.
// @rationale Every entry is a header an existing harness already sends without configuration —
// measured from shipped artifacts: OpenCode sends `x-session-affinity`, `X-Session-Id` and
// `x-opencode-session` on every request; LangChain/Fireworks and openclaw send `x-session-affinity`;
// pi sends `x-session-affinity` and `session_id`; Hermes forwards `x-opencode-session` or a
// user-configured header name. `x-session-affinity` is the only name shared by four independent
// implementations, so it is checked first. Header lookups are case-insensitive in net/http.
// @preconditions None.
// @param None.
// @returns Header names in priority order.
// @throws None.
// @invariants Immutable read-only slice.
var affinityHeaderNames = []string{
	"X-Session-Affinity",
	"X-Session-Id",
	"Session-Id",
	"X-Opencode-Session",
	"X-Opencode-Session-Id",
}

// conversationKeyFrom extracts a harness conversation identifier from a request.
//
// @purpose Decides which directory a turn runs in, so two live conversations cannot overwrite each
// other's files.
// @rationale The identity is only used for directory isolation: it never selects an account, never
// influences eligibility, and never becomes part of a path in raw form. That separation is what makes
// a caller-controlled header safe to honour.
// @preconditions None.
// @param r Incoming request; may be nil.
// @param promptCacheKey Optional body-level key (OpenAI `prompt_cache_key`), used only when no header
// is present because a body field is weaker evidence of conversation identity than a header.
// @returns A 64-character hex digest and true, or "" and false when the request carries no identifier.
// @throws None.
// @invariants O(len(value)); the digest is always fixed-length hex.
func conversationKeyFrom(r *http.Request, promptCacheKey string) (string, bool) {
	raw := ""
	if r != nil {
		for _, name := range affinityHeaderNames {
			if v := strings.TrimSpace(r.Header.Get(name)); v != "" {
				raw = v
				break
			}
		}
	}
	if raw == "" {
		raw = strings.TrimSpace(promptCacheKey)
	}
	return hashConversationKey(raw)
}

// hashConversationKey turns a harness identifier into a safe directory name.
//
// @purpose Removes every character a caller could use to traverse or collide paths.
// @rationale The value is a request header, so it can contain `..`, `/`, NUL, or invalid UTF-8.
// Hashing makes the output a fixed 64-character lowercase hex string that is safe as a single path
// segment while still being stable for the same conversation.
// @preconditions None.
// @param raw Identifier as received.
// @returns The digest and true, or "" and false for an empty or oversized value.
// @throws None.
// @invariants O(len(raw)); never returns a path separator.
func hashConversationKey(raw string) (string, bool) {
	if raw == "" || len(raw) > maxConversationKeyBytes {
		return "", false
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:]), true
}

// conversationWorkspace resolves the directory a turn runs in.
//
// @purpose One durable workspace root, with an optional per-conversation subdirectory derived from the
// harness's own conversation id.
// @rationale File state is the only thing that survives a turn: every request opens and closes its own
// ACP session, so an agent's files are what it re-reads next turn. Two live conversations sharing one
// directory can overwrite each other, so the identifier the harness already sends isolates them. A
// request without one keeps running in the root, which means no harness behaves worse than before.
// @preconditions root is the configured storage.workspace_dir; key is output of conversationKeyFrom.
// @param root Absolute workspace root; empty disables the per-conversation directory.
// @param key Hex digest, or "" for none.
// @returns The directory to use and its name relative to the root ("" when using the root itself).
// @throws The directory-creation error, so the caller answers explicitly instead of running in a
// directory the agent cannot write to.
// @invariants O(1); creates at most one directory; the name is a fixed-length hex digest so no caller
// input reaches the path.
func conversationWorkspace(root, key string) (string, string, error) {
	if root == "" || key == "" {
		// No conversation identity (or no workspace configured): run in the worker's own Cwd, which is
		// already the shared workspace root. Creating the root here would duplicate the provisioning
		// step and make an unwritable default path fail every request.
		return "", "", nil
	}
	dir := filepath.Join(root, key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	return dir, key, nil
}

// workspaceDirForRequest resolves the directory one request runs in.
//
// @purpose Single call site for both provider surfaces, so the isolation rule cannot drift between them.
// @rationale Anthropic has no body-level cache key, so it passes an empty promptCacheKey and relies on
// headers only; keeping that difference at the caller instead of duplicating the rule is what makes the
// two providers behave identically for a harness that only sends headers.
// @preconditions cfg non-nil.
// @param cfg Gateway configuration providing storage.workspace_dir.
// @param r Incoming request.
// @param promptCacheKey Optional body-level conversation identifier.
// @returns The directory for this turn ("" when no workspace is configured) and the relative name.
// @throws The directory-creation error.
// @invariants O(1); no path component is derived from caller input without hashing.
func workspaceDirForRequest(cfg *config.Config, r *http.Request, promptCacheKey string) (string, string, error) {
	root := ""
	if cfg != nil {
		root = cfg.Storage.EffectiveWorkspaceDir()
	}
	key, _ := conversationKeyFrom(r, promptCacheKey)
	return conversationWorkspace(root, key)
}
