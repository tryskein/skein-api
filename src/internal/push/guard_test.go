package push

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// loopbackClient is the push client a test sender gets to reach a loopback
// httptest server: it lets loopback addresses through the dial guard, which
// keeps refusing every other non-public address. The tests of the guard
// itself give the sender the production client (NewGuardedClient).
func loopbackClient() *http.Client {
	return newPushClient(func(a netip.Addr) error {
		if a.Unmap().IsLoopback() {
			return nil
		}
		return checkPublicAddr(a)
	})
}

// relaxForLoopback makes a test sender built with loopbackClient accept any
// endpoint URL, so it reaches the loopback httptest server.
func relaxForLoopback(s *Sender) {
	s.validate = func(string) error { return nil }
}

// The address classes of the deny-list (sk-977.7.8): every one is refused,
// public unicast addresses of both families pass, and IPv6 forms that embed
// an IPv4 address are judged by that IPv4 address.
func TestBlockedAddr(t *testing.T) {
	cases := []struct {
		addr    string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"10.1.2.3", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true}, // cloud metadata
		{"100.64.0.1", true},      // CGNAT
		{"0.1.2.3", true},
		{"0.0.0.0", true},
		{"224.0.0.1", true},
		{"255.255.255.255", true},
		{"240.0.0.1", true},
		{"198.18.0.1", true},
		{"192.0.0.8", true},
		{"::1", true},
		{"::", true},
		{"fc00::1", true},
		{"fe80::1", true},
		{"ff02::1", true},
		{"::ffff:127.0.0.1", true},       // IPv4-mapped loopback
		{"::ffff:169.254.169.254", true}, // IPv4-mapped link-local
		{"64:ff9b::a9fe:a9fe", true},     // NAT64 of 169.254.169.254
		{"64:ff9b::808:808", false},      // NAT64 of 8.8.8.8
		{"64:ff9b:1::808:808", true},     // local-use NAT64: site-defined layout
		{"fe80::1%eth0", true},           // zoned
		{"2a00:1450::1%eth0", true},      // zoned global
		{"100::1", true},                 // discard-only
		{"2001:2::1", true},              // benchmarking (in 2001::/23)
		{"3fff::1", true},                // documentation (RFC 9637)
		{"4000::1", true},                // outside 2000::/3
		{"192.88.99.1", true},
		{"2002:a00:1::1", true},       // 6to4 of 10.0.0.1
		{"2002:808:808::1", false},    // 6to4 of 8.8.8.8
		{"2001:0:4136:e378::1", true}, // Teredo
		{"2001:db8::1", true},
		{"8.8.8.8", false},
		{"142.250.74.106", false},
		{"2a00:1450:4001:80e::200a", false},
	}
	for _, tc := range cases {
		a := netip.MustParseAddr(tc.addr)
		if got := blockedAddr(a); got != tc.blocked {
			t.Errorf("blockedAddr(%s) = %v, want %v", tc.addr, got, tc.blocked)
		}
	}
}

// A push endpoint that is a public DNS name but leads to an internal address
// is refused at connect time: the production client dials 127.0.0.1 for the
// httptest server and the guard refuses it before any byte is sent.
func TestSend_DialGuardRefusesInternalAddress(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	s := NewSender(&fakePruner{}, NewGuardedClient(), realVAPIDPublic, realVAPIDPrivate, "mailto:test@skein.local")
	s.validate = func(string) error { return nil } // the URL check is not under test here
	err := s.Send(context.Background(), testSubscription(srv.URL), []byte(`{}`))
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("Send to a loopback push server: err = %v, want ErrBlockedAddress", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("the internal server was reached %d times, want 0", hits.Load())
	}
}

// A push service that answers with a redirect is not followed: the 3xx is
// the answer (a StatusError), and the redirect target — internal here — is
// never requested, not even when the guard would let the first hop through.
func TestSend_RedirectIsNotFollowed(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var targetHits atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				targetHits.Add(1)
			}))
			defer target.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"/latest/meta-data", status)
			}))
			defer origin.Close()

			s := NewSender(&fakePruner{}, loopbackClient(), realVAPIDPublic, realVAPIDPrivate, "mailto:test@skein.local")
			relaxForLoopback(s)
			err := s.Send(context.Background(), testSubscription(origin.URL), []byte(`{}`))
			var se *StatusError
			if !errors.As(err, &se) || se.StatusCode != status {
				t.Fatalf("Send: err = %v, want a StatusError with %d", err, status)
			}
			if targetHits.Load() != 0 {
				t.Fatalf("the redirect target was requested %d times, want 0", targetHits.Load())
			}
		})
	}
}

