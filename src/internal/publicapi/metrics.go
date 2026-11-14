package publicapi

// metrics.go — the metrics of the public entry's request chain (spec
// "Limits: Observability"). Requests by route and entry are counted by
// handler.PublicHTTPMetrics (http_requests_total{entry="public"}).

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
)

// The reasons of AuthFailuresTotal beyond the rejections of
// auth.AuthenticateAPIKey.
const (
	// reasonInternalError — the key could not be checked (a failed lookup
	// or status read); the client got 500 or 503, not 401.
	reasonInternalError = "internal_error"
	// reasonFailureLimit — a cold key was refused 429 because its client
	// address is over the failed-authentication limit; it was not checked.
	reasonFailureLimit = "failure_limit"
)

// The limits of RateLimitedTotal.
const (
	limitRequests     = "requests"
	limitAuthFailures = "auth_failures"
)

var (
	// AuthFailuresTotal counts requests of the public entry that did not
	// authenticate, by reason.
	//
	// @metric description Requests to the public REST API that did not authenticate, one increment per request, by reason. The credential reasons are the closed set of the key authentication: the key is missing or not of the key format, no key is stored under its prefix, the secret does not match, the key is revoked or expired, its owner is inactive; each of them answered the client 401 and counted toward the failed-authentication limit of its address. internal_error is a key that could not be checked because a lookup or status read failed (the client got 500, or 503 when the database could not be reached); it is not the client's failure and does not count toward the limit. failure_limit is a cold key refused 429 without being checked because its address is over that limit. These are ordinary answers of a public API and not an alert signal by themselves; a surge of one credential reason from many addresses is the signal of key guessing.
	// @metric calculation Incremented in publicapi.Chain.Wrap: with the rejection reason when auth.AuthenticateAPIKey answers rejected, with internal_error when it answers an internal error, with failure_limit when the address is over the failed-authentication limit and the presented key is not warm. Every series is created at zero when the package loads.
	// @label reason Why the request did not authenticate. Values: "malformed", "unknown", "mismatch", "revoked", "expired", "owner_inactive", "internal_error", "failure_limit".
	AuthFailuresTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_public_api_auth_failures_total",
			Help: "Public API requests that did not authenticate, by reason",
		},
		[]string{"reason"},
	)

	// RateLimitedTotal counts requests of the public entry refused 429 by a
	// limit, by limit.
	//
	// @metric description Requests to the public REST API refused with 429 by one of its two limits, one increment per refused request. requests is the per-key limit (SKEIN_PUBLIC_API_RATE_LIMIT_PER_MINUTE requests per minute per key, per replica); auth_failures is the failed-authentication limit of a client address (SKEIN_PUBLIC_API_AUTH_FAILURES_PER_MINUTE failures per minute per IPv4 address or IPv6 /64, per replica), which refuses cold keys from that address. A refused request ran no operation. 429 is an ordinary answer of a public API, not an alert by itself; a steady requests rate means clients run at the limit, a steady auth_failures rate means an address keeps failing to authenticate.
	// @metric calculation Incremented in publicapi.Chain.Wrap when it answers 429: limit="auth_failures" before authentication when the address is over its failure limit and the key is not warm, limit="requests" after authentication when the key used up its window. Both series are created at zero when the package loads.
	// @label limit Which limit refused the request. Values: "requests", "auth_failures".
	RateLimitedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_public_api_rate_limited_total",
			Help: "Public API requests refused 429 by a limit, by limit",
		},
		[]string{"limit"},
	)

	// ScopeRefusalsTotal counts requests of the public entry refused
	// because the key lacks the right of the operation.
	//
	// @metric description Authenticated requests to the public REST API refused with 403 insufficient_scope because the API key does not hold the right the operation requires in the request's mode, one increment per refused request, by the required right. The right is checked before any object is looked up, so a refused request read and wrote nothing. An ordinary answer of a public API, not an alert by itself; a steady rate for one right means clients call operations their keys were not issued for.
	// @metric calculation Incremented in publicapi.Chain.Wrap when authz.KeyMeets refuses the requirement of the route in the request's mode (force=true on a guarded operation requires manage), right before the 403 is written. Every series is created at zero when the package loads.
	// @label required_right The right the operation required. Values: "read", "write", "manage", "admin".
	ScopeRefusalsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "skein_public_api_scope_refusals_total",
			Help: "Public API requests refused 403 insufficient_scope, by required right",
		},
		[]string{"required_right"},
	)
)

func init() {
	for _, r := range []string{
		string(auth.RejectMalformed), string(auth.RejectUnknown), string(auth.RejectMismatch),
		string(auth.RejectRevoked), string(auth.RejectExpired), string(auth.RejectOwnerInactive),
		reasonInternalError, reasonFailureLimit,
	} {
		AuthFailuresTotal.WithLabelValues(r)
	}
	for _, l := range []string{limitRequests, limitAuthFailures} {
		RateLimitedTotal.WithLabelValues(l)
	}
	for _, r := range []authz.KeyRight{authz.KeyRightRead, authz.KeyRightWrite, authz.KeyRightManage, authz.KeyRightAdmin} {
		ScopeRefusalsTotal.WithLabelValues(r.String())
	}
}
