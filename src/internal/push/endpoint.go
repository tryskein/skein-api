package push

import (
	"errors"
	"fmt"
	"net/netip"
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
//   - an IP-literal host is rejected when it is not a public unicast address
//     (blockedAddr: loopback, private, shared, link-local, multicast,
//     reserved, and IPv6 forms embedding such an IPv4 address).
//
// This checks the URL as written. A DNS name is not resolved here: the send
// path checks the address actually dialed and follows no redirects
// (newPushClient), which also covers a name that resolves, or is later
// rebound, to an internal address.
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
	if _, err := portOf(u); err != nil {
		return err
	}
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("%w: host must not be localhost", ErrInvalidEndpoint)
	}

	// IP-literal hosts: block anything that targets the local host or a
	// private/internal network. DNS names fall through (they're the normal
	// case for real push services).
	if ip, err := netip.ParseAddr(host); err == nil && blockedAddr(ip) {
		return fmt.Errorf("%w: host must be a public address", ErrInvalidEndpoint)
	}

	return nil
}
