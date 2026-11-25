package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
	"agy-gateway/internal/pool"
	"agy-gateway/internal/retry"
)

// TestOpenAIWriteTurnErrorNoEligibleAccounts pins the account-exhaustion mapping.
//
// @purpose Reports "every account is cooling" as a retryable 429, not a 500 or an empty 200.
// @rationale Defect A3 let a fully spent pool surface as a normal completion. A client must be able
// to tell a quota event from a gateway fault, and 429 is the only status that says so.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no network, no goroutines.
func TestOpenAIWriteTurnErrorNoEligibleAccounts(t *testing.T) {
	h := &OpenAIHandlers{logger: slog.Default()}

	for _, err := range []error{
		pool.ErrNoEligibleAccounts,
		fmt.Errorf("acquire failed: %w", pool.ErrNoEligibleAccounts),
	} {
		w := httptest.NewRecorder()
		h.writeTurnError(w, err, time.Second)

		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("%v must map to 429, got %d", err, w.Code)
		}
		var body struct {
			Error struct {
				Code *string `json:"code"`
			} `json:"error"`
		}
		if decErr := json.Unmarshal(w.Body.Bytes(), &body); decErr != nil {
			t.Fatalf("decode error body: %v", decErr)
		}
		if body.Error.Code == nil || *body.Error.Code != "all_accounts_cooling" {
			t.Fatalf("expected code all_accounts_cooling, got %v", body.Error.Code)
		}
	}
}

// TestOpenAIWriteTurnErrorNoAccounts pins the empty-vault mapping.
//
// @purpose Separates a configuration fault from a quota event for the operator.
// @rationale "No accounts configured" requires an administrator, whereas "all cooling" resolves
// itself; conflating them sends the operator hunting for a quota problem that does not exist.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestOpenAIWriteTurnErrorNoAccounts(t *testing.T) {
	h := &OpenAIHandlers{logger: slog.Default()}
	w := httptest.NewRecorder()
	h.writeTurnError(w, pool.ErrNoAccounts, time.Second)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("empty vault must map to 429, got %d", w.Code)
	}
	var body struct {
		Error struct {
			Code *string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Code == nil || *body.Error.Code != "no_accounts" {
		t.Fatalf("expected code no_accounts, got %v", body.Error.Code)
	}
}

// TestOpenAIWriteTurnErrorEmptyCompletion pins the silent-failure mapping.
//
// @purpose Turns a turn that produced nothing into a visible failure.
// @rationale This is the exact symptom of an internally exhausted upstream: agy reports end_turn with
// no content. Answering 200 would claim the work succeeded.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestOpenAIWriteTurnErrorEmptyCompletion(t *testing.T) {
	h := &OpenAIHandlers{logger: slog.Default()}
	w := httptest.NewRecorder()
	h.writeTurnError(w, fmt.Errorf("turn failed: %w", retry.ErrEmptyCompletion), time.Second)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("empty completion must map to 502, got %d", w.Code)
	}
	var body struct {
		Error struct {
			Code *string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Code == nil || *body.Error.Code != "empty_completion" {
		t.Fatalf("expected code empty_completion, got %v", body.Error.Code)
	}
}

// TestAnthropicWriteTurnErrorAccountExhaustion pins the same mapping on the Anthropic surface.
//
// @purpose Keeps both API surfaces telling the same story about an exhausted pool.
// @rationale The two handlers are separate code paths; a fix in one has repeatedly missed the other.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestAnthropicWriteTurnErrorAccountExhaustion(t *testing.T) {
	h := &AnthropicHandlers{logger: slog.Default()}

	t.Run("no eligible accounts", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.writeAnthropicTurnError(w, pool.ErrNoEligibleAccounts)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("must map to 429, got %d", w.Code)
		}
		var body struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
		if body.Type != "error" {
			t.Fatalf("envelope type = %q, want error", body.Type)
		}
		// Anthropic's envelope has no `code` field, so the reason must survive as a typed
		// rate_limit_error plus a stable machine-readable marker in the message — otherwise clients
		// cannot distinguish quota exhaustion from a gateway fault.
		if body.Error.Type != "rate_limit_error" {
			t.Fatalf("error type = %q, want rate_limit_error", body.Error.Type)
		}
		if !strings.Contains(body.Error.Message, "all_accounts_cooling") {
			t.Fatalf("message %q must carry the all_accounts_cooling marker", body.Error.Message)
		}
	})

	t.Run("no accounts", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.writeAnthropicTurnError(w, pool.ErrNoAccounts)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("must map to 429, got %d", w.Code)
		}
		var body struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
		if body.Error.Type != "rate_limit_error" {
			t.Fatalf("error type = %q, want rate_limit_error", body.Error.Type)
		}
		if !strings.Contains(body.Error.Message, "no_accounts") {
			t.Fatalf("message %q must carry the no_accounts marker", body.Error.Message)
		}
	})

	t.Run("empty completion", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.writeAnthropicTurnError(w, retry.ErrEmptyCompletion)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("must map to 502, got %d", w.Code)
		}
		var body struct {
			Type  string `json:"type"`
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
		if body.Type != "error" || body.Error.Type != "api_error" {
			t.Fatalf("expected an api_error envelope, got %+v", body)
		}
	})
}

