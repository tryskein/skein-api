package main

// public_commit_schema_test.go — sk-977.7.69: the url, message and author
// limits of a commit ref (2048, 1000, 256 characters) apply after
// surrounding whitespace is trimmed, so a value at the limit plus a trailing
// space is one the server accepts. The served document must declare these
// fields of the closeIssue body, and a client that validates its request by
// the document must accept such a value: a whole closeIssue body is
// validated against the document's body schema by a JSON Schema 2020-12
// validator (the dialect of OpenAPI 3.1), with the document's component
// references resolved, so constraints on the enclosing objects and arrays
// take part.

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"skein-api/internal/publicapi"
)

func TestPublicCloseBodyAcceptsPaddedCommitFields(t *testing.T) {
	// Port "0": the server is never started, the document is served in
	// memory through its handler.
	srv, err := publicServer(publicConfig(true, "0", ""), nil, publicapi.Deps{}, slog.New(&recordingHandler{}))
	if err != nil || srv == nil {
		t.Fatalf("publicServer = %v, %v; want a server", srv, err)
	}
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, publicapi.OpenAPIPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", publicapi.OpenAPIPath, rec.Code, rec.Body.String())
	}
	raw := rec.Body.Bytes()

	doc := servedDocument(t, srv.Handler)
	op := documentOperation(doc, http.MethodPost, "/api/v1/issues/{issue}/close")
	if op == nil || op.RequestBody == nil {
		t.Fatal("closeIssue with a body is not in the document")
	}
	body := op.RequestBody.Value.Content.Get("application/json")
	if body == nil || body.Schema == nil || body.Schema.Ref == "" {
		t.Fatal("closeIssue: the JSON body schema is not a component reference")
	}
	commits := body.Schema.Value.Properties["commits"]
	if commits == nil || commits.Value.Items == nil || commits.Value.Items.Value == nil {
		t.Fatal("closeIssue: commits with an item schema is not declared")
	}
	for _, field := range []string{"url", "message", "author"} {
		if commits.Value.Items.Value.Properties[field] == nil {
			t.Errorf("closeIssue: commits[].%s is not declared", field)
		}
	}

	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the served document is not JSON: %v", err)
	}
	const docURL = "https://example.invalid/openapi.json"
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource(docURL, parsed); err != nil {
		t.Fatalf("add the document: %v", err)
	}
	schema, err := c.Compile(docURL + body.Schema.Ref)
	if err != nil {
		t.Fatalf("compile %s: %v", body.Schema.Ref, err)
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
	if err := schema.Validate(request); err != nil {
		t.Errorf("a closeIssue body with commit fields at the limit plus a trailing space is refused by the document: %v", err)
	}
}
