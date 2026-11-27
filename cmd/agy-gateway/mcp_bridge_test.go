package main

import (
	"net/http"
	"testing"
)

// TestBridgeShouldExit covers S11: only a rejected token terminates the bridge, so a transient
// gateway error stays retryable and an orphaned bridge cannot survive a dead session.
func TestBridgeShouldExit(t *testing.T) {
	cases := []struct {
		status int
		want   bool
		why    string
	}{
		{http.StatusUnauthorized, true, "token is per-session and never becomes valid again"},
		{http.StatusInternalServerError, false, "may recover after a gateway restart"},
		{http.StatusBadGateway, false, "transient upstream failure"},
		{http.StatusServiceUnavailable, false, "transient"},
		{http.StatusTeapot, false, "an unexpected status must not kill a live session"},
	}
	for _, tc := range cases {
		if got := bridgeShouldExit(tc.status); got != tc.want {
			t.Errorf("bridgeShouldExit(%d) = %v, want %v (%s)", tc.status, got, tc.want, tc.why)
		}
	}
}
