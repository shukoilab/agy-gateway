// Package media resolves image references supplied by API consumers into the inline base64 form
// that the ACP image content block requires.
//
// @purpose Owns the single place where a caller-controlled URL becomes bytes.
// @rationale An API consumer can send an arbitrary URL in an image part. Fetching it blindly from
// inside the gateway is a server-side request forgery primitive: the gateway can reach loopback
// services, link-local metadata endpoints and internal networks that the caller cannot. All scheme,
// host, size and content-type policy therefore lives here rather than being re-implemented per
// provider adapter, where one of them would inevitably drift.
package media

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrUnsupportedScheme reports a non-http(s) remote reference.
	ErrUnsupportedScheme = errors.New("unsupported image URL scheme")
	// ErrForbiddenHost reports a host the policy refuses to contact.
	ErrForbiddenHost = errors.New("image URL host is not allowed")
	// ErrTooLarge reports a payload exceeding the configured ceiling.
	ErrTooLarge = errors.New("image payload exceeds the maximum size")
	// ErrUnsupportedMediaType reports a content type outside the allow-list.
	ErrUnsupportedMediaType = errors.New("unsupported image media type")
	// ErrMalformedDataURI reports a syntactically invalid data: URI.
	ErrMalformedDataURI = errors.New("malformed data URI")
)

// Resolver converts an image reference into a media type plus base64 payload.
//
// @purpose Single policy surface for consumer-supplied image references.
// @rationale Keeps the SSRF decision list in one auditable type instead of scattered per adapter.
//
// @invariants Immutable after construction; safe for concurrent use.
type Resolver struct {
	// AllowRemote gates http(s) fetching entirely.
	AllowRemote bool
	// MaxBytes bounds the decoded payload size.
	MaxBytes int64
	// Timeout bounds one fetch including redirects.
	Timeout time.Duration
	// AllowedMimeTypes is the accepted media type set (lower-case, no parameters).
	AllowedMimeTypes map[string]struct{}
	// AllowPrivateHosts permits loopback, private and link-local destinations. Off by default
	// because those are exactly the targets an SSRF attempt wants.
	AllowPrivateHosts bool
}

// Config is the operator-facing form of the resolver policy.
//
// @purpose Mirrors Resolver without coupling the config package to this package's internals.
// @rationale The config layer stays dependency-free; the server builds a Resolver from it.
type Config struct {
	AllowRemote       bool
	MaxBytes          int64
	Timeout           time.Duration
	AllowedMimeTypes  []string
	AllowPrivateHosts bool
}

// NewResolver builds a Resolver from configuration, applying defensive minimums.
//
// @purpose Single construction point so a zero-valued config cannot disable the size ceiling.
// @preconditions cfg is the validated media configuration.
// @param cfg Operator configuration.
// @returns A ready Resolver. A non-positive MaxBytes or Timeout falls back to a safe default.
// @throws None.
// @invariants O(len(AllowedMimeTypes)).
func NewResolver(cfg Config) *Resolver {
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 8 << 20 // 8 MiB
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	allowed := make(map[string]struct{}, len(cfg.AllowedMimeTypes))
	for _, m := range cfg.AllowedMimeTypes {
		m = strings.ToLower(strings.TrimSpace(m))
		if m != "" {
			allowed[m] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		for _, m := range []string{"image/png", "image/jpeg", "image/gif", "image/webp"} {
			allowed[m] = struct{}{}
		}
	}
	return &Resolver{
		AllowRemote:       cfg.AllowRemote,
		MaxBytes:          maxBytes,
		Timeout:           timeout,
		AllowedMimeTypes:  allowed,
		AllowPrivateHosts: cfg.AllowPrivateHosts,
	}
}

// Accepted reports whether a media type is in the allow-list.
//
// @purpose Lets callers validate inline payloads against the same set as fetched ones.
// @param mediaType Media type possibly carrying parameters and odd casing.
// @returns true when the normalised base type is allowed.
// @invariants O(1).
func (r *Resolver) Accepted(mediaType string) bool {
	if r == nil {
		return true
	}
	_, ok := r.AllowedMimeTypes[normalizeMediaType(mediaType)]
	return ok
}

// Resolve turns a reference into a media type and base64 payload.
//
// @purpose Provides one call for both inline data URIs and remote URLs.
// @rationale Inline payloads need the same size and type checks as remote ones, otherwise the
// allow-list is trivially bypassed by sending `data:` instead of `https:`.
//
// @preconditions ref is a `data:` URI or an absolute http(s) URL.
// @param ctx Cancellation root for the fetch.
// @param ref Consumer-supplied image reference.
// @returns Media type (no parameters) and base64 payload without the `data:` prefix.
// @throws ErrMalformedDataURI, ErrUnsupportedScheme, ErrForbiddenHost, ErrTooLarge,
// ErrUnsupportedMediaType, or a wrapped transport error.
// @invariants O(payload size); at most one HTTP request plus redirects.
func (r *Resolver) Resolve(ctx context.Context, ref string) (string, string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", fmt.Errorf("%w: empty image reference", ErrMalformedDataURI)
	}
	if strings.HasPrefix(strings.ToLower(ref), "data:") {
		return r.resolveDataURI(ref)
	}
	return r.fetch(ctx, ref)
}

