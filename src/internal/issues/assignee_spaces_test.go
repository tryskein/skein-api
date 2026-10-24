package issues_test

// assignee_spaces_test.go — sk-674.18, the test delivery of rows 124–127
// (T028, INV-30, E-32, E-33, E-37). The references are INV-30 and the edge
// cases as the spec words them, on the fixture legend of
// testsupport.SeedTenancy, written before the code. The issue is the one of
// A.members (members visibility, space A); its owner vP writes.
//
//   - 124 (E-32): a choice by id of a person who may no longer work in the
//     project (he left while the form was open) is refused with a code and
//     the previous assignment stays — on the store, REST and MCP paths.
//   - 125 (E-33, E-37): free text — the e-mail or username of a person the
//     writer does not see and who has no relation to the issue, a former
//     participant of the space included — is stored as text and becomes
//     neither a name (assigneeStatus) nor an id (assigneeUserId): not on
//     write, not in the card, not in the list row, not in the live event;
//     the answer equals the one for text naming nobody.
//   - 126 (display): a person with a historical relation to the issue
//     (here: he commented while he had access) is shown — after leaving,
//     marked departed; a deleted one, deleted with the pseudonym. Leaving
//     or deletion alone is no ground: without the relation the text stays
//     text.
//   - 127 (delivery): assigneeUserId, the person's assigned-to list (the
//     «on me» lens and the Inbox read it) — only while he has access to
//     the project; the same text resolves later, once access appears, and
//     stops when it is lost.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/handler"
	"skein-api/internal/issues"
	"skein-api/internal/mcp/tools"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

// assigneeWorld is the fixture plus the issue under test and its writer.
type assigneeWorld struct {
	pool  *pgxpool.Pool
	f     *testsupport.Tenancy
	store *issues.Store
	p     testsupport.TenancyProject
	owner testsupport.TenancyUser
}

func newAssigneeWorld(t *testing.T) *assigneeWorld {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	f := testsupport.SeedTenancy(t, pool)
	w := &assigneeWorld{pool: pool, f: f, store: issues.NewStore(pool), p: f.AMembers,
		owner: f.User(t, testsupport.ActorProjectOwner)}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE project_id = $1`, w.p.ID)
	})
	return w
}

func (w *assigneeWorld) version(t *testing.T) int {
	t.Helper()
	var v int
	if err := w.pool.QueryRow(context.Background(), `SELECT version FROM issues WHERE id = $1`, w.p.IssueID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (w *assigneeWorld) assign(t *testing.T, handle string) (*issues.IssueDetail, error) {
	t.Helper()
	return w.store.PatchIssue(context.Background(), w.p.IssueID, issues.PatchIssueInput{
		ExpectedVersion: w.version(t),
		Assignee:        &handle,
	}, issues.HumanActor(w.owner.ID, w.owner.Username))
}

func (w *assigneeWorld) storedAssignee(t *testing.T) string {
	t.Helper()
	var a *string
	if err := w.pool.QueryRow(context.Background(), `SELECT assignee FROM issues WHERE id = $1`, w.p.IssueID).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if a == nil {
		return ""
	}
	return *a
}

// newMember creates a user who may work in A.members: a participant of
// space A with a member row (testsupport.AddProjectMember).
func (w *assigneeWorld) newMember(t *testing.T) uuid.UUID {
	t.Helper()
	id, _ := testsupport.SeedUser(t, w.pool, "user")
	testsupport.AddProjectMember(t, w.pool, w.p.ID, id, "member")
	return id
}

// leave takes the person out of space A and its projects.
func (w *assigneeWorld) leave(t *testing.T, user uuid.UUID) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM project_members WHERE user_id = $1 AND project_id IN (SELECT id FROM projects WHERE space_id = $2)`,
		`DELETE FROM space_members WHERE user_id = $1 AND space_id = $2`,
	} {
		if _, err := w.pool.Exec(context.Background(), q, user, w.f.SpaceA); err != nil {
			t.Fatalf("leave space A: %v", err)
		}
	}
}

