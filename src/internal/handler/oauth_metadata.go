package handler

import (
	"log/slog"
	"net/http"

	"skein-api/internal/oauthmeta"
)

// ProtectedResourceMetadata serves the OAuth 2.0 Protected Resource Metadata
// document (RFC 9728) for the MCP door.
//
// The SAME handler answers on both registered paths (sk-304):
//
//	GET /.well-known/oauth-protected-resource        (root, RFC 9728 §3)
//	GET /.well-known/oauth-protected-resource/mcp    (path-aware, RFC 9728 §3.1)
//
// and both return a document whose `resource` is the PATH-BEARING identifier
// "https://<public-host>/mcp".
//
// The path-aware document is simply correct: RFC 9728 §3.1 inserts the
// well-known suffix between host and path, and §3.3 requires the returned
// `resource` to be identical to the identifier that URL was built from.
//
// The root document is a deliberate, documented deviation from §3.3, which
// would have it name the ORIGIN. The reasoning:
//
//   - The origin is not a protected resource here. On the MCP host everything
//     outside /mcp is the cookie-authenticated BFF, which has no OAuth
//     identity to describe. So a strictly-correct root document describes
//     nothing that exists.
//   - The only client that ever reads the root document is one that arrived
//     at /mcp and either could not read the WWW-Authenticate pointer or does
//     not implement path-aware discovery. Handing THAT client the origin is
//     precisely the sk-304 failure: it compares byte-for-byte, sees
//     "https://host" where it expected "https://host/mcp", concludes the
//     document belongs to someone else's resource, and escalates to
//     interactive authorization that a headless agent cannot perform.
//   - Another MCP-resource deployment we run serves the same /mcp-bearing
//     document on both URLs; matching it keeps client behaviour uniform
//     across the fleet.
//
// Honesty about the trade-off: a client that genuinely wanted to describe the
// ORIGIN as a resource would be misinformed by our root document. Nothing
// treats the origin as an OAuth resource today, and the path-aware document
// plus the 401 challenge mean a conformant client never consults the root one.
// If the origin ever becomes a protected resource in its own right, the root
// document must be split off then.
//
// Caching: `no-store`. RFC 9728 §7.10 recommends Cache-Control with a max-age,
// and that is the right steady state — but this is the FIRST release of a
// document that is simultaneously being retired from the cluster gateway,
// where a hard-coded copy currently answers on the root URL. While two
// publishers are being collapsed into one, a cached copy of the wrong one is
// the exact failure that let sk-280 be closed on a green probe while the
// defect was live. The document is a few hundred bytes fetched only on a 401,
// so the cost of not caching is negligible; a deliberate max-age is a
// follow-up once the gateway workaround is gone and the document has been
// observed stable.
//
// @Summary      OAuth 2.0 Protected Resource Metadata for the MCP endpoint
// @Description  Returns the RFC 9728 protected-resource metadata document
// @Description  describing the MCP Streamable-HTTP door. `resource` is the
// @Description  exact canonical identifier of that door, INCLUDING its /mcp
// @Description  path — MCP clients compare it byte-for-byte against the URL
// @Description  they were pointed at (RFC 9728 §6, no normalisation), so a
// @Description  trailing slash or a different case makes them reject the
// @Description  document and fall back to interactive authorization.
// @Description
// @Description  Registered on TWO paths that return the same body: the
// @Description  path-aware URI (RFC 9728 §3.1, the one named by the
// @Description  WWW-Authenticate challenge on a 401 from /mcp) and the root
// @Description  URI (§3, kept for clients that do not implement path-aware
// @Description  discovery).
// @Description
// @Description  Unauthenticated by design — a client reads it precisely
// @Description  because it does not yet know how to authenticate. The
// @Description  contents are admin-configured (SKEIN_AGENT_PUBLIC_URL,
// @Description  SKEIN_OAUTH_AUTHORIZATION_SERVERS) and never derived from the
// @Description  request Host or X-Forwarded-* headers, so the advertised
// @Description  identity cannot be spoofed by an edge that forwards a hostile
// @Description  Host.
// @Description
// @Description  Side effects: none. Response is a fixed byte string computed
// @Description  at startup and served with Cache-Control: no-store.
// @Tags         oauth-metadata
// @Produce      json
// @Success      200  {object}  oauthmeta.Document  "Protected resource metadata"
//
// Both registered paths are documented (sk-304 review F6). swag maps one
// operation onto several paths when several @Router lines are given, which is
// what this is: one handler, one body, two URIs. The path-aware one is the URI
// every WWW-Authenticate challenge points at, so a spec listing only the root
// URI would omit the endpoint clients actually fetch — an undocumented
// endpoint is a non-existent endpoint to a consumer of the spec.
//
// The literal "/mcp" suffix is the pathless deployment's URI. A deployment
// whose SKEIN_AGENT_PUBLIC_URL carries a base path serves the suffix after
// that path instead (RFC 9728 §3.1); see oauthmeta.Descriptor.WellKnownPath.
// @Router       /.well-known/oauth-protected-resource [get]
// @Router       /.well-known/oauth-protected-resource/mcp [get]
func ProtectedResourceMetadata(d *oauthmeta.Descriptor) http.HandlerFunc {
	if d == nil {
		panic("ProtectedResourceMetadata: descriptor is nil")
	}
	body := d.Body
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(body); err != nil {
			slog.Warn("write protected resource metadata failed", "error", err)
		}
	}
}
