// Package oauthmeta builds the OAuth 2.0 Protected Resource Metadata
// document (RFC 9728) that MCP clients read before they authenticate, plus
// the `WWW-Authenticate` challenge that points them at it (RFC 9728 §5.1,
// MCP authorization spec "Authorization Server Location").
//
// Why this exists at all (sk-304): an MCP client that is handed a static
// credential still probes the door UNAUTHENTICATED first, reads the metadata
// document named by the 401, and compares the document's `resource` field
// with the URL it was pointed at — BYTE FOR BYTE, no normalisation
// (RFC 9728 §6). Until this package existed, the only document answering on
// the public host was a hard-coded string served by the cluster gateway that
// named the ORIGIN (`https://mcp.example.com`) while the client had asked
// about `https://mcp.example.com/mcp`. The mismatch made the client
// discard the document as belonging to somebody else's resource and fall into
// interactive authorization — which a headless agent does not have. The key it
// already held was never tried.
//
// The document is therefore built ONLY from admin-supplied configuration
// (SKEIN_AGENT_PUBLIC_URL). It is never derived from the request `Host` or any
// `X-Forwarded-*` header: those are attacker-controllable at the edge whenever
// no trusted-proxy allow-list is configured — the supported no-proxy mode — and
// a forgeable
// `resource` value is exactly the attack RFC 9728 §6's exact-match rule
// exists to stop — an attacker could otherwise hang a document naming a
// reputable resource next to their own door and harvest tokens minted for it.
package oauthmeta

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// WellKnownRootPath is the root well-known URI defined by RFC 9728 §3, used
// when the resource identifier has no path component. We still SERVE it (see
// Descriptor.Body) because clients that predate path-aware discovery, and the
// gateway workaround this replaces, both look here.
const WellKnownRootPath = "/.well-known/oauth-protected-resource"

// MCPPath is the path of the MCP Streamable-HTTP door, relative to the public
// base URL. It must stay in sync with the r.Route("/mcp", …) registration in
// cmd/api/main.go: this constant decides what the metadata document CLAIMS the
// resource identifier is, and a claim that does not match the real door is the
// very bug this package fixes.
const MCPPath = "/mcp"

// WellKnownMCPPath is the path-aware well-known URI (RFC 9728 §3.1) of a
// deployment whose public base URL carries NO base path — i.e. every real
// deployment today.
//
// It exists as a named constant because this exact string is DUPLICATED in two
// other repositories, which cannot import it (sk-304 review F8):
//
//   - skein-web  — src/Middleware/McpProxyMiddleware.cs, const
//     ResourceMetadataMcpPath: the BFF terminates and forwards exactly this
//     path, deliberately as an exact match rather than a /.well-known prefix.
//   - skein-frontend — nginx.conf, `location = /.well-known/oauth-protected-resource/mcp`,
//     for stacks where the SPA image is the public entrypoint.
//
// skein-api COMPUTES its copy (origin + base path + MCPPath), the other two
// hard-code theirs. On a deployment with a base path the three diverge and
// discovery fails silently — which is how sk-304 presented in the first place.
// TestWellKnownMCPPath_MatchesTheHardcodedCopies pins this constant against the
// computed value so that changing MCPPath or the well-known suffix here turns
// red and names the two files that must change with it; the base-path case is
// surfaced at startup by a warning from buildRouter, since no test in this repo
// can reach an operator's gateway configuration.
const WellKnownMCPPath = WellKnownRootPath + MCPPath

// Enabled reports whether OAuth discovery is configured at all, i.e. whether
// SKEIN_OAUTH_AUTHORIZATION_SERVERS names at least one non-blank issuer.
//
// Why this exists (sk-304 review, F1). The first cut made the variable
// REQUIRED: Describe rejected an empty list and config.Load turned that into a
// startup failure, so a pod without the variable did not boot AT ALL. The
// project rule "env vars without fallbacks — no variable, fail fast" exists to
// stop SILENTLY WRONG DEFAULTS, and there is genuinely no issuer URL we could
// invent. But the cost was wildly out of proportion: the live production
// ConfigMap does not carry the variable, so the next rollout of this image —
// including one shipping somebody else's unrelated fix — would have taken the
// WHOLE API down, not merely OAuth discovery, in exchange for a feature that
// is optional to every client holding a static api key.
//
// So the behaviour is degraded, not fatal: unset means the two metadata routes
// are not registered and the 401 carries no challenge, the service serves
// /mcp with a static key exactly as before, and startup logs a warning naming
// the variable so a silently-off feature stays visible. A value that is SET
// but MALFORMED is still fatal (see Describe) — that is the silently-wrong
// case the rule was written for, and there the operator has the value in hand.
//
// A list whose entries are all blank ("", " , ") counts as unset: it carries
// no issuer, so treating it as a malformed value would fail a pod for a stray
// comma while communicating nothing an empty value does not.
func Enabled(authorizationServers []string) bool {
	for _, raw := range authorizationServers {
		if strings.TrimSpace(raw) != "" {
			return true
		}
	}
	return false
}

