package media

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// tinyPNG is a 1x1 transparent PNG, valid enough for DetectContentType.
const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="

func testResolver() *Resolver {
	return NewResolver(Config{
		AllowRemote:      true,
		MaxBytes:         1024,
		Timeout:          2 * time.Second,
		AllowedMimeTypes: []string{"image/png", "image/jpeg"},
	})
}

// TestResolve_DataURIRoundTrip pins the base64 data-URI path.
//
// @purpose Proves the most common image delivery form (a data URL from a harness) is decoded.
// @rationale Both provider surfaces hand images as data URLs; if this path regressed, every image
// turn would fail silently as an empty prompt block.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No network, no filesystem.
func TestResolve_DataURIRoundTrip(t *testing.T) {
	r := testResolver()
	mediaType, payload, err := r.Resolve(context.Background(), "data:image/png;base64,"+tinyPNG)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if mediaType != "image/png" {
		t.Fatalf("mediaType = %q, want image/png", mediaType)
	}
	if payload != tinyPNG {
		t.Fatalf("payload was re-encoded or truncated (%d bytes)", len(payload))
	}
}

// TestResolve_RejectsUnsupportedScheme pins the SSRF boundary.
//
// @purpose Proves a caller cannot make the gateway read local files or speak other protocols.
// @rationale An image reference is fully attacker-controlled; `file:`, `gopher:` and friends would
// turn the resolver into a local file disclosure primitive.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No network.
func TestResolve_RejectsUnsupportedScheme(t *testing.T) {
	r := testResolver()
	for _, ref := range []string{
		"file:///etc/passwd",
		"gopher://example.com/x",
		"ftp://example.com/x.png",
		"//example.com/x.png",
	} {
		if _, _, err := r.Resolve(context.Background(), ref); !errors.Is(err, ErrUnsupportedScheme) {
			t.Errorf("%q: err = %v, want ErrUnsupportedScheme", ref, err)
		}
	}
}

// TestResolve_RejectsLoopbackAndPrivateHosts pins the internal-network guard.
//
// @purpose Proves the gateway will not be used to probe localhost or private ranges.
// @rationale Without this the resolver is an SSRF proxy for anything reachable from the container,
// including the admin API and the vault.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No outbound network: the guard rejects before dialling.
func TestResolve_RejectsLoopbackAndPrivateHosts(t *testing.T) {
	r := testResolver()
	for _, ref := range []string{
		"http://127.0.0.1/x.png",
		"http://localhost/x.png",
		"http://10.0.0.1/x.png",
		"http://192.168.1.1/x.png",
		"http://169.254.169.254/latest/meta-data",
		"http://[::1]/x.png",
	} {
		_, _, err := r.Resolve(context.Background(), ref)
		if err == nil {
			t.Errorf("%q: expected rejection, got nil", ref)
			continue
		}
		if !errors.Is(err, ErrForbiddenHost) && !errors.Is(err, ErrUnsupportedScheme) {
			t.Errorf("%q: err = %v, want ErrForbiddenHost", ref, err)
		}
	}
}

// TestResolve_AllowPrivateHostsOptIn pins the explicit escape hatch.
//
// @purpose Proves a deployment that wants a local image server can enable it deliberately.
// @rationale The default must be closed, but a self-hosted setup may serve images from a LAN host;
// the knob must actually work or operators will disable the guard entirely.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Uses an httptest server, so only loopback traffic occurs.
func TestResolve_AllowPrivateHostsOptIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A 1x1 PNG body so the content sniff accepts it.
		raw, _ := base64.StdEncoding.DecodeString(tinyPNG)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	r := NewResolver(Config{
		AllowRemote:       true,
		MaxBytes:          4096,
		Timeout:           2 * time.Second,
		AllowedMimeTypes:  []string{"image/png"},
		AllowPrivateHosts: true,
	})
	mediaType, payload, err := r.Resolve(context.Background(), srv.URL+"/x.png")
	if err != nil {
		t.Fatalf("Resolve with AllowPrivateHosts: %v", err)
	}
	if mediaType != "image/png" || payload == "" {
		t.Fatalf("unexpected result %q/%d bytes", mediaType, len(payload))
	}
}

