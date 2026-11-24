package version_test

import (
	"strings"
	"testing"

	"agy-gateway/internal/version"
)

func TestVersion_Get(t *testing.T) {
	v := version.Get()
	if v == "" {
		t.Fatal("expected non-empty version string")
	}
	if v != version.Version {
		t.Fatalf("expected version %s, got %s", version.Version, v)
	}
}

func TestVersion_UserAgent(t *testing.T) {
	uaDefault := version.UserAgent("")
	if !strings.HasPrefix(uaDefault, "agy-gateway/") {
		t.Fatalf("expected prefix 'agy-gateway/', got %s", uaDefault)
	}
	if !strings.Contains(uaDefault, version.Version) {
		t.Fatalf("expected user agent to contain version %s, got %s", version.Version, uaDefault)
	}

	uaUpdater := version.UserAgent("updater")
	if !strings.HasPrefix(uaUpdater, "agy-gateway-updater/") {
		t.Fatalf("expected prefix 'agy-gateway-updater/', got %s", uaUpdater)
	}
}
