package config

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestProxyConfig_DefaultState(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Proxy.Enabled {
		t.Error("expected Proxy.Enabled to be false by default")
	}
	if cfg.Proxy.HTTPProxy != "" || cfg.Proxy.HTTPSProxy != "" {
		t.Errorf("expected empty default proxy URLs, got http=%q https=%q", cfg.Proxy.HTTPProxy, cfg.Proxy.HTTPSProxy)
	}
	if !strings.Contains(cfg.Proxy.NoProxy, "localhost") || !strings.Contains(cfg.Proxy.NoProxy, "127.0.0.1") {
		t.Errorf("expected default NoProxy to include loopback addresses, got %q", cfg.Proxy.NoProxy)
	}
}

func TestProxyConfig_EnvironmentOverrides(t *testing.T) {
	os.Setenv("HTTP_PROXY", "http://proxy-user:secret-pass@corp-proxy.local:3128")
	os.Setenv("HTTPS_PROXY", "http://proxy-user:secret-pass@corp-proxy.local:3128")
	os.Setenv("NO_PROXY", "custom.domain.internal")
	defer func() {
		os.Unsetenv("HTTP_PROXY")
		os.Unsetenv("http_proxy")
		os.Unsetenv("HTTPS_PROXY")
		os.Unsetenv("https_proxy")
		os.Unsetenv("NO_PROXY")
		os.Unsetenv("no_proxy")
	}()

	cfg := DefaultConfig()
	applyEnvOverrides(cfg)

	if !cfg.Proxy.Enabled {
		t.Error("expected Proxy.Enabled to be true when proxy environment variables are present")
	}
	if cfg.Proxy.HTTPProxy != "http://proxy-user:secret-pass@corp-proxy.local:3128" {
		t.Errorf("unexpected HTTPProxy: %s", cfg.Proxy.HTTPProxy)
	}
	if cfg.Proxy.HTTPSProxy != "http://proxy-user:secret-pass@corp-proxy.local:3128" {
		t.Errorf("unexpected HTTPSProxy: %s", cfg.Proxy.HTTPSProxy)
	}

	// Validate & check loopback protection
	if err := validate(cfg); err != nil {
		t.Fatalf("validation failed: %v", err)
	}

	if !strings.Contains(cfg.Proxy.NoProxy, "127.0.0.1") || !strings.Contains(cfg.Proxy.NoProxy, "localhost") {
		t.Errorf("expected loopback addresses to be appended to NoProxy, got %q", cfg.Proxy.NoProxy)
	}

	// Test EnvSlice for child subprocesses
	envSlice := cfg.Proxy.EnvSlice()
	if len(envSlice) == 0 {
		t.Fatal("expected non-empty EnvSlice()")
	}
	foundHTTP := false
	foundNodeProxy := false
	for _, env := range envSlice {
		if strings.HasPrefix(env, "HTTP_PROXY=") {
			foundHTTP = true
		}
		if env == "NODE_USE_ENV_PROXY=1" {
			foundNodeProxy = true
		}
	}
	if !foundHTTP || !foundNodeProxy {
		t.Errorf("missing expected environment variables in EnvSlice: %+v", envSlice)
	}
}

func TestProxyConfig_URLValidation(t *testing.T) {
	// 1. Invalid scheme (e.g. ftp://)
	cfg1 := DefaultConfig()
	cfg1.Proxy.Enabled = true
	cfg1.Proxy.HTTPProxy = "ftp://proxy.lan:21"
	if err := validate(cfg1); err == nil {
		t.Error("expected validation error for ftp:// scheme, got nil")
	}

	// 2. Malformed URL
	cfg2 := DefaultConfig()
	cfg2.Proxy.Enabled = true
	cfg2.Proxy.HTTPProxy = "http://bad-proxy[invalid]:port"
	if err := validate(cfg2); err == nil {
		t.Error("expected validation error for malformed URL, got nil")
	}

	// 3. Valid HTTP and HTTPS proxies
	cfg3 := DefaultConfig()
	cfg3.Proxy.Enabled = true
	cfg3.Proxy.HTTPProxy = "http://10.0.0.1:8080"
	cfg3.Proxy.HTTPSProxy = "https://secure-proxy.corp:8443"
	if err := validate(cfg3); err != nil {
		t.Errorf("unexpected validation error for valid URLs: %v", err)
	}
}

func TestProxyConfig_LiveMockProxyInterception(t *testing.T) {
	// Spin up a mock HTTP proxy server using httptest
	proxyHitCount := 0
	mockProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHitCount++
		w.Header().Set("X-Via-Mock-Proxy", "true")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"proxied":true}`))
	}))
	defer mockProxy.Close()

	// Configure proxy transport with mock proxy
	proxyURL, _ := url.Parse(mockProxy.URL)
	tr := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
	}
	client := &http.Client{Transport: tr}

	// Make request through proxy
	req, err := http.NewRequest(http.MethodGet, "http://external-api.example.com/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request through mock proxy failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Via-Mock-Proxy") != "true" {
		t.Errorf("expected header X-Via-Mock-Proxy: true, got %q", resp.Header.Get("X-Via-Mock-Proxy"))
	}
	if proxyHitCount != 1 {
		t.Errorf("expected proxyHitCount=1, got %d", proxyHitCount)
	}
}
