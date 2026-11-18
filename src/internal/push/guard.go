package push

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"syscall"
	"time"
)

// ErrBlockedAddress is returned (wrapped) when a push delivery would reach an
// address that is not a public unicast address of the internet: the host
// itself, a private or shared network, link-local, multicast, a reserved
// range, or an IPv6 form that embeds such an IPv4 address. Real push
// services (FCM, Mozilla autopush, WNS, Apple) are always public, so a
// subscription that leads anywhere else is an SSRF attempt or a broken row.
var ErrBlockedAddress = errors.New("push target address is not public")

// blockedPrefixes are the IPv4 and IPv6 ranges a push delivery must never
// reach, on top of what netip.Addr already classifies (loopback, private,
// link-local, multicast, unspecified). IPv4-mapped IPv6 addresses are
// unmapped before the check, and NAT64 / 6to4 addresses are checked by the
// IPv4 address they embed (see blockedAddr).
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // shared address space (CGNAT)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation (TEST-NET-1)
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation (TEST-NET-2)
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation (TEST-NET-3)
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and 255.255.255.255
	netip.MustParsePrefix("100::/64"),        // discard-only
	netip.MustParsePrefix("2001::/32"),       // Teredo (embeds an obfuscated IPv4)
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
}

// nat64Prefixes embed an IPv4 address in their last 32 bits (RFC 6052, RFC
// 8215); 6to4 (RFC 3056) embeds it in bits 16..47.
var (
	nat64Prefixes = []netip.Prefix{
		netip.MustParsePrefix("64:ff9b::/96"),
		netip.MustParsePrefix("64:ff9b:1::/48"),
	}
	sixToFour = netip.MustParsePrefix("2002::/16")
)

// blockedAddr reports whether a push delivery must not reach a.
func blockedAddr(a netip.Addr) bool {
	if !a.IsValid() {
		return true
	}
	a = a.Unmap()
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	if a.Is6() {
		b := a.As16()
		for _, p := range nat64Prefixes {
			if p.Contains(a) {
				return blockedAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
			}
		}
		if sixToFour.Contains(a) {
			return blockedAddr(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
		}
	}
	return false
}

// addrGuard decides whether a delivery may reach an address; nil allows it.
// Production uses checkPublicAddr; tests of the send path against a loopback
// httptest server use a guard that also lets loopback through.
type addrGuard func(netip.Addr) error

// checkPublicAddr is the production addrGuard.
func checkPublicAddr(a netip.Addr) error {
	if blockedAddr(a) {
		return fmt.Errorf("%w: %s", ErrBlockedAddress, a.Unmap())
	}
	return nil
}

// newPushClient builds the HTTP client of the push sender. It closes the
// SSRF paths that a check of the endpoint URL alone leaves open:
//
//   - every connection is checked against guard by the IP actually dialed
//     (net.Dialer.Control runs after name resolution, for every address
//     tried), so a DNS name that resolves — or is rebound — to an internal
//     address is refused at connect time;
//   - redirects are not followed: the 3xx answer itself is returned and
//     reported like any other non-2xx answer of a push service;
//   - an outbound proxy from the environment (HTTPS_PROXY / HTTP_PROXY /
//     NO_PROXY, as before this change) keeps working: the connection to the
//     proxy is not checked (the operator chose it), and instead the target
//     host is resolved here and refused when any of its addresses fails the
//     guard. The proxy resolves the name again on its own, so with a proxy a
//     rebinding between the two lookups is not closed — direct delivery has
//     no such gap.
func newPushClient(guard addrGuard) *http.Client {
	return newPushClientWithProxy(guard, http.ProxyFromEnvironment)
}

// newPushClientWithProxy is newPushClient with the proxy selection given
// (http.ProxyFromEnvironment in production; it reads the environment once
// per process, so tests pass their own).
func newPushClientWithProxy(guard addrGuard, proxyFor func(*http.Request) (*url.URL, error)) *http.Client {
	dialer := &net.Dialer{
		Timeout:   pushSendTimeout,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: cannot parse dialed address %q", ErrBlockedAddress, address)
			}
			return guard(ap.Addr())
		},
	}
	direct := &net.Dialer{Timeout: pushSendTimeout, KeepAlive: 30 * time.Second}

	// proxies holds the addresses of the proxies the Proxy function handed
	// out; a dial to one of them is the proxy hop and is not guarded.
	var proxies sync.Map

	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = func(req *http.Request) (*url.URL, error) {
		p, err := proxyFor(req)
		if err != nil || p == nil {
			return p, err
		}
		if err := guardHost(req.Context(), req.URL.Hostname(), guard); err != nil {
			return nil, err
		}
		proxies.Store(proxyAddr(p), struct{}{})
		return p, nil
	}
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if _, ok := proxies.Load(address); ok {
			return direct.DialContext(ctx, network, address)
		}
		return dialer.DialContext(ctx, network, address)
	}
	return &http.Client{
		Timeout:   pushSendTimeout,
		Transport: t,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// guardHost resolves host (or parses it, for an IP literal) and refuses it
// when any of its addresses fails guard.
func guardHost(ctx context.Context, host string, guard addrGuard) error {
	if a, err := netip.ParseAddr(host); err == nil {
		return guard(a)
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolving push host: %w", err)
	}
	for _, a := range addrs {
		if err := guard(a); err != nil {
			return err
		}
	}
	return nil
}

// proxyAddr is the host:port net/http dials for proxy p (the default port of
// its scheme when none is given).
func proxyAddr(p *url.URL) string {
	port := p.Port()
	if port == "" {
		switch p.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(p.Hostname(), port)
}
