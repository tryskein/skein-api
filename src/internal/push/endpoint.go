package push

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrInvalidEndpoint is returned (wrapped) by ValidateEndpoint when a push
// endpoint URL fails validation. It is the sentinel callers branch on to map
// the failure to a 422 at subscribe-time or a "skip + log" at send-time.
var ErrInvalidEndpoint = errors.New("invalid push endpoint")

// ValidateEndpoint rejects push endpoint URLs that are unsafe to use as an
// outbound HTTP target. The endpoint is user-supplied (the browser hands it
// over verbatim from PushSubscription.endpoint) and, once the sender is wired
// into fan-out, becomes a server-side POST target — so an attacker who can
// register a subscription could otherwise point the API at internal infra
// (SSRF). Real browser push endpoints are ALWAYS public DNS names over https
// (FCM, Mozilla autopush, WNS), so we can safely require that shape and reject
// anything that smells like an internal/loopback target.
//
// Rules:
//   - scheme must be https;
//   - host must be non-empty;
//   - the hostname "localhost" is rejected outright;
//   - an IP-literal host is rejected when it is loopback / private (RFC 1918,
//     ULA) / link-local / unspecified.
//
// NOTE (DNS rebinding): this validates the URL/host as written, NOT the IP a
// public DNS name resolves to at send-time. A hostname that resolves to a
// private address (or flips after validation) is not blocked here. Fully
// closing that gap requires resolving + pinning the address in a custom
// net.Dialer.Control on the send-path HTTP client and re-checking each dialed
// IP — out of scope for this change; can be hardened later.
func ValidateEndpoint(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("%w: not a valid URL", ErrInvalidEndpoint)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%w: scheme must be https", ErrInvalidEndpoint)
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: empty host", ErrInvalidEndpoint)
	}
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("%w: host must not be localhost", ErrInvalidEndpoint)
	}

	// IP-literal hosts: block anything that targets the local host or a
	// private/internal network. DNS names fall through (they're the normal
	// case for real push services).
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("%w: host must not be a loopback/private/link-local address", ErrInvalidEndpoint)
		}
	}

	return nil
}
