package publicapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"skein-api/internal/publicapi"
)

// getDocument requests the document from the product router built with the
// external address serverURL, without any key, and decodes it.
func getDocument(t *testing.T, serverURL string) map[string]json.RawMessage {
	t.Helper()
	doc, err := publicapi.NewOpenAPIDocument(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	h := publicapi.NewRouter(publicapi.RouterOptions{OperationTimeout: 30 * time.Second}, publicapi.ProductRoutes(publicapi.Deps{Doc: doc}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/openapi.json without a key = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("document is not a JSON object: %v", err)
	}
	var version string
	if err := json.Unmarshal(body["openapi"], &version); err != nil || version != "3.1.0" {
		t.Errorf("openapi %s, want \"3.1.0\"", body["openapi"])
	}
	return body
}

// TestOpenAPIDocument_Servers — row 55 (US-9, Changelog US-10). GET
// /api/v1/openapi.json without a key serves the embedded OpenAPI 3.1
// document; with the external address set, servers[0].url is that address;
// without it the document has no servers member — never a server with an
// empty URL.
func TestOpenAPIDocument_Servers(t *testing.T) {
	t.Run("address set", func(t *testing.T) {
		const url = "https://api.skein.example/"
		body := getDocument(t, url)
		raw, ok := body["servers"]
		if !ok {
			t.Fatal("no servers member with the address set")
		}
		var servers []struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(raw, &servers); err != nil {
			t.Fatalf("servers is not a list of objects: %v (%s)", err, raw)
		}
		if len(servers) == 0 || servers[0].URL != url {
			t.Errorf("servers %s, want servers[0].url %q", raw, url)
		}
	})

	t.Run("address unset", func(t *testing.T) {
		body := getDocument(t, "")
		if raw, ok := body["servers"]; ok {
			t.Errorf("servers %s present without the address, want the member absent", raw)
		}
	})
}
