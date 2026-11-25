package auth

import "testing"

// TestIsEligibleForModel_ScopesToFamily pins defect A12: an account whose third-party bucket is
// nearly spent must remain eligible for Gemini traffic, and vice versa.
//
// @purpose Stops the eligibility check from disqualifying an account because of a bucket the
// requested model does not draw from.
// @rationale The live failure was an account at 8% on "Claude and GPT models" being dropped for
// pure Gemini work, removing capacity that was actually available. Iterating every bucket is only
// correct when the caller has no model context.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no I/O.
func TestIsEligibleForModel_ScopesToFamily(t *testing.T) {
	acc := &Account{
		ID:      "acct_mixed",
		Enabled: true,
		Status:  StatusHealthy,
		Tier:    "payg",
		Quotas: map[string]QuotaGroup{
			"gemini-weekly": {RemainingPct: 65, Name: "Gemini Models"},
			"gemini-5h":     {RemainingPct: 95, Name: "Gemini Models"},
			"3p-weekly":     {RemainingPct: 3, Name: "Claude and GPT models"},
			"3p-5h":         {RemainingPct: 40, Name: "Claude and GPT models"},
		},
	}

	cases := []struct {
		name  string
		model string
		want  bool
	}{
		// The defect: a depleted third-party bucket must not mask Gemini availability.
		{"gemini model ignores the depleted third-party bucket", "gemini-3.8-flash", true},
		{"gemini variant suffix is still gemini", "gemini-4-hydrogen-xhigh", true},
		{"claude model is limited by its own depleted bucket", "claude-sonnet-4-6", false},
		{"gpt model is limited by its own depleted bucket", "gpt-oss-120b-medium", false},
		{"unknown vendor checks every family and fails on the depleted bucket", "some-vendor-model", false},
		{"empty model keeps the conservative all-family verdict", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := acc.IsEligibleForModel(tc.model, false, 5.0); got != tc.want {
				t.Fatalf("IsEligibleForModel(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

// TestIsEligibleForModel_GatesStatusTierAndFloor covers the non-quota gates.
//
// @purpose Ensures the family scoping change did not weaken the status, tier, or quota-floor checks.
// @rationale Scoping by family narrows which buckets are read; it must never bypass a disabled,
// cooling-down, or tier-ineligible account.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no I/O.
func TestIsEligibleForModel_GatesStatusTierAndFloor(t *testing.T) {
	healthy := func() *Account {
		return &Account{
			ID: "acct_ok", Enabled: true, Status: StatusHealthy, Tier: "payg",
			Quotas: map[string]QuotaGroup{
				"gemini-weekly": {RemainingPct: 65},
				"3p-weekly":     {RemainingPct: 100},
			},
		}
	}

	if !healthy().IsEligibleForModel("gemini-3.8-flash", false, 5.0) {
		t.Fatal("baseline healthy account must be eligible")
	}

	disabled := healthy()
	disabled.Enabled = false
	if disabled.IsEligibleForModel("gemini-3.8-flash", false, 5.0) {
		t.Error("disabled account must be ineligible")
	}

	cooling := healthy()
	cooling.Status = StatusCooldownWeekly
	if cooling.IsEligibleForModel("gemini-3.8-flash", false, 5.0) {
		t.Error("account in cooldown must be ineligible")
	}

	free := healthy()
	free.Tier = "free"
	if free.IsEligibleForModel("gemini-3.8-flash", true, 5.0) {
		t.Error("free account must be ineligible when a paid tier is required")
	}
	if !free.IsEligibleForModel("gemini-3.8-flash", false, 5.0) {
		t.Error("free account must stay eligible when no paid tier is required")
	}

	breached := healthy()
	breached.Quotas = map[string]QuotaGroup{"gemini-weekly": {RemainingPct: 3}}
	if breached.IsEligibleForModel("gemini-3.8-flash", false, 5.0) {
		t.Error("bucket below the configured floor must make the account ineligible")
	}

	exhausted := healthy()
	exhausted.Quotas = map[string]QuotaGroup{"gemini-weekly": {RemainingPct: 99, IsExhausted: true}}
	if exhausted.IsEligibleForModel("gemini-3.8-flash", false, 5.0) {
		t.Error("bucket flagged exhausted must make the account ineligible regardless of percentage")
	}

	// A nil account must not panic.
	var nilAcc *Account
	if nilAcc.IsEligibleForModel("gemini-3.8-flash", false, 5.0) {
		t.Error("nil account must be ineligible")
	}
}

// TestQuotaFamilyClassification pins the model and bucket classifiers the eligibility logic relies on.
//
// @purpose Keeps the family vocabulary that decides rotation correct at its boundary.
// @rationale Eligibility, cooldowns, and rotation all derive behaviour from these two functions; a
// wrong classification here is silently wrong everywhere else.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no I/O.
func TestQuotaFamilyClassification(t *testing.T) {
	for _, tc := range []struct{ model, want string }{
		{"gemini-3.8-flash", QuotaFamilyGemini},
		{"GEMINI-4-hydrogen", QuotaFamilyGemini},
		{"  gemini-4-hydrogen-high  ", QuotaFamilyGemini},
		{"claude-sonnet-4-6", QuotaFamilyThirdParty},
		{"gpt-oss-120b-medium", QuotaFamilyThirdParty},
		{"", ""},
		{"mystery-model", ""},
	} {
		if got := QuotaFamilyForModel(tc.model); got != tc.want {
			t.Errorf("QuotaFamilyForModel(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}

	for _, tc := range []struct{ bucket, want string }{
		{"gemini-weekly", QuotaFamilyGemini},
		{"gemini-5h", QuotaFamilyGemini},
		{"3p-weekly", QuotaFamilyThirdParty},
		{"3p-5h", QuotaFamilyThirdParty},
		{"Claude and GPT models-weekly", ""},
	} {
		if got := QuotaFamilyOfBucket(tc.bucket); got != tc.want {
			t.Errorf("QuotaFamilyOfBucket(%q) = %q, want %q", tc.bucket, got, tc.want)
		}
	}
}
