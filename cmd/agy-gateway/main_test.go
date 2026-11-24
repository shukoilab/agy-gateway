package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestCheckHealth_Success(t *testing.T) {
	// Start a local test server listening on a dynamic port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen on dynamic port: %v", err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port

	ts := &httptest.Server{
		Listener: listener,
		Config: &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthz" {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"status":"healthy"}`))
					return
				}
				http.NotFound(w, r)
			}),
		},
	}
	ts.Start()
	defer ts.Close()

	t.Setenv("PORT", strconv.Itoa(port))
	t.Setenv("AGY_GW_PORT", "")

	if err := checkHealth(""); err != nil {
		t.Fatalf("expected checkHealth to succeed for healthy server, got: %v", err)
	}
}

func TestCheckHealth_UnhealthyStatus(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen on dynamic port: %v", err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port

	ts := &httptest.Server{
		Listener: listener,
		Config: &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"status":"degraded"}`))
			}),
		},
	}
	ts.Start()
	defer ts.Close()

	t.Setenv("AGY_GW_PORT", strconv.Itoa(port))

	if err := checkHealth(""); err == nil {
		t.Fatalf("expected checkHealth to fail for 503 Service Unavailable, got nil")
	}
}

func TestCheckHealth_ConnectionRefused(t *testing.T) {
	// Probe an unused port
	t.Setenv("AGY_GW_PORT", "59999")
	t.Setenv("PORT", "59999")

	if err := checkHealth(""); err == nil {
		t.Fatalf("expected checkHealth to fail when port has no listener, got nil")
	}
}

func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"DEBUG", "DEBUG"},
		{"info", "INFO"},
		{"warn", "WARN"},
		{"warning", "WARN"},
		{"ERROR", "ERROR"},
		{"unknown", "INFO"},
	}

	for _, tc := range cases {
		lvl := parseLogLevel(tc.input)
		if lvl.String() != tc.expected {
			t.Errorf("parseLogLevel(%q) = %v; want %v", tc.input, lvl.String(), tc.expected)
		}
	}
}
