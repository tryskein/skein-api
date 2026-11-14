package publicapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"skein-api/internal/authz"
	"skein-api/internal/publicapi"
)

// fixedVersions stands in for the database at the version provider seam:
// every resource is at version current. It counts the reads.
type fixedVersions struct {
	current int
	reads   int
}

func (v *fixedVersions) ResourceVersion(_ context.Context, _ publicapi.Route, _ *http.Request) (int, bool, error) {
	v.reads++
	return v.current, true, nil
}

// occRig is a router over three test routes, one per If-Match rule, whose
// handler records whether it ran and answers the ETag of the version If-Match
// named.
type occRig struct {
	h        http.Handler
	versions *fixedVersions
	called   bool
	expected int
}

func newOCCRig() *occRig {
	rig := &occRig{versions: &fixedVersions{current: 5}}
	op := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.called = true
		if v, ok := publicapi.ExpectedVersion(r.Context()); ok {
			rig.expected = v
			publicapi.SetETag(w, v)
		}
		w.WriteHeader(http.StatusOK)
	})
	route := func(id, path string, rule publicapi.IfMatch) publicapi.Route {
		return publicapi.Route{
			OperationID: id, Method: http.MethodPost, Path: path,
			Auth: publicapi.AuthKey, Key: authz.KeyRequirement{Right: authz.KeyRightWrite},
			IfMatch: rule, Handler: op,
		}
	}
	rig.h = publicapi.NewRouter(publicapi.RouterOptions{OperationTimeout: 30 * time.Second, Versions: rig.versions},
		[]publicapi.Route{
			route("testUpdateThing", "/api/v1/things/{thing}/update", publicapi.IfMatchRequired),
			route("testAcceptThing", "/api/v1/things/{thing}/accept", publicapi.IfMatchOptional),
			route("testUnclaimThing", "/api/v1/things/{thing}/unclaim", publicapi.IfMatchNone),
		})
	return rig
}

func (rig *occRig) do(path, ifMatch string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	rig.h.ServeHTTP(rec, req)
	return rec
}

// TestOCC_IfMatchByRouteRule — row 69 (US-6, SC-006, the mechanism). The
// resource is at version 5. On a route that requires If-Match: no header is
// 428 precondition_required and a stale "4" is 412 version_conflict with
// currentVersion 5, both before the operation runs; "5" runs the operation,
// which gets the expected version 5 and answers ETag "5". On a route where
// If-Match is optional (as acceptIssue) its absence runs the operation; on a
// route that does not check it (as unclaimIssue) a stale header has no
// effect and the version is not read.
func TestOCC_IfMatchByRouteRule(t *testing.T) {
	const (
		required = "/api/v1/things/t1/update"
		optional = "/api/v1/things/t1/accept"
		none     = "/api/v1/things/t1/unclaim"
	)

	t.Run("required, absent: 428 before the operation", func(t *testing.T) {
		rig := newOCCRig()
		rec := rig.do(required, "")
		if rec.Code != http.StatusPreconditionRequired {
			t.Fatalf("status %d, want 428", rec.Code)
		}
		if code := problemCode(t, rec); code != "precondition_required" {
			t.Errorf("code %q, want precondition_required", code)
		}
		if rig.called {
			t.Error("the operation ran before the 428")
		}
	})

	t.Run("required, stale: 412 version_conflict with the current version before the operation", func(t *testing.T) {
		rig := newOCCRig()
		rec := rig.do(required, `"4"`)
		if rec.Code != http.StatusPreconditionFailed {
			t.Fatalf("status %d, want 412", rec.Code)
		}
		p := problem(t, rec)
		if p["code"] != "version_conflict" {
			t.Errorf("code %v, want version_conflict", p["code"])
		}
		if p["currentVersion"] != float64(5) {
			t.Errorf("currentVersion %v, want 5", p["currentVersion"])
		}
		if rig.called {
			t.Error("the operation ran before the 412")
		}
	})

	t.Run("required, current: the operation runs and answers its ETag", func(t *testing.T) {
		rig := newOCCRig()
		rec := rig.do(required, `"5"`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if !rig.called {
			t.Fatal("the operation did not run")
		}
		if rig.expected != 5 {
			t.Errorf("expected version in the operation %d, want 5", rig.expected)
		}
		if etag := rec.Header().Get("ETag"); etag != `"5"` {
			t.Errorf(`ETag %q, want "5"`, etag)
		}
	})

	t.Run("optional, absent: the operation runs", func(t *testing.T) {
		rig := newOCCRig()
		rec := rig.do(optional, "")
		if rec.Code != http.StatusOK || !rig.called {
			t.Fatalf("status %d, operation ran %v; want 200 and the operation run", rec.Code, rig.called)
		}
	})

	for _, ifMatch := range []string{"", `"4"`} {
		t.Run("not checked, If-Match "+ifMatch+": no effect", func(t *testing.T) {
			rig := newOCCRig()
			rec := rig.do(none, ifMatch)
			if rec.Code != http.StatusOK || !rig.called {
				t.Fatalf("status %d, operation ran %v; want 200 and the operation run", rec.Code, rig.called)
			}
			if rig.versions.reads != 0 {
				t.Errorf("the version was read %d times on a route that does not check it", rig.versions.reads)
			}
		})
	}
}
