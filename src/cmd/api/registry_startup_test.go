package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"skein-api/internal/monitorregistry"
)

// TestBuildServingRouter_BrokenRegistryStopsStartup — П-1, Р-1: the startup
// step that main calls before any listener opens refuses a broken registry
// document: it returns the registry's validation error and never builds the
// router, so main has nothing to serve and exits.
func TestBuildServingRouter_BrokenRegistryStopsStartup(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		rule monitorregistry.Rule
	}{
		{"not a JSON document", `{"schemaVersion": 1,`, monitorregistry.RuleMalformed},
		{"general record without a source", `{"schemaVersion": 1,
  "general": {"about": "general", "source": ""},
  "agents": [], "channels": [],
  "shells": {"source": "s", "names": ["sh"]}, "zavodEngines": {}}`, monitorregistry.RuleMissingSource},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			built := false
			router, drain, err := buildServingRouter([]byte(tc.doc), func(*monitorregistry.Registry, []byte) (chi.Router, func()) {
				built = true
				return chi.NewRouter(), func() {}
			})
			if err == nil {
				t.Fatal("startup accepted a broken registry")
			}
			var verrs monitorregistry.ValidationErrors
			if !errors.As(err, &verrs) || !verrs.Has(tc.rule) {
				t.Fatalf("error %v does not carry rule %s", err, tc.rule)
			}
			if built || router != nil || drain != nil {
				t.Fatalf("router built on a broken registry (built=%t router=%v)", built, router)
			}
		})
	}
}

// TestBuildServingRouter_EmbeddedRegistryBuildsRouter — control of the test
// above: with the embedded registry.json, the document main passes, startup
// reaches a built router that serves, and the registry it hands to the
// router builder is the loaded one.
func TestBuildServingRouter_EmbeddedRegistryBuildsRouter(t *testing.T) {
	var got *monitorregistry.Registry
	router, drain, err := buildServingRouter(monitorregistry.Embedded(), func(reg *monitorregistry.Registry, _ []byte) (chi.Router, func()) {
		got = reg
		r := chi.NewRouter()
		r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		return r, func() {}
	})
	if err != nil {
		t.Fatalf("startup refused the embedded registry: %v", err)
	}
	if got == nil || router == nil || drain == nil {
		t.Fatalf("startup did not reach a built router (registry=%v router=%v)", got, router)
	}
	if res := got.Resolve("no-such-agent"); res.Known() || res.General == nil {
		t.Fatalf("unknown client did not resolve to the general record: %+v", res)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("built router answered %d, want %d", rec.Code, http.StatusNoContent)
	}
}
