package push

import (
	"context"
	"errors"
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

// With an outbound proxy, the proxy hop itself is not guarded (the operator
// chose the proxy, often on an internal address), the target is: a target
// that resolves to an internal address never reaches the proxy, and a public
// one is sent to the proxy addressed by its VETTED IP — the proxy's own name
// resolution never picks the destination — with the original host kept in
// the Host header.
func TestPushClient_ProxyIsAskedForTheVettedAddress(t *testing.T) {
	type seen struct{ uri, host string }
	got := make(chan seen, 4)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{uri: r.RequestURI, host: r.Host}
		w.WriteHeader(http.StatusCreated)
	}))
	defer proxy.Close()
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "push.example":
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		case "rebind.example":
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("169.254.169.254")}, nil
		}
		return nil, errors.New("no such host")
	}
	c := newPushClientWith(checkPublicAddr, func(*http.Request) (*url.URL, error) {
		return url.Parse(proxy.URL)
	}, lookup)

	for _, target := range []string{"http://169.254.169.254/latest/meta-data", "http://rebind.example/x"} {
		if _, err := c.Get(target); !errors.Is(err, ErrBlockedAddress) {
			t.Fatalf("%s through the proxy: err = %v, want ErrBlockedAddress", target, err)
		}
	}
	if len(got) != 0 {
		t.Fatalf("the proxy got %d requests for internal targets, want 0", len(got))
	}

	resp, err := c.Get("http://push.example/wpush/v2/abc")
	if err != nil {
		t.Fatalf("public target through the proxy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("public target: status %d, want 201", resp.StatusCode)
	}
	s := <-got
	if s.uri != "http://8.8.8.8:80/wpush/v2/abc" {
		t.Fatalf("the proxy was asked for %q (Host %q); want the vetted http://8.8.8.8:80/wpush/v2/abc", s.uri, s.host)
	}
}

// Over https (the only scheme a push endpoint may have) the proxy is asked to
// CONNECT to the vetted address, not to the name.
func TestPushClient_ProxyConnectsToTheVettedAddress(t *testing.T) {
	connects := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			connects <- r.Host
		}
		w.WriteHeader(http.StatusForbidden) // refuse the tunnel: the target is what is checked
	}))
	defer proxy.Close()
	lookup := func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.4.4")}, nil
	}
	c := newPushClientWith(checkPublicAddr, func(*http.Request) (*url.URL, error) {
		return url.Parse(proxy.URL)
	}, lookup)
	if _, err := c.Get("https://push.example/wpush/v2/abc"); err == nil {
		t.Fatal("the refused tunnel did not fail the request")
	}
	select {
	case target := <-connects:
		if target != "8.8.4.4:443" {
			t.Fatalf("CONNECT %q, want the vetted 8.8.4.4:443", target)
		}
	default:
		t.Fatal("the proxy got no CONNECT")
	}
}

// A direct delivery is always guarded — also to the address a proxy lives
// at: whatever the proxy configuration, the direct path never inherits the
// proxy hop's exemption.
func TestPushClient_DirectPathIsAlwaysGuarded(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	proxyURL, _ := url.Parse(srv.URL)
	c := newPushClientWith(checkPublicAddr, func(r *http.Request) (*url.URL, error) {
		if r.URL.Host == proxyURL.Host {
			return nil, nil // NO_PROXY for the proxy's own address
		}
		return proxyURL, nil
	}, defaultLookup)
	// Through the proxy first, so a proxy hop to that address has happened.
	if resp, err := c.Get("http://8.8.8.8/"); err != nil {
		t.Fatalf("through the proxy: %v", err)
	} else {
		resp.Body.Close()
	}
	before := hits.Load()
	if before != 1 {
		t.Fatalf("the proxy got %d requests, want 1", before)
	}
	// Then directly to the proxy's own address: refused by the guard.
	if _, err := c.Get(srv.URL + "/internal"); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("direct request to the proxy's address: err = %v, want ErrBlockedAddress", err)
	}
	if hits.Load() != before {
		t.Fatalf("the direct request reached the server")
	}
}
