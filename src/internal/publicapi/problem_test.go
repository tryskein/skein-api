package publicapi_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"skein-api/internal/errcode"
	"skein-api/internal/publicapi"
	"skein-api/internal/publicapi/apitest"
)

// origins maps the reference's origin names to the problem writer's.
var origins = map[apitest.Origin]publicapi.Origin{
	apitest.Path:   publicapi.OriginPath,
	apitest.Body:   publicapi.OriginBody,
	apitest.Parse:  publicapi.OriginParse,
	apitest.Domain: publicapi.OriginDomain,
}

// TestWriteProblem_EveryCodeByOrigin — row 54 (spec "Response codes of the
// public API", D-11, OST-3-3). Every code of errcode, written with every
// origin the literal reference allows for it, answers the reference status as
// application/problem+json carrying the code, and retryable: true exactly
// when the reference marks the code transient. Every code of errcode is in
// the reference and every reference code is in errcode. Red when, say,
// locked_by_other is declared a not-found code (404 by path), when a domain
// validation_error answers 400, or when a code changes class in errcode.
func TestWriteProblem_EveryCodeByOrigin(t *testing.T) {
	all := errcode.All()
	if len(all) == 0 {
		t.Fatal("errcode defines no codes")
	}
	for _, code := range all {
		want, ok := apitest.Codes[code.String()]
		if !ok {
			t.Errorf("errcode %q is not in the reference table apitest.Codes", code.String())
			continue
		}
		if len(want.Status) == 0 {
			t.Errorf("reference of %q names no origin", code.String())
		}
		for origin, status := range want.Status {
			t.Run(code.String()+"/"+string(origin), func(t *testing.T) {
				rec := httptest.NewRecorder()
				publicapi.WriteProblem(rec, publicapi.Problem{Code: code, Origin: origins[origin]})

				if rec.Code != status {
					t.Errorf("status %d, reference %d", rec.Code, status)
				}
				if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
					t.Errorf("Content-Type %q, want application/problem+json", ct)
				}
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
				}
				if body["code"] != code.String() {
					t.Errorf("code %v, want %q", body["code"], code.String())
				}
				if s, _ := body["status"].(float64); int(s) != status {
					t.Errorf("status member %v, want %d", body["status"], status)
				}
				if _, ok := body["type"].(string); !ok {
					t.Errorf("no type member: %v", body)
				}
				if _, ok := body["title"].(string); !ok {
					t.Errorf("no title member: %v", body)
				}
				retryable, present := body["retryable"]
				switch {
				case want.Retryable && retryable != true:
					t.Errorf("retryable %v, want true", retryable)
				case !want.Retryable && present:
					t.Errorf("retryable %v on a permanent code, want the member absent", retryable)
				}
			})
		}
	}
	for name := range apitest.Codes {
		if _, ok := errcode.Lookup(name); !ok {
			t.Errorf("reference code %q is not defined in errcode", name)
		}
	}
}

// TestWriteProblem_NamesTheObjectByOneMember — a refusal names its object
// under one member per code in every operation of the public API: the shared
// operations name the same issue id (or project handle) differently, and the
// client reads it from issueId, project, … whichever operation refused.
// Red when an alias reaches the body or the member is lost.
func TestWriteProblem_NamesTheObjectByOneMember(t *testing.T) {
	cases := []struct {
		code   errcode.Code
		ext    map[string]any
		member string
		want   any
	}{
		{errcode.IssueNotFound, map[string]any{"id": "demo-1"}, "issueId", "demo-1"},
		{errcode.IssueNotFound, map[string]any{"issueId": "demo-2"}, "issueId", "demo-2"},
		{errcode.ProjectNotFound, map[string]any{"q": "nope"}, "project", "nope"},
		{errcode.ProjectNotFound, map[string]any{"projectId": "p-uuid"}, "project", "p-uuid"},
		{errcode.AmbiguousProject, map[string]any{"q": "two", "candidates": []any{}}, "project", "two"},
		{errcode.CommentNotFound, map[string]any{"id": "c1"}, "commentId", "c1"},
		{errcode.UserNotFound, map[string]any{"username": "bob"}, "user", "bob"},
		{errcode.UserNotFound, map[string]any{"userId": "u1"}, "user", "u1"},
		{errcode.MemberNotFound, map[string]any{"userId": "u2"}, "user", "u2"},
	}
	aliases := []string{"id", "q", "projectId", "userId", "username"}
	for _, c := range cases {
		t.Run(c.code.String(), func(t *testing.T) {
			rec := httptest.NewRecorder()
			publicapi.WriteProblem(rec, publicapi.Problem{Code: c.code, Origin: publicapi.OriginPath, Extensions: c.ext})
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
			if body[c.member] != c.want {
				t.Errorf("%s with %v: %s = %v, want %v (body %s)", c.code, c.ext, c.member, body[c.member], c.want, rec.Body.String())
			}
			for _, a := range aliases {
				if _, present := body[a]; present && a != c.member {
					t.Errorf("%s with %v: alias %s is in the body %s", c.code, c.ext, a, rec.Body.String())
				}
			}
		})
	}
}

// TestWriteProblem_LeavesOtherCodesAlone — the members of a code without an
// object member are written as given.
func TestWriteProblem_LeavesOtherCodesAlone(t *testing.T) {
	rec := httptest.NewRecorder()
	publicapi.WriteProblem(rec, publicapi.Problem{Code: errcode.LockedByOther, Origin: publicapi.OriginDomain,
		Extensions: map[string]any{"id": "demo-1", "lockedBy": "s"}})
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["id"] != "demo-1" || body["lockedBy"] != "s" {
		t.Errorf("body %s, want id and lockedBy as given", rec.Body.String())
	}
}