// tunnelProxy is a forward proxy for the tests: it records every CONNECT
// target, refuses the ones in refuse, and for the others plays the origin
// itself inside the tunnel — optionally over TLS with originCert — answering
// one request 201 and recording its Host header.
type tunnelProxy struct {
	connects   chan string
	hosts      chan string
	refuse     map[string]bool
	originCert *tls.Certificate
}

func newTunnelProxy() *tunnelProxy {
	return &tunnelProxy{connects: make(chan string, 8), hosts: make(chan string, 8), refuse: map[string]bool{}}
}

func (p *tunnelProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	p.connects <- r.Host
	if p.refuse[r.Host] {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = rw.WriteString("HTTP/1.1 200 Connection established\r\n\r\n")
	_ = rw.Flush()
	var inner net.Conn = conn
	if p.originCert != nil {
		tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{*p.originCert}})
		if err := tc.Handshake(); err != nil {
			return
		}
		inner = tc
	}
	req, err := http.ReadRequest(bufio.NewReader(inner))
	if err != nil {
		return
	}
	p.hosts <- req.Host
	_, _ = io.WriteString(inner, "HTTP/1.1 201 Created\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
}

// selfSignedCert is a certificate for name, and a pool trusting it.
func selfSignedCert(t *testing.T, name string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func staticLookup(m map[string][]string) lookupFunc {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		var out []netip.Addr
		for _, a := range m[host] {
			out = append(out, netip.MustParseAddr(a))
		}
		if out == nil {
			return nil, errors.New("no such host")
		}
		return out, nil
	}
}

// With an outbound proxy, the target is checked and pinned: a target that
// resolves to an internal address (even one of several) never reaches the
// proxy; a public one is tunnelled with CONNECT to its vetted IP — the
// proxy's own name resolution never picks the destination — and the request
// in the tunnel keeps its original Host.
func TestPushClient_ProxyTunnelsToTheVettedAddress(t *testing.T) {
	tp := newTunnelProxy()
	proxy := httptest.NewServer(tp)
	defer proxy.Close()
	c := newPushClientWith(checkPublicAddr, func(*http.Request) (*url.URL, error) {
		return url.Parse(proxy.URL)
	}, staticLookup(map[string][]string{
		"push.example":   {"8.8.8.8"},
		"rebind.example": {"8.8.8.8", "169.254.169.254"},
	}))

	for _, target := range []string{"http://169.254.169.254/latest/meta-data", "http://rebind.example/x"} {
		if _, err := c.Get(target); !errors.Is(err, ErrBlockedAddress) {
			t.Fatalf("%s through the proxy: err = %v, want ErrBlockedAddress", target, err)
		}
	}
	if len(tp.connects) != 0 {
		t.Fatalf("the proxy got %d CONNECTs for internal targets, want 0", len(tp.connects))
	}

	resp, err := c.Get("http://push.example/wpush/v2/abc")
	if err != nil {
		t.Fatalf("public target through the proxy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, want 201", resp.StatusCode)
	}
	if got := <-tp.connects; got != "8.8.8.8:80" {
		t.Fatalf("CONNECT %q, want the vetted 8.8.8.8:80", got)
	}
	if got := <-tp.hosts; got != "push.example" {
		t.Fatalf("Host in the tunnel %q, want push.example", got)
	}
}

// The proxy cannot reach the first vetted address: the next one is tried.
func TestPushClient_ProxyTriesTheNextVettedAddress(t *testing.T) {
	tp := newTunnelProxy()
	tp.refuse["8.8.8.8:80"] = true
	proxy := httptest.NewServer(tp)
	defer proxy.Close()
	c := newPushClientWith(checkPublicAddr, func(*http.Request) (*url.URL, error) {
		return url.Parse(proxy.URL)
	}, staticLookup(map[string][]string{"push.example": {"8.8.8.8", "8.8.4.4"}}))

	resp, err := c.Get("http://push.example/x")
	if err != nil {
		t.Fatalf("through the proxy: %v", err)
	}
	resp.Body.Close()
	first, second := <-tp.connects, <-tp.connects
	if first != "8.8.8.8:80" || second != "8.8.4.4:80" {
		t.Fatalf("CONNECTs %q then %q, want 8.8.8.8:80 then 8.8.4.4:80", first, second)
	}
}

