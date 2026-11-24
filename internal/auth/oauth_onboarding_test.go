package auth_test

import (
	"encoding/json"
	"testing"
	"time"

	"agy-gateway/internal/auth"
)

func TestOAuthSession_MarshalJSON(t *testing.T) {
	session := &auth.OAuthSession{
		ID:         "test-login-123",
		Alias:      "test-account",
		AuthURL:    "https://accounts.google.com/o/oauth2/auth?test=1",
		PinnedPort: "43125",
		CreatedAt:  time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC),
	}
	session.Completed.Store(true)
	session.Failed.Store(false)

	data, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("failed to marshal OAuthSession to JSON: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	if parsed["id"] != "test-login-123" {
		t.Errorf("expected id 'test-login-123', got %v", parsed["id"])
	}
	if parsed["completed"] != true {
		t.Errorf("expected completed=true, got %v", parsed["completed"])
	}
	if parsed["failed"] != false {
		t.Errorf("expected failed=false, got %v", parsed["failed"])
	}
}
