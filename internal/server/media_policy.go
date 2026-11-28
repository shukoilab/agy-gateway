package server

import (
	"errors"
	"net/http"

	"agy-gateway/internal/config"
	"agy-gateway/internal/media"
)

// mediaConfig projects the operator configuration onto the media resolver policy.
//
// @purpose Keeps the config→resolver mapping in one place so both provider handlers agree.
// @rationale Two adapters resolving images with different policies would mean the same URL is
// accepted on one endpoint and refused on the other, which is exactly the kind of drift this
// refactor exists to remove.
// @preconditions cfg may be nil.
// @param cfg Gateway configuration.
// @returns Resolver configuration with the configured policy, or the defensive defaults.
// @throws None.
// @invariants O(len(AllowedMimeTypes)).
func mediaConfig(cfg *config.Config) media.Config {
	if cfg == nil {
		return media.Config{}
	}
	return media.Config{
		AllowRemote:       cfg.MCP.Media.AllowRemote,
		MaxBytes:          cfg.MCP.Media.MaxBytes,
		Timeout:           cfg.MCP.Media.FetchTimeout,
		AllowedMimeTypes:  cfg.MCP.Media.AllowedMimeTypes,
		AllowPrivateHosts: cfg.MCP.Media.AllowPrivateHosts,
	}
}

// mediaErrorStatus maps a media resolution failure onto the provider's 4xx space.
//
// @purpose Distinguishes "the caller sent something we refuse" from "we failed".
// @rationale A forbidden host or an oversized payload is a client error about the image field; an
// upstream network failure is not. Reporting both as 400 would hide a real outage, and reporting
// both as 500 would invite the caller to retry a request that can never succeed.
// @preconditions err is non-nil.
// @param err Error returned by the resolver.
// @returns HTTP status for the provider error envelope.
// @throws None.
// @invariants Pure.
func mediaErrorStatus(err error) int {
	switch {
	case errors.Is(err, media.ErrUnsupportedScheme),
		errors.Is(err, media.ErrForbiddenHost),
		errors.Is(err, media.ErrTooLarge),
		errors.Is(err, media.ErrUnsupportedMediaType),
		errors.Is(err, media.ErrMalformedDataURI):
		return http.StatusBadRequest
	default:
		// A transport failure talking to the image host is the caller's reference failing, not the
		// gateway being broken, but it is also not retryable-as-is.
		return http.StatusBadRequest
	}
}