// TestResolve_RejectsOversizedPayload pins the size cap.
//
// @purpose Proves a huge remote body cannot exhaust memory.
// @rationale The body is streamed through a LimitReader; without the cap a single reference could
// allocate unbounded memory in a worker shared by many sessions.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Only reads MaxBytes+1 from the server.
func TestResolve_RejectsOversizedPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		buf := make([]byte, 8192)
		for i := range buf {
			buf[i] = 'A'
		}
		_, _ = w.Write(buf)
	}))
	defer srv.Close()

	r := NewResolver(Config{
		AllowRemote:       true,
		MaxBytes:          1024,
		Timeout:           2 * time.Second,
		AllowedMimeTypes:  []string{"image/png"},
		AllowPrivateHosts: true,
	})
	if _, _, err := r.Resolve(context.Background(), srv.URL+"/big.png"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

// TestResolve_RejectsNonImageContentType pins the media-type allow-list.
//
// @purpose Proves a text or binary document cannot be smuggled in as an image.
// @rationale The sniffed type is authoritative; trusting the caller's claim would let any payload
// reach the agent as an "image" block.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Uses httptest only.
func TestResolve_RejectsNonImageContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("not an image"))
	}))
	defer srv.Close()

	r := NewResolver(Config{
		AllowRemote:       true,
		MaxBytes:          4096,
		Timeout:           2 * time.Second,
		AllowedMimeTypes:  []string{"image/png"},
		AllowPrivateHosts: true,
	})
	_, _, err := r.Resolve(context.Background(), srv.URL+"/x.png")
	if !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("err = %v, want ErrUnsupportedMediaType", err)
	}
}

// TestResolve_RemoteDisabledByDefault pins the closed default.
//
// @purpose Proves the zero-value/absent config cannot fetch anything.
// @rationale A missing `mcp.media` block must not silently enable outbound requests.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No network.
func TestResolve_RemoteDisabledByDefault(t *testing.T) {
	r := NewResolver(Config{})
	if _, _, err := r.Resolve(context.Background(), "http://example.com/x.png"); !errors.Is(err, ErrUnsupportedScheme) {
		t.Fatalf("err = %v, want ErrUnsupportedScheme", err)
	}
}

// TestParseDataURI_RejectsMalformed pins the decoder's strictness.
//
// @purpose Proves a non-base64 or typeless data URI is refused rather than half-decoded.
// @rationale A silently empty payload becomes an empty image block, which the agent answers
// nonsensically — worse than an explicit 400.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestParseDataURI_RejectsMalformed(t *testing.T) {
	for _, ref := range []string{
		"data:image/png,plaintext",
		"data:;base64," + tinyPNG,
		"data:image/png;base64",
		"image/png;base64," + tinyPNG,
	} {
		if _, _, err := ParseDataURI(ref); !errors.Is(err, ErrMalformedDataURI) {
			t.Errorf("%q: err = %v, want ErrMalformedDataURI", ref, err)
		}
	}
}

// TestAccepted_HonoursConfiguredTypes pins the allow-list predicate.
//
// @purpose Proves the resolver's own answer agrees with what Resolve enforces.
// @rationale The translators call Accepted to decide whether to forward a block; a mismatch would
// mean forwarding a type that Resolve then rejects mid-turn.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestAccepted_HonoursConfiguredTypes(t *testing.T) {
	r := testResolver()
	if !r.Accepted("image/png") || !r.Accepted("IMAGE/PNG") {
		t.Fatal("image/png must be accepted, case-insensitively")
	}
	if !r.Accepted("image/jpeg; charset=binary") {
		t.Fatal("parameters must be stripped before matching")
	}
	if r.Accepted("image/svg+xml") {
		t.Fatal("an unconfigured type must be refused")
	}
	// A nil resolver means "no policy object was wired", and Accepted must not then reject a valid
	// inline payload: the translators use it as a guard, not as the authority.
	var nilResolver *Resolver
	if !nilResolver.Accepted("image/png") {
		t.Fatal("a nil resolver must accept, so an unwired policy cannot reject valid input")
	}
}