// An https proxy and an https push service: TLS to the proxy is verified
// against the proxy's own name, TLS inside the tunnel against the push
// service's name — two different certificates, each valid only for its own
// hop — and the tunnel goes to the vetted address.
func TestPushClient_HTTPSProxyAndOriginAreVerifiedSeparately(t *testing.T) {
	proxyCert, proxyRoots := selfSignedCert(t, "proxy.test")
	originCert, originRoots := selfSignedCert(t, "push.test")
	tp := newTunnelProxy()
	tp.originCert = &originCert
	proxy := httptest.NewUnstartedServer(tp)
	proxy.TLS = &tls.Config{Certificates: []tls.Certificate{proxyCert}}
	proxy.StartTLS()
	defer proxy.Close()
	_, port, _ := net.SplitHostPort(proxy.Listener.Addr().String())

	c := newPushClientWith(checkPublicAddr, func(*http.Request) (*url.URL, error) {
		return url.Parse("https://proxy.test:" + port)
	}, staticLookup(map[string][]string{"push.test": {"8.8.8.8"}}))
	// The proxy's name resolves to the loopback test server; both trust
	// roots are known to the client.
	roots := originRoots
	roots.AddCert(proxyCert.Leaf)
	_ = proxyRoots
	tr := c.Transport.(*pushTransport)
	tr.direct.TLSClientConfig = &tls.Config{RootCAs: roots}
	tr.direct.DialContext = nil
	proxyDial := &net.Dialer{}
	tr.dialProxy = func(ctx context.Context, addr string) (net.Conn, error) {
		if addr != "proxy.test:"+port {
			return nil, fmt.Errorf("unexpected proxy address %s", addr)
		}
		return proxyDial.DialContext(ctx, "tcp", proxy.Listener.Addr().String())
	}

	resp, err := c.Get("https://push.test/wpush/v2/abc")
	if err != nil {
		t.Fatalf("https through an https proxy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, want 201", resp.StatusCode)
	}
	if got := <-tp.connects; got != "8.8.8.8:443" {
		t.Fatalf("CONNECT %q, want the vetted 8.8.8.8:443", got)
	}
}

// A direct delivery is always guarded — also to the address a proxy lives
// at: whatever the proxy configuration, the direct path never inherits the
// proxy hop's exemption.
func TestPushClient_DirectPathIsAlwaysGuarded(t *testing.T) {
	tp := newTunnelProxy()
	srv := httptest.NewServer(tp)
	defer srv.Close()
	proxyURL, _ := url.Parse(srv.URL)
	c := newPushClientWith(checkPublicAddr, func(r *http.Request) (*url.URL, error) {
		if r.URL.Host == proxyURL.Host {
			return nil, nil // NO_PROXY for the proxy's own address
		}
		return proxyURL, nil
	}, defaultLookup)
	// Through the proxy first, so a proxy hop to that address has happened.
	resp, err := c.Get("http://8.8.8.8/")
	if err != nil {
		t.Fatalf("through the proxy: %v", err)
	}
	resp.Body.Close()
	<-tp.connects
	<-tp.hosts
	// Then directly to the proxy's own address: refused by the guard.
	if _, err := c.Get(srv.URL + "/internal"); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("direct request to the proxy's address: err = %v, want ErrBlockedAddress", err)
	}
	if len(tp.connects) != 0 || len(tp.hosts) != 0 {
		t.Fatalf("the direct request reached the server")
	}
}

// An explicit port outside 1..65535 is refused, not replaced by the default.
func TestPortOf(t *testing.T) {
	for raw, want := range map[string]int{
		"https://push.example/x":       443,
		"http://push.example/x":        80,
		"https://push.example:8443/x":  8443,
		"https://push.example:0/x":     -1,
		"https://push.example:65536/x": -1,
		"https://push.example:99999/x": -1,
	} {
		u, _ := url.Parse(raw)
		got, err := portOf(u)
		if want < 0 {
			if !errors.Is(err, ErrInvalidEndpoint) {
				t.Errorf("portOf(%s) = %d, %v; want ErrInvalidEndpoint", raw, got, err)
			}
			continue
		}
		if err != nil || int(got) != want {
			t.Errorf("portOf(%s) = %d, %v; want %d", raw, got, err, want)
		}
	}
}

// pipeProxyClient is a push client whose proxy is serve, reached over an
// in-memory pipe: inside a synctest bubble the proxy's silence is durable
// blocking, so the bubble's clock shows when the client gives up on it.
func pipeProxyClient(serve func(net.Conn)) *http.Client {
	c := newPushClientWith(checkPublicAddr, func(*http.Request) (*url.URL, error) {
		return url.Parse("http://proxy.test:3128")
	}, staticLookup(map[string][]string{"push.example": {"8.8.8.8"}}))
	c.Transport.(*pushTransport).dialProxy = func(context.Context, string) (net.Conn, error) {
		client, proxy := net.Pipe()
		go serve(proxy)
		return client, nil
	}
	return c
}

