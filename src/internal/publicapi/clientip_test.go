package publicapi_test

import (
	"net/http/httptest"
	"net/netip"
	"testing"

	"skein-api/internal/publicapi"
)

// TestClientIP_ResolveAndLimitKey — row 58 (NFR "client IP", SC-024, D-9).
// The client is the direct peer; forwarding headers count only from a
// trusted proxy — X-Forwarded-For right to left skipping the trusted
// addresses, so a forged leftmost element changes nothing; X-Real-IP only
// without X-Forwarded-For. ::ffff:a.b.c.d is the IPv4 address; IPv6 is keyed
// by its /64; two IPv4-mapped addresses stay two keys. Red when the leftmost
// X-Forwarded-For element is taken.
func TestClientIP_ResolveAndLimitKey(t *testing.T) {
	trusted := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("fd00::/64"),
	}
	resolver := publicapi.NewClientIP(trusted)

	for _, tc := range []struct {
		name    string
		peer    string
		xff     []string
		realIP  string
		wantKey string
	}{
		{"direct peer", "203.0.113.7:4321", nil, "", "203.0.113.7"},
		{"untrusted peer: X-Forwarded-For ignored", "203.0.113.7:4321", []string{"198.51.100.1"}, "", "203.0.113.7"},
		{"untrusted peer: X-Real-IP ignored", "203.0.113.7:4321", nil, "198.51.100.1", "203.0.113.7"},
		{"trusted peer: the rightmost untrusted hop", "10.0.0.5:80", []string{"198.51.100.9, 198.51.100.1, 10.1.1.1"}, "", "198.51.100.1"},
		{"trusted peer: a forged leftmost hop does not count", "10.0.0.5:80", []string{"1.1.1.1, 198.51.100.1"}, "", "198.51.100.1"},
		{"trusted peer: repeated headers read as one list", "10.0.0.5:80", []string{"1.1.1.1", "198.51.100.1, 10.2.2.2"}, "", "198.51.100.1"},
		{"trusted peer: every hop trusted, the leftmost", "10.0.0.5:80", []string{"10.3.3.3, 10.2.2.2"}, "", "10.3.3.3"},
		{"trusted peer: X-Real-IP without X-Forwarded-For", "10.0.0.5:80", nil, "198.51.100.4", "198.51.100.4"},
		{"trusted peer: X-Real-IP ignored with X-Forwarded-For", "10.0.0.5:80", []string{"198.51.100.1"}, "198.51.100.4", "198.51.100.1"},
		{"mapped peer is IPv4", "[::ffff:203.0.113.7]:4321", nil, "", "203.0.113.7"},
		{"mapped hop is IPv4", "10.0.0.5:80", []string{"::ffff:198.51.100.1"}, "", "198.51.100.1"},
		{"mapped trusted peer counts as trusted", "[::ffff:10.0.0.5]:80", []string{"198.51.100.1"}, "", "198.51.100.1"},
		{"IPv6 peer keyed by /64", "[2001:db8:1:2:aaaa::1]:443", nil, "", "2001:db8:1:2::/64"},
		{"IPv6 trusted proxy and IPv6 client", "[fd00::1]:443", []string{"2001:db8:9:9::5"}, "", "2001:db8:9:9::/64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/v1/x", nil)
			req.RemoteAddr = tc.peer
			for _, v := range tc.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			if tc.realIP != "" {
				req.Header.Set("X-Real-IP", tc.realIP)
			}
			addr, ok := resolver.Resolve(req)
			if !ok {
				t.Fatalf("no address resolved from peer %q", tc.peer)
			}
			if got := publicapi.LimitKey(addr); got != tc.wantKey {
				t.Errorf("limit key %q, want %q", got, tc.wantKey)
			}
		})
	}

	t.Run("one /64 shares a key, two mapped IPv4 addresses do not", func(t *testing.T) {
		a := publicapi.LimitKey(netip.MustParseAddr("2001:db8:1:2::1"))
		b := publicapi.LimitKey(netip.MustParseAddr("2001:db8:1:2:ffff:ffff:ffff:ffff"))
		if a != b {
			t.Errorf("addresses of one /64 keyed %q and %q, want one key", a, b)
		}
		m1 := publicapi.LimitKey(netip.MustParseAddr("::ffff:1.2.3.4"))
		m2 := publicapi.LimitKey(netip.MustParseAddr("::ffff:5.6.7.8"))
		if m1 == m2 {
			t.Errorf("::ffff:1.2.3.4 and ::ffff:5.6.7.8 share key %q, want two keys", m1)
		}
		if m1 != "1.2.3.4" {
			t.Errorf("::ffff:1.2.3.4 keyed %q, want 1.2.3.4", m1)
		}
	})
}
