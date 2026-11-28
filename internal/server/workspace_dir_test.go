package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agy-gateway/internal/config"
)

// TestHashConversationKey_IsStableAndOpaque pins the hashing rule that keeps caller input out of a path.
//
// @purpose Proves a caller-supplied header becomes a fixed-length digest.
// @rationale The header flows into a directory name. Anything that preserves caller bytes there is a
// traversal and a log-injection risk, so the value must be a digest and nothing else.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestHashConversationKey_IsStableAndOpaque(t *testing.T) {
	const raw = "ses_de2edc493ffe4wVCO6FHdDFaV2"
	got, ok := hashConversationKey(raw)
	if !ok {
		t.Fatal("a normal key must be accepted")
	}
	sum := sha256.Sum256([]byte(raw))
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("hash = %q, want %q", got, want)
	}
	if len(got) != 64 {
		t.Fatalf("hash length = %d, want 64", len(got))
	}
	if again, _ := hashConversationKey(raw); again != got {
		t.Fatal("the same key must always hash to the same value")
	}
	if other, _ := hashConversationKey(raw + "x"); other == got {
		t.Fatal("different keys must not collide")
	}

	// A traversal-looking value must not survive into a path.
	traversal, ok := hashConversationKey("../../etc/passwd")
	if !ok {
		t.Fatal("any non-empty value within the size bound must be accepted")
	}
	if strings.ContainsAny(traversal, "./\\ ") {
		t.Fatalf("hash %q must not contain path characters", traversal)
	}
}

// TestHashConversationKey_RejectsEmptyAndOversized pins the two rejection cases.
//
// @purpose Proves an empty or enormous value is refused instead of silently used.
// @rationale An oversized header would otherwise become an oversized map key; an empty one must mean
// "no affinity", which is the pre-existing behaviour.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestHashConversationKey_RejectsEmptyAndOversized(t *testing.T) {
	if _, ok := hashConversationKey(""); ok {
		t.Fatal("empty key must be rejected")
	}
	if _, ok := hashConversationKey("   "); !ok {
		t.Fatal("whitespace is a legitimate opaque value")
	}
	huge := strings.Repeat("a", maxConversationKeyBytes+1)
	if _, ok := hashConversationKey(huge); ok {
		t.Fatal("a key over the size bound must be rejected")
	}
	edge := strings.Repeat("a", maxConversationKeyBytes)
	if _, ok := hashConversationKey(edge); !ok {
		t.Fatal("a key exactly at the size bound must be accepted")
	}
}

// TestConversationKeyFrom_HeaderPriorityAndBodyFallback pins the measured harness inventory.
//
// @purpose Proves the documented header order is honoured and the body field is the last resort.
// @rationale Four independent harnesses were measured sending `x-session-affinity`; the remaining
// names are fallbacks for the ones that do not. Choosing the wrong one silently splits a
// conversation across two directories.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestConversationKeyFrom_HeaderPriorityAndBodyFallback(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		body    string
		want    string
	}{
		{"affinity header wins over all", map[string]string{
			"X-Session-Affinity": "a", "X-Session-Id": "b", "X-Opencode-Session": "c",
		}, "d", "a"},
		{"X-Session-Id next", map[string]string{"X-Session-Id": "b", "X-Opencode-Session": "c"}, "d", "b"},
		{"session-id next", map[string]string{"Session-Id": "e", "X-Opencode-Session": "c"}, "d", "e"},
		{"opencode session next", map[string]string{"X-Opencode-Session": "c"}, "d", "c"},
		{"opencode session id last header", map[string]string{"X-Opencode-Session-Id": "f"}, "d", "f"},
		{"body fallback", nil, "d", "d"},
		{"nothing at all", nil, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			got, _ := conversationKeyFrom(r, tc.body)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("key = %q, want empty", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("key is empty, want the hash of %q", tc.want)
			}
			if want, ok := hashConversationKey(tc.want); ok && got != want {
				t.Fatalf("key = %q, want hash of %q (%q)", got, tc.want, want)
			}
		})
	}
}

