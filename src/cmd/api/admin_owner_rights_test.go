package main

// admin_owner_rights_test.go — sk-741, the regression of the production
// defect: an instance admin, no member of a members-visibility project of a
// non-personal space and no participant of that space, got `forbidden: your
// access to this project does not allow this action` creating an issue.
// Reference — the maintainers' decision 2026-10-17 (spec sk-674.18, US-6 as
// changed by sk-741), written before the code: in a project of a
// non-personal space the instance admin has the project owner's rights —
// he creates issues and may be their assignee (INV-30); he belongs to the
// project's context of people (INV-9 as changed by sk-741): not marked
// departed as the author or the assignee, and a mention of him notifies
// him; another user's "Personal" stays out of his reach (INV-1), answered as
// a missing project (INV-3).
//
// Fixture: testsupport.SeedTenancy — A.members (visibility members, space
// A) and adm0, an instance admin with no membership anywhere.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/testsupport"
)

func TestAdminWithoutMembership_CreatesAndIsAssignee(t *testing.T) {
	s := newParityStack(t)
	ctx := context.Background()
	p := s.f.AMembers
	admin := s.f.User(t, testsupport.ActorAdmin0)
	sid := s.session(testsupport.ActorAdmin0)

	var inProject, inSpace bool
	if err := s.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM project_members WHERE project_id = $1 AND user_id = $3),
       EXISTS (SELECT 1 FROM space_members WHERE space_id = $2 AND user_id = $3)`,
		p.ID, s.f.SpaceA, admin.ID).Scan(&inProject, &inSpace); err != nil {
		t.Fatal(err)
	}
	if inProject || inSpace {
		t.Fatalf("precondition: adm0 is a member of the project (%v) or of its space (%v)", inProject, inSpace)
	}

	// assigneeUserID reads the card as the admin sees it over REST.
	assigneeUserID := func(t *testing.T, issueID string) *uuid.UUID {
		t.Helper()
		status, raw := s.rest(testsupport.ActorAdmin0, http.MethodGet, "/api/v1/projects/"+p.ID.String()+"/issues/"+issueID, nil)
		if status != http.StatusOK {
			t.Fatalf("GET %s: %d %s", issueID, status, raw)
		}
		var card struct {
			AssigneeUserID *uuid.UUID `json:"assigneeUserId"`
		}
		if err := json.Unmarshal(raw, &card); err != nil {
			t.Fatalf("decode card %s: %v", raw, err)
		}
		return card.AssigneeUserID
	}
	issueByTitle := func(t *testing.T, title string) string {
		t.Helper()
		var id string
		if err := s.pool.QueryRow(ctx, `SELECT id FROM issues WHERE project_id = $1 AND title = $2`, p.ID, title).Scan(&id); err != nil {
			t.Fatalf("issue %q: %v", title, err)
		}
		return id
	}

	t.Run("MCP skein_create assigned to himself by username", func(t *testing.T) {
		title := "sk-741 mcp " + uuid.NewString()
		res := s.tool(testsupport.ActorAdmin0, sid, "skein_create", map[string]any{
			"project": p.IDPrefix, "title": title, "type": "task", "kind": "directed", "assignee": admin.Username,
		})
		if res.isError {
			t.Fatalf("skein_create refused: %s", res.text)
		}
		got := assigneeUserID(t, issueByTitle(t, title))
		if got == nil || *got != admin.ID {
			t.Errorf("assigneeUserId %v, want the admin %s", got, admin.ID)
		}
	})

	t.Run("REST create assigned to himself by id", func(t *testing.T) {
		title := "sk-741 rest " + uuid.NewString()
		status, raw := s.rest(testsupport.ActorAdmin0, http.MethodPost, "/api/v1/projects/"+p.ID.String()+"/issues",
			map[string]any{"title": title, "assignee": admin.ID.String()})
		if status != http.StatusCreated {
			t.Fatalf("POST issue: %d %s, want 201", status, raw)
		}
		got := assigneeUserID(t, issueByTitle(t, title))
		if got == nil || *got != admin.ID {
			t.Errorf("assigneeUserId %v, want the admin %s", got, admin.ID)
		}
	})

	t.Run("not departed as the author nor as the assignee", func(t *testing.T) {
		title := "sk-741 marks " + uuid.NewString()
		status, raw := s.rest(testsupport.ActorAdmin0, http.MethodPost, "/api/v1/projects/"+p.ID.String()+"/issues",
			map[string]any{"title": title, "assignee": admin.Username})
		if status != http.StatusCreated {
			t.Fatalf("POST issue: %d %s, want 201", status, raw)
		}
		status, raw = s.rest(testsupport.ActorProjectOwner, http.MethodGet,
			"/api/v1/projects/"+p.ID.String()+"/issues/"+issueByTitle(t, title), nil)
		if status != http.StatusOK {
			t.Fatalf("GET as the project owner: %d %s", status, raw)
		}
		var card struct {
			CreatedBy       uuid.UUID       `json:"createdBy"`
			AssigneeUserID  *uuid.UUID      `json:"assigneeUserId"`
			CreatedByStatus json.RawMessage `json:"createdByStatus"`
			AssigneeStatus  json.RawMessage `json:"assigneeStatus"`
		}
		if err := json.Unmarshal(raw, &card); err != nil {
			t.Fatalf("decode card %s: %v", raw, err)
		}
		if card.CreatedBy != admin.ID || card.AssigneeUserID == nil || *card.AssigneeUserID != admin.ID {
			t.Fatalf("precondition: createdBy %s, assigneeUserId %v; want the admin %s", card.CreatedBy, card.AssigneeUserID, admin.ID)
		}
		if card.CreatedByStatus != nil {
			t.Errorf("createdByStatus %s, want no mark (the admin is in the project's context)", card.CreatedByStatus)
		}
		if card.AssigneeStatus != nil {
			t.Errorf("assigneeStatus %s, want no mark (the admin is in the project's context)", card.AssigneeStatus)
		}
	})

	t.Run("a mention of him notifies him", func(t *testing.T) {
		count := func() int {
			var n int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notifications
 WHERE user_id = $1 AND type = 'comment.mention' AND project_id = $2`, admin.ID, p.ID).Scan(&n); err != nil {
				t.Fatalf("count mention notifications: %v", err)
			}
			return n
		}
		before := count()
		status, raw := s.rest(testsupport.ActorProjectOwner, http.MethodPost,
			"/api/v1/projects/"+p.ID.String()+"/issues/"+p.IssueID+"/comments",
			map[string]any{"body": "sk-741 ping @" + admin.Username})
		if status != http.StatusCreated && status != http.StatusOK {
			t.Fatalf("POST comment: %d %s", status, raw)
		}
		if got := count(); got != before+1 {
			t.Errorf("comment.mention notifications of the admin: %d → %d, want one more", before, got)
		}
	})

	// Deleting another author's comment is the owner's right
	// (authz.ActionDeleteAnyComment): the admin without membership has it,
	// a plain member of the project does not (403 forbidden in REST,
	// forbidden in MCP).
	t.Run("deletes another author's comment, a plain member may not", func(t *testing.T) {
		owner := s.f.User(t, testsupport.ActorProjectOwner)
		commentsOf := "/api/v1/projects/" + p.ID.String() + "/issues/" + p.IssueID + "/comments/"
		for _, c := range []struct {
			actor   testsupport.Actor
			allowed bool
		}{
			{testsupport.ActorAdmin0, true},
			{testsupport.ActorOnlyA, false},
		} {
			t.Run(string(c.actor), func(t *testing.T) {
				cid := testsupport.SeedComment(t, s.pool, p.IssueID, owner.ID, "sk-741 owner's comment")
				status, raw := s.rest(c.actor, http.MethodDelete, commentsOf+cid.String(), nil)
				var body map[string]any
				_ = json.Unmarshal(raw, &body)
				switch {
				case c.allowed && status != http.StatusOK:
					t.Errorf("REST delete: %d %s, want 200", status, raw)
				case !c.allowed && (status != http.StatusForbidden || body["error"] != "forbidden"):
					t.Errorf("REST delete: %d %s, want 403 forbidden", status, raw)
				}

				cid = testsupport.SeedComment(t, s.pool, p.IssueID, owner.ID, "sk-741 owner's comment")
				res := s.tool(c.actor, s.session(c.actor), "skein_comment_delete", map[string]any{"commentId": cid.String()})
				body = nil
				_ = json.Unmarshal([]byte(res.text), &body)
				switch {
				case c.allowed && res.isError:
					t.Errorf("MCP delete refused: %s", res.text)
				case !c.allowed && (!res.isError || body["error"] != "forbidden"):
					t.Errorf("MCP delete: isError %v %s, want forbidden", res.isError, res.text)
				}
			})
		}
	})

	// In another user's "Personal" the admin is nobody who may work (INV-1:
	// the relation never gives it to him), so its owner choosing him by id
	// is refused as an id naming nobody (INV-30, E-32).
	t.Run("not assignable by id in another user's Personal", func(t *testing.T) {
		personal := s.f.Personal[testsupport.ActorOnlyA]
		var version int
		if err := s.pool.QueryRow(ctx, `SELECT version FROM issues WHERE id = $1`, personal.IssueID).Scan(&version); err != nil {
			t.Fatal(err)
		}
		status, raw := s.restWith(testsupport.ActorOnlyA, http.MethodPatch,
			"/api/v1/projects/"+personal.ID.String()+"/issues/"+personal.IssueID,
			map[string]any{"assignee": admin.ID.String()}, map[string]string{"If-Match": fmt.Sprint(version)})
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if status != http.StatusUnprocessableEntity || body["error"] != "assignee_not_allowed" {
			t.Errorf("assign the admin by id in Personal(uA): %d %s, want 422 assignee_not_allowed", status, raw)
		}
	})

	t.Run("another user's Personal stays out of reach", func(t *testing.T) {
		personal := s.f.Personal[testsupport.ActorOnlyA]
		status, raw := s.rest(testsupport.ActorAdmin0, http.MethodPost, "/api/v1/projects/"+personal.ID.String()+"/issues",
			map[string]any{"title": "sk-741 personal"})
		if status != http.StatusNotFound {
			t.Errorf("REST create in Personal(uA): %d %s, want 404", status, raw)
		}
		res := s.tool(testsupport.ActorAdmin0, sid, "skein_create", map[string]any{
			"project": personal.IDPrefix, "title": "sk-741 personal", "type": "task", "kind": "directed",
		})
		var body map[string]any
		_ = json.Unmarshal([]byte(res.text), &body)
		if !res.isError || body["error"] != "project_not_found" {
			t.Errorf("MCP skein_create in Personal(uA): isError %v, %s; want project_not_found", res.isError, res.text)
		}
	})
}
