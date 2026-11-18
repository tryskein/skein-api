package push

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
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

// blockedPrefixes4 are the IPv4 ranges a push delivery must never reach, on
// top of what netip.Addr already classifies (loopback, private, link-local,
// multicast, unspecified).
var blockedPrefixes4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // shared address space (CGNAT)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation (TEST-NET-1)
	netip.MustParsePrefix("192.88.99.0/24"),  // deprecated 6to4 relay anycast
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation (TEST-NET-2)
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation (TEST-NET-3)
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and 255.255.255.255
}

// IPv6 is allowed only inside the global unicast block 2000::/3, minus the
// special-purpose ranges inside it (blockedPrefixes6). Everything outside it —
// ULA, link-local, multicast, the discard prefix, the local-use NAT64 block
// 64:ff9b:1::/48 (whose layout is site-defined) — is refused, with one
// exception: the well-known NAT64 prefix 64:ff9b::/96 (RFC 6052), whose last
// 32 bits are the IPv4 address it reaches and which is judged by that
// address. 6to4 (2002::/16, RFC 3056) is judged by the IPv4 address in bits
// 16..47.
var (
	globalUnicast6   = netip.MustParsePrefix("2000::/3")
	wellKnownNAT64   = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour        = netip.MustParsePrefix("2002::/16")
	blockedPrefixes6 = []netip.Prefix{
		netip.MustParsePrefix("2001::/23"),     // IETF protocol assignments: Teredo, benchmarking, ORCHID, …
		netip.MustParsePrefix("2001:db8::/32"), // documentation
		netip.MustParsePrefix("3fff::/20"),     // documentation (RFC 9637)
	}
)

// blockedAddr reports whether a push delivery must not reach a. An address
// with an IPv6 zone (fe80::1%eth0) is always refused: a zone names a local
// interface, and netip.Prefix.Contains never matches a zoned address.
func blockedAddr(a netip.Addr) bool {
	if !a.IsValid() || a.Zone() != "" {
		return true
	}
	a = a.Unmap()
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	if a.Is4() {
		for _, p := range blockedPrefixes4 {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	b := a.As16()
	if wellKnownNAT64.Contains(a) {
		return blockedAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	if !globalUnicast6.Contains(a) {
		return true
	}
	if sixToFour.Contains(a) {
		return blockedAddr(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
	}
	for _, p := range blockedPrefixes6 {
		if p.Contains(a) {
			return true
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

// lookupFunc resolves a host name to its addresses
// (net.DefaultResolver.LookupNetIP in production).
type lookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

func defaultLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// newPushClient builds the HTTP client of the push sender. It closes the
// SSRF paths that a check of the endpoint URL alone leaves open:
//
//   - a direct delivery dials through a dialer whose Control checks every
//     address actually dialed against guard (after name resolution, for
//     every address tried), so a name that resolves — or is rebound — to an
//     internal address is refused at connect time;
//   - a delivery through an outbound proxy (HTTPS_PROXY / HTTP_PROXY /
//     NO_PROXY, as net/http reads them) resolves the endpoint's host here,
//     refuses it when any address fails guard, and sends the request to the
//     vetted address itself — for https the proxy is asked to CONNECT to
//     that IP, and TLS still verifies the certificate against the original
//     host name —
//     so the proxy's own name resolution never picks the destination. The
//     proxy hop is not guarded: the operator chose the proxy;
//   - redirects are not followed: the 3xx answer itself is returned and
//     reported like any other non-2xx answer of a push service.
func newPushClient(guard addrGuard) *http.Client {
	return newPushClientWith(guard, http.ProxyFromEnvironment, defaultLookup)
}

// newPushClientWith is newPushClient with the proxy selection and the
// resolver given (http.ProxyFromEnvironment reads the environment once per
// process, so tests pass their own).
func newPushClientWith(guard addrGuard, proxyFor func(*http.Request) (*url.URL, error), lookup lookupFunc) *http.Client {
	guarded := &net.Dialer{
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
	direct := http.DefaultTransport.(*http.Transport).Clone()
	direct.Proxy = nil
	direct.DialContext = guarded.DialContext
	return &http.Client{
		Timeout: pushSendTimeout,
		Transport: &pushTransport{
			guard:    guard,
			proxyFor: proxyFor,
			lookup:   lookup,
			direct:   direct,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// pushTransport picks the direct or the proxied path for each request.
type pushTransport struct {
	guard    addrGuard
	proxyFor func(*http.Request) (*url.URL, error)
	lookup   lookupFunc
	// direct never uses a proxy and dials only through the guard.
	direct *http.Transport
}

func (t *pushTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	p, err := t.proxyFor(req)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return t.direct.RoundTrip(req)
	}
	host := req.URL.Hostname()
	addr, err := vettedAddr(req.Context(), host, t.guard, t.lookup)
	if err != nil {
		return nil, err
	}
	// One transport per proxied request: it dials only the proxy (unguarded,
	// the operator's choice), asks it for the vetted address, and verifies
	// TLS against the original host name. Push deliveries are few, so the
	// lost connection reuse costs nothing that matters.
	via := t.direct.Clone()
	via.Proxy = http.ProxyURL(p)
	via.DialContext = (&net.Dialer{Timeout: pushSendTimeout, KeepAlive: 30 * time.Second}).DialContext
	via.DisableKeepAlives = true
	if via.TLSClientConfig == nil {
		via.TLSClientConfig = &tls.Config{}
	}
	via.TLSClientConfig.ServerName = host
	defer via.CloseIdleConnections()

	pinned := req.Clone(req.Context())
	pinned.URL.Host = netip.AddrPortFrom(addr, portOf(req.URL)).String()
	if req.URL.Scheme == "https" {
		// The proxy gets CONNECT <vetted ip:port>; inside the tunnel the
		// request keeps its original Host and TLS its original name.
		pinned.Host = req.URL.Host
	} else {
		// Plain http through a proxy is addressed by Host in the request
		// line, so the vetted address must be the Host too. Push endpoints
		// are https (ValidateEndpoint); this only keeps the pin intact.
		pinned.Host = ""
	}
	return via.RoundTrip(pinned)
}

// vettedAddr resolves host (or parses it, for an IP literal), refuses it
// when any of its addresses fails guard, and returns the first one.
func vettedAddr(ctx context.Context, host string, guard addrGuard, lookup lookupFunc) (netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return a, guard(a)
	}
	addrs, err := lookup(ctx, host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("resolving push host: %w", err)
	}
	if len(addrs) == 0 {
		return netip.Addr{}, fmt.Errorf("resolving push host: no address")
	}
	for _, a := range addrs {
		if err := guard(a); err != nil {
			return netip.Addr{}, err
		}
	}
	return addrs[0], nil
}

// portOf is the port of u, the default of its scheme when none is given.
func portOf(u *url.URL) uint16 {
	if p := u.Port(); p != "" {
		if n, err := strconv.ParseUint(p, 10, 16); err == nil {
			return uint16(n)
		}
	}
	if u.Scheme == "http" {
		return 80
	}
	return 443
}
