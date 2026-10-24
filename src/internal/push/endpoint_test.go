package push

import (
	"errors"
	"testing"
)

func TestValidateEndpoint(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"https public host", "https://fcm.googleapis.com/fcm/send/abc123", false},
		{"https public host with port", "https://updates.push.services.mozilla.com:443/wpush/v2/xyz", false},
		{"http scheme rejected", "http://fcm.googleapis.com/fcm/send/abc", true},
		{"ws scheme rejected", "ws://fcm.googleapis.com/x", true},
		{"localhost rejected", "https://localhost/push", true},
		{"localhost with port rejected", "https://localhost:8443/push", true},
		{"loopback ipv4 rejected", "https://127.0.0.1/push", true},
		{"loopback ipv6 rejected", "https://[::1]/push", true},
		{"private 10.x rejected", "https://10.0.0.5/push", true},
		{"private 192.168.x rejected", "https://192.168.1.10/push", true},
		{"private 172.16.x rejected", "https://172.16.0.1/push", true},
		{"link-local 169.254.x rejected", "https://169.254.169.254/latest/meta-data", true},
		{"unspecified 0.0.0.0 rejected", "https://0.0.0.0/push", true},
		{"empty host rejected", "https:///push", true},
		{"empty string rejected", "", true},
		{"public ip allowed", "https://8.8.8.8/push", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEndpoint(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidateEndpoint(%q) = nil, want error", tc.raw)
				}
				if !errors.Is(err, ErrInvalidEndpoint) {
					t.Fatalf("ValidateEndpoint(%q) error = %v, want wrapping ErrInvalidEndpoint", tc.raw, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateEndpoint(%q) = %v, want nil", tc.raw, err)
			}
		})
	}
}