func (w *assigneeWorld) emailOf(t *testing.T, user uuid.UUID) string {
	t.Helper()
	var e string
	if err := w.pool.QueryRow(context.Background(), `SELECT email FROM users WHERE id = $1`, user).Scan(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

// TestAssigneeSpaces_ChoiceByIdOfLeaverRefused — row 124.
func TestAssigneeSpaces_ChoiceByIdOfLeaverRefused(t *testing.T) {
	w := newAssigneeWorld(t)
	previous := w.f.User(t, testsupport.ActorOnlyA)
	if _, err := w.assign(t, previous.Username); err != nil {
		t.Fatalf("the previous assignment: %v", err)
	}

	// Each surface: the form was opened while the person could work; he
	// left before the save.
	surfaces := map[string]func(t *testing.T, chosen uuid.UUID) (refused bool, detail string){
		"store": func(t *testing.T, chosen uuid.UUID) (bool, string) {
			_, err := w.assign(t, chosen.String())
			return errors.Is(err, issues.ErrAssigneeNotAllowed), fmt.Sprint(err)
		},
		"rest": func(t *testing.T, chosen uuid.UUID) (bool, string) {
			r := chi.NewRouter()
			r.Use(handler.ParseUserHeaders)
			r.Use(handler.RequireUser)
			h := handler.NewIssuesHandler(w.pool, projects.NewStore(w.pool), dependencies.NewStore(w.pool))
			r.Patch("/api/v1/projects/{projectID}/issues/{issueID}", h.Patch)
			raw, _ := json.Marshal(map[string]any{"assignee": chosen.String()})
			req := httptest.NewRequest(http.MethodPatch, "/api/v1/projects/"+w.p.ID.String()+"/issues/"+w.p.IssueID, bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("If-Match", fmt.Sprint(w.version(t)))
			req.Header.Set(auth.HeaderUserID, w.owner.ID.String())
			req.Header.Set(auth.HeaderUsername, w.owner.Username)
			req.Header.Set(auth.HeaderUserRole, w.owner.Role)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			var body struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			return rec.Code == http.StatusUnprocessableEntity && body.Error == "assignee_not_allowed", rec.Body.String()
		},
		"mcp": func(t *testing.T, chosen uuid.UUID) (bool, string) {
			s := server.NewMCPServer("t", "0", server.WithToolCapabilities(true))
			tools.RegisterUpdate(s, w.pool, w.store)
			ctx := auth.WithUserID(context.Background(), w.owner.ID)
			ctx = auth.WithUserRole(ctx, w.owner.Role)
			ctx = auth.WithAPIKeyID(ctx, uuid.New())
			ctx = auth.WithSessionID(ctx, "assignee-spaces")
			msg, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "tools/call",
				"params": map[string]any{"name": "skein_update", "arguments": map[string]any{
					"id": w.p.IssueID, "expectedVersion": w.version(t), "assignee": chosen.String(),
				}},
			})
			out, _ := json.Marshal(s.HandleMessage(ctx, msg))
			return bytes.Contains(out, []byte(`\"error\":\"assignee_not_allowed\"`)), string(out)
		},
	}
	for name, call := range surfaces {
		t.Run(name, func(t *testing.T) {
			chosen := w.newMember(t)
			w.leave(t, chosen)
			refused, detail := call(t, chosen)
			if !refused {
				t.Errorf("a choice by id of a person who left was not refused with assignee_not_allowed: %s", detail)
			}
			if got := w.storedAssignee(t); got != previous.Username {
				t.Errorf("assignee after the refusal = %q, want the previous %q", got, previous.Username)
			}
		})
	}
	// Positive control: a person who still may work is accepted by id.
	t.Run("a worker by id is accepted", func(t *testing.T) {
		worker := w.newMember(t)
		dto, err := w.assign(t, worker.String())
		if err != nil {
			t.Fatalf("assign a worker by id: %v", err)
		}
		if dto.AssigneeUserID == nil || *dto.AssigneeUserID != worker {
			t.Errorf("assigneeUserId = %v, want %s", dto.AssigneeUserID, worker)
		}
	})
}

// TestAssigneeSpaces_FreeTextStaysText — row 125.
func TestAssigneeSpaces_FreeTextStaysText(t *testing.T) {
	w := newAssigneeWorld(t)
	ctx := context.Background()
	otherSpace := w.f.User(t, testsupport.ActorOnlyB)
	former := w.newMember(t)
	w.leave(t, former)
	var formerName string
	if err := w.pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, former).Scan(&formerName); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		AssigneeUserID *uuid.UUID
		HasStatus      bool
	}
	read := func(t *testing.T, handle string) (outcome, []byte) {
		t.Helper()
		dto, err := w.assign(t, handle)
		if err != nil {
			t.Fatalf("free text %q refused: %v", handle, err)
		}
		if dto.Assignee == nil || *dto.Assignee != handle {
			t.Errorf("assignee on write = %v, want the text %q", dto.Assignee, handle)
		}
		card, err := w.store.GetIssue(ctx, w.p.IssueID)
		if err != nil {
			t.Fatal(err)
		}
		list, err := w.store.ListIssues(ctx, issues.ListIssuesArgs{ProjectID: w.p.ID, Limit: 100})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var row *issues.IssueSummary
		for i := range list.Items {
			if list.Items[i].ID == w.p.IssueID {
				row = &list.Items[i]
			}
		}
		if row == nil {
			t.Fatalf("the issue is not in its project's list")
		}
		var live []byte
		if err := pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			bodies, _, err := issues.NeutralRowSource{}.NeutralRowsJSON(ctx, tx, []string{w.p.IssueID})
			live = bodies[w.p.IssueID]
			return err
		}); err != nil {
			t.Fatalf("live row: %v", err)
		}
		all, _ := json.Marshal([]any{dto, card, row})
		return outcome{AssigneeUserID: card.AssigneeUserID, HasStatus: card.AssigneeStatus != nil}, append(all, live...)
	}

	cases := map[string]struct {
		handle string
		person uuid.UUID
		name   string
	}{
		"e-mail of a person of another space":         {w.emailOf(t, otherSpace.ID), otherSpace.ID, otherSpace.Username},
		"username of a person of another space":       {otherSpace.Username, otherSpace.ID, otherSpace.Username},
		"e-mail of a former participant of the space": {w.emailOf(t, former), former, formerName},
	}
	reference, _ := read(t, "nobody-"+strings.ReplaceAll(uuid.NewString()[:8], "-", "")+"@nowhere.example")
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, bodies := read(t, c.handle)
			if got.AssigneeUserID != nil || got.HasStatus {
				t.Errorf("free text resolved: assigneeUserId %v, assigneeStatus present %v", got.AssigneeUserID, got.HasStatus)
			}
			if fmt.Sprint(got) != fmt.Sprint(reference) {
				t.Errorf("answer %+v differs from the one for text naming nobody %+v", got, reference)
			}
			if bytes.Contains(bodies, []byte(c.person.String())) {
				t.Errorf("the person's id appears in the write answer, card, list row or live row")
			}
			if !strings.Contains(c.handle, c.name) && bytes.Contains(bodies, []byte(`"`+c.name+`"`)) {
				t.Errorf("the person's username appears although the text named him by e-mail")
			}
			var events []byte
			if err := w.pool.QueryRow(ctx, `
SELECT coalesce(string_agg(coalesce(old_data::text, '') || coalesce(new_data::text, ''), ' '), '')
  FROM events WHERE entity_type = 'issue' AND entity_id = $1`, w.p.IssueID).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(events, []byte(c.person.String())) {
				t.Errorf("the person's id appears in the issue's events")
			}
		})
	}
}