// Document is the RFC 9728 §2 protected-resource metadata document.
//
// Field selection, and what we deliberately leave out:
//
//   - `resource` (REQUIRED, §2) — the resource identifier, exact string.
//   - `authorization_servers` (OPTIONAL in RFC 9728, but the MCP
//     authorization spec makes it a MUST: "The Protected Resource Metadata
//     document returned by the MCP server MUST include the
//     authorization_servers field containing at least one authorization
//     server"). Hence the config value behind it is required, not optional.
//   - `bearer_methods_supported` is NOT emitted (sk-304 review). RFC 9728 §2
//     lists it as OPTIONAL — unlike `resource`, which is REQUIRED — so
//     omitting it costs nothing, and emitting `["header"]` would have been a
//     lie: it declares "hand me the token in the standard `Authorization`
//     request header" (RFC 6750 §2.1), and today NOTHING here reads that
//     header. The MCP door takes `X-Skein-Api-Key`; the BFF in front of it
//     strips `Authorization` outright (McpProxyMiddleware), so the header
//     cannot even reach this process. A client that read the document,
//     obtained a token and presented it as advertised would get another 401
//     and could loop: 401 → discovery → token → 401. That is the same
//     "do not promise a contract we do not honour" rule already applied to
//     `scopes_supported` below. It is added back the day the
//     `Authorization` header is actually accepted (sk-304 stage 2), together
//     with the code that accepts it — never before.
//   - `scopes_supported` is NOT emitted. RFC 9728 lists it as RECOMMENDED and
//     another MCP-resource deployment we run advertises `["mcp:tools"]`, but
//     Skein does not mint, parse or enforce a single OAuth scope today (R5.1
//     is deferred; the live credential is the `ska_` api key on
//     X-Skein-Api-Key). Advertising a scope we never check would be a
//     fabricated contract: a client would request it, the AS would issue it,
//     and nothing here would honour it. It is added the day scope enforcement
//     exists, not before.
type Document struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
}

// Descriptor is everything the HTTP layer needs, computed once at startup
// from validated configuration.
type Descriptor struct {
	// ResourceID is the canonical resource identifier of the MCP door,
	// e.g. "https://mcp.example.com/mcp". This exact string is what a
	// client compares against, so it carries no trailing slash and no
	// case folding — see Describe.
	ResourceID string

	// WellKnownPath is the path-aware well-known URI path built per
	// RFC 9728 §3.1: the well-known suffix is inserted BETWEEN the host and
	// the resource path, giving
	// "/.well-known/oauth-protected-resource/mcp" — NOT "/mcp/.well-known/…".
	// A deployment base path in the public URL trails the suffix too
	// ("/.well-known/oauth-protected-resource/gw/mcp"), which is why this is a
	// computed field and not a constant.
	WellKnownPath string

	// MetadataURL is the absolute URL of WellKnownPath on the public host.
	// It is the value advertised in the WWW-Authenticate challenge.
	MetadataURL string

	// Document is the parsed form (handy for tests and for callers that want
	// a field rather than bytes).
	Document Document

	// Body is the serialised document, marshalled once at startup: the
	// response is a fixed byte string, so re-encoding it per request would
	// be pure waste on a path that a fleet of agents hits on every 401.
	Body []byte

	// Challenge is the full WWW-Authenticate header value for a 401 on the
	// MCP door, per RFC 9728 §5.1:
	//
	//	Bearer resource_metadata="https://host/.well-known/oauth-protected-resource/mcp"
	//
	// It names the PATH-AWARE document deliberately. The MCP authorization
	// spec makes the header the primary discovery route and the well-known
	// probe the fallback, so an explicit pointer removes the client's guess
	// between the root and the path-aware document — the guess that produced
	// sk-304.
	Challenge string
}

