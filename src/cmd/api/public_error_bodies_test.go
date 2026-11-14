package main

// public_error_bodies_test.go — the members of a refusal are named alike in
// every operation of the public API, as a client parses them: the missing
// issue of the path is issueId on a read and on a write, a project filter
// that resolves to nothing is project (not the resolver's internal q), and a
// member of the wrong type is field by its name in the request. The
// reference is the defect report: GET answered issueId and PATCH id, the
// filter answered q, a create named "issueFields.priority".

import (
	"net/http"
	"testing"

	"skein-api/internal/apikeys"
)

func TestPublicErrorBodies(t *testing.T) {
	w := newClassWorld(t)
	key := w.key(w.owner, levelKey(apikeys.LevelWrite))
	missing := w.pPrefix + "-999999"

	cases := []struct {
		name         string
		call         opCall
		status       int
		code, member string
		want         string
		absent       []string
	}{
		{"getIssue of a missing issue", opCall{http.MethodGet, "/api/v1/issues/" + missing, nil, nil},
			http.StatusNotFound, "issue_not_found", "issueId", missing, []string{"id"}},
		{"updateIssue of a missing issue", opCall{http.MethodPatch, "/api/v1/issues/" + missing,
			map[string]any{"title": "x"}, map[string]string{"If-Match": `"1"`}},
			http.StatusNotFound, "issue_not_found", "issueId", missing, []string{"id"}},
		{"closeIssue of a missing issue", opCall{http.MethodPost, "/api/v1/issues/" + missing + "/close",
			map[string]any{"reason": "r", "closeOutcome": "done"}, map[string]string{"If-Match": `"1"`}},
			http.StatusNotFound, "issue_not_found", "issueId", missing, []string{"id"}},
		{"claimIssue of a missing issue", opCall{http.MethodPost, "/api/v1/issues/" + missing + "/claim", nil, nil},
			http.StatusNotFound, "issue_not_found", "issueId", missing, []string{"id"}},
		{"listIssues with a project that resolves to nothing", opCall{http.MethodGet, "/api/v1/issues?project=nope" + w.sfx, nil, nil},
			http.StatusUnprocessableEntity, "project_not_found", "project", "nope" + w.sfx, []string{"q"}},
		{"getProject of a missing project", opCall{http.MethodGet, "/api/v1/projects/nope" + w.sfx, nil, nil},
			http.StatusNotFound, "project_not_found", "project", "nope" + w.sfx, []string{"q", "projectId"}},
		{"createIssue with priority of the wrong type", opCall{http.MethodPost, "/api/v1/projects/" + w.p.String() + "/issues",
			map[string]any{"title": "x", "type": "task", "priority": "high"}, nil},
			http.StatusBadRequest, "validation_error", "field", "priority", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := w.do(key, c.call, "", nil)
			if a.status != c.status || a.code() != c.code {
				t.Fatalf("%d %s, want %d %s", a.status, a.body, c.status, c.code)
			}
			body := a.object(t)
			if body[c.member] != c.want {
				t.Errorf("%s = %v, want %q (body %s)", c.member, body[c.member], c.want, a.body)
			}
			for _, k := range c.absent {
				if _, present := body[k]; present {
					t.Errorf("member %s is in the body %s", k, a.body)
				}
			}
		})
	}
}