// TestAssigneeSpaces_DisplayNeedsHistoricalRelation — row 126.
func TestAssigneeSpaces_DisplayNeedsHistoricalRelation(t *testing.T) {
	w := newAssigneeWorld(t)
	ctx := context.Background()
	type marks struct {
		Departed  bool    `json:"departed"`
		Deleted   bool    `json:"deleted"`
		Pseudonym *string `json:"pseudonym"`
	}
	statusOf := func(t *testing.T, handle string) *marks {
		t.Helper()
		if _, err := w.assign(t, handle); err != nil {
			t.Fatalf("assign %q: %v", handle, err)
		}
		card, err := w.store.GetIssue(ctx, w.p.IssueID)
		if err != nil {
			t.Fatal(err)
		}
		if card.AssigneeUserID != nil {
			t.Errorf("assigneeUserId %v for a person without access — delivery, not display", *card.AssigneeUserID)
		}
		if card.AssigneeStatus == nil {
			return nil
		}
		raw, _ := json.Marshal(card.AssigneeStatus)
		var m marks
		_ = json.Unmarshal(raw, &m)
		return &m
	}
	usernameOf := func(id uuid.UUID) string {
		var u string
		if err := w.pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, id).Scan(&u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	deleteAccount := func(t *testing.T, id uuid.UUID) string {
		t.Helper()
		var pseudonym string
		if err := w.pool.QueryRow(ctx, `
UPDATE users SET deleted_at = now(), pseudonym = 'deleted-' || substr(md5(id::text), 1, 8)
 WHERE id = $1 RETURNING pseudonym`, id).Scan(&pseudonym); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = w.pool.Exec(context.Background(), `UPDATE users SET deleted_at = NULL, pseudonym = NULL WHERE id = $1`, id)
		})
		return pseudonym
	}

	t.Run("linked person who left is shown departed", func(t *testing.T) {
		linked := w.newMember(t)
		testsupport.SeedComment(t, w.pool, w.p.IssueID, linked, "a comment while he had access")
		w.leave(t, linked)
		if m := statusOf(t, usernameOf(linked)); m == nil || !m.Departed {
			t.Errorf("marks %+v, want departed", m)
		}
	})
	t.Run("former participant without a relation stays text", func(t *testing.T) {
		unlinked := w.newMember(t)
		w.leave(t, unlinked)
		if m := statusOf(t, usernameOf(unlinked)); m != nil {
			t.Errorf("marks %+v, want none — leaving alone is no ground to show him", m)
		}
	})
	t.Run("linked deleted account is shown by its pseudonym", func(t *testing.T) {
		linked := w.newMember(t)
		testsupport.SeedComment(t, w.pool, w.p.IssueID, linked, "a comment before deletion")
		w.leave(t, linked)
		name := usernameOf(linked)
		pseudonym := deleteAccount(t, linked)
		if m := statusOf(t, name); m == nil || !m.Deleted || m.Pseudonym == nil || *m.Pseudonym != pseudonym {
			t.Errorf("marks %+v, want deleted with pseudonym %q", m, pseudonym)
		}
	})
	t.Run("deleted account without a relation stays text", func(t *testing.T) {
		unlinked := w.newMember(t)
		w.leave(t, unlinked)
		name := usernameOf(unlinked)
		deleteAccount(t, unlinked)
		if m := statusOf(t, name); m != nil {
			t.Errorf("marks %+v, want none — deletion alone is no ground to show him", m)
		}
	})
}