// Describe validates the inputs and builds the descriptor. It is called from
// config.Load (so a misconfigured pod refuses to start rather than serving a
// document that lies about where the door is) and again from the router
// composition root, which is why it must be deterministic and side-effect
// free.
//
// publicBaseURL is the operator-supplied public base URL of the MCP door
// (SKEIN_AGENT_PUBLIC_URL). authorizationServers are the issuer identifiers of
// the OAuth 2.0 authorization servers that mint tokens for this resource
// (SKEIN_OAUTH_AUTHORIZATION_SERVERS).
func Describe(publicBaseURL string, authorizationServers []string) (*Descriptor, error) {
	origin, basePath, err := parsePublicBase(publicBaseURL)
	if err != nil {
		return nil, err
	}

	if len(authorizationServers) == 0 {
		return nil, fmt.Errorf("authorization servers: at least one issuer identifier is required " +
			"(the MCP authorization spec requires authorization_servers to be non-empty)")
	}
	servers := make([]string, 0, len(authorizationServers))
	for i, raw := range authorizationServers {
		s := strings.TrimSpace(raw)
		u, err := url.Parse(s)
		if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			// u.User != nil rejects a userinfo component (sk-304 review, F3).
			// This entry is published VERBATIM — the trimmed input string is
			// what lands in the document, not a re-serialised URL — so
			// "https://user:pass@iden.example/realms/x" would put an operator's
			// credentials into a document we hand to anyone who asks, on a
			// route that is unauthenticated BY DESIGN. RFC 9110 §4.2.4 is
			// explicit that a sender MUST NOT generate a userinfo subcomponent
			// when an http/https URI is emitted as a field value, and RFC 3986
			// §3.2.1 deprecates "user:password" outright. Rejecting at startup
			// beats silently stripping: an issuer identifier is compared
			// byte-for-byte by clients, so quietly publishing a different
			// string than the operator configured would be its own defect.
			return nil, fmt.Errorf("authorization servers[%d]: must be an absolute http(s) issuer "+
				"identifier without query, fragment or userinfo; got %q", i, raw)
		}
		servers = append(servers, s)
	}

	// RFC 9728 §3.1: the well-known suffix is inserted BETWEEN the host and
	// the resource's path, so the whole path of the identifier — including
	// any deployment base path — trails the suffix.
	resourceID := origin + basePath + MCPPath
	wellKnownPath := WellKnownRootPath + basePath + MCPPath

	doc := Document{
		Resource:             resourceID,
		AuthorizationServers: servers,
	}
	body, err := json.Marshal(doc)
	if err != nil {
		// Unreachable for a struct of strings, but an ignored error here
		// would ship an empty body on the one endpoint whose whole job is
		// to be byte-exact.
		return nil, fmt.Errorf("marshalling protected resource metadata: %w", err)
	}

	metadataURL := origin + wellKnownPath

	return &Descriptor{
		ResourceID:    resourceID,
		WellKnownPath: wellKnownPath,
		MetadataURL:   metadataURL,
		Document:      doc,
		Body:          body,
		Challenge:     `Bearer resource_metadata=` + quoteAuthParam(metadataURL),
	}, nil
}

