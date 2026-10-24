package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"skein-api/internal/oauthmeta"
)

func mustDescribe(t *testing.T) *oauthmeta.Descriptor {
	t.Helper()
	d, err := oauthmeta.Describe("https://mcp.example.com", []string{"https://auth.example.com/realms/main"})
	if err != nil {
		t.Fatalf("oauthmeta.Describe: %v", err)
	}
	return d
}

// TestProtectedResourceMetadata_Response pins the wire contract a client
// reads: 200, JSON content type, no-store, and a `resource` that carries the
// /mcp path byte-for-byte (sk-304).
func TestProtectedResourceMetadata_Response(t *testing.T) {
	h := ProtectedResourceMetadata(mustDescribe(t))

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}

	var doc oauthmeta.Document
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("body is not the metadata document: %v (%s)", err, rec.Body.String())
	}
	if want := "https://mcp.example.com/mcp"; doc.Resource != want {
		t.Fatalf("resource = %q, want %q", doc.Resource, want)
	}
	if len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != "https://auth.example.com/realms/main" {
		t.Fatalf("authorization_servers = %v", doc.AuthorizationServers)
	}
}

// TestProtectedResourceMetadata_SameBodyOnBothPaths — the root and the
// path-aware URI intentionally return the identical document, both naming the
// /mcp identifier. See the handler doc comment for why the root one deviates
// from RFC 9728 §3.3 on purpose.
func TestProtectedResourceMetadata_SameBodyOnBothPaths(t *testing.T) {
	h := ProtectedResourceMetadata(mustDescribe(t))

	bodies := make([]string, 0, 2)
	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
	} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, rec.Code)
		}
		bodies = append(bodies, rec.Body.String())
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("root and path-aware documents differ:\nroot: %s\nmcp:  %s", bodies[0], bodies[1])
	}
}
