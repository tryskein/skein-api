package config

import (
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"skein-api/internal/oauthmeta"
)

// withBaseEnv sets the always-required vars (DATABASE_URL, SKEIN_INTERNAL_SECRET)
// so Load() reaches the validation under test, then applies the per-case
// overrides. t.Setenv restores the prior environment after the test.
func withBaseEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	base := map[string]string{
		"DATABASE_URL":          "postgres://u:p@localhost:5432/db?sslmode=disable",
		"SKEIN_INTERNAL_SECRET": "aaaaaaaaaaaaaaaaaaaa",
		// sk-142.7 — required (no default); set a valid value so cases that
		// expect Load() to succeed reach the validation under test.
		"SKEIN_AGENT_PUBLIC_URL": "https://mcp.example.com",
		// sk-304 — the https-in-production rules key off ENVIRONMENT (which
		// defaults to "production"). Pin development here so cases that are
		// not about that rule can keep using plain-http URLs; the cases that
		// DO exercise it set ENVIRONMENT explicitly.
		"ENVIRONMENT": "development",
		// sk-304 — OPTIONAL (unset = discovery off, review F1). Set a valid
		// value here so cases that are not about the optionality exercise the
		// enabled path; TestLoad_OAuthDiscoveryOptional clears it.
		"SKEIN_OAUTH_AUTHORIZATION_SERVERS": "https://auth.example.com/realms/main",
		// Clear push vars by default; cases set what they need.
		"SKEIN_VAPID_PUBLIC":  "",
		"SKEIN_VAPID_PRIVATE": "",
		"SKEIN_VAPID_SUBJECT": "mailto:admin@skein.local",
	}
	for k, v := range base {
		t.Setenv(k, v)
	}
	for k, v := range overrides {
		t.Setenv(k, v)
	}
}

