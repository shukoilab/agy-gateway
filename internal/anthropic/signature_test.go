package anthropic

import "testing"

// TestGenerateThinkingSignature_ContentDerived covers S14: the signature must be a function of the
// thinking text, because a constant one (HMAC over "") is identical for every block in every turn and
// client SDKs use that to detect replayed or forged reasoning.
func TestGenerateThinkingSignature_ContentDerived(t *testing.T) {
	a := GenerateThinkingSignature("first thought")
	b := GenerateThinkingSignature("second thought")
	if a == b {
		t.Fatal("different thinking text must produce different signatures")
	}
	if a != GenerateThinkingSignature("first thought") {
		t.Fatal("the same thinking text must produce a stable signature")
	}
	if empty := GenerateThinkingSignature(""); empty == a || empty == b {
		t.Fatal("empty thinking must not collide with real content")
	}
	for _, sig := range []string{a, b, GenerateThinkingSignature("")} {
		if len(sig) < 8 || sig[:4] != "sig_" {
			t.Fatalf("signature must keep the sig_ prefix, got %q", sig)
		}
	}
}
