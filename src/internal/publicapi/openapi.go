package publicapi

// openapi.go — GET /api/v1/openapi.json: the OpenAPI document of the public
// API, embedded in the binary (plan D-12).

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
)

// OpenAPIPath is the path of the document of the public API.
const OpenAPIPath = PathPrefix + "openapi.json"

// OperationGetOpenAPI is the operationId of the document's route.
const OperationGetOpenAPI = "getOpenApi"

// openapiJSON is the committed document generated from the annotations of
// this package (scripts/generate-public-openapi.sh).
//
//go:embed openapi/openapi.json
var openapiJSON []byte

// OpenAPIDocument serves the embedded document. The body is built once, at
// construction.
type OpenAPIDocument struct {
	body []byte
}

// NewOpenAPIDocument builds the served document from the embedded one. A
// non-empty serverURL (SKEIN_PUBLIC_API_URL) becomes servers[0].url; with an
// empty one the document carries no servers member at all, never a server
// with an empty URL.
func NewOpenAPIDocument(serverURL string) (*OpenAPIDocument, error) {
	return newOpenAPIDocument(openapiJSON, serverURL)
}

func newOpenAPIDocument(embedded []byte, serverURL string) (*OpenAPIDocument, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(embedded, &doc); err != nil {
		return nil, fmt.Errorf("parsing the embedded OpenAPI document: %w", err)
	}
	delete(doc, "servers")
	if serverURL != "" {
		servers, err := json.Marshal([]map[string]string{{"url": serverURL}})
		if err != nil {
			return nil, fmt.Errorf("encoding the servers of the OpenAPI document: %w", err)
		}
		doc["servers"] = servers
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encoding the OpenAPI document: %w", err)
	}
	return &OpenAPIDocument{body: body}, nil
}

// ServeHTTP answers the document.
//
// @Summary Get the OpenAPI document of the public API
// @Description Returns the OpenAPI 3.1 document of this instance's public API: every operation, the right of an API key each one requires and the security scheme of the key.
// @Description
// @Description The document is embedded in the server and is the same on every replica of a release. When the instance sets SKEIN_PUBLIC_API_URL, servers[0].url is that address; otherwise the document has no servers member.
// @Description
// @Description No API key is needed; the request has no side effects.
// @Tags Meta
// @Produce json
// @Success 200 {object} object "The OpenAPI 3.1 document"
// @Router /api/v1/openapi.json [get]
// @ID getOpenApi
func (d *OpenAPIDocument) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// A failed write means the client went away; there is nobody left to
	// answer and nothing to undo.
	_, _ = w.Write(d.body)
}
