package main

// nul_character_test.go — U+0000 in caller input is a validation failure on
// every transport: the public REST API, the internal REST API and the MCP
// tools. PostgreSQL cannot store the character, so before the guard it
// reached the INSERT and came back as 500 internal_error. The reference is
// the defect report: 422 validation_error naming the field, nothing
// written, no ERROR log line.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/apikeys"
	"skein-api/internal/testsupport"
)

// errorRecorder keeps the ERROR records of the default logger.
type errorRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (h *errorRecorder) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelError }
func (h *errorRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *errorRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *errorRecorder) WithGroup(string) slog.Handler      { return h }

func (h *errorRecorder) take() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.msgs
	h.msgs = nil
	return out
}

// recordErrors swaps the default logger for one that keeps ERROR records.
func recordErrors(t *testing.T) *errorRecorder {
	t.Helper()
	rec := &errorRecorder{}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return rec
}

const nulText = "nul\x00byte"

func TestNULCharacter_PublicAPI(t *testing.T) {
	w := newClassWorld(t)
	errs := recordErrors(t)
	key := w.key(w.owner, levelKey(apikeys.LevelManage))
	proj := "/api/v1/projects/" + w.p.String()
	comment := w.seedComment(w.issue)
	issuesOf := func() int { return w.idemCount(`SELECT count(*) FROM issues WHERE project_id = $1`, w.p) }
	commentsOf := func() int { return w.idemCount(`SELECT count(*) FROM comments WHERE issue_id = $1`, w.issue) }
	ifMatch := func(v int) map[string]string { return map[string]string{"If-Match": `"` + strconv.Itoa(v) + `"`} }

	cases := []struct {
		name  string
		call  opCall
		query string
		field string
	}{
		{"createIssue title", opCall{http.MethodPost, proj + "/issues",
			map[string]any{"title": nulText, "type": "note", "force": true}, nil}, "", "title"},
		{"createIssue label", opCall{http.MethodPost, proj + "/issues",
			map[string]any{"title": w.fresh("nul"), "type": "note", "force": true, "labels": []string{"ok", nulText}}, nil}, "", "labels[1]"},
		{"updateIssue description", opCall{http.MethodPatch, "/api/v1/issues/" + w.issue,
			map[string]any{"description": nulText}, ifMatch(w.issueVersion(w.issue))}, "", "description"},
		{"createComment body", opCall{http.MethodPost, "/api/v1/issues/" + w.issue + "/comments",
			map[string]any{"body": nulText}, nil}, "", "body"},
		{"updateComment body", opCall{http.MethodPatch, "/api/v1/comments/" + comment,
			map[string]any{"body": nulText}, ifMatch(1)}, "", "body"},
		{"createProject name", opCall{http.MethodPost, "/api/v1/projects",
			map[string]any{"name": nulText}, nil}, "", "name"},
		{"listIssues query", opCall{http.MethodGet, "/api/v1/issues", nil, nil}, "q=" + "nul%00byte", "q"},
		{"getIssue path", opCall{http.MethodGet, "/api/v1/issues/nul%00byte", nil, nil}, "", "path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nIssues, nComments := issuesOf(), commentsOf()
			version := w.issueVersion(w.issue)
			a := w.do(key, c.call, c.query, nil)
			if a.status != http.StatusUnprocessableEntity {
				t.Fatalf("status %d %s, want 422", a.status, a.body)
			}
			body := a.object(t)
			if body["code"] != "validation_error" || body["validation"] != "nul_character" || body["field"] != c.field {
				t.Errorf("body %s, want code validation_error, validation nul_character, field %q", a.body, c.field)
			}
			if issuesOf() != nIssues || commentsOf() != nComments || w.issueVersion(w.issue) != version {
				t.Errorf("the refused request wrote")
			}
			if logged := errs.take(); len(logged) > 0 {
				t.Errorf("ERROR logged for a client error: %v", logged)
			}
		})
	}
}