// TestAssigneeSpaces_DeliveryOnlyWithAccess — row 127.
func TestAssigneeSpaces_DeliveryOnlyWithAccess(t *testing.T) {
	w := newAssigneeWorld(t)
	ctx := context.Background()
	person, name := testsupport.SeedUser(t, w.pool, "user")

	delivered := func(t *testing.T) (resolved, listed bool) {
		t.Helper()
		card, err := w.store.GetIssue(ctx, w.p.IssueID)
		if err != nil {
			t.Fatal(err)
		}
		resolved = card.AssigneeUserID != nil && *card.AssigneeUserID == person
		res, err := w.store.ListByUser(ctx, issues.ListByUserArgs{
			UserID: person, Mode: issues.ModeAssignedTo, CallerUsername: name, Limit: 100,
			Scope: authz.ForUser(person),
		})
		if err != nil {
			t.Fatalf("assigned-to list: %v", err)
		}
		for _, it := range res.Items {
			listed = listed || it.ID == w.p.IssueID
		}
		return resolved, listed
	}

	if _, err := w.assign(t, name); err != nil {
		t.Fatalf("assign the text: %v", err)
	}
	if r, l := delivered(t); r || l {
		t.Errorf("without access: assigneeUserId resolved %v, in his assigned-to list %v; want neither", r, l)
	}

	testsupport.AddProjectMember(t, w.pool, w.p.ID, person, "member")
	if r, l := delivered(t); !r || !l {
		t.Errorf("after access appeared: assigneeUserId resolved %v, in his list %v; want both (the text resolves later)", r, l)
	}

	w.leave(t, person)
	if r, l := delivered(t); r || l {
		t.Errorf("after access was lost: assigneeUserId resolved %v, in his list %v; want neither", r, l)
	}
}

