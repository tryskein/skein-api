package push

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
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
//     NO_PROXY, as net/http reads them; http and https proxies) resolves the
//     endpoint's host here, refuses it when any address fails guard, and
//     opens the tunnel itself: CONNECT to a vetted IP (the next one when the
//     proxy cannot reach it), so the proxy's own name resolution never picks
//     the destination. TLS to an https proxy is verified against the
//     proxy's name, TLS inside the tunnel against the push service's name.
//     The proxy hop is not guarded: the operator chose the proxy;
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
	// dialProxy opens the TCP connection to a proxy (host:port); nil dials
	// it directly. Tests point it at a loopback server.
	dialProxy func(ctx context.Context, addr string) (net.Conn, error)
}

func (t *pushTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	p, err := t.proxyFor(req)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return t.direct.RoundTrip(req)
	}
	port, err := portOf(req.URL)
	if err != nil {
		return nil, err
	}
	addrs, err := vettedAddrs(req.Context(), req.URL.Hostname(), t.guard, t.lookup)
	if err != nil {
		return nil, err
	}
	// One transport per proxied request. Its only dial opens a tunnel
	// through the proxy to a vetted address (connectVia); the request
	// itself is unchanged, so TLS inside the tunnel verifies the push
	// service's own name and the Host header stays the original. Push
	// deliveries are few, so the lost connection reuse costs nothing that
	// matters.
	via := t.direct.Clone()
	via.Proxy = nil
	via.DisableKeepAlives = true
	base := via.TLSClientConfig
	via.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		var lastErr error
		for _, a := range addrs {
			conn, err := connectVia(ctx, t.dialProxy, p, base, netip.AddrPortFrom(a, port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
	defer via.CloseIdleConnections()
	return via.RoundTrip(req)
}

// connectVia opens a tunnel to target through the forward proxy p: it
// connects to the proxy (over TLS verified against the PROXY's own name for
// an https proxy, with the trust roots of base), sends CONNECT target — the
// vetted IP, never the name — with the proxy's credentials when p carries
// them, and returns the tunnel once the proxy answers 2xx. The proxy hop is
// not checked by the address guard: the operator chose the proxy.
func connectVia(ctx context.Context, dial func(context.Context, string) (net.Conn, error), p *url.URL, base *tls.Config, target netip.AddrPort) (net.Conn, error) {
	if p.Scheme != "http" && p.Scheme != "https" {
		return nil, fmt.Errorf("push proxy scheme %q is not supported (http or https)", p.Scheme)
	}
	if dial == nil {
		d := &net.Dialer{Timeout: pushSendTimeout, KeepAlive: 30 * time.Second}
		dial = func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }
	}
	conn, err := dial(ctx, proxyAddr(p))
	if err != nil {
		return nil, fmt.Errorf("connecting to the push proxy: %w", err)
	}
	if p.Scheme == "https" {
		cfg := &tls.Config{}
		if base != nil {
			cfg = base.Clone()
		}
		cfg.ServerName = p.Hostname()
		cfg.NextProtos = nil
		tc := tls.Client(conn, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("TLS to the push proxy: %w", err)
		}
		conn = tc
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(pushSendTimeout))
	}
	// A cancelled delivery interrupts the CONNECT exchange at once instead of
	// waiting for the deadline.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	authority := target.String()
	connect := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: authority},
		Host:   authority,
		Header: make(http.Header),
	}
	if u := p.User; u != nil {
		pw, _ := u.Password()
		connect.Header.Set("Proxy-Authorization",
			"Basic "+base64.StdEncoding.EncodeToString([]byte(u.Username()+":"+pw)))
	}
	if err := connect.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("sending CONNECT to the push proxy: %w", err)
	}
	// The answer's head is read through a bounded reader: a proxy cannot
	// make it unbounded. Its body is never read — a 2xx answer to CONNECT
	// has none (the tunnel follows), and on a refusal the connection is
	// dropped anyway.
	br := bufio.NewReader(io.LimitReader(conn, maxConnectResponseBytes))
	resp, err := http.ReadResponse(br, connect)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading the push proxy's CONNECT answer: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		conn.Close()
		return nil, fmt.Errorf("push proxy refused CONNECT %s: %s", authority, resp.Status)
	}
	if br.Buffered() > 0 {
		conn.Close()
		return nil, errors.New("push proxy sent data before the tunnel was used")
	}
	if !stop() {
		// The delivery was cancelled while the tunnel was being set up.
		conn.Close()
		return nil, ctx.Err()
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// maxConnectResponseBytes bounds the head of a proxy's answer to CONNECT.
const maxConnectResponseBytes = 16 << 10

// proxyAddr is the host:port of proxy p (the default port of its scheme
// when none is given).
func proxyAddr(p *url.URL) string {
	port := p.Port()
	if port == "" {
		port = "80"
		if p.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(p.Hostname(), port)
}

// vettedAddrs resolves host (or parses it, for an IP literal) and refuses
// it when any of its addresses fails guard; the addresses are tried in
// order.
func vettedAddrs(ctx context.Context, host string, guard addrGuard, lookup lookupFunc) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		if err := guard(a); err != nil {
			return nil, err
		}
		return []netip.Addr{a}, nil
	}
	addrs, err := lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolving push host: %w", err)
	}
	if len(addrs) == 0 {
		return nil, errors.New("resolving push host: no address")
	}
	for _, a := range addrs {
		if err := guard(a); err != nil {
			return nil, err
		}
	}
	return addrs, nil
}

// portOf is the port of u: the explicit one, which must be 1..65535, or the
// default of its scheme when none is given.
func portOf(u *url.URL) (uint16, error) {
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return 0, fmt.Errorf("%w: invalid port %q", ErrInvalidEndpoint, p)
		}
		return uint16(n), nil
	}
	if u.Scheme == "http" {
		return 80, nil
	}
	return 443, nil
}