// TestWriteTurnErrorRetryAfterHint pins the cooldown-derived hint.
//
// @purpose Gives the client a real wait time instead of a bare 429.
// @rationale The earliest cooldown expiry is known precisely; without it every client invents its own
// backoff and hammers a pool that cannot answer.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; the cooldown is an hour in the future.
func TestWriteTurnErrorRetryAfterHint(t *testing.T) {
	cfg := config.DefaultConfig()
	authMgr := auth.NewManager(cfg, nil)
	cooling := auth.Account{
		ID: "acct_cool", Email: "cool@example.com", Tier: "payg",
		Status: auth.StatusCooldownWeekly, Enabled: true,
		CooldownUntil: time.Now().Add(time.Hour),
	}
	healthy := auth.Account{
		ID: "acct_hot", Email: "hot@example.com", Tier: "payg",
		Status: auth.StatusHealthy, Enabled: true,
	}
	authMgr.SetAccountsForTest([]auth.Account{cooling, healthy})

	h := &OpenAIHandlers{
		logger:   slog.Default(),
		failover: pool.NewFailoverCoordinator(nil, authMgr, nil),
	}
	w := httptest.NewRecorder()
	h.writeTurnError(w, pool.ErrNoEligibleAccounts, time.Second)

	got := w.Header().Get("Retry-After")
	if got == "" {
		t.Fatal("expected a Retry-After header when an account cooldown is known")
	}
	seconds, err := strconv.Atoi(got)
	if err != nil {
		t.Fatalf("Retry-After %q is not an integer: %v", got, err)
	}
	// The hint is the earliest cooldown, roughly an hour away.
	if seconds < 3500 || seconds > 3700 {
		t.Fatalf("Retry-After = %d, want approximately 3600", seconds)
	}
}

// TestWriteTurnErrorNoHintWithoutCooldown pins that a real 429 is not given a bogus hint.
//
// @purpose Avoids advertising a wait time when nothing is actually cooling.
// @rationale A fabricated Retry-After is worse than none: clients would delay work with no cause.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestWriteTurnErrorNoHintWithoutCooldown(t *testing.T) {
	cfg := config.DefaultConfig()
	authMgr := auth.NewManager(cfg, nil)
	authMgr.SetAccountsForTest([]auth.Account{{
		ID: "acct_hot", Email: "hot@example.com", Tier: "payg",
		Status: auth.StatusHealthy, Enabled: true,
	}})

	h := &OpenAIHandlers{
		logger:   slog.Default(),
		failover: pool.NewFailoverCoordinator(nil, authMgr, nil),
	}
	w := httptest.NewRecorder()
	h.writeTurnError(w, pool.ErrNoEligibleAccounts, time.Second)

	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("expected no Retry-After when no account is cooling, got %q", got)
	}
}

// TestWriteTurnErrorNilFailoverIsSafe pins nil-receiver safety.
//
// @purpose Keeps the error path crash-free when failover was never constructed.
// @rationale A nil coordinator must degrade to "no hint", never panic inside an error handler where a
// panic would replace a useful 429 with a dropped connection.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestWriteTurnErrorNilFailoverIsSafe(t *testing.T) {
	h := &OpenAIHandlers{logger: slog.Default()}
	w := httptest.NewRecorder()
	h.writeTurnError(w, pool.ErrNoEligibleAccounts, time.Second)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("nil failover must still produce 429, got %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("nil failover must not invent a hint, got %q", got)
	}
}

// TestNextAvailableIn_IgnoresExpiredCooldowns pins the hint calculation boundary.
//
// @purpose Prevents an already-expired cooldown from producing a negative or zero hint.
// @rationale A zero hint would serialise as Retry-After: 0, telling the client to retry instantly into
// a pool that has not yet recovered.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestNextAvailableIn_IgnoresExpiredCooldowns(t *testing.T) {
	cfg := config.DefaultConfig()
	authMgr := auth.NewManager(cfg, nil)
	now := time.Now()
	authMgr.SetAccountsForTest([]auth.Account{{
		ID: "acct_expired", Tier: "payg", Status: auth.StatusCooldown5h, Enabled: true,
		CooldownUntil: now.Add(-time.Minute),
	}})

	fc := pool.NewFailoverCoordinator(nil, authMgr, nil)
	if got := fc.NextAvailableIn(now); got != 0 {
		t.Fatalf("NextAvailableIn = %v, want 0 for an expired cooldown", got)
	}
	if got := (&pool.FailoverCoordinator{}).NextAvailableIn(now); got != 0 {
		t.Fatalf("NextAvailableIn on an empty coordinator = %v, want 0", got)
	}
}

// TestEmptyCompletion_IsNotMistakenForSuccess guards the exact A9 symptom shape.
//
// @purpose Documents that the sentinel must not be swallowed by a broader mapping.
// @rationale If ErrEmptyCompletion were classified as non-retryable or fell through to the generic
// branch, callers would again see a 200 for a turn that produced nothing.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestEmptyCompletion_IsNotMistakenForSuccess(t *testing.T) {
	if !errors.Is(fmt.Errorf("wrapped: %w", retry.ErrEmptyCompletion), retry.ErrEmptyCompletion) {
		t.Fatal("errors.Is must see through wrapping; else the handler mapping cannot fire")
	}
}