func TestLoad_VAPIDAllOrNothing(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string // substring; empty = expect success
	}{
		{
			name: "both empty is fine (push disabled)",
			env:  map[string]string{},
		},
		{
			name: "both set with mailto subject ok",
			env: map[string]string{
				"SKEIN_VAPID_PUBLIC":  "pub",
				"SKEIN_VAPID_PRIVATE": "priv",
				"SKEIN_VAPID_SUBJECT": "mailto:ops@skein.local",
			},
		},
		{
			name: "both set with https subject ok",
			env: map[string]string{
				"SKEIN_VAPID_PUBLIC":  "pub",
				"SKEIN_VAPID_PRIVATE": "priv",
				"SKEIN_VAPID_SUBJECT": "https://skein.local/contact",
			},
		},
		{
			name: "public only rejected",
			env: map[string]string{
				"SKEIN_VAPID_PUBLIC": "pub",
			},
			wantErr: "both be set or both empty",
		},
		{
			name: "private only rejected",
			env: map[string]string{
				"SKEIN_VAPID_PRIVATE": "priv",
			},
			wantErr: "both be set or both empty",
		},
		{
			name: "bad subject rejected when enabled",
			env: map[string]string{
				"SKEIN_VAPID_PUBLIC":  "pub",
				"SKEIN_VAPID_PRIVATE": "priv",
				"SKEIN_VAPID_SUBJECT": "ops@skein.local",
			},
			wantErr: "must start with mailto: or https:",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withBaseEnv(t, tc.env)
			_, err := Load()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load() error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestLoad_MonitorCredentialDurations covers the sk-142.1 cleanup knobs:
// the interval must be strictly positive (a zero/negative interval would
// silently never sweep) and the revoked-retention must be non-negative
// (0 is valid — delete revoked rows on the next sweep). Defaults (10m / 24h)
// from withBaseEnv reaching Load() unchanged must be accepted.
func TestLoad_MonitorCredentialDurations(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string // substring; empty = expect success
	}{
		{
			name: "defaults accepted",
			env:  map[string]string{},
		},
		{
			name: "explicit positive interval + zero retention ok",
			env: map[string]string{
				"SKEIN_MONITOR_CREDENTIAL_CLEANUP_INTERVAL":  "5m",
				"SKEIN_MONITOR_CREDENTIAL_REVOKED_RETENTION": "0s",
			},
		},
		{
			name: "zero interval rejected",
			env: map[string]string{
				"SKEIN_MONITOR_CREDENTIAL_CLEANUP_INTERVAL": "0s",
			},
			wantErr: "skein_monitor_credential_cleanup_interval must be > 0",
		},
		{
			name: "negative interval rejected",
			env: map[string]string{
				"SKEIN_MONITOR_CREDENTIAL_CLEANUP_INTERVAL": "-5s",
			},
			wantErr: "skein_monitor_credential_cleanup_interval must be > 0",
		},
		{
			name: "negative retention rejected",
			env: map[string]string{
				"SKEIN_MONITOR_CREDENTIAL_REVOKED_RETENTION": "-1s",
			},
			wantErr: "skein_monitor_credential_revoked_retention must be >= 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withBaseEnv(t, tc.env)
			_, err := Load()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load() error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestLoad_MonitorRunnerCadence covers the sk-142.6 runner poll-interval knob
// templated into the served Python runner: it must be strictly positive (a
// zero/negative value would template a busy-loop into the runner). The 2s
// default reaching Load() unchanged must be accepted, and non-default values
// must round-trip onto the Config.
func TestLoad_MonitorRunnerCadence(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string // substring; empty = expect success
	}{
		{
			name: "defaults accepted",
			env:  map[string]string{},
		},
		{
			name: "explicit positive cadence ok",
			env: map[string]string{
				"SKEIN_MONITOR_RUNNER_POLL_INTERVAL": "3s",
			},
		},
		{
			name:    "zero poll interval rejected",
			env:     map[string]string{"SKEIN_MONITOR_RUNNER_POLL_INTERVAL": "0s"},
			wantErr: "skein_monitor_runner_poll_interval must be > 0",
		},
		{
			name:    "negative poll interval rejected",
			env:     map[string]string{"SKEIN_MONITOR_RUNNER_POLL_INTERVAL": "-2s"},
			wantErr: "skein_monitor_runner_poll_interval must be > 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withBaseEnv(t, tc.env)
			cfg, err := Load()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load() error = %q, want substring %q", err.Error(), tc.wantErr)
			}
			_ = cfg
		})
	}
}

// TestLoad_MonitorRunnerCadenceDefaults pins the documented defaults so a typo
// in the struct tag (which would silently zero the field and trip validation,
// or worse template a wrong cadence) is caught.
func TestLoad_MonitorRunnerCadenceDefaults(t *testing.T) {
	withBaseEnv(t, map[string]string{})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if cfg.MonitorRunnerPollInterval != 2*time.Second {
		t.Fatalf("MonitorRunnerPollInterval = %s, want 2s", cfg.MonitorRunnerPollInterval)
	}
}

// TestLoad_AgentPublicURL covers the sk-142.7 monitor-bootstrap base URL:
// required (no default), and must be an absolute http(s) URL with a host.
// Empty (env unset/blank), relative, non-http(s) scheme, and host-less forms
// are all rejected; a valid http or https URL round-trips onto the Config.
func TestLoad_AgentPublicURL(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr string // substring; empty = expect success
	}{
		{name: "valid https ok", value: "https://mcp.example.com"},
		{name: "valid http ok", value: "http://localhost:5100"},
		{name: "valid https with path ok", value: "https://host.example/agent-gw"},
		{name: "empty rejected", value: "", wantErr: "skein_agent_public_url must be an absolute http(s) URL"},
		{name: "relative rejected", value: "/agent/v1", wantErr: "skein_agent_public_url must be an absolute http(s) URL"},
		{name: "no-scheme host rejected", value: "mcp.example.com", wantErr: "skein_agent_public_url must be an absolute http(s) URL"},
		{name: "non-http scheme rejected", value: "ftp://host", wantErr: "skein_agent_public_url must be an absolute http(s) URL"},
		{name: "scheme without host rejected", value: "http://", wantErr: "skein_agent_public_url must be an absolute http(s) URL"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withBaseEnv(t, map[string]string{"SKEIN_AGENT_PUBLIC_URL": tc.value})
			cfg, err := Load()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load() = %v, want nil", err)
				}
				if cfg.AgentPublicURL != tc.value {
					t.Fatalf("AgentPublicURL = %q, want %q", cfg.AgentPublicURL, tc.value)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load() error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestLoad_OAuthAuthorizationServers covers the sk-304 metadata inputs:
// SKEIN_OAUTH_AUTHORIZATION_SERVERS is OPTIONAL — unset switches discovery off
// (see TestLoad_OAuthDiscoveryOptional) — but every entry that IS supplied must
// be an absolute http(s) issuer identifier. A comma separates entries.
func TestLoad_OAuthAuthorizationServers(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    []string
		wantErr string // substring; empty = expect success
	}{
		{
			name:  "single issuer ok",
			value: "https://auth.example.com/realms/main",
			want:  []string{"https://auth.example.com/realms/main"},
		},
		{
			name:  "two issuers ok",
			value: "https://auth.example.com/realms/main,https://auth.example.com/realms/other",
			want:  []string{"https://auth.example.com/realms/main", "https://auth.example.com/realms/other"},
		},
		{
			// F1 — an unset variable is a switched-off feature, not a
			// misconfiguration. Covered in depth by
			// TestLoad_OAuthDiscoveryOptional.
			name:  "empty accepted, discovery off",
			value: "",
			want:  nil,
		},
		{
			name:    "relative rejected",
			value:   "/realms/main",
			wantErr: "authorization servers[0]",
		},
		{
			name:    "fragment rejected",
			value:   "https://auth.example.com/realms/main#frag",
			wantErr: "authorization servers[0]",
		},
		{
			name:    "second entry validated too",
			value:   "https://auth.example.com/realms/main,not-a-url",
			wantErr: "authorization servers[1]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withBaseEnv(t, map[string]string{"SKEIN_OAUTH_AUTHORIZATION_SERVERS": tc.value})
			cfg, err := Load()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load() = %v, want nil", err)
				}
				if len(cfg.OAuthAuthorizationServers) != len(tc.want) {
					t.Fatalf("OAuthAuthorizationServers = %v, want %v", cfg.OAuthAuthorizationServers, tc.want)
				}
				for i := range tc.want {
					if cfg.OAuthAuthorizationServers[i] != tc.want[i] {
						t.Fatalf("OAuthAuthorizationServers = %v, want %v", cfg.OAuthAuthorizationServers, tc.want)
					}
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load() error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestLoad_PublicURLHTTPSInProduction — sk-304. The public base URL is now
// the identity we PUBLISH to MCP clients, so production must not advertise it
// over plain http; development keeps http (local compose runs the whole stack
// on http://host.docker.internal:8080). Same rule for the issuer identifiers.
func TestLoad_PublicURLHTTPSInProduction(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name: "http public url in development ok",
			env: map[string]string{
				"ENVIRONMENT":                       "development",
				"SKEIN_AGENT_PUBLIC_URL":            "http://host.docker.internal:8080",
				"SKEIN_OAUTH_AUTHORIZATION_SERVERS": "http://iden.local/realms/x",
			},
		},
		{
			name: "https public url in production ok",
			env: map[string]string{
				"ENVIRONMENT":            "production",
				"SKEIN_AGENT_PUBLIC_URL": "https://mcp.example.com",
			},
		},
		{
			name: "http public url in production rejected",
			env: map[string]string{
				"ENVIRONMENT":            "production",
				"SKEIN_AGENT_PUBLIC_URL": "http://mcp.example.com",
			},
			wantErr: "skein_agent_public_url must use https in production",
		},
		{
			name: "http issuer in production rejected",
			env: map[string]string{
				"ENVIRONMENT":                       "production",
				"SKEIN_OAUTH_AUTHORIZATION_SERVERS": "http://auth.example.com/realms/main",
			},
			wantErr: "skein_oauth_authorization_servers must use https in production",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withBaseEnv(t, tc.env)
			_, err := Load()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load() error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestLoad_AuthModeHybrid — sk-163 S4a. The valid auth-mode set is now
// {local, keycloak, hybrid}. hybrid activates OIDC for internal users, so
// the OIDC default-role enum is validated for it as well as keycloak.
func TestLoad_AuthModeHybrid(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string // substring; empty = expect success
	}{
		{
			name: "hybrid accepted",
			env:  map[string]string{"SKEIN_AUTH_MODE": "hybrid"},
		},
		{
			name: "hybrid with explicit default role accepted",
			env:  map[string]string{"SKEIN_AUTH_MODE": "hybrid", "SKEIN_OIDC_DEFAULT_ROLE": "user"},
		},
		{
			name:    "hybrid with bad default role rejected",
			env:     map[string]string{"SKEIN_AUTH_MODE": "hybrid", "SKEIN_OIDC_DEFAULT_ROLE": "users"},
			wantErr: "skein_oidc_default_role must be one of {user, admin}",
		},
		{
			name:    "unknown mode rejected, message lists hybrid",
			env:     map[string]string{"SKEIN_AUTH_MODE": "ldap"},
			wantErr: "skein_auth_mode must be one of {local, keycloak, hybrid}",
		},
		{
			name: "break-glass admin carried through",
			env:  map[string]string{"SKEIN_AUTH_MODE": "hybrid", "SKEIN_BREAKGLASS_ADMIN": "boot@skein.local"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withBaseEnv(t, tc.env)
			cfg, err := Load()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load() = %v, want nil", err)
				}
				if cfg.AuthMode != tc.env["SKEIN_AUTH_MODE"] {
					t.Fatalf("AuthMode = %q, want %q", cfg.AuthMode, tc.env["SKEIN_AUTH_MODE"])
				}
				if want := tc.env["SKEIN_BREAKGLASS_ADMIN"]; want != "" && cfg.BreakGlassAdmin != want {
					t.Fatalf("BreakGlassAdmin = %q, want %q", cfg.BreakGlassAdmin, want)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load() error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestLoad_MonitorPollSafetyLag covers the sk-142.4 poll watermark knob:
// the lag must be strictly positive (a zero/negative lag disables the
// commit-order skip fence). The 5s default reaching Load() unchanged must
// be accepted.
func TestLoad_MonitorPollSafetyLag(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string // substring; empty = expect success
	}{
		{
			name: "default accepted",
			env:  map[string]string{},
		},
		{
			name: "explicit positive lag ok",
			env:  map[string]string{"SKEIN_MONITOR_POLL_SAFETY_LAG": "2s"},
		},
		{
			name:    "zero lag rejected",
			env:     map[string]string{"SKEIN_MONITOR_POLL_SAFETY_LAG": "0s"},
			wantErr: "skein_monitor_poll_safety_lag must be > 0",
		},
		{
			name:    "negative lag rejected",
			env:     map[string]string{"SKEIN_MONITOR_POLL_SAFETY_LAG": "-5s"},
			wantErr: "skein_monitor_poll_safety_lag must be > 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withBaseEnv(t, tc.env)
			_, err := Load()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load() error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// sk-274.1 — MCP GET-stream heartbeat knob. The interval is the only thing
// standing between an idle agent stream and the gateway's idle timeout, so a
// misconfigured value must not silently degrade into "no keepalive".
func TestLoad_MCPHeartbeatInterval(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    time.Duration // checked when wantErr == ""
		wantErr string        // substring; empty = expect success
	}{
		{
			name: "default is 30s",
			env:  map[string]string{},
			want: 30 * time.Second,
		},
		{
			name: "explicit interval honoured",
			env:  map[string]string{"MCP_HEARTBEAT_INTERVAL": "45s"},
			want: 45 * time.Second,
		},
		{
			name: "zero disables heartbeats",
			env:  map[string]string{"MCP_HEARTBEAT_INTERVAL": "0s"},
			want: 0,
		},
		{
			// caarlos0/env falls back to envDefault for a set-but-empty var,
			// so an empty value is NOT a way to disable the heartbeat. Pinned
			// here because the reverse is the intuitive assumption.
			name: "empty value falls back to the default, not to disabled",
			env:  map[string]string{"MCP_HEARTBEAT_INTERVAL": ""},
			want: 30 * time.Second,
		},
		{
			name:    "negative rejected",
			env:     map[string]string{"MCP_HEARTBEAT_INTERVAL": "-5s"},
			wantErr: "mcp_heartbeat_interval must be 0 (disabled) or >= 1s",
		},
		{
			// mcp-go arms one ticker per open GET stream, so a sub-second
			// typo becomes a hot write loop multiplied by the agent fleet.
			name:    "sub-second rejected",
			env:     map[string]string{"MCP_HEARTBEAT_INTERVAL": "1ms"},
			wantErr: "mcp_heartbeat_interval must be 0 (disabled) or >= 1s",
		},
		{
			name: "exactly the floor accepted",
			env:  map[string]string{"MCP_HEARTBEAT_INTERVAL": "1s"},
			want: time.Second,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withBaseEnv(t, tc.env)
			cfg, err := Load()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load() = %v, want nil", err)
				}
				if cfg.MCPHeartbeatInterval != tc.want {
					t.Fatalf("MCPHeartbeatInterval = %s, want %s", cfg.MCPHeartbeatInterval, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load() error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestLoad_OAuthDiscoveryOptional — sk-304 review F1. The first cut made
// SKEIN_OAUTH_AUTHORIZATION_SERVERS REQUIRED, so a pod whose ConfigMap
// predated the variable did not boot AT ALL: not "no OAuth discovery" but "no
// API" — every REST route, every MCP session, taken down by the next rollout
// of this image in exchange for an optional discovery document. The live
// production ConfigMap is exactly such a ConfigMap.
//
// The contract this test pins:
//
//   - unset            -> Load succeeds, discovery reported off;
//   - blank / spaces   -> same (a stray comma must not fail a pod);
//   - set but MALFORMED -> still fatal, in production AND development. That is
//     the silently-wrong-value case the fallback-free env rule exists for, and
//     the operator has the value in hand.
func TestLoad_OAuthDiscoveryOptional(t *testing.T) {
	t.Run("unset boots with discovery disabled", func(t *testing.T) {
		withBaseEnv(t, nil)
		os.Unsetenv("SKEIN_OAUTH_AUTHORIZATION_SERVERS")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() with no SKEIN_OAUTH_AUTHORIZATION_SERVERS = %v, want nil: "+
				"an unconfigured optional feature must not stop the service from starting", err)
		}
		if oauthmeta.Enabled(cfg.OAuthAuthorizationServers) {
			t.Fatalf("discovery reported enabled with no issuer configured: %v",
				cfg.OAuthAuthorizationServers)
		}
	})

	t.Run("blank value boots with discovery disabled", func(t *testing.T) {
		for _, v := range []string{"", " ", " , "} {
			withBaseEnv(t, map[string]string{"SKEIN_OAUTH_AUTHORIZATION_SERVERS": v})
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() with SKEIN_OAUTH_AUTHORIZATION_SERVERS=%q = %v, want nil", v, err)
			}
			if oauthmeta.Enabled(cfg.OAuthAuthorizationServers) {
				t.Fatalf("SKEIN_OAUTH_AUTHORIZATION_SERVERS=%q reported as enabled", v)
			}
		}
	})

	t.Run("malformed value is still fatal", func(t *testing.T) {
		withBaseEnv(t, map[string]string{"SKEIN_OAUTH_AUTHORIZATION_SERVERS": "not-a-url"})
		if _, err := Load(); err == nil {
			t.Fatal("Load() = nil for a malformed issuer: a value that is SET but wrong " +
				"is the silently-wrong case fail-fast exists for")
		}
	})
}

// TestLoad_PublicURLRejectsUserinfo — sk-304 review F3. url.Parse keeps
// userinfo in a SEPARATE field, so "https://user:pass@host" passed every
// existing check and the identifier was silently rebuilt from scheme+host —
// publishing a different string than the operator configured, on an endpoint
// whose entire contract is byte-for-byte equality. RFC 9110 §4.2.4 forbids
// emitting a userinfo subcomponent in an http(s) URI used as a field value.
func TestLoad_PublicURLRejectsUserinfo(t *testing.T) {
	for _, raw := range []string{
		"https://user:pass@mcp.example.com",
		"https://user@mcp.example.com",
	} {
		withBaseEnv(t, map[string]string{
			"SKEIN_AGENT_PUBLIC_URL":            raw,
			"SKEIN_OAUTH_AUTHORIZATION_SERVERS": "https://auth.example.com/realms/main",
		})
		_, err := Load()
		if err == nil {
			t.Fatalf("Load() = nil for public URL %q, want a userinfo rejection", raw)
		}
		if !strings.Contains(err.Error(), "userinfo") {
			t.Fatalf("Load() error = %q, want it to name the userinfo component", err.Error())
		}
	}
}

// TestLoad_AuthorizationServerRejectsUserinfo — sk-304 review F3, the sharper
// half. An issuer entry is published VERBATIM (the trimmed input string lands
// in the document as-is), so a userinfo component here is not merely dropped —
// it is served, credentials and all, on a route that is unauthenticated by
// design.
func TestLoad_AuthorizationServerRejectsUserinfo(t *testing.T) {
	withBaseEnv(t, map[string]string{
		"SKEIN_OAUTH_AUTHORIZATION_SERVERS": "https://admin:hunter2@auth.example.com/realms/main",
	})
	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil for an issuer carrying userinfo: it would be published " +
			"verbatim in a document served without authentication")
	}
	if !strings.Contains(err.Error(), "userinfo") {
		t.Fatalf("Load() error = %q, want it to name the userinfo component", err.Error())
	}
}

// TestLoad_PwnedPasswordsTimeout — sk-303.20 review (codex W, grok W).
// http.Client.Timeout treats <= 0 as "no timeout at all" (net/http docs),
// not "fast" — so SKEIN_PWNED_PASSWORDS_TIMEOUT=0 (an operator's plausible
// typo reaching for "no delay") would install an unbounded outbound client
// instead. Validated unconditionally, mirroring
// TestLoad_MonitorPollSafetyLag: the 2s default must be accepted untouched,
// zero/negative must fail fast at startup.
func TestLoad_PwnedPasswordsTimeout(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string // substring; empty = expect success
	}{
		{
			name: "default accepted",
			env:  map[string]string{},
		},
		{
			name: "explicit positive timeout ok",
			env:  map[string]string{"SKEIN_PWNED_PASSWORDS_TIMEOUT": "5s"},
		},
		{
			name:    "zero timeout rejected",
			env:     map[string]string{"SKEIN_PWNED_PASSWORDS_TIMEOUT": "0s"},
			wantErr: "skein_pwned_passwords_timeout must be > 0",
		},
		{
			name:    "negative timeout rejected",
			env:     map[string]string{"SKEIN_PWNED_PASSWORDS_TIMEOUT": "-1s"},
			wantErr: "skein_pwned_passwords_timeout must be > 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withBaseEnv(t, tc.env)
			_, err := Load()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load() error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestLoad_MonitorTokenIdleTTL — sk-579 matrix row 1 (US-2, SC-003, R1): the
// renewal window must satisfy 0 < window <= 12h, the 6h default boots, and
// every rejected value leaves Load with no config. Only the variable under
// test is wrong in each case, so the failure belongs to it by construction.
func TestLoad_MonitorTokenIdleTTL(t *testing.T) {
	cases := []struct {
		name    string
		value   string // "" = leave unset (default)
		wantErr bool
	}{
		{name: "default 6h boots", value: ""},
		{name: "exactly 12h boots", value: "12h"},
		{name: "zero rejected", value: "0s", wantErr: true},
		{name: "negative rejected", value: "-1h", wantErr: true},
		{name: "12h1s rejected", value: "12h1s", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			if tc.value != "" {
				env["SKEIN_MONITOR_TOKEN_IDLE_TTL"] = tc.value
			}
			withBaseEnv(t, env)
			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load() = nil error for SKEIN_MONITOR_TOKEN_IDLE_TTL=%q, want rejection", tc.value)
				}
				if cfg != nil {
					t.Fatalf("Load() returned a config alongside the error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() = %v, want nil", err)
			}
			if tc.value == "" && cfg.MonitorTokenIdleTTL != 6*time.Hour {
				t.Fatalf("default MonitorTokenIdleTTL = %s, want 6h", cfg.MonitorTokenIdleTTL)
			}
		})
	}
}

// TestLoad_MonitorStallThreshold — sk-579 matrix row 2 (INV-6, SC-003, US-2):
// the stall threshold must exceed both the longest gap between requests of a
// live runner at the CURRENT poll interval (backoff ceiling max(5·P, 10s) plus
// the per-request limit) and the 60s agents last-seen granularity.
func TestLoad_MonitorStallThreshold(t *testing.T) {
	// At P = 20s the backoff ceiling is 5·20s = 100s, so the live-runner gap
	// is 100s + request limit — above the 60s floor, so case (b) isolates
	// the gap bound. P = 21s / 19s move the gap by ±5s.
	gapAt20 := 100*time.Second + MonitorRunnerRequestLimit
	secs := func(d time.Duration) string { return d.String() }

	cases := []struct {
		name     string
		interval string // SKEIN_MONITOR_RUNNER_POLL_INTERVAL; "" = default 2s
		stall    string // SKEIN_MONITOR_STALL_THRESHOLD; "" = default 3m
		wantErr  bool
	}{
		// (a) defaults boot.
		{name: "a default 3m at default interval boots"},
		// (b) threshold equal to the gap is rejected, 1s above boots.
		{name: "b threshold equal to gap rejected", interval: "20s", stall: secs(gapAt20), wantErr: true},
		{name: "b threshold gap+1s boots", interval: "20s", stall: secs(gapAt20 + time.Second)},
		// (c) the same threshold against a larger / smaller interval.
		{name: "c same threshold at larger interval rejected", interval: "21s", stall: secs(gapAt20 + time.Second), wantErr: true},
		{name: "c same threshold at smaller interval boots", interval: "19s", stall: secs(gapAt20 + time.Second)},
		// (d) at the default 2s interval the gap (10s + limit) is under 60s:
		// the agents granularity floor decides.
		{name: "d 60s rejected by granularity floor", stall: "60s", wantErr: true},
		{name: "d 61s boots", stall: "61s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			if tc.interval != "" {
				env["SKEIN_MONITOR_RUNNER_POLL_INTERVAL"] = tc.interval
			}
			if tc.stall != "" {
				env["SKEIN_MONITOR_STALL_THRESHOLD"] = tc.stall
			}
			withBaseEnv(t, env)
			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load() = nil error (interval=%q stall=%q), want rejection", tc.interval, tc.stall)
				}
				if cfg != nil {
					t.Fatalf("Load() returned a config alongside the error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() = %v, want nil", err)
			}
			if tc.stall == "" && cfg.MonitorStallThreshold != 3*time.Minute {
				t.Fatalf("default MonitorStallThreshold = %s, want 3m", cfg.MonitorStallThreshold)
			}
		})
	}
}

// TestRunnerTimings — sk-579 matrix row 3 (INV-6, US-1 «Unchanged»): the
// backoff ceiling keeps the runner's historical formula max(5·P, 10s); the
// per-request limit is positive and finite.
func TestRunnerTimings(t *testing.T) {
	cases := []struct {
		interval    time.Duration
		wantCeiling time.Duration
	}{
		{1 * time.Second, 10 * time.Second},
		{2 * time.Second, 10 * time.Second},
		{4 * time.Second, 20 * time.Second},
	}
	for _, tc := range cases {
		got := RunnerTimings(tc.interval)
		if got.BackoffCeiling != tc.wantCeiling {
			t.Errorf("RunnerTimings(%s).BackoffCeiling = %s, want %s", tc.interval, got.BackoffCeiling, tc.wantCeiling)
		}
		if got.RequestLimit <= 0 || got.RequestLimit >= time.Duration(math.MaxInt64) {
			t.Errorf("RunnerTimings(%s).RequestLimit = %s, want positive and finite", tc.interval, got.RequestLimit)
		}
	}
}

// TestLoad_WatchingWindowAndRetention — sk-622.21, test matrix row 9 (US-8,
// edge 17). Reference — spec US-8: defaults are a 21-day Watching window and
// a 90-day retention; a retention shorter than the window, and zero or
// negative values, are configuration errors at startup; retention equal to
// the window is allowed.
func TestLoad_WatchingWindowAndRetention(t *testing.T) {
	cases := []struct {
		name       string
		window     string // WATCHING_FEED_WINDOW_DAYS; "" = default
		retention  string // NOTIFICATIONS_RETENTION_DAYS; "" = default
		wantErr    bool
		wantWindow int
		wantRetain int
	}{
		{name: "defaults 21 and 90", wantWindow: 21, wantRetain: 90},
		{name: "retention equal to window boots", window: "30", retention: "30", wantWindow: 30, wantRetain: 30},
		{name: "retention shorter than window rejected", window: "30", retention: "29", wantErr: true},
		{name: "default window over a short retention rejected", retention: "20", wantErr: true},
		{name: "zero window rejected", window: "0", wantErr: true},
		{name: "negative window rejected", window: "-1", wantErr: true},
		{name: "zero retention rejected", retention: "0", wantErr: true},
		{name: "negative retention rejected", retention: "-5", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			if tc.window != "" {
				env["WATCHING_FEED_WINDOW_DAYS"] = tc.window
			}
			if tc.retention != "" {
				env["NOTIFICATIONS_RETENTION_DAYS"] = tc.retention
			}
			withBaseEnv(t, env)
			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load() = nil error (window=%q retention=%q), want rejection", tc.window, tc.retention)
				}
				if cfg != nil {
					t.Fatalf("Load() returned a config alongside the error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() = %v, want nil", err)
			}
			if cfg.WatchingFeedWindowDays != tc.wantWindow || cfg.NotificationsRetentionDays != tc.wantRetain {
				t.Errorf("window/retention = %d/%d, want %d/%d",
					cfg.WatchingFeedWindowDays, cfg.NotificationsRetentionDays, tc.wantWindow, tc.wantRetain)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// sk-674.18 — installation settings (test matrix rows 1–3)
// ---------------------------------------------------------------------------

// installationVars are every variable of the installation settings that
// these tests touch. withInstallationEnv clears them all first, so a value
// left in the developer's environment cannot decide a case.
var installationVars = []string{
	"SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_COMMON_SPACE_ENABLED",
	"SKEIN_SPACE_MEMBER_LIMIT", "SKEIN_SPACE_OWNED_LIMIT", "SKEIN_SPACE_INVITE_TTL",
	"SKEIN_SPACE_INVITES_PER_DAY", "SKEIN_SPACE_CREATION", "SKEIN_PROJECT_DEFAULT_VISIBILITY",
	"SKEIN_SIGNUP_ENABLED", "SKEIN_TURNSTILE_SITE_KEY", "SKEIN_TURNSTILE_SECRET",
	"SKEIN_PROJECT_PREFIX_MIN_LENGTH", "SKEIN_EMAIL_ENABLED",
	"SKEIN_SMTP_HOST", "SKEIN_SMTP_PORT", "SKEIN_SMTP_USERNAME", "SKEIN_SMTP_PASSWORD",
	"SKEIN_SMTP_FROM", "SKEIN_SMTP_TLS_MODE", "SKEIN_EMAIL_DELIVERY_TTL", "SKEIN_APP_BASE_URL",
	"SKEIN_OPERATOR_API_ENABLED", "SKEIN_OPERATOR_API_PORT", "SKEIN_OPERATOR_API_KEY",
	"SKEIN_QUOTA_PERSONAL_PROJECTS", "SKEIN_QUOTA_PERSONAL_STORAGE_BYTES",
	"SKEIN_QUOTA_SPACE_PROJECTS", "SKEIN_QUOTA_SPACE_STORAGE_BYTES",
	"SKEIN_TERMS_URL", "SKEIN_PRIVACY_URL", "SKEIN_INSTANCE_NAME",
	"SKEIN_AUTH_MODE", "SKEIN_OIDC_DEFAULT_ROLE", "SKEIN_AUTO_PROVISION",
	"SKEIN_INITIAL_ADMIN_EMAIL", "SKEIN_INITIAL_ADMIN_PASSWORD",
	"SKEIN_INITIAL_ADMIN_USERNAME", "SKEIN_INITIAL_ADMIN_NAME",
	"SKEIN_BOOTSTRAP_ADMIN_EMAIL", "SKEIN_BREAKGLASS_ADMIN",
}

// withInstallationEnv is withBaseEnv with every installation variable
// cleared (an empty value means unset to caarlos0/env), then overrides.
func withInstallationEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	cleared := map[string]string{}
	for _, name := range installationVars {
		cleared[name] = ""
	}
	withBaseEnv(t, cleared)
	for k, v := range overrides {
		t.Setenv(k, v)
	}
}

// mailOn is a complete SMTP configuration — the relay and the public address
// of the UI the email links point into: cases that need mail switched on
// without contradicting anything else merge it in.
var mailOn = map[string]string{
	"SKEIN_APP_BASE_URL":  "https://skein.example.com",
	"SKEIN_EMAIL_ENABLED": "true",
	"SKEIN_SMTP_HOST":     "smtp.example.com",
	"SKEIN_SMTP_PORT":     "465",
	"SKEIN_SMTP_USERNAME": "skein",
	"SKEIN_SMTP_PASSWORD": "smtp-password",
	"SKEIN_SMTP_FROM":     "skein@example.com",
	"SKEIN_SMTP_TLS_MODE": "implicit",
}

func merge(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, n := range a {
		seen[n]++
	}
	for _, n := range b {
		seen[n]--
	}
	for _, c := range seen {
		if c != 0 {
			return false
		}
	}
	return true
}

// TestLoad_SettingConflictsNameTheirVariables is matrix row 1 (SC-007,
// US-18, INV-21 by variables). The reference is the table of SAAS-PLAN 2.5:
// every row decided by the variables alone, plus the two INV-21 combinations
// that are (signup ⇒ email, signup ⇒ local), must refuse startup, and the
// refusal must name exactly the variables of that row — the machine part of
// the SC-007 contract, ConflictError.Vars. The wording is not asserted.
//
// Each case contradicts ONE row only: the admin-off cases switch mail on, so
// the "no admin and no mail with a password login" row cannot answer for
// them. A consistent SaaS preset is the control that must load.
func TestLoad_SettingConflictsNameTheirVariables(t *testing.T) {
	adminOff := merge(mailOn, map[string]string{"SKEIN_INSTANCE_ADMIN_ENABLED": "false"})
	cases := []struct {
		name     string
		env      map[string]string
		wantVars []string // nil = Load must succeed
	}{
		// 2.5 row 1: admin off, yet an admin is requested — each variable.
		{"admin off + initial admin email", merge(adminOff, map[string]string{"SKEIN_INITIAL_ADMIN_EMAIL": "root@example.com"}),
			[]string{"SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_INITIAL_ADMIN_EMAIL"}},
		{"admin off + initial admin password", merge(adminOff, map[string]string{"SKEIN_INITIAL_ADMIN_PASSWORD": "secret-secret"}),
			[]string{"SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_INITIAL_ADMIN_PASSWORD"}},
		{"admin off + initial admin username", merge(adminOff, map[string]string{"SKEIN_INITIAL_ADMIN_USERNAME": "root"}),
			[]string{"SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_INITIAL_ADMIN_USERNAME"}},
		{"admin off + initial admin name", merge(adminOff, map[string]string{"SKEIN_INITIAL_ADMIN_NAME": "Root"}),
			[]string{"SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_INITIAL_ADMIN_NAME"}},
		{"admin off + bootstrap admin email", merge(adminOff, map[string]string{"SKEIN_BOOTSTRAP_ADMIN_EMAIL": "root@example.com"}),
			[]string{"SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_BOOTSTRAP_ADMIN_EMAIL"}},
		{"admin off + break-glass admin", merge(adminOff, map[string]string{"SKEIN_BREAKGLASS_ADMIN": "root@example.com"}),
			[]string{"SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_BREAKGLASS_ADMIN"}},
		// 2.5 row 2.
		{"admin off + OIDC default role admin", merge(adminOff, map[string]string{"SKEIN_OIDC_DEFAULT_ROLE": "admin"}),
			[]string{"SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_OIDC_DEFAULT_ROLE"}},
		// 2.5 row 4.
		{"admin off + space creation admin", merge(adminOff, map[string]string{"SKEIN_SPACE_CREATION": "admin"}),
			[]string{"SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_SPACE_CREATION"}},
		// 2.5 row 7 / INV-21 signup ⇒ email.
		{"signup on + mail off", map[string]string{"SKEIN_SIGNUP_ENABLED": "true"},
			[]string{"SKEIN_SIGNUP_ENABLED", "SKEIN_EMAIL_ENABLED"}},
		// 2.5 row 8, both password modes.
		{"no mail, no admin, local", map[string]string{"SKEIN_INSTANCE_ADMIN_ENABLED": "false", "SKEIN_AUTH_MODE": "local"},
			[]string{"SKEIN_EMAIL_ENABLED", "SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_AUTH_MODE"}},
		{"no mail, no admin, hybrid", map[string]string{"SKEIN_INSTANCE_ADMIN_ENABLED": "false", "SKEIN_AUTH_MODE": "hybrid"},
			[]string{"SKEIN_EMAIL_ENABLED", "SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_AUTH_MODE"}},
		// 2.5 row 9 / INV-21 signup ⇒ local.
		{"signup on + keycloak", merge(mailOn, map[string]string{"SKEIN_SIGNUP_ENABLED": "true", "SKEIN_AUTH_MODE": "keycloak"}),
			[]string{"SKEIN_SIGNUP_ENABLED", "SKEIN_AUTH_MODE"}},
		{"signup on + hybrid", merge(mailOn, map[string]string{"SKEIN_SIGNUP_ENABLED": "true", "SKEIN_AUTH_MODE": "hybrid"}),
			[]string{"SKEIN_SIGNUP_ENABLED", "SKEIN_AUTH_MODE"}},
		// Control: the SaaS preset contradicts nothing.
		{"SaaS preset loads", merge(mailOn, map[string]string{
			"SKEIN_INSTANCE_ADMIN_ENABLED":     "false",
			"SKEIN_COMMON_SPACE_ENABLED":       "false",
			"SKEIN_SIGNUP_ENABLED":             "true",
			"SKEIN_AUTH_MODE":                  "local",
			"SKEIN_SPACE_CREATION":             "all",
			"SKEIN_PROJECT_DEFAULT_VISIBILITY": "space",
			"SKEIN_SPACE_MEMBER_LIMIT":         "25",
			"SKEIN_SPACE_OWNED_LIMIT":          "3",
			"SKEIN_SPACE_INVITES_PER_DAY":      "20",
			"SKEIN_TURNSTILE_SITE_KEY":         "site-key",
			"SKEIN_TURNSTILE_SECRET":           "site-secret",
			"SKEIN_OPERATOR_API_ENABLED":       "true",
			"SKEIN_OPERATOR_API_KEY":           "operator-key-0123456789",
		}), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withInstallationEnv(t, tc.env)
			cfg, err := Load()
			if tc.wantVars == nil {
				if err != nil {
					t.Fatalf("Load() = %v, want a consistent configuration to load", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() succeeded, want refusal naming %v", tc.wantVars)
			}
			if cfg != nil {
				t.Errorf("Load() returned a config alongside the refusal")
			}
			var conflictErr *ConflictError
			if !errors.As(err, &conflictErr) {
				t.Fatalf("Load() = %v (%T), want a *ConflictError naming %v", err, err, tc.wantVars)
			}
			if !sameNames(conflictErr.Vars, tc.wantVars) {
				t.Errorf("refusal names %v, want exactly %v", conflictErr.Vars, tc.wantVars)
			}
		})
	}
}

// TestLoad_InstallationDefaultsAreTodaysCorporateBehaviour is matrix row 2
// (K-30): with none of the new variables set, Load yields what a corporate
// installation does today, so upgrading without new values changes nothing.
func TestLoad_InstallationDefaultsAreTodaysCorporateBehaviour(t *testing.T) {
	withInstallationEnv(t, nil)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want the defaults to load", err)
	}
	checks := []struct {
		what string
		ok   bool
	}{
		{"SKEIN_INSTANCE_ADMIN_ENABLED=true", cfg.InstanceAdminEnabled},
		{"SKEIN_COMMON_SPACE_ENABLED=true", cfg.CommonSpaceEnabled},
		{"SKEIN_SIGNUP_ENABLED=false", !cfg.SignupEnabled},
		{"SKEIN_EMAIL_ENABLED=false", !cfg.EmailEnabled},
		{"SKEIN_SPACE_CREATION=all", cfg.SpaceCreation == "all"},
		{"SKEIN_PROJECT_DEFAULT_VISIBILITY=members", cfg.ProjectDefaultVisibility == "members"},
		{"SKEIN_SPACE_MEMBER_LIMIT empty", cfg.SpaceMemberLimit == nil},
		{"SKEIN_SPACE_OWNED_LIMIT empty", cfg.SpaceOwnedLimit == nil},
		{"SKEIN_SPACE_INVITES_PER_DAY empty", cfg.SpaceInvitesPerDay == nil},
		{"SKEIN_QUOTA_PERSONAL_PROJECTS empty", cfg.QuotaPersonalProjects == nil},
		{"SKEIN_QUOTA_PERSONAL_STORAGE_BYTES empty", cfg.QuotaPersonalStorageBytes == nil},
		{"SKEIN_QUOTA_SPACE_PROJECTS empty", cfg.QuotaSpaceProjects == nil},
		{"SKEIN_QUOTA_SPACE_STORAGE_BYTES empty", cfg.QuotaSpaceStorageBytes == nil},
		{"SKEIN_OPERATOR_API_PORT=5101", cfg.OperatorAPIPort == "5101"},
		{"SKEIN_OPERATOR_API_ENABLED=false", !cfg.OperatorAPIEnabled},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("default %s does not hold", c.what)
		}
	}
}

// TestLoad_EnabledFeatureRequiresItsAddressesAndKeys is matrix row 3
// ("env without fallback", K-10): while mail is on every SKEIN_SMTP_* and
// SKEIN_APP_BASE_URL (the absolute address the links are built on, T022) is
// required, while the operator API is on its key is required, a Turnstile
// site key requires the secret — and with the feature off the empty values
// are accepted.
func TestLoad_EnabledFeatureRequiresItsAddressesAndKeys(t *testing.T) {
	smtpVars := []string{
		"SKEIN_SMTP_HOST", "SKEIN_SMTP_PORT", "SKEIN_SMTP_USERNAME",
		"SKEIN_SMTP_PASSWORD", "SKEIN_SMTP_FROM", "SKEIN_SMTP_TLS_MODE",
		"SKEIN_APP_BASE_URL",
	}
	type tc struct {
		name    string
		env     map[string]string
		wantErr bool
	}
	cases := []tc{
		{"mail on, complete SMTP", mailOn, false},
		{"mail off, SMTP empty", map[string]string{"SKEIN_EMAIL_ENABLED": "false"}, false},
		{"mail on, sender with a display name", merge(mailOn, map[string]string{"SKEIN_SMTP_FROM": "Skein <skein@example.com>"}), false},
		{"mail on, sender with a line break", merge(mailOn, map[string]string{"SKEIN_SMTP_FROM": "skein@example.com\r\nBcc: x@example.org"}), true},
		{"mail on, sender not an address", merge(mailOn, map[string]string{"SKEIN_SMTP_FROM": "skein at example"}), true},
		{"mail on, base URL under a path", merge(mailOn, map[string]string{"SKEIN_APP_BASE_URL": "https://example.com/skein/"}), false},
		{"mail on, base URL relative", merge(mailOn, map[string]string{"SKEIN_APP_BASE_URL": "/skein"}), true},
		{"mail on, base URL with a query", merge(mailOn, map[string]string{"SKEIN_APP_BASE_URL": "https://skein.example.com/?a=b"}), true},
		{"operator on with key", map[string]string{"SKEIN_OPERATOR_API_ENABLED": "true", "SKEIN_OPERATOR_API_KEY": "operator-key-aaaaaaaaaa"}, false},
		{"operator on without key", map[string]string{"SKEIN_OPERATOR_API_ENABLED": "true"}, true},
		{"operator off without key", map[string]string{"SKEIN_OPERATOR_API_ENABLED": "false"}, false},
		{"Turnstile site key without secret", map[string]string{"SKEIN_TURNSTILE_SITE_KEY": "site-key"}, true},
		{"Turnstile both keys", map[string]string{"SKEIN_TURNSTILE_SITE_KEY": "site-key", "SKEIN_TURNSTILE_SECRET": "site-secret"}, false},
		{"Turnstile neither key", map[string]string{}, false},
	}
	for _, name := range smtpVars {
		cases = append(cases, tc{"mail on, " + name + " empty", merge(mailOn, map[string]string{name: ""}), true})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withInstallationEnv(t, c.env)
			cfg, err := Load()
			if c.wantErr {
				if err == nil {
					t.Fatalf("Load() succeeded, want refusal: an enabled feature without its address or key")
				}
				if cfg != nil {
					t.Errorf("Load() returned a config alongside the refusal")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() = %v, want success", err)
			}
		})
	}
}
