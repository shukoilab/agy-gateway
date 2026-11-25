package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestQuotaGroups_KeysByBucketIDAndParsesReset pins the mapping eligibility depends on.
//
// @purpose Guarantees the quota map is keyed by bucket id, because the family discriminator is the
// key prefix.
// @rationale Keying by display name would silently break family scoping: "Claude and GPT models" has
// no `3p` prefix, so every third-party bucket would classify as unknown and become unchecked.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no I/O.
func TestQuotaGroups_KeysByBucketIDAndParsesReset(t *testing.T) {
	groups := []AgyUsageGroup{
		{Name: "Gemini Models", Buckets: []AgyUsageBucket{
			{ID: "gemini-weekly", Window: "weekly", RemainingFraction: 0.65, ResetTime: "2026-11-28T08:57:41Z"},
			{ID: "gemini-5h", Window: "5h", RemainingFraction: 1.0, ResetTime: "2026-11-25T17:09:00Z"},
		}},
		{Name: "Claude and GPT models", Buckets: []AgyUsageBucket{
			{ID: "3p-weekly", Window: "weekly", RemainingFraction: 0.08, ResetTime: "2026-11-27T16:39:00Z"},
		}},
	}

	quotas := QuotaGroups(groups)
	if len(quotas) != 3 {
		t.Fatalf("expected 3 buckets, got %d: %+v", len(quotas), quotas)
	}

	weekly, ok := quotas["gemini-weekly"]
	if !ok {
		t.Fatal("expected key gemini-weekly")
	}
	if weekly.RemainingPct != 65 {
		t.Errorf("gemini-weekly remaining = %v, want 65", weekly.RemainingPct)
	}
	if weekly.ResetTime.UTC().Format("2006-01-02T15:04:05Z") != "2026-11-28T08:57:41Z" {
		t.Errorf("gemini-weekly reset = %v, want 2026-11-28T08:57:41Z", weekly.ResetTime.UTC())
	}

	if got := quotas["3p-weekly"].RemainingPct; got != 8 {
		t.Errorf("3p-weekly remaining = %v, want 8", got)
	}

	// Family classification must survive the round trip through the map keys.
	if QuotaFamilyOfBucket("3p-weekly") != QuotaFamilyThirdParty {
		t.Error("3p-weekly must classify as the third-party family")
	}
}

// TestQuotaGroups_ToleratesMalformedInput verifies no bucket is invented or dropped silently.
//
// @purpose Prevents a malformed reset time from erasing a real quota bucket.
// @rationale Dropping the bucket would make an exhausted account look unconstrained; keeping the
// percentage while leaving the reset zero is the only honest reading.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no I/O.
func TestQuotaGroups_ToleratesMalformedInput(t *testing.T) {
	quotas := QuotaGroups([]AgyUsageGroup{
		{Name: "Gemini Models", Buckets: []AgyUsageBucket{
			{ID: "gemini-weekly", Window: "weekly", RemainingFraction: 0.0, ResetTime: "not-a-timestamp"},
		}},
	})

	bucket, ok := quotas["gemini-weekly"]
	if !ok {
		t.Fatal("a malformed reset time must not discard the bucket")
	}
	if bucket.RemainingPct != 0 {
		t.Errorf("remaining = %v, want 0", bucket.RemainingPct)
	}
	if !bucket.ResetTime.IsZero() {
		t.Errorf("reset = %v, want zero for an unparseable timestamp", bucket.ResetTime)
	}

	if got := QuotaGroups(nil); got == nil {
		t.Error("QuotaGroups must never return a nil map")
	}
}

