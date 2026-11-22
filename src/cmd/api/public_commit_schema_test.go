package main

// public_commit_schema_test.go — sk-977.7.69: the url, message and author
// limits of a commit ref (2048, 1000, 256 characters) apply after
// surrounding whitespace is trimmed, so a value at the limit plus a trailing
// space is one the server accepts. A client that validates its request by
// the served document must accept it too: a whole closeIssue body carrying
// such a commit is validated against the document's body schema with the
// JSON Schema 2020-12 validator of OpenAPI 3.1, so constraints on the
// enclosing objects and arrays take part.

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"skein-api/internal/publicapi"
)

func TestPublicCloseBodyAcceptsPaddedCommitFields(t *testing.T) {
	srv, err := publicServer(publicConfig(true, freePort(t), ""), nil, publicapi.Deps{}, slog.New(&recordingHandler{}))
	if err != nil || srv == nil {
		t.Fatalf("publicServer = %v, %v; want a server", srv, err)
	}
	doc := servedDocument(t, srv.Handler)
	op := documentOperation(doc, http.MethodPost, "/api/v1/issues/{issue}/close")
	if op == nil || op.RequestBody == nil {
		t.Fatal("closeIssue with a body is not in the document")
	}
	body := op.RequestBody.Value.Content.Get("application/json")
	if body == nil || body.Schema == nil || body.Schema.Value == nil {
		t.Fatal("closeIssue: no JSON body schema")
	}
	request := map[string]any{
		"reason":       "done",
		"closeOutcome": "done",
		"kind":         "directed",
		"commits": []any{map[string]any{
			"hash":    "abcdef1",
			"url":     "https://example.com/" + strings.Repeat("a", 2048-len("https://example.com/")) + " ",
			"message": strings.Repeat("m", 1000) + " ",
			"author":  strings.Repeat("a", 256) + " ",
		}},
	}
	if err := body.Schema.Value.VisitJSON(request, openapi3.EnableJSONSchema2020()); err != nil {
		t.Errorf("a closeIssue body with commit fields at the limit plus a trailing space is refused by the document: %v", err)
	}
}
