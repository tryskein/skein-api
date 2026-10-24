package handler

// instance_config_test.go — sk-674.18, test matrix row 10 (D-7, US-18).
//
// Reference: the contract accounts/contract.go#InstanceConfig (T012) —
// GET /api/v1/internal/instance-config answers only with the service secret
// X-Skein-Internal-Secret (no secret, or user identity headers alone → 401),
// and its fields equal the configuration; an unset optional value is null.
// The handler is wrapped in RequireInternalSecret exactly as buildRouter
// mounts it.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"skein-api/internal/auth"
	"skein-api/internal/config"
)

const instanceConfigSecret = "instance-config-test-secret"

func instanceConfigRoute(cfg *config.Config) http.Handler {
	return RequireInternalSecret(instanceConfigSecret, "")(
		http.HandlerFunc(NewInstanceConfigHandler(cfg).Get))
}

func getInstanceConfig(t *testing.T, h http.Handler, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/internal/instance-config", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestInstanceConfig_OnlyWithTheServiceSecret(t *testing.T) {
	h := instanceConfigRoute(&config.Config{AuthMode: "local"})
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"no headers", nil},
		{"user identity headers only", map[string]string{
			auth.HeaderUserID:   "0199a1b2-0000-7000-8000-000000000001",
			auth.HeaderUserRole: "admin",
		}},
		{"wrong secret", map[string]string{auth.HeaderInternalSecret: "not-the-secret-at-all"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := getInstanceConfig(t, h, tc.headers); rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401: the settings are for skein-web only", rec.Code)
			}
		})
	}
}

func TestInstanceConfig_FieldsEqualTheConfiguration(t *testing.T) {
	minLen := 3
	cases := []struct {
		name string
		cfg  *config.Config
		want map[string]any
	}{
		{
			name: "SaaS-like, every optional value set",
			cfg: &config.Config{
				AuthMode: "local", SignupEnabled: true, EmailEnabled: true,
				TurnstileSiteKey: "site-key", ProjectPrefixMinLength: &minLen,
				TermsURL: "https://skein.example.com/terms", PrivacyURL: "https://skein.example.com/privacy",
				InstanceAdminEnabled: false,
			},
			want: map[string]any{
				"authMode": "local", "signupEnabled": true, "emailEnabled": true,
				"turnstileSiteKey": "site-key", "projectPrefixMinLength": float64(3),
				"termsUrl": "https://skein.example.com/terms", "privacyUrl": "https://skein.example.com/privacy",
				"instanceAdminEnabled": false,
			},
		},
		{
			name: "corporate-like, optional values unset are null",
			cfg:  &config.Config{AuthMode: "keycloak", InstanceAdminEnabled: true},
			want: map[string]any{
				"authMode": "keycloak", "signupEnabled": false, "emailEnabled": false,
				"turnstileSiteKey": nil, "projectPrefixMinLength": nil,
				"termsUrl": nil, "privacyUrl": nil,
				"instanceAdminEnabled": true,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := getInstanceConfig(t, instanceConfigRoute(tc.cfg),
				map[string]string{auth.HeaderInternalSecret: instanceConfigSecret})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			var got map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
				t.Fatalf("decoding body: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Errorf("body has %d keys, want exactly %d: %v", len(got), len(tc.want), got)
			}
			for key, want := range tc.want {
				value, present := got[key]
				if !present {
					t.Errorf("key %q missing — every key is always present", key)
					continue
				}
				if value != want {
					t.Errorf("%s = %#v, want %#v", key, value, want)
				}
			}
		})
	}
}