// TestExhaustedFamily_PrefersWeeklyAndHonoursFamily pins the cooldown-window selection.
//
// @purpose Makes a cooldown last until the limiting bucket actually refreshes.
// @rationale Defect A11 used one hardcoded duration for both windows, so a weekly exhaustion put the
// account to sleep for 30 minutes and then woke it into the same wall.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no I/O.
func TestExhaustedFamily_PrefersWeeklyAndHonoursFamily(t *testing.T) {
	quotas := map[string]QuotaGroup{
		"gemini-weekly": {RemainingPct: 1, ResetTime: mustTime(t, "2026-11-28T08:57:41Z")},
		"gemini-5h":     {RemainingPct: 1, ResetTime: mustTime(t, "2026-11-25T17:09:00Z")},
		"3p-weekly":     {RemainingPct: 80, ResetTime: mustTime(t, "2026-11-27T16:39:00Z")},
		"3p-5h":         {RemainingPct: 90, ResetTime: mustTime(t, "2026-11-25T18:06:00Z")},
	}

	// Both Gemini windows are depleted; weekly recovers later and must win.
	window, reset, exhausted := ExhaustedFamily(quotas, QuotaFamilyGemini, 5.0)
	if !exhausted {
		t.Fatal("expected the gemini family to be exhausted")
	}
	if window != "weekly" {
		t.Errorf("window = %q, want weekly (it recovers later)", window)
	}
	if reset.UTC().Format("2006-01-02T15:04:05Z") != "2026-11-28T08:57:41Z" {
		t.Errorf("reset = %v, want the weekly reset", reset.UTC())
	}

	// The third-party family is healthy, so it must not be reported as exhausted.
	if _, _, ex := ExhaustedFamily(quotas, QuotaFamilyThirdParty, 5.0); ex {
		t.Error("third-party family is above the floor and must not be reported exhausted")
	}

	// An all-family check sees the depleted Gemini buckets.
	if _, _, ex := ExhaustedFamily(quotas, "", 5.0); !ex {
		t.Error("an all-family check must report exhaustion when any bucket is below the floor")
	}
}

// TestExhaustedFamily_FiveHourOnly verifies a 5h-only depletion is labelled correctly.
//
// @purpose Distinguishes a short window from a weekly one so the cooldown can be short.
// @rationale Using CooldownWeekly for a 5h depletion would bench an account for days.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no I/O.
func TestExhaustedFamily_FiveHourOnly(t *testing.T) {
	quotas := map[string]QuotaGroup{
		"gemini-weekly": {RemainingPct: 90},
		"gemini-5h":     {RemainingPct: 0, IsExhausted: true, ResetTime: mustTime(t, "2026-11-25T17:09:00Z")},
	}

	window, _, exhausted := ExhaustedFamily(quotas, QuotaFamilyGemini, 5.0)
	if !exhausted {
		t.Fatal("expected exhaustion on the 5h bucket")
	}
	if window != "5h" {
		t.Fatalf("window = %q, want 5h", window)
	}
}

// mustTime parses an RFC3339 timestamp or fails the test.
//
// @purpose Removes repetitive error handling from table fixtures.
// @rationale Test clarity only; a parse failure is a fixture bug and must stop the test loudly.
// @preconditions value is a valid RFC3339 string.
// @param t Standard Go test handle.
// @param value Timestamp text.
// @returns The parsed time.
// @throws None; calls t.Fatalf on a malformed value.
// @invariants O(1).
func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("bad fixture timestamp %q: %v", value, err)
	}
	return parsed
}

// TestReadSingleCredential_EnforcesExactlyOneFile pins the export contract.
//
// @purpose Refuses a whole-vault export instead of arbitrarily installing one of several accounts.
// @rationale The account selector is what makes per-worker identity trustworthy; if the export ever
// ignored it, silently picking a file would recreate the shared-credential defect.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Uses only a temporary directory.
func TestReadSingleCredential_EnforcesExactlyOneFile(t *testing.T) {
	cred := exportedCredential{
		AuthMethod:   "consumer",
		IDTokenEmail: "user@example.com",
	}
	cred.Token.AccessToken = "access"
	cred.Token.RefreshToken = "refresh"
	raw, err := json.Marshal(cred)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	t.Run("exactly one file is accepted", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "user-acct.json"), raw, 0600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		got, err := readSingleCredential(dir)
		if err != nil {
			t.Fatalf("readSingleCredential: %v", err)
		}
		if got.IDTokenEmail != "user@example.com" {
			t.Errorf("email = %q, want user@example.com", got.IDTokenEmail)
		}
	})

	t.Run("zero files is an error", func(t *testing.T) {
		if _, err := readSingleCredential(t.TempDir()); err == nil {
			t.Fatal("expected an error for an empty staging directory")
		}
	})

	t.Run("multiple files is an error", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"a.json", "b.json"} {
			if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
		}
		if _, err := readSingleCredential(dir); err == nil {
			t.Fatal("expected an error when the export produced more than one file")
		}
	})
}