func TestNULCharacter_InternalAPIAndMCP(t *testing.T) {
	s := newParityStack(t)
	errs := recordErrors(t)
	owner := testsupport.ActorProjectOwner
	p := s.f.AMembers
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	issuesOf := func() int { return count(`SELECT count(*) FROM issues WHERE project_id = $1`, p.ID) }
	commentsOf := func() int { return count(`SELECT count(*) FROM comments WHERE issue_id = $1`, p.IssueID) }

	t.Run("internal API", func(t *testing.T) {
		base := "/ui/v1/projects/" + p.ID.String() + "/issues"
		cases := []struct {
			name, method, path string
			body               any
			field              string
		}{
			{"create title", http.MethodPost, base, map[string]any{"title": nulText}, "title"},
			{"create notes", http.MethodPost, base, map[string]any{"title": "nul " + uuid.NewString(), "notes": nulText}, "notes"},
			{"patch description", http.MethodPatch, base + "/" + p.IssueID, map[string]any{"description": nulText}, "description"},
			{"comment body", http.MethodPost, base + "/" + p.IssueID + "/comments", map[string]any{"body": nulText}, "body"},
			{"member name in the body", http.MethodPost, base, map[string]any{"ti\x00tle": "x"}, ""},
			{"list query", http.MethodGet, "/ui/v1/issues?q=nul%00byte", nil, "q"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				nIssues, nComments := issuesOf(), commentsOf()
				status, raw := s.rest(owner, c.method, c.path, c.body)
				if status != http.StatusUnprocessableEntity {
					t.Fatalf("status %d %s, want 422", status, raw)
				}
				var body map[string]any
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Fatalf("decode %s: %v", raw, err)
				}
				field, _ := body["field"].(string)
				if body["error"] != "validation_error" || body["code"] != "nul_character" || field != c.field {
					t.Errorf("body %s, want error validation_error, code nul_character, field %q", raw, c.field)
				}
				if issuesOf() != nIssues || commentsOf() != nComments {
					t.Errorf("the refused request wrote")
				}
				if logged := errs.take(); len(logged) > 0 {
					t.Errorf("ERROR logged for a client error: %v", logged)
				}
			})
		}
	})

	t.Run("MCP", func(t *testing.T) {
		sid := s.session(owner)
		cases := []struct {
			tool  string
			args  map[string]any
			field string
		}{
			{"skein_create", map[string]any{"project": p.ID.String(), "title": nulText, "type": "note", "kind": "autonomous"}, "title"},
			{"skein_create", map[string]any{"project": p.ID.String(), "title": "nul " + uuid.NewString(), "type": "note",
				"kind": "autonomous", "dependencies": []any{map[string]any{"toId": p.IssueID, "type": "related", "reason": nulText}}},
				"dependencies[0].reason"},
			{"skein_comment", map[string]any{"issueId": p.IssueID, "body": nulText, "kind": "autonomous"}, "body"},
		}
		for _, c := range cases {
			t.Run(c.tool+" "+c.field, func(t *testing.T) {
				nIssues, nComments := issuesOf(), commentsOf()
				res := s.tool(owner, sid, c.tool, c.args)
				if !res.isError {
					t.Fatalf("want a refusal, got %s", res.text)
				}
				var body map[string]any
				if err := json.Unmarshal([]byte(res.text), &body); err != nil {
					t.Fatalf("decode %s: %v", res.text, err)
				}
				if body["error"] != "validation_error" || body["code"] != "nul_character" || body["field"] != c.field {
					t.Errorf("refusal %s, want error validation_error, code nul_character, field %q", res.text, c.field)
				}
				if issuesOf() != nIssues || commentsOf() != nComments {
					t.Errorf("the refused call wrote")
				}
				if logged := errs.take(); len(logged) > 0 {
					t.Errorf("ERROR logged for a client error: %v", logged)
				}
			})
		}
	})
}
