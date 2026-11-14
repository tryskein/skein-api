package main

// public_spec_limits_test.go — the numeric limits the public OpenAPI
// document declares are the limits the operations apply. The reference is
// the served document itself: a client sizes its pages and titles by it, so
// for every list operation the bounds of its limit parameter must be
// accepted and the values just outside them refused, and the declared title
// length of a create and an update must be accepted at the bound and
// refused one character past it. Red when the document and the behaviour
// disagree in either direction (the defect: the document said 1..100 and
// 1..500 where the code applied 1..500 and 1..1000).

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"skein-api/internal/apikeys"
)

func TestPublicSpecLimitsMatchBehaviour(t *testing.T) {
	w := newClassWorld(t)
	doc := servedDocument(t, w.h)
	ownerKey := w.key(w.owner, levelKey(apikeys.LevelManage))
	opsByID := map[string]publicOp{}
	for _, op := range publicOps {
		opsByID[op.id] = op
	}

	t.Run("limit of every list operation", func(t *testing.T) {
		checked := 0
		for path, item := range doc.Paths.Map() {
			for method, op := range item.Operations() {
				p := op.Parameters.GetByInAndName(openapi3.ParameterInQuery, "limit")
				if p == nil {
					continue
				}
				checked++
				s := p.Schema.Value
				if s.Min == nil || s.Max == nil {
					t.Errorf("%s %s: limit declares no minimum and maximum", method, path)
					continue
				}
				pop, ok := opsByID[op.OperationID]
				if !ok {
					t.Fatalf("%s: not in the literal publicOps", op.OperationID)
				}
				key := w.keyFor(pop, ownerKey)
				c := w.call(pop, w.p, key)
				lo, hi := int(*s.Min), int(*s.Max)
				for _, v := range []struct {
					limit int
					ok    bool
				}{{lo - 1, false}, {lo, true}, {hi, true}, {hi + 1, false}} {
					a := w.do(key, c, "limit="+strconv.Itoa(v.limit), nil)
					switch {
					case v.ok && a.status != http.StatusOK:
						t.Errorf("%s limit=%d (declared %d..%d): %d %s, want 200", op.OperationID, v.limit, lo, hi, a.status, a.body)
					case !v.ok && (a.status != http.StatusBadRequest || a.code() != "validation_error"):
						t.Errorf("%s limit=%d (declared %d..%d): %d %s, want 400 validation_error", op.OperationID, v.limit, lo, hi, a.status, a.body)
					}
				}
			}
		}
		if checked == 0 {
			t.Fatal("the document declares no limit parameter: nothing was checked")
		}
	})

	titleMax := func(t *testing.T, method, path string) int {
		t.Helper()
		op := documentOperation(doc, method, path)
		if op == nil || op.RequestBody == nil {
			t.Fatalf("%s %s: no operation with a body in the document", method, path)
		}
		body := op.RequestBody.Value.Content.Get("application/json")
		if body == nil || body.Schema == nil {
			t.Fatalf("%s %s: no JSON body schema", method, path)
		}
		title := body.Schema.Value.Properties["title"]
		if title == nil || title.Value.MaxLength == nil {
			t.Fatalf("%s %s: title declares no maxLength", method, path)
		}
		return int(*title.Value.MaxLength)
	}
	// A non-ASCII title: the limit counts characters, as maxLength does.
	titleOf := func(n int) string { return strings.Repeat("ж", n) }

	t.Run("title of createIssue", func(t *testing.T) {
		max := titleMax(t, http.MethodPost, "/api/v1/projects/{project}/issues")
		path := "/api/v1/projects/" + w.p.String() + "/issues"
		at := w.do(ownerKey, opCall{http.MethodPost, path, map[string]any{"title": titleOf(max), "type": "note", "force": true}, nil}, "", nil)
		if at.status != http.StatusCreated {
			t.Errorf("a title of %d characters: %d %s, want 201", max, at.status, at.body)
		}
		past := w.do(ownerKey, opCall{http.MethodPost, path, map[string]any{"title": titleOf(max + 1), "type": "note", "force": true}, nil}, "", nil)
		if past.status != http.StatusUnprocessableEntity || past.code() != "validation_error" || past.object(t)["validation"] != "invalid_title" {
			t.Errorf("a title of %d characters: %d %s, want 422 validation_error/invalid_title", max+1, past.status, past.body)
		}
	})

	t.Run("title of updateIssue", func(t *testing.T) {
		max := titleMax(t, http.MethodPatch, "/api/v1/issues/{issue}")
		id := w.freshIssue(w.p)
		patch := func(title string) answer {
			return w.do(ownerKey, opCall{http.MethodPatch, "/api/v1/issues/" + id, map[string]any{"title": title},
				map[string]string{"If-Match": `"` + strconv.Itoa(w.issueVersion(id)) + `"`}}, "", nil)
		}
		if a := patch(titleOf(max)); a.status != http.StatusOK {
			t.Errorf("a title of %d characters: %d %s, want 200", max, a.status, a.body)
		}
		if a := patch(titleOf(max + 1)); a.status != http.StatusUnprocessableEntity || a.code() != "validation_error" || a.object(t)["validation"] != "invalid_title" {
			t.Errorf("a title of %d characters: %d %s, want 422 validation_error/invalid_title", max+1, a.status, a.body)
		}
	})
}