// TestAssigneeSpaces_FreeTextBuildsNoHistory — row 125 over the history.
// Reference — E-33 / E-37 and INV-30 as the
// spec words them: free text naming a person the writer may not assign is
// text and stays indistinguishable from text naming nobody — after it is
// replaced too. The delegation chain (previous_assignees) admits the old
// assignee only when he could legitimately hold the issue (he may work in
// the project at the change) or already has a historical relation to it
// (INV-30's display ground); otherwise clearing or replacing the text would
// turn it into a person on the card — his id and username in
// previousAssignees / previousAssigneeUsernames.
func TestAssigneeSpaces_FreeTextBuildsNoHistory(t *testing.T) {
	w := newAssigneeWorld(t)
	ctx := context.Background()
	otherSpace := w.f.User(t, testsupport.ActorOnlyB)
	former := w.newMember(t)
	w.leave(t, former)
	var formerName string
	if err := w.pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, former).Scan(&formerName); err != nil {
		t.Fatal(err)
	}
	card := func(t *testing.T) (*issues.IssueDetail, []byte) {
		t.Helper()
		c, err := w.store.GetIssue(ctx, w.p.IssueID)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(c)
		return c, b
	}
	inChain := func(c *issues.IssueDetail, id uuid.UUID) bool {
		for _, p := range c.PreviousAssignees {
			if p == id {
				return true
			}
		}
		return false
	}
	people := map[string]struct {
		handle string
		id     uuid.UUID
		name   string
	}{
		"e-mail of a person of another space":         {w.emailOf(t, otherSpace.ID), otherSpace.ID, otherSpace.Username},
		"username of a person of another space":       {otherSpace.Username, otherSpace.ID, otherSpace.Username},
		"e-mail of a former participant of the space": {w.emailOf(t, former), former, formerName},
	}
	for name, p := range people {
		for _, next := range []string{"", "someone-else-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")} {
			t.Run(fmt.Sprintf("%s, then %q", name, next), func(t *testing.T) {
				if _, err := w.assign(t, p.handle); err != nil {
					t.Fatalf("free text refused: %v", err)
				}
				if _, err := w.assign(t, next); err != nil {
					t.Fatalf("replacing the text: %v", err)
				}
				c, body := card(t)
				if inChain(c, p.id) {
					t.Errorf("previousAssignees holds the person the text named")
				}
				if bytes.Contains(body, []byte(p.id.String())) {
					t.Errorf("the person's id appears on the card")
				}
				if bytes.Contains(body, []byte(`"`+p.name+`"`)) && !strings.Contains(p.handle, p.name) {
					t.Errorf("the person's username appears on the card")
				}
			})
		}
	}
	t.Run("positive control: a member assigned and then cleared joins the chain", func(t *testing.T) {
		member := w.newMember(t)
		if _, err := w.assign(t, w.emailOf(t, member)); err != nil {
			t.Fatal(err)
		}
		if _, err := w.assign(t, ""); err != nil {
			t.Fatal(err)
		}
		if c, _ := card(t); !inChain(c, member) {
			t.Errorf("a legitimate former assignee is missing from previousAssignees %v", c.PreviousAssignees)
		}
	})
}
