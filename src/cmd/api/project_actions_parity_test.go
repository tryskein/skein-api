package main

// project_actions_parity_test.go — sk-674.18 T024, matrix row 92 (US-6).
// Reference — the action table of US-6 ("Actions on a project of a space -
// one table for REST and MCP") as changed by the owner
// 2026-10-17 (sk-741: in a project of a non-personal space the instance
// admin has the project owner's rights without a membership row), for the
// project A.members of the fixture (testsupport.SeedTenancy) and its actors:
//
//	action                        vP  vPr  member uc¬p adm0 vnsh
//	see the project and issues    +   +    +      −    +    +
//	create / edit issues          +   +    +      −    +    +
//	be the assignee               +   +    +      −    +    +
//	manage project members        +   +    −      −    +    −
//	switch the visibility         +   +    −      −    +    −
//	rename (archive, aliases, …)  +   +    −      −    +    −
//	force_unlock                  +   +    −      −    +    −
//	create a project in the space +   +    +      +    −    −
//
// Each cell is asked on both channels — REST (the BFF channel) and MCP (the
// actor's API key) — and both must give the table's answer. Requests that
// change state are chosen idempotent (the current name, the current
// visibility, an existing member's role) so the order of actors does not
// change the cell. force_unlock waits for T034 (the unlock route and the
// table's rule for it). The issue write paths answer the table since T066
// (sk-689.37); every such path is in issue_write_actions_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