// resolveDataURI decodes and validates an inline data: URI.
func (r *Resolver) resolveDataURI(ref string) (string, string, error) {
	mediaType, payload, err := ParseDataURI(ref)
	if err != nil {
		return "", "", err
	}
	if !r.Accepted(mediaType) {
		return "", "", fmt.Errorf("%w: %s", ErrUnsupportedMediaType, mediaType)
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", "", fmt.Errorf("%w: invalid base64 payload", ErrMalformedDataURI)
	}
	if int64(len(decoded)) > r.MaxBytes {
		return "", "", fmt.Errorf("%w: %d > %d bytes", ErrTooLarge, len(decoded), r.MaxBytes)
	}
	return mediaType, payload, nil
}

// ParseDataURI splits a data: URI into its media type and base64 payload.
//
// @purpose Isolates the RFC 2397 parsing so both providers share it.
// @preconditions ref begins with "data:" (case-insensitive).
// @param ref The data URI.
// @returns Normalised media type and the raw base64 text (no `data:` prefix).
// @throws ErrMalformedDataURI for a non-base64 or header-less URI.
// @invariants O(len(ref)).
func ParseDataURI(ref string) (string, string, error) {
	if !strings.HasPrefix(strings.ToLower(ref), "data:") {
		return "", "", fmt.Errorf("%w: missing data: prefix", ErrMalformedDataURI)
	}
	rest := ref[len("data:"):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", fmt.Errorf("%w: missing comma", ErrMalformedDataURI)
	}
	header, payload := rest[:comma], rest[comma+1:]
	if !strings.Contains(strings.ToLower(header), ";base64") {
		return "", "", fmt.Errorf("%w: only base64 data URIs are supported", ErrMalformedDataURI)
	}
	mediaType := header
	if idx := strings.IndexByte(mediaType, ';'); idx >= 0 {
		mediaType = mediaType[:idx]
	}
	mediaType = normalizeMediaType(mediaType)
	if mediaType == "" {
		return "", "", fmt.Errorf("%w: missing media type", ErrMalformedDataURI)
	}
	// A data URI may percent-encode the payload; base64 characters are not percent-encoded in
	// practice, so decoding only the escapes keeps the fast path allocation-free.
	if strings.Contains(payload, "%") {
		if unescaped, err := url.PathUnescape(payload); err == nil {
			payload = unescaped
		}
	}
	return mediaType, payload, nil
}

