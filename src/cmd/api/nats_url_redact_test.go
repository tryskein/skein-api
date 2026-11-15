package main

import "testing"

// TestRedactNATSURL asserts that NATS credentials in NATS_URL — a
// user:password pair or a bare token in the user part, in any entry of the
// comma-separated server list, with or without a scheme — never reach a log
// line, while the endpoint itself stays readable.
func TestRedactNATSURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"nats://nats:4222", "nats://nats:4222"},
		{"nats://skein:s3cret@nats:4222", "nats://nats:4222"},
		{"nats://t0ken@nats:4222", "nats://nats:4222"},
		{"skein:s3cret@nats:4222", "nats://nats:4222"},
		{"nats://a:p1@n1:4222, tls://b:p2@n2:4222", "nats://n1:4222,tls://n2:4222"},
		{"nats://u:p@[::1:4222", "<invalid url>"},
	}
	for _, tc := range cases {
		if got := redactNATSURL(tc.in); got != tc.want {
			t.Errorf("redactNATSURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
