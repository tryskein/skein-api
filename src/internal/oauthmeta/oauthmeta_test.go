package oauthmeta

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const (
	testBase = "https://mcp.example.com"
	testAS   = "https://auth.example.com/realms/main"
)

// TestDescribe_ResourceIsExactIdentifier is the heart of sk-304. The client
// compares `resource` with the URL it was pointed at BYTE FOR BYTE
// (RFC 9728 §6, no normalisation), so this asserts the literal string — not a
// parsed URL, not a case-insensitive match.
func TestDescribe_ResourceIsExactIdentifier(t *testing.T) {
	d, err := Describe(testBase, []string{testAS})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	const want = "https://mcp.example.com/mcp"
	if d.ResourceID != want {
		t.Fatalf("ResourceID = %q, want %q", d.ResourceID, want)
	}
	if d.Document.Resource != want {
		t.Fatalf("Document.Resource = %q, want %q", d.Document.Resource, want)
	}

	// The bug that produced sk-304: the origin, without the path.
	if d.Document.Resource == testBase {
		t.Fatal("Document.Resource is the bare origin — this is exactly the sk-304 defect")
	}
	// Trailing slash is significant to the comparing client.
	if strings.HasSuffix(d.Document.Resource, "/") {
		t.Fatalf("Document.Resource has a trailing slash: %q", d.Document.Resource)
	}
}

// TestDescribe_WellKnownIsPathAware pins RFC 9728 §3.1: the well-known suffix
// goes BETWEEN the host and the path, so the document for
// "https://host/mcp" lives at "/.well-known/oauth-protected-resource/mcp" —
// not at "/mcp/.well-known/oauth-protected-resource".
func TestDescribe_WellKnownIsPathAware(t *testing.T) {
	d, err := Describe(testBase, []string{testAS})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	if want := "/.well-known/oauth-protected-resource/mcp"; d.WellKnownPath != want {
		t.Fatalf("WellKnownPath = %q, want %q", d.WellKnownPath, want)
	}
	if want := "https://mcp.example.com/.well-known/oauth-protected-resource/mcp"; d.MetadataURL != want {
		t.Fatalf("MetadataURL = %q, want %q", d.MetadataURL, want)
	}
	if strings.Contains(d.WellKnownPath, "/mcp/.well-known") {
		t.Fatalf("well-known suffix appended after the path: %q", d.WellKnownPath)
	}
}

// TestDescribe_Challenge pins the RFC 9728 §5.1 header value, which the MCP
// authorization spec makes the client's primary discovery route. It must name
// the PATH-AWARE document, quoted.
func TestDescribe_Challenge(t *testing.T) {
	d, err := Describe(testBase, []string{testAS})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	const want = `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/mcp"`
	if d.Challenge != want {
		t.Fatalf("Challenge = %q, want %q", d.Challenge, want)
	}
}

// TestDescribe_Body checks the serialised document: required fields present,
// and no scope claim we do not enforce.
func TestDescribe_Body(t *testing.T) {
	d, err := Describe(testBase, []string{testAS, "https://auth.example.com/realms/other"})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(d.Body, &got); err != nil {
		t.Fatalf("body is not valid JSON: %v (%s)", err, d.Body)
	}

	if got["resource"] != "https://mcp.example.com/mcp" {
		t.Fatalf("body resource = %v", got["resource"])
	}
	servers, ok := got["authorization_servers"].([]any)
	if !ok || len(servers) != 2 || servers[0] != testAS {
		t.Fatalf("body authorization_servers = %v", got["authorization_servers"])
	}
	// F2 — we do not advertise a bearer method we do not accept. `header`
	// means "present the token in `Authorization`", which nothing here reads:
	// the door takes X-Skein-Api-Key and the BFF strips `Authorization`
	// entirely. A client that believed the advertisement would loop
	// 401 → discovery → token → 401. The field is OPTIONAL (RFC 9728 §2), so
	// silence is a conformant answer; it comes back with the code that honours
	// it (sk-304 stage 2).
	if _, present := got["bearer_methods_supported"]; present {
		t.Fatalf("body advertises bearer_methods_supported, but no bearer method is "+
			"accepted on this door: %s", d.Body)
	}
	// We do not advertise scopes we do not enforce (see Document doc comment).
	if _, present := got["scopes_supported"]; present {
		t.Fatalf("body advertises scopes_supported, which nothing enforces: %s", d.Body)
	}
}

// TestDescribe_TrailingSlashPolicy: a single trailing slash in the operator's
// value is normalised away, so the identifier never comes out as
// "https://host//mcp" — which would fail every client's byte comparison.
func TestDescribe_TrailingSlashPolicy(t *testing.T) {
	for _, in := range []string{testBase, testBase + "/"} {
		d, err := Describe(in, []string{testAS})
		if err != nil {
			t.Fatalf("Describe(%q): %v", in, err)
		}
		if want := "https://mcp.example.com/mcp"; d.ResourceID != want {
			t.Fatalf("Describe(%q).ResourceID = %q, want %q", in, d.ResourceID, want)
		}
	}
}

