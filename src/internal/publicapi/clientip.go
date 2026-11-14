package publicapi

// clientip.go — the client address of a request on the public port and the
// key the failed-authentication limiter counts it under (spec NFR "client
// IP"; plan D-9).

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP resolves the client address of a request.
//
// The client is the direct peer. Forwarding headers count only when the peer
// is one of the trusted proxies (SKEIN_PUBLIC_API_TRUSTED_PROXIES):
//
//   - X-Forwarded-For is read right to left, skipping trusted addresses; the
//     first untrusted one is the client. Elements left of it are what the
//     client itself sent and are never believed. When every element is
//     trusted, the leftmost is the client. An element that is not an address
//     stops the walk: the client is then the last address the walk reached.
//   - X-Real-IP counts only when there is no X-Forwarded-For.
//
// Addresses are normalised with Unmap: an IPv4-mapped IPv6 address
// (::ffff:a.b.c.d) is the IPv4 address.
type ClientIP struct {
	trusted []netip.Prefix
}

// NewClientIP builds a resolver trusting the given proxy prefixes; none means
// forwarding headers are always ignored.
func NewClientIP(trusted []netip.Prefix) ClientIP {
	return ClientIP{trusted: trusted}
}

// Resolve returns the client address of r. ok is false when not even the
// direct peer is an address (a request not received over TCP).
func (c ClientIP) Resolve(r *http.Request) (addr netip.Addr, ok bool) {
	peer, ok := parseHostAddr(r.RemoteAddr)
	if !ok {
		return netip.Addr{}, false
	}
	if !c.isTrusted(peer) {
		return peer, true
	}
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		return c.walkForwardedFor(peer, xff), true
	}
	if real, ok := parseHostAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ok {
		return real, true
	}
	return peer, true
}

// walkForwardedFor walks the X-Forwarded-For chain right to left from the
// trusted peer.
func (c ClientIP) walkForwardedFor(peer netip.Addr, headers []string) netip.Addr {
	var hops []string
	for _, h := range headers {
		hops = append(hops, strings.Split(h, ",")...)
	}
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		hop, ok := parseHostAddr(strings.TrimSpace(hops[i]))
		if !ok {
			return client
		}
		client = hop
		if !c.isTrusted(hop) {
			return hop
		}
	}
	return client
}

func (c ClientIP) isTrusted(a netip.Addr) bool {
	for _, p := range c.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// parseHostAddr parses an address with or without a port ("1.2.3.4",
// "1.2.3.4:80", "[::1]:80", "::1") and unmaps it.
func parseHostAddr(s string) (netip.Addr, bool) {
	if s == "" {
		return netip.Addr{}, false
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap(), true
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	a, err := netip.ParseAddr(strings.Trim(s, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.WithZone("").Unmap(), true
}

// LimitKey is the key the failed-authentication limiter counts addr under:
// an IPv4 address whole, an IPv6 address by its /64 — the smallest block one
// subscriber gets, so a client cannot escape the limit by changing its
// address inside it. addr must be unmapped (Resolve returns it so); a
// mapped address keyed as IPv6 would put every IPv4 client in one /64.
func LimitKey(addr netip.Addr) string {
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String()
	}
	p, err := addr.Prefix(64)
	if err != nil {
		return addr.String()
	}
	return p.String()
}