// restWith is parityStack.rest with extra headers (If-Match).
func (s *parityStack) restWith(a testsupport.Actor, method, path string, body any, headers map[string]string) (int, []byte) {
	s.t.Helper()
	u := s.f.User(s.t, a)
	raw, err := json.Marshal(body)
	if err != nil {
		s.t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
	req.Header.Set(auth.HeaderUserID, u.ID.String())
	req.Header.Set(auth.HeaderSessionEpoch, wiringSessionEpoch)
	req.Header.Set(auth.HeaderUsername, u.Username)
	req.Header.Set(auth.HeaderUserRole, u.Role)
	if u.External {
		req.Header.Set(auth.HeaderUserIsExternal, "true")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestProjectActions_TableIsOneInRESTAndMCP(t *testing.T) {
	s := newParityStack(t)
	ctx := context.Background()
	p := s.f.AMembers
	var spaceShort string
	if err := s.pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, s.f.SpaceA).Scan(&spaceShort); err != nil {
		t.Fatal(err)
	}
	member := s.f.User(t, testsupport.ActorOnlyA) // an existing member of A.members
	base := "/api/v1/projects/" + p.ID.String()
	version := func(issueID string) int {
		var v int
		if err := s.pool.QueryRow(ctx, `SELECT version FROM issues WHERE id = $1`, issueID).Scan(&v); err != nil {
			t.Fatalf("version of %s: %v", issueID, err)
		}
		return v
	}
	forget := func(raw []byte, key string) {
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if inner, ok := body[key].(map[string]any); ok {
			body = inner
		}
		id, err := uuid.Parse(fmt.Sprint(body["id"]))
		if err != nil {
			return
		}
		t.Cleanup(func() {
			c := context.Background()
			for _, q := range []string{
				`DELETE FROM events WHERE project_id = $1`,
				`DELETE FROM project_members WHERE project_id = $1`,
				`DELETE FROM project_counters WHERE project_id = $1`,
				`DELETE FROM projects WHERE id = $1`,
			} {
				_, _ = s.pool.Exec(c, q, id)
			}
		})
	}

	// assigneeUserID reads assigneeUserId of an issue card; uuid.Nil when
	// absent or unreadable.
	assigneeUserID := func(raw []byte) uuid.UUID {
		var card struct {
			AssigneeUserID *uuid.UUID `json:"assigneeUserId"`
		}
		if json.Unmarshal(raw, &card) != nil || card.AssigneeUserID == nil {
			return uuid.Nil
		}
		return *card.AssigneeUserID
	}

	type cell struct {
		rest func(a testsupport.Actor) bool
		mcp  func(a testsupport.Actor, sid string) bool
	}
	ok := func(code int) bool { return code >= 200 && code < 300 }
	actions := []struct {
		name string
		want map[testsupport.Actor]bool
		cell cell
	}{
		{"see the project and issues", map[testsupport.Actor]bool{
			testsupport.ActorProjectOwner: true, testsupport.ActorSpaceOwner: true, testsupport.ActorOnlyA: true,
			testsupport.ActorAdmin0: true, testsupport.ActorExternal: true,
		}, cell{
			rest: func(a testsupport.Actor) bool {
				code, _ := s.rest(a, http.MethodGet, base+"/issues/"+p.IssueID, nil)
				return ok(code)
			},
			mcp: func(a testsupport.Actor, sid string) bool {
				return !s.tool(a, sid, "skein_show", map[string]any{"id": p.IssueID}).isError
			},
		}},
		{"create issues", map[testsupport.Actor]bool{
			testsupport.ActorProjectOwner: true, testsupport.ActorSpaceOwner: true, testsupport.ActorOnlyA: true,
			testsupport.ActorExternal: true, testsupport.ActorAdmin0: true,
		}, cell{
			rest: func(a testsupport.Actor) bool {
				code, _ := s.rest(a, http.MethodPost, base+"/issues", map[string]any{"title": "row 92 rest " + string(a)})
				return ok(code)
			},
			mcp: func(a testsupport.Actor, sid string) bool {
				return !s.tool(a, sid, "skein_create", map[string]any{"project": p.IDPrefix, "title": "row 92 mcp " + string(a), "type": "task", "kind": "directed"}).isError
			},
		}},
		{"be the assignee", map[testsupport.Actor]bool{
			testsupport.ActorProjectOwner: true, testsupport.ActorSpaceOwner: true, testsupport.ActorOnlyA: true,
			testsupport.ActorExternal: true, testsupport.ActorAdmin0: true,
		}, cell{
			// Being the assignee is the assignment resolving to the person
			// (assigneeUserId, INV-30), not only a stored text.
			rest: func(a testsupport.Actor) bool {
				u := s.f.User(t, a)
				code, raw := s.restWith(a, http.MethodPatch, base+"/issues/"+p.IssueID,
					map[string]any{"assignee": u.Username},
					map[string]string{"If-Match": fmt.Sprint(version(p.IssueID))})
				return ok(code) && assigneeUserID(raw) == u.ID
			},
			mcp: func(a testsupport.Actor, sid string) bool {
				u := s.f.User(t, a)
				if s.tool(a, sid, "skein_update", map[string]any{
					"id": p.IssueID, "expectedVersion": version(p.IssueID), "assignee": u.Username,
				}).isError {
					return false
				}
				code, raw := s.rest(a, http.MethodGet, base+"/issues/"+p.IssueID, nil)
				return ok(code) && assigneeUserID(raw) == u.ID
			},
		}},
		{"manage project members", map[testsupport.Actor]bool{
			testsupport.ActorProjectOwner: true, testsupport.ActorSpaceOwner: true, testsupport.ActorAdmin0: true,
		}, cell{
			rest: func(a testsupport.Actor) bool {
				code, _ := s.rest(a, http.MethodPut, base+"/members/"+member.ID.String(), map[string]any{"role": "member"})
				return ok(code)
			},
			mcp: func(a testsupport.Actor, sid string) bool {
				return !s.tool(a, sid, "skein_project_members", map[string]any{
					"action": "add", "project": p.IDPrefix, "userId": member.ID.String(), "role": "member",
				}).isError
			},
		}},
		{"switch the visibility", map[testsupport.Actor]bool{
			testsupport.ActorProjectOwner: true, testsupport.ActorSpaceOwner: true, testsupport.ActorAdmin0: true,
		}, cell{
			rest: func(a testsupport.Actor) bool {
				code, _ := s.rest(a, http.MethodPatch, base, map[string]any{"visibility": "members"})
				return ok(code)
			},
			mcp: func(a testsupport.Actor, sid string) bool {
				return !s.tool(a, sid, "skein_project_update", map[string]any{"project": p.IDPrefix, "visibility": "members"}).isError
			},
		}},
		{"rename", map[testsupport.Actor]bool{
			testsupport.ActorProjectOwner: true, testsupport.ActorSpaceOwner: true, testsupport.ActorAdmin0: true,
		}, cell{
			rest: func(a testsupport.Actor) bool {
				code, _ := s.rest(a, http.MethodPatch, base, map[string]any{"name": p.Name})
				return ok(code)
			},
			mcp: func(a testsupport.Actor, sid string) bool {
				return !s.tool(a, sid, "skein_project_update", map[string]any{"project": p.IDPrefix, "name": p.Name}).isError
			},
		}},
		{"create a project in the space", map[testsupport.Actor]bool{
			testsupport.ActorProjectOwner: true, testsupport.ActorSpaceOwner: true, testsupport.ActorOnlyA: true,
			testsupport.ActorParticipantNoMembers: true,
		}, cell{
			rest: func(a testsupport.Actor) bool {
				prefix := "r92r" + strings.ReplaceAll(uuid.NewString()[:6], "-", "")
				code, raw := s.rest(a, http.MethodPost, "/api/v1/projects",
					map[string]any{"name": prefix, "idPrefix": prefix, "spaceId": spaceShort})
				if ok(code) {
					forget(raw, "")
				}
				return ok(code)
			},
			mcp: func(a testsupport.Actor, sid string) bool {
				prefix := "r92m" + strings.ReplaceAll(uuid.NewString()[:6], "-", "")
				res := s.tool(a, sid, "skein_project_create", map[string]any{"name": prefix, "idPrefix": prefix, "space": spaceShort})
				if !res.isError {
					forget([]byte(res.text), "project")
				}
				return !res.isError
			},
		}},
	}

	actors := []testsupport.Actor{
		testsupport.ActorProjectOwner, testsupport.ActorSpaceOwner, testsupport.ActorOnlyA,
		testsupport.ActorParticipantNoMembers, testsupport.ActorAdmin0, testsupport.ActorExternal,
	}
	for _, a := range actors {
		t.Run(string(a), func(t *testing.T) {
			sid := s.session(a)
			for _, act := range actions {
				t.Run(act.name, func(t *testing.T) {
					want := act.want[a]
					if got := act.cell.rest(a); got != want {
						t.Errorf("REST: allowed=%v, the table says %v", got, want)
					}
					if got := act.cell.mcp(a, sid); got != want {
						t.Errorf("MCP: allowed=%v, the table says %v", got, want)
					}
				})
			}
			t.Run("force_unlock", func(t *testing.T) {
				t.Skip("waiting for narrowing: T034 (the unlock route and the table's rule for force_unlock)")
			})
		})
	}
}