// TestDescribe_CaseIsPreserved: we must NOT canonicalise case. RFC 9728 §6
// forbids normalisation on the comparison side, so if an operator configures
// a mixed-case host we publish it verbatim and a mismatch is visible rather
// than silently "fixed" into a value the client never asked about.
func TestDescribe_CaseIsPreserved(t *testing.T) {
	d, err := Describe("https://MCP.Example.com", []string{testAS})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if want := "https://MCP.Example.com/mcp"; d.ResourceID != want {
		t.Fatalf("ResourceID = %q, want %q (case must not be folded)", d.ResourceID, want)
	}
	if d.ResourceID == "https://mcp.example.com/mcp" {
		t.Fatal("host was lower-cased — comparison downstream is case-sensitive")
	}
}

func TestDescribe_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		base    string
		servers []string
		wantErr string
	}{
		{"empty base", "", []string{testAS}, "absolute http(s) URL"},
		{"relative base", "/mcp", []string{testAS}, "absolute http(s) URL"},
		{"no scheme", "mcp.example.com", []string{testAS}, "absolute http(s) URL"},
		{"non-http scheme", "ftp://host", []string{testAS}, "absolute http(s) URL"},
		{"scheme without host", "https://", []string{testAS}, "absolute http(s) URL"},
		{"fragment", "https://host#frag", []string{testAS}, "query or fragment"},
		{"query", "https://host?a=b", []string{testAS}, "query or fragment"},
		{"no authorization servers", testBase, nil, "at least one issuer"},
		{"empty authorization server list", testBase, []string{}, "at least one issuer"},
		{"blank authorization server", testBase, []string{""}, "authorization servers[0]"},
		{"relative authorization server", testBase, []string{"/realms/x"}, "authorization servers[0]"},
		{"authorization server with fragment", testBase, []string{"https://iden/realms/x#f"}, "authorization servers[0]"},
		{"second authorization server invalid", testBase, []string{testAS, "nope"}, "authorization servers[1]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Describe(tc.base, tc.servers)
			if err == nil {
				t.Fatalf("Describe(%q, %v) = %+v, want error", tc.base, tc.servers, d)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestDescribe_BasePath — SKEIN_AGENT_PUBLIC_URL has allowed a deployment
// base path since sk-142.7 and this document must not quietly outlaw it.
// RFC 9728 §3.1 puts the whole identifier path AFTER the well-known suffix.
func TestDescribe_BasePath(t *testing.T) {
	for _, in := range []string{"https://host.example/agent-gw", "https://host.example/agent-gw/"} {
		d, err := Describe(in, []string{testAS})
		if err != nil {
			t.Fatalf("Describe(%q): %v", in, err)
		}
		if want := "https://host.example/agent-gw/mcp"; d.ResourceID != want {
			t.Fatalf("Describe(%q).ResourceID = %q, want %q", in, d.ResourceID, want)
		}
		if want := "/.well-known/oauth-protected-resource/agent-gw/mcp"; d.WellKnownPath != want {
			t.Fatalf("Describe(%q).WellKnownPath = %q, want %q", in, d.WellKnownPath, want)
		}
		if want := "https://host.example/.well-known/oauth-protected-resource/agent-gw/mcp"; d.MetadataURL != want {
			t.Fatalf("Describe(%q).MetadataURL = %q, want %q", in, d.MetadataURL, want)
		}
	}
}

// TestDescribe_HTTPBaseAllowed — local development runs the whole stack over
// plain http (docker-compose.local.yml points SKEIN_AGENT_PUBLIC_URL at
// http://host.docker.internal:8080). The https requirement is enforced by
// config.Load for production only; this package must not second-guess it.
func TestDescribe_HTTPBaseAllowed(t *testing.T) {
	d, err := Describe("http://host.docker.internal:8080", []string{"http://iden.local/realms/x"})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if want := "http://host.docker.internal:8080/mcp"; d.ResourceID != want {
		t.Fatalf("ResourceID = %q, want %q", d.ResourceID, want)
	}
}

// TestChallenge_IsAWellFormedQuotedString — sk-304 review F4. The challenge
// used to be assembled by plain concatenation, and url.Parse does NOT reject a
// DQUOTE in the host component: url.Parse(`https://ho"st/`) succeeds with
// Host == `ho"st`. Concatenation therefore produced
//
//	Bearer resource_metadata="https://ho"st/.well-known/oauth-protected-resource/mcp"
//
// which every RFC 9110 §11.2 parser reads as a metadata URL truncated at the
// stray quote — a discovery pointer silently aimed somewhere else, the exact
// class of failure sk-304 is about. RFC 9110 §5.6.4 requires DQUOTE and
// backslash inside a quoted-string to be escaped as quoted-pairs.
//
// The assertion is not "the header contains the URL" (concatenation passes
// that) but "a conformant parser recovers the URL we meant", so it fails on the
// truncation the escaping exists to prevent.
func TestChallenge_IsAWellFormedQuotedString(t *testing.T) {
	cases := []struct {
		name string
		base string
	}{
		{"ordinary host", "https://mcp.example.com"},
		{"host carrying a double quote", `https://ho"st`},
		{"host carrying a backslash", `https://ho\st`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Describe(tc.base, []string{testAS})
			if err != nil {
				t.Skipf("Describe(%q) rejected the input (%v) — nothing to escape", tc.base, err)
			}

			const prefix = "Bearer resource_metadata="
			if !strings.HasPrefix(d.Challenge, prefix) {
				t.Fatalf("challenge = %q, want prefix %q", d.Challenge, prefix)
			}
			got, err := unquoteAuthParam(strings.TrimPrefix(d.Challenge, prefix))
			if err != nil {
				t.Fatalf("challenge %q is not a well-formed quoted-string: %v", d.Challenge, err)
			}
			if got != d.MetadataURL {
				t.Fatalf("a conformant parser recovers %q from the challenge, but the "+
					"document actually lives at %q — the pointer is truncated or mangled\n"+
					"challenge: %s", got, d.MetadataURL, d.Challenge)
			}
		})
	}
}

// unquoteAuthParam is a minimal RFC 9110 §5.6.4 quoted-string reader, standing
// in for the client-side parser: DQUOTE-delimited, quoted-pair means "the octet
// after the backslash, verbatim", and an unescaped DQUOTE ends the value.
func unquoteAuthParam(s string) (string, error) {
	if len(s) < 2 || s[0] != '"' {
		return "", fmt.Errorf("not a quoted-string: %q", s)
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
			if i >= len(s) {
				return "", fmt.Errorf("trailing backslash in %q", s)
			}
			b.WriteByte(s[i])
		case '"':
			if i != len(s)-1 {
				return "", fmt.Errorf("quoted-string ends at offset %d but %d octets follow: %q",
					i, len(s)-1-i, s)
			}
			return b.String(), nil
		default:
			b.WriteByte(s[i])
		}
	}
	return "", fmt.Errorf("unterminated quoted-string: %q", s)
}

// TestWellKnownMCPPath_MatchesTheHardcodedCopies — sk-304 review F8. The
// path-aware well-known URI is COMPUTED here but HARD-CODED in two other
// repositories that cannot import this package:
//
//	skein-web       src/Middleware/McpProxyMiddleware.cs  ResourceMetadataMcpPath
//	skein-frontend  nginx.conf                            location = …
//
// If MCPPath or the well-known suffix ever changes here, skein-api starts
// advertising a URI that neither the BFF nor the SPA's nginx forwards, and
// discovery dies silently on the public route while every test in THIS repo
// stays green — the sk-304 failure shape exactly. This test is the tripwire:
// it fails on such a change and names the two files that must change with it.
func TestWellKnownMCPPath_MatchesTheHardcodedCopies(t *testing.T) {
	const hardcodedInSkeinWebAndNginx = "/.well-known/oauth-protected-resource/mcp"

	if WellKnownMCPPath != hardcodedInSkeinWebAndNginx {
		t.Fatalf("WellKnownMCPPath = %q, but skein-web "+
			"(src/Middleware/McpProxyMiddleware.cs, ResourceMetadataMcpPath) and "+
			"skein-frontend (nginx.conf, location = …) hard-code %q. "+
			"Change all three together or discovery breaks on the public route only",
			WellKnownMCPPath, hardcodedInSkeinWebAndNginx)
	}

	d, err := Describe(testBase, []string{testAS})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if d.WellKnownPath != hardcodedInSkeinWebAndNginx {
		t.Fatalf("a pathless deployment computes well-known path %q, but the entrypoints "+
			"forward %q", d.WellKnownPath, hardcodedInSkeinWebAndNginx)
	}
}

// TestDescribe_RejectsUserinfo — sk-304 review F3, unit level. See
// config.TestLoad_PublicURLRejectsUserinfo for why silent stripping is not an
// option and RFC 9110 §4.2.4 for why the component may not be emitted at all.
func TestDescribe_RejectsUserinfo(t *testing.T) {
	t.Run("public base URL", func(t *testing.T) {
		if _, err := Describe("https://user:pass@mcp.example.com", []string{testAS}); err == nil {
			t.Fatal("Describe accepted a public base URL carrying userinfo; the origin would " +
				"be silently rebuilt without it, publishing a string the operator never wrote")
		}
	})
	t.Run("authorization server", func(t *testing.T) {
		_, err := Describe(testBase, []string{"https://admin:hunter2@auth.example.com/realms/main"})
		if err == nil {
			t.Fatal("Describe accepted an issuer carrying userinfo; that entry is published " +
				"VERBATIM in a document served without authentication")
		}
	})
}

// TestEnabled — sk-304 review F1. The switch that decides whether discovery is
// wired at all. A blank or whitespace-only value carries no issuer and must
// read as "off", not as "malformed": failing a pod over a stray comma
// communicates nothing that an empty value does not.
func TestEnabled(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want bool
	}{
		{nil, false},
		{[]string{}, false},
		{[]string{""}, false},
		{[]string{" ", " "}, false},
		{[]string{testAS}, true},
		{[]string{"", testAS}, true},
	} {
		if got := Enabled(tc.in); got != tc.want {
			t.Fatalf("Enabled(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