// TestConversationWorkspace_DerivesSubdirOrRoot pins the two resolutions.
//
// @purpose Proves a keyed turn gets its own directory and an unkeyed turn keeps the worker's own
// working directory.
// @rationale The unkeyed case must not create the root: an unwritable configured path would then
// fail every request for harnesses that send no affinity header at all.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestConversationWorkspace_DerivesSubdirOrRoot(t *testing.T) {
	root := t.TempDir()

	dir, name, err := conversationWorkspace(root, "")
	if err != nil {
		t.Fatalf("unkeyed: %v", err)
	}
	if dir != "" || name != "" {
		t.Fatalf("unkeyed must defer to the worker cwd, got dir=%q name=%q", dir, name)
	}

	key, ok := hashConversationKey("conv-1")
	if !ok {
		t.Fatal("hash failed")
	}
	dir, name, err = conversationWorkspace(root, key)
	if err != nil {
		t.Fatalf("keyed: %v", err)
	}
	if name != key {
		t.Fatalf("dir name = %q, want %q", name, key)
	}
	if filepath.Dir(dir) != filepath.Clean(root) {
		t.Fatalf("dir = %q, want a direct child of %q", dir, root)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("per-conversation dir must exist, stat err = %v", err)
	}

	// An empty root means "not configured": no directory, no error.
	if dir, _, err := conversationWorkspace("", key); err != nil || dir != "" {
		t.Fatalf("empty root must yield no dir, got %q / %v", dir, err)
	}
}

// TestWorkspaceDirForRequest_UsesEffectiveConfigAndSeparatesConversations pins the end-to-end wiring.
//
// @purpose Proves two different affinity values get two different directories under one config.
// @rationale This is the property that stops two harness conversations from overwriting each other's
// files while still letting a single conversation keep its files across account rotations.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestWorkspaceDirForRequest_UsesEffectiveConfigAndSeparatesConversations(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Storage.WorkspacesRootDir = t.TempDir()

	// The shipped absolute default must be ignored for a temp root: EffectiveWorkspaceDir derives
	// `<workspaces_root_dir>/shared` instead, which is what makes every test work untouched.
	root := cfg.Storage.EffectiveWorkspaceDir()
	if want := filepath.Join(cfg.Storage.WorkspacesRootDir, "shared"); root != want {
		t.Fatalf("effective workspace = %q, want %q", root, want)
	}

	reqA := httptest.NewRequest("POST", "/v1/messages", nil)
	reqA.Header.Set("X-Session-Affinity", "harness-conversation-a")
	dirA, _, err := workspaceDirForRequest(cfg, reqA, "")
	if err != nil {
		t.Fatalf("request A: %v", err)
	}

	reqB := httptest.NewRequest("POST", "/v1/messages", nil)
	reqB.Header.Set("X-Session-Affinity", "harness-conversation-b")
	dirB, _, err := workspaceDirForRequest(cfg, reqB, "")
	if err != nil {
		t.Fatalf("request B: %v", err)
	}

	if dirA == "" || dirB == "" {
		t.Fatalf("both keyed requests need a directory, got %q and %q", dirA, dirB)
	}
	if dirA == dirB {
		t.Fatalf("different conversations must not share a directory (%q)", dirA)
	}
	for _, d := range []string{dirA, dirB} {
		if !strings.HasPrefix(d, filepath.Clean(root)+string(filepath.Separator)) {
			t.Fatalf("dir %q must live under the workspace root %q", d, root)
		}
	}

	plain := httptest.NewRequest("POST", "/v1/messages", nil)
	if dir, _, err := workspaceDirForRequest(cfg, plain, ""); err != nil || dir != "" {
		t.Fatalf("a request without affinity must stay on the worker cwd, got %q / %v", dir, err)
	}
}
