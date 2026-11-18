package push

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
)

// relaxForLoopback lets a test sender reach a loopback httptest server: it
// accepts any endpoint URL and lets loopback addresses through the dial
// guard, which keeps refusing every other non-public address. The tests of
// the guard itself use the production sender (NewSender) untouched.
func relaxForLoopback(s *Sender) {
	s.validate = func(string) error { return nil }
	s.httpClient = newPushClient(func(a netip.Addr) error {
		if a.Unmap().IsLoopback() {
			return nil
		}
		return checkPublicAddr(a)
	})
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
		{"2002:a00:1::1", true},          // 6to4 of 10.0.0.1
		{"2002:808:808::1", false},       // 6to4 of 8.8.8.8
		{"2001:0:4136:e378::1", true},    // Teredo
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

	s := NewSender(&fakePruner{}, realVAPIDPublic, realVAPIDPrivate, "mailto:test@skein.local")
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

			s := NewSender(&fakePruner{}, realVAPIDPublic, realVAPIDPrivate, "mailto:test@skein.local")
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

// With an outbound proxy configured, the proxy hop itself is not guarded
// (the operator chose the proxy, often on an internal address), but the
// target host still is: a target that is an internal address never reaches
// the proxy, and a public one goes through it.
func TestPushClient_ProxyKeepsWorkingAndTargetIsGuarded(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer proxy.Close()
	// http.ProxyFromEnvironment reads the environment once per process, so
	// the test hands the proxy selection in directly.
	c := newPushClientWithProxy(checkPublicAddr, func(*http.Request) (*url.URL, error) {
		return url.Parse(proxy.URL)
	})

	// Internal target: refused before the proxy is contacted.
	if _, err := c.Get("http://169.254.169.254/latest/meta-data"); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("internal target through the proxy: err = %v, want ErrBlockedAddress", err)
	}
	if proxied.Load() != 0 {
		t.Fatalf("the proxy got %d requests for an internal target, want 0", proxied.Load())
	}

	// Public target (an IP literal, so no DNS is needed): goes through the
	// proxy although the proxy itself is on loopback.
	resp, err := c.Get("http://8.8.8.8/")
	if err != nil {
		t.Fatalf("public target through the proxy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || proxied.Load() != 1 {
		t.Fatalf("public target: status %d, proxy requests %d; want 201 and 1", resp.StatusCode, proxied.Load())
	}
}

// proxyAddr names the address net/http dials for a proxy, so the proxy hop
// can be recognised.
func TestProxyAddr(t *testing.T) {
	for raw, want := range map[string]string{
		"http://proxy.internal:3128": "proxy.internal:3128",
		"http://proxy.internal":      "proxy.internal:80",
		"https://proxy.internal":     "proxy.internal:443",
		"socks5://proxy.internal":    "proxy.internal:1080",
		"http://[fd00::1]:8080":      net.JoinHostPort("fd00::1", "8080"),
	} {
		u, _ := url.Parse(raw)
		if got := proxyAddr(u); got != want {
			t.Errorf("proxyAddr(%s) = %q, want %q", raw, got, want)
		}
	}
}