// parsePublicBase validates the public base URL and splits it into the origin
// ("scheme://host") and a normalised base path ("" or "/segment[/segment…]",
// never with a trailing slash).
//
// Trailing-slash policy (the "predictable policy" sk-304 asks for): every
// trailing slash is stripped, from the origin and from the base path alike, so
// "https://host", "https://host/" and "https://host/gw/" all yield identifiers
// of the form "https://host[/gw]/mcp" — never "https://host//mcp". This
// follows the MCP authorization spec's note that implementations SHOULD use
// the form without the trailing slash, and it matters because the comparison
// downstream is byte-for-byte: one stray slash and every client rejects the
// document.
//
// A base path is ACCEPTED, not folded away: SKEIN_AGENT_PUBLIC_URL has allowed
// one since sk-142.7 (the monitor runner appends /agent/v1/… to it), and
// silently changing that contract would break a deployment behind a
// path-prefixed gateway on its next restart. RFC 9728 §3.1 handles the case
// exactly: the well-known suffix goes between host and path, so the document
// for "https://host/gw/mcp" lives at
// "/.well-known/oauth-protected-resource/gw/mcp". Aligning the gateway so that
// path reaches this process is the operator's job — the same job the /mcp
// route itself already imposes.
//
// A query or fragment is rejected outright: the MCP authorization spec lists
// "https://mcp.example.com#fragment" among its invalid canonical URIs, and
// neither has any meaning in a base URL.
func parsePublicBase(raw string) (origin, basePath string, err error) {
	s := strings.TrimSpace(raw)
	u, parseErr := url.Parse(s)
	if parseErr != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", fmt.Errorf("public base URL: must be an absolute http(s) URL with a host; got %q", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("public base URL: must not carry a query or fragment "+
			"(a resource identifier with a fragment is invalid per the MCP authorization spec); got %q", raw)
	}
	// Userinfo is rejected rather than dropped (sk-304 review, F3). The origin
	// below is rebuilt from u.Scheme + u.Host, and url.Parse keeps userinfo in
	// the separate u.User field — so "https://user:pass@host" would have PASSED
	// validation and silently published "https://host/mcp", an identifier that
	// is not the string the operator wrote. The same value also feeds the agent
	// public URL used elsewhere, where the credentials would NOT be dropped.
	// RFC 9110 §4.2.4: a sender MUST NOT generate the userinfo subcomponent
	// when an http/https URI reference is emitted as a target URI or field
	// value — and this URL becomes both (the `resource` field and the
	// WWW-Authenticate metadata URL). Silent normalisation is the wrong answer
	// on an endpoint whose entire contract is byte-for-byte equality.
	if u.User != nil {
		return "", "", fmt.Errorf("public base URL: must not carry a userinfo component "+
			"(RFC 9110 §4.2.4 forbids emitting one in an http(s) URI); got %q", raw)
	}
	// EscapedPath keeps any percent-encoding the operator wrote: the
	// identifier we publish must be the string the client sees, not a decoded
	// variant of it.
	basePath = strings.TrimRight(u.EscapedPath(), "/")
	if basePath != "" && !strings.HasPrefix(basePath, "/") {
		// url.Parse only produces this for opaque URLs, which IsAbs+Host
		// already excluded; guard anyway rather than emit "https://hostgw/mcp".
		return "", "", fmt.Errorf("public base URL: unusable path component; got %q", raw)
	}
	return u.Scheme + "://" + u.Host, basePath, nil
}

// quoteAuthParam renders v as an RFC 9110 §11.2 auth-param value in
// quoted-string form, escaping per §5.6.4:
//
//	quoted-string = DQUOTE *( qdtext / quoted-pair ) DQUOTE
//	quoted-pair   = "\" ( HTAB / SP / VCHAR / obs-text )
//
// qdtext excludes DQUOTE and backslash, so those two octets — and only those
// two — must be escaped ("A sender SHOULD NOT generate a quoted-pair in a
// quoted-string except where necessary to quote DQUOTE and backslash octets
// occurring within that string").
//
// Why this is not dead code (sk-304 review, F4). The header used to be built
// by plain concatenation, and url.Parse does NOT reject a DQUOTE in the host:
// url.Parse("https://ho\"st/") succeeds with Host == `ho"st` (verified). A
// configured URL carrying one therefore produced
// `Bearer resource_metadata="https://ho"st/.well-known/…"`, which every
// conformant parser reads as a metadata URL truncated at the stray quote — a
// discovery pointer silently aimed at the wrong place, the exact class of
// failure sk-304 is about. There is no newline injection to worry about
// (url.Parse rejects control characters outright), but "no injection" is not
// the same as "well-formed".
//
// Escaping is preferred over rejecting such a URL at startup because it is
// correct BY CONSTRUCTION for every possible input: the alternative would mean
// hand-rolling a charset check over the host component and keeping it in step
// with whatever net/url accepts in the next Go release — precisely the sort of
// rule that rots silently. F3's userinfo rejection narrows the input further;
// this makes the output well-formed regardless.
func quoteAuthParam(v string) string {
	var b strings.Builder
	b.Grow(len(v) + 2)
	b.WriteByte('"')
	for i := 0; i < len(v); i++ {
		if c := v[i]; c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(v[i])
	}
	b.WriteByte('"')
	return b.String()
}