// fetch downloads a remote image under the configured policy.
func (r *Resolver) fetch(ctx context.Context, ref string) (string, string, error) {
	if !r.AllowRemote {
		return "", "", fmt.Errorf("%w: remote image fetching is disabled", ErrUnsupportedScheme)
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrUnsupportedScheme, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", "", fmt.Errorf("%w: %q", ErrUnsupportedScheme, u.Scheme)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("%w: missing host", ErrUnsupportedScheme)
	}
	if err := r.checkHost(u.Hostname()); err != nil {
		return "", "", err
	}

	client := &http.Client{
		Timeout: r.Timeout,
		// Redirects must not be able to downgrade the scheme or reach a forbidden host; the
		// default policy re-checks both here rather than trusting the initial URL only.
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			nextScheme := strings.ToLower(next.URL.Scheme)
			if nextScheme != "http" && nextScheme != "https" {
				return fmt.Errorf("%w: redirect to %q", ErrUnsupportedScheme, next.URL.Scheme)
			}
			return r.checkHost(next.URL.Hostname())
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrUnsupportedScheme, err)
	}
	req.Header.Set("Accept", "image/*")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("image fetch failed: HTTP %d", resp.StatusCode)
	}

	mediaType := normalizeMediaType(resp.Header.Get("Content-Type"))
	// Sniff when the server omits or lies about the type: the bytes decide.
	if mediaType == "" || mediaType == "application/octet-stream" {
		sniff := make([]byte, 512)
		n, readErr := io.ReadFull(resp.Body, sniff)
		if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
			return "", "", readErr
		}
		mediaType = normalizeMediaType(http.DetectContentType(sniff[:n]))
		data, readAllErr := io.ReadAll(io.MultiReader(strings.NewReader(string(sniff[:n])), io.LimitReader(resp.Body, r.MaxBytes+1-int64(n))))
		if readAllErr != nil {
			return "", "", readAllErr
		}
		if int64(len(data)) > r.MaxBytes {
			return "", "", fmt.Errorf("%w: %d > %d bytes", ErrTooLarge, len(data), r.MaxBytes)
		}
		if !r.Accepted(mediaType) {
			return "", "", fmt.Errorf("%w: %s", ErrUnsupportedMediaType, mediaType)
		}
		return mediaType, base64.StdEncoding.EncodeToString(data), nil
	}

	if !r.Accepted(mediaType) {
		return "", "", fmt.Errorf("%w: %s", ErrUnsupportedMediaType, mediaType)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, r.MaxBytes+1))
	if err != nil {
		return "", "", err
	}
	if int64(len(data)) > r.MaxBytes {
		return "", "", fmt.Errorf("%w: %d > %d bytes", ErrTooLarge, len(data), r.MaxBytes)
	}
	if !strings.HasPrefix(http.DetectContentType(data), "image/") {
		return "", "", fmt.Errorf("%w: body is not an image", ErrUnsupportedMediaType)
	}
	return mediaType, base64.StdEncoding.EncodeToString(data), nil
}

// checkHost rejects destinations that an SSRF attempt would aim for.
//
// @purpose Blocks loopback, private, link-local and multicast targets without a DNS rebinding gap.
// @rationale The caller cannot normally reach these, so allowing them converts the gateway into a
// proxy for internal services. Resolution happens here, and the address is what gets judged.
//
// @preconditions host is non-empty.
// @param host Hostname or literal IP from the URL.
// @returns ErrForbiddenHost when the policy refuses the destination.
// @invariants O(1) for literals, one DNS lookup for names.
func (r *Resolver) checkHost(host string) error {
	if r.AllowPrivateHosts {
		return nil
	}
	if host == "" {
		return fmt.Errorf("%w: empty host", ErrForbiddenHost)
	}
	ips, err := resolveHost(host)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if isForbiddenIP(ip) {
			return fmt.Errorf("%w: %s resolves to a private destination", ErrForbiddenHost, host)
		}
	}
	return nil
}

// resolveHost returns the addresses for host without any caching.
func resolveHost(host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrForbiddenHost, err)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%w: %s has no addresses", ErrForbiddenHost, host)
	}
	return ips, nil
}

// isForbiddenIP reports whether an address is loopback, private, link-local or otherwise internal.
func isForbiddenIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsUnspecified()
}

// normalizeMediaType lower-cases a media type and strips its parameters.
//
// @purpose Makes allow-list comparison independent of casing and charset suffixes.
// @param mediaType Raw header or URI value.
// @returns Base media type, or "" when empty.
// @invariants O(len(mediaType)).
func normalizeMediaType(mediaType string) string {
	if idx := strings.IndexByte(mediaType, ';'); idx >= 0 {
		mediaType = mediaType[:idx]
	}
	return strings.ToLower(strings.TrimSpace(mediaType))
}
