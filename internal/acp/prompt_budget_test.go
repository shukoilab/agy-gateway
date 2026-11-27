package acp

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEstimatePromptTokens(t *testing.T) {
	if got := EstimatePromptTokens(""); got != 0 {
		t.Fatalf("empty prompt estimate = %d, want 0", got)
	}
	if got := EstimatePromptTokens(strings.Repeat("abcd", 100)); got != 100 {
		t.Fatalf("400-char prompt estimate = %d, want 100", got)
	}
}

func TestBoundPrompt_WithinBudgetIsUnchanged(t *testing.T) {
	prompt := "User: hi\n\nAssistant: hello\n\n"
	if got := BoundPrompt(prompt, 1000); got != prompt {
		t.Fatalf("prompt within budget was modified: %q", got)
	}
}

func TestBoundPrompt_DisabledLeavesPromptIntact(t *testing.T) {
	prompt := strings.Repeat("x", 10_000)
	if got := BoundPrompt(prompt, 0); got != prompt {
		t.Fatal("budget 0 must disable the bound")
	}
	if got := BoundPrompt(prompt, -1); got != prompt {
		t.Fatal("negative budget must disable the bound")
	}
}

func TestBoundPrompt_KeepsNewestTurnsAndMarksOmission(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("User: OBJECTIVE deliver the patch\n\n")
	for i := 0; i < 500; i++ {
		sb.WriteString("User: old turn ")
		sb.WriteString(strings.Repeat("z", 100))
		sb.WriteString("\n\n")
	}
	sb.WriteString("User: FINAL QUESTION\n\n")
	prompt := sb.String()

	const budget = 2000 // ≈8000 chars
	got := BoundPrompt(prompt, budget)

	if !strings.Contains(got, historyOmittedMarker) {
		t.Fatal("truncated prompt must contain the omission marker")
	}
	if !strings.Contains(got, "User: OBJECTIVE deliver the patch") {
		t.Fatal("truncation dropped the opening objective")
	}
	if !strings.Contains(got, "User: FINAL QUESTION") {
		t.Fatal("truncation dropped the newest turn")
	}
	if EstimatePromptTokens(got) > budget+EstimatePromptTokens(historyOmittedMarker)+1 {
		t.Fatalf("bounded prompt exceeds budget: %d tokens", EstimatePromptTokens(got))
	}
	if len(got) >= len(prompt) {
		t.Fatalf("prompt was not actually truncated: %d >= %d", len(got), len(prompt))
	}
}

func TestBoundPrompt_TruncationStartsOnTurnBoundary(t *testing.T) {
	prompt := "User: a\n\nAssistant: b\n\n" + strings.Repeat("User: padding\n\n", 200)
	got := BoundPrompt(prompt, 50)
	body := strings.TrimPrefix(got, historyOmittedMarker+"\n\n")
	if !strings.HasPrefix(body, "User:") && !strings.HasPrefix(body, "Assistant:") {
		t.Fatalf("truncated body does not start on a turn boundary: %q", body[:40])
	}
}

func TestToolResultCharsCap(t *testing.T) {
	if got := ToolResultCharsCap(0); got != 0 {
		t.Fatalf("disabled budget must disable compaction, got %d", got)
	}
	if got := ToolResultCharsCap(60000); got != 15000 {
		t.Fatalf("cap = %d, want 15000", got)
	}
}

func TestCompactToolOutput(t *testing.T) {
	small := "short result"
	if got := CompactToolOutput(small, 15000); got != small {
		t.Fatal("small result must be untouched")
	}
	if got := CompactToolOutput(small, 0); got != small {
		t.Fatal("cap 0 must disable compaction")
	}

	big := strings.Repeat("A", 5000) + "MIDDLE" + strings.Repeat("Z", 5000)
	got := CompactToolOutput(big, 1000)

	if len(got) > 1000+40 {
		t.Fatalf("compacted result too long: %d", len(got))
	}
	if !strings.Contains(got, "chars omitted]") {
		t.Fatalf("compaction marker missing: %q", got[:120])
	}
	if !strings.HasPrefix(got, "AAA") {
		t.Fatal("compaction must keep the head")
	}
	if !strings.HasSuffix(got, "ZZZ") {
		t.Fatal("compaction must keep the tail")
	}

	// Multi-byte runes must survive compaction without producing invalid UTF-8.
	multi := strings.Repeat("日本語テキスト", 500)
	gotMulti := CompactToolOutput(multi, 101)
	if !utf8.ValidString(gotMulti) {
		t.Fatal("compaction must not split multi-byte runes")
	}
}