// A proxy that answers CONNECT with a head of 64 KiB, four times
// maxConnectResponseBytes, and then holds the connection open gets an error
// at once, not at the delivery deadline, and its connection is closed.
func TestPushClient_ProxyAnswerHeadIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		proxyClosedAfter := make(chan time.Duration, 1)
		c := pipeProxyClient(func(conn net.Conn) {
			defer conn.Close()
			if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
				proxyClosedAfter <- -1
				return
			}
			_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n")
			filler := "X-Filler: " + strings.Repeat("a", 1000) + "\r\n"
			for i := 0; i < (64<<10)/len(filler); i++ {
				if _, err := io.WriteString(conn, filler); err != nil {
					break
				}
			}
			// The head stays unfinished and the connection open: only the
			// client closing it ends this read.
			_, _ = conn.Read(make([]byte, 1))
			proxyClosedAfter <- time.Since(start)
		})

		_, err := c.Get("http://push.example/x")
		if err == nil {
			t.Fatal("a 64 KiB CONNECT answer head was accepted")
		}
		if took := time.Since(start); took >= pushSendTimeout {
			t.Fatalf("the oversized head failed the delivery after %v, want before the %v deadline", took, pushSendTimeout)
		}
		if d := <-proxyClosedAfter; d < 0 || d >= pushSendTimeout {
			t.Fatalf("the proxy connection was closed after %v, want before the %v deadline", d, pushSendTimeout)
		}
	})
}

// A delivery cancelled while a silent proxy has not yet answered CONNECT
// fails with the cancellation, and the proxy connection is closed at once,
// not at the delivery deadline.
func TestPushClient_CancelledDeliveryInterruptsProxyConnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		start := time.Now()
		proxyClosedAfter := make(chan time.Duration, 1)
		c := pipeProxyClient(func(conn net.Conn) {
			defer conn.Close()
			if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
				proxyClosedAfter <- -1
				return
			}
			// The proxy has the CONNECT and stays silent; the delivery is
			// cancelled now.
			cancel()
			_, _ = conn.Read(make([]byte, 1))
			proxyClosedAfter <- time.Since(start)
		})

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://push.example/x", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Do(req); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled delivery: err = %v, want context.Canceled", err)
		}
		if d := <-proxyClosedAfter; d < 0 || d >= pushSendTimeout {
			t.Fatalf("the proxy connection was closed after %v, want before the %v deadline", d, pushSendTimeout)
		}
	})
}

// answeredBeforeCancelConn is a proxy connection whose whole answer to
// CONNECT had arrived when the delivery was cancelled, as on a socket whose
// data came in before the cancellation set its deadline: the first Read
// cancels the delivery, waits until the cancellation has reached the
// connection (a deadline in the past) and still returns the answer.
type answeredBeforeCancelConn struct {
	cancel    context.CancelFunc
	cancelled bool
	expired   chan struct{}
	expireOne sync.Once
	answer    *strings.Reader
	closed    atomic.Bool
}

func (c *answeredBeforeCancelConn) Read(p []byte) (int, error) {
	if !c.cancelled {
		c.cancelled = true
		c.cancel()
		select {
		case <-c.expired:
		case <-time.After(2 * pushSendTimeout):
		}
	}
	return c.answer.Read(p)
}

func (c *answeredBeforeCancelConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *answeredBeforeCancelConn) Close() error                     { c.closed.Store(true); return nil }
func (c *answeredBeforeCancelConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *answeredBeforeCancelConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *answeredBeforeCancelConn) SetReadDeadline(time.Time) error  { return nil }
func (c *answeredBeforeCancelConn) SetWriteDeadline(time.Time) error { return nil }
func (c *answeredBeforeCancelConn) SetDeadline(t time.Time) error {
	if !t.IsZero() && t.Before(time.Now()) {
		c.expireOne.Do(func() { close(c.expired) })
	}
	return nil
}

// A delivery cancelled after the proxy has already answered CONNECT 200
// gets no tunnel: the setup fails with the cancellation and the proxy
// connection is closed.
func TestConnectVia_CancelledAfterProxyAnswerGetsNoTunnel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		conn := &answeredBeforeCancelConn{
			cancel:  cancel,
			expired: make(chan struct{}),
			answer:  strings.NewReader("HTTP/1.1 200 Connection established\r\n\r\n"),
		}
		p, err := url.Parse("http://proxy.test:3128")
		if err != nil {
			t.Fatal(err)
		}
		dial := func(context.Context, string) (net.Conn, error) { return conn, nil }

		tunnel, err := connectVia(ctx, dial, p, nil, netip.MustParseAddrPort("8.8.8.8:443"))
		if tunnel != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("connectVia = %v, %v; want no tunnel and context.Canceled", tunnel, err)
		}
		if !conn.closed.Load() {
			t.Fatal("the proxy connection of the cancelled setup was left open")
		}
	})
}
