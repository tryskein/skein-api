package handler

// people_spaces_test.go — sk-674.18, the test delivery of rows 120, 121 and
// 122 (T027: people by INV-9, INV-27, INV-18 / E-22). The references are the
// invariants as the spec words them, applied to the fixture legend of
// testsupport.SeedTenancy, written before the code:
//
//   - INV-9: a person v is visible to a caller u only if v is a participant
//     of one of u's spaces, or an external member of a project u can access;
//     an external u sees only the co-members of his projects; an instance
//     admin has no exception (the decision of 2026-10-14). In the context
//     of an issue a mention resolves only to the people of that issue's
//     context — the participants of its space and its external members.
//   - INV-27: no answer except one's own account, the admin screens of
//     users, the pending invitations for a space's owner/admin and the
//     assignee's free text carries another user's email.
//   - INV-18, E-22, US-13: a person who left the space of the context is
//     marked departed; a deleted account is marked deleted with its
//     permanent pseudonym wherever people are shown, a mention included.
//
// The per-actor sweep of the same rules over every surface is row 29
// (isolation_matrix_test.go, T004); these rows hold the rule itself — one
// case per relation of INV-9 — and the surfaces row 29 does not read.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

// TestPeopleSpaces_VisibilityByRelation — row 120. One case per relation of
// INV-9; each case is read through the directory by exact username, the
// directory by a name prefix (the autocomplete), the directory by e-mail
// (never a match — the e-mail is not searchable, so it is no oracle either)
// and the profile. Red if a person of another space only, an external
// member of a project the caller cannot access or a non-co-member of an
// external caller is shown, or if a visible person is not.
func TestPeopleSpaces_VisibilityByRelation(t *testing.T) {
	w := newIsoWorld(t)
	ctx := context.Background()
	missing := "zz" + strings.ReplaceAll(uuid.NewString()[:12], "-", "")

	// Distinct display names, so the autocomplete prefix names one person.
	nameOf := map[testsupport.Actor]string{}
	for _, a := range testsupport.TenancyActors {
		u := w.f.User(t, a)
		nameOf[a] = "pp" + w.word + string(a)
		if _, err := w.pool.Exec(ctx, `UPDATE users SET name = $2 WHERE id = $1`, u.ID, nameOf[a]); err != nil {
			t.Fatalf("name %s: %v", a, err)
		}
	}
	emailOf := func(a testsupport.Actor) string {
		var email string
		if err := w.pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, w.f.User(t, a).ID).Scan(&email); err != nil {
			t.Fatalf("email of %s: %v", a, err)
		}
		return email
	}

	cases := []struct {
		relation       string
		viewer, person testsupport.Actor
		visible        bool
	}{
		{"co-participant of a space", testsupport.ActorOnlyA, testsupport.ActorBoth, true},
		{"participant of another space only", testsupport.ActorOnlyA, testsupport.ActorOnlyB, false},
		{"participant of the viewer's second space", testsupport.ActorBoth, testsupport.ActorOnlyB, true},
		{"external member of a project the viewer accesses", testsupport.ActorOnlyA, testsupport.ActorExternal, true},
		{"external member of a project the viewer does not access", testsupport.ActorParticipantNoMembers, testsupport.ActorExternal, false},
		{"co-member of the external viewer's project", testsupport.ActorExternal, testsupport.ActorProjectOwner, true},
		{"participant of the space who is not the external viewer's co-member", testsupport.ActorExternal, testsupport.ActorParticipantNoMembers, false},
		{"admin and a person of a space he is not in", testsupport.ActorAdmin0, testsupport.ActorOnlyA, false},
		{"admin and a co-participant of his space", testsupport.ActorAdminA, testsupport.ActorOnlyA, true},
		{"outsider and a participant of a space", testsupport.ActorOutsider, testsupport.ActorOnlyA, false},
	}
	for _, c := range cases {
		t.Run(c.relation, func(t *testing.T) {
			person := w.f.User(t, c.person)
			listed := func(q string) bool {
				t.Helper()
				r := w.get(c.viewer, "/api/v1/users/directory?limit=100&q="+url.QueryEscape(q))
				if r.status != http.StatusOK {
					t.Fatalf("directory q=%q: %d %s", q, r.status, r.body)
				}
				var page users.DirectoryResult
				decodeInto(t, r, &page)
				for _, it := range page.Items {
					if it.ID == person.ID {
						return true
					}
				}
				return false
			}
			if got := listed(person.Username); got != c.visible {
				t.Errorf("directory by username: listed %v, want %v", got, c.visible)
			}
			if got := listed(nameOf[c.person][:len(nameOf[c.person])-2]); got != c.visible {
				t.Errorf("autocomplete by name prefix: listed %v, want %v", got, c.visible)
			}
			if listed(emailOf(c.person)) {
				t.Errorf("directory by e-mail found the person — the e-mail is searchable")
			}
			pr := w.get(c.viewer, "/api/v1/users/profile/"+person.Username)
			if c.visible {
				if pr.status != http.StatusOK {
					t.Errorf("profile: %d %s, want 200", pr.status, pr.body)
				}
				return
			}
			assertSameAsMissing(t, "profile", pr, w.get(c.viewer, "/api/v1/users/profile/"+missing))
		})
	}

	// In the context of an issue: uAB sees uB (space B) and uA (space A),
	// but a mention in an issue of A.space resolves only to the people of
	// space A — uB stays text.
	t.Run("mention resolves in the issue's context only", func(t *testing.T) {
		p := w.byKey[keyASpace]
		onlyA, onlyB := w.f.User(t, testsupport.ActorOnlyA), w.f.User(t, testsupport.ActorOnlyB)
		if _, err := w.pool.Exec(ctx, `UPDATE issues SET description = $2 WHERE id = $1`,
			p.IssueID, "@"+onlyA.Username+" @"+onlyB.Username); err != nil {
			t.Fatal(err)
		}
		r := w.get(testsupport.ActorBoth, "/api/v1/projects/"+p.ID.String()+"/issues/"+p.IssueID+"/")
		if r.status != http.StatusOK {
			t.Fatalf("card: %d %s", r.status, r.body)
		}
		var card struct {
			Mentions []struct {
				UserID uuid.UUID `json:"userId"`
			} `json:"mentions"`
		}
		decodeInto(t, r, &card)
		got := map[uuid.UUID]bool{}
		for _, m := range card.Mentions {
			got[m.UserID] = true
		}
		if !got[onlyA.ID] {
			t.Errorf("a participant of the issue's space is not resolved")
		}
		if got[onlyB.ID] {
			t.Errorf("a person of another space resolved in this issue's context")
		}
	})
}

// TestPeopleSpaces_NoForeignEmail — row 121. A member who owns nothing (uA)
// reads every surface that shows people of space A: the directory, the
// profiles, the members of a project, the space with its members, an issue
// card, its comments, its watchers, its activity and the project's
// activity. No answer carries the e-mail of any other user; every surface
// does carry other people (their username or id), so the absence is
// meaningful.
// The assignee's free text typed as an e-mail is the one exception: it is
// shown as typed. Red at any other user's e-mail.
func TestPeopleSpaces_NoForeignEmail(t *testing.T) {
	w := newIsoWorld(t)
	ctx := context.Background()
	viewer := testsupport.ActorOnlyA
	me := w.f.User(t, viewer)
	p := w.byKey[keyAMembers]
	other := w.f.User(t, testsupport.ActorProjectOwner)

	// People on the issue: a comment and a watch by others, a mention.
	testsupport.SeedComment(t, w.pool, p.IssueID, w.f.User(t, testsupport.ActorBoth).ID, "a comment by another person")
	if _, err := w.pool.Exec(ctx, `UPDATE issues SET description = $2 WHERE id = $1`,
		p.IssueID, "@"+other.Username+" please look"); err != nil {
		t.Fatal(err)
	}
	for _, a := range []testsupport.Actor{testsupport.ActorProjectOwner, testsupport.ActorBoth} {
		if _, err := w.pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, '{manual}')
ON CONFLICT DO NOTHING`, p.IssueID, w.f.User(t, a).ID); err != nil {
			t.Fatalf("seed watcher %s: %v", a, err)
		}
	}
	var all []uuid.UUID
	for _, a := range testsupport.TenancyActors {
		all = append(all, w.f.User(t, a).ID)
	}

	rows, err := w.pool.Query(ctx, `SELECT email FROM users WHERE id = ANY($1) AND email IS NOT NULL AND email <> ''`, all)
	if err != nil {
		t.Fatal(err)
	}
	var foreign []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		var mine string
		_ = w.pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, me.ID).Scan(&mine)
		if !strings.EqualFold(e, mine) {
			foreign = append(foreign, strings.ToLower(e))
		}
	}
	rows.Close()
	if len(foreign) == 0 {
		t.Fatal("the fixture has no e-mails of other users — the check would prove nothing")
	}

	var spaceShort string
	if err := w.pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, w.f.SpaceA).Scan(&spaceShort); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/projects/" + p.ID.String()
	surfaces := map[string]string{
		"directory":        "/api/v1/users/directory?limit=100",
		"profile":          "/api/v1/users/profile/" + other.Username,
		"project members":  base + "/members",
		"space":            "/api/v1/spaces/" + spaceShort,
		"issue card":       base + "/issues/" + p.IssueID + "/",
		"comments":         base + "/issues/" + p.IssueID + "/comments",
		"watchers":         "/api/v1/issues/" + p.IssueID + "/watchers",
		"issue activity":   base + "/issues/" + p.IssueID + "/activity",
		"project activity": base + "/activity",
	}
	for name, path := range surfaces {
		t.Run(name, func(t *testing.T) {
			r := w.get(viewer, path)
			if r.status != http.StatusOK {
				t.Fatalf("GET %s: %d %s", path, r.status, r.body)
			}
			body := bytes.ToLower(r.body)
			carriesPeople := false
			for _, a := range testsupport.TenancyActors {
				u := w.f.User(t, a)
				if a != viewer && (bytes.Contains(body, []byte(strings.ToLower(u.Username))) ||
					bytes.Contains(body, []byte(u.ID.String()))) {
					carriesPeople = true
				}
			}
			if !carriesPeople {
				t.Fatalf("%s carries no other person — the e-mail check would prove nothing: %s", name, r.body)
			}
			for _, e := range foreign {
				if bytes.Contains(body, []byte(e)) {
					t.Errorf("%s carries another user's e-mail %q", name, e)
				}
			}
		})
	}

	// The exception: the assignee's free text typed as an e-mail is shown
	// as typed to the project's members.
	t.Run("assignee free text e-mail is shown as typed", func(t *testing.T) {
		typed := "outside-" + w.word + "@nowhere.example"
		testsupport.SetAssignee(t, w.pool, p.IssueID, typed)
		r := w.get(viewer, base+"/issues/"+p.IssueID+"/")
		var card struct {
			Assignee string `json:"assignee"`
		}
		decodeInto(t, r, &card)
		if card.Assignee != typed {
			t.Errorf("assignee = %q, want the free text %q", card.Assignee, typed)
		}
	})
}

// TestPeopleSpaces_DepartedAndDeletedMarks — row 122 (INV-18, US-13, E-22).
// In an issue of A.space: uAB commented and is the assignee, then left
// space A; uPN commented, is mentioned, and his account was deleted. The
// space owner reads the card, the comments, the issue's activity, the
// space and the members of A.members (uPN is made a member of it first).
// Red if the departed person is not marked departed, or the deleted one is
// not marked deleted with his pseudonym, on any of them.
func TestPeopleSpaces_DepartedAndDeletedMarks(t *testing.T) {
	w := newIsoWorld(t)
	ctx := context.Background()
	reader := testsupport.ActorSpaceOwner
	p := w.byKey[keyASpace]
	departed := w.f.User(t, testsupport.ActorBoth)
	deleted := w.f.User(t, testsupport.ActorParticipantNoMembers)
	members := w.byKey[keyAMembers]
	testsupport.AddProjectMember(t, w.pool, members.ID, deleted.ID, "member")

	testsupport.SeedComment(t, w.pool, p.IssueID, departed.ID, "before leaving")
	testsupport.SeedComment(t, w.pool, p.IssueID, deleted.ID, "before deletion")
	testsupport.SetAssignee(t, w.pool, p.IssueID, departed.Username)
	if _, err := w.pool.Exec(ctx, `UPDATE issues SET description = $2 WHERE id = $1`,
		p.IssueID, "ask @"+deleted.Username); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_type, actor_user_id, payload)
VALUES (now(), 'issue.updated', 'issue', $1, $2, $3, 'human', $4, '{}'::jsonb)`, p.IssueID, p.ID, deleted.Username, deleted.ID); err != nil {
		t.Fatalf("seed an event by the person to be deleted: %v", err)
	}

	// uAB leaves space A: no participation, no membership of its projects.
	for _, q := range []string{
		`DELETE FROM project_members WHERE user_id = $1 AND project_id IN (SELECT id FROM projects WHERE space_id = $2)`,
		`DELETE FROM space_members WHERE user_id = $1 AND space_id = $2`,
	} {
		if _, err := w.pool.Exec(ctx, q, departed.ID, w.f.SpaceA); err != nil {
			t.Fatalf("leave space A: %v", err)
		}
	}
	// uPN's account is deleted (the shape of an account deletion: the
	// timestamp and the permanent pseudonym).
	var pseudonym string
	if err := w.pool.QueryRow(ctx, `
UPDATE users SET deleted_at = now(), pseudonym = 'deleted-' || substr(md5(id::text), 1, 8)
 WHERE id = $1 RETURNING pseudonym`, deleted.ID).Scan(&pseudonym); err != nil {
		t.Fatalf("delete the account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(), `UPDATE users SET deleted_at = NULL, pseudonym = NULL WHERE id = $1`, deleted.ID)
	})

	type marks struct {
		Departed  bool    `json:"departed"`
		Deleted   bool    `json:"deleted"`
		Pseudonym *string `json:"pseudonym"`
	}
	wantDeleted := func(t *testing.T, where string, m *marks) {
		t.Helper()
		if m == nil || !m.Deleted || m.Pseudonym == nil || *m.Pseudonym != pseudonym {
			t.Errorf("%s: marks %+v, want deleted with pseudonym %q", where, m, pseudonym)
		}
	}
	wantDeparted := func(t *testing.T, where string, m *marks) {
		t.Helper()
		if m == nil || !m.Departed || m.Deleted {
			t.Errorf("%s: marks %+v, want departed", where, m)
		}
	}
	base := "/api/v1/projects/" + p.ID.String() + "/issues/" + p.IssueID

	t.Run("issue card", func(t *testing.T) {
		r := w.get(reader, base+"/")
		if r.status != http.StatusOK {
			t.Fatalf("card: %d %s", r.status, r.body)
		}
		var card struct {
			AssigneeStatus *marks `json:"assigneeStatus"`
			Mentions       []struct {
				UserID uuid.UUID `json:"userId"`
				Status *marks    `json:"status"`
			} `json:"mentions"`
		}
		decodeInto(t, r, &card)
		wantDeparted(t, "assignee", card.AssigneeStatus)
		found := false
		for _, m := range card.Mentions {
			if m.UserID == deleted.ID {
				found = true
				wantDeleted(t, "mention", m.Status)
			}
		}
		if !found {
			t.Errorf("the mention of the deleted account is not shown (want it with the pseudonym)")
		}
	})

	t.Run("comments", func(t *testing.T) {
		r := w.get(reader, base+"/comments")
		if r.status != http.StatusOK {
			t.Fatalf("comments: %d %s", r.status, r.body)
		}
		var page struct {
			Items []struct {
				AuthorUserID *uuid.UUID `json:"authorUserId"`
				AuthorStatus *marks     `json:"authorStatus"`
			} `json:"items"`
		}
		if err := json.Unmarshal(r.body, &page); err != nil || len(page.Items) == 0 {
			var flat []struct {
				AuthorUserID *uuid.UUID `json:"authorUserId"`
				AuthorStatus *marks     `json:"authorStatus"`
			}
			if err := json.Unmarshal(r.body, &flat); err != nil {
				t.Fatalf("decode comments %s: %v", r.body, err)
			}
			page.Items = flat
		}
		seen := 0
		for _, c := range page.Items {
			switch {
			case c.AuthorUserID != nil && *c.AuthorUserID == departed.ID:
				seen++
				wantDeparted(t, "comment of the departed", c.AuthorStatus)
			case c.AuthorUserID != nil && *c.AuthorUserID == deleted.ID:
				seen++
				wantDeleted(t, "comment of the deleted", c.AuthorStatus)
			}
		}
		if seen != 2 {
			t.Fatalf("comments of the two people: %d listed, want 2 (%s)", seen, r.body)
		}
	})

	t.Run("issue activity", func(t *testing.T) {
		r := w.get(reader, base+"/activity")
		if r.status != http.StatusOK {
			t.Fatalf("activity: %d %s", r.status, r.body)
		}
		var page struct {
			Items []struct {
				Actor struct {
					UserID *uuid.UUID `json:"userId"`
					marks
				} `json:"actor"`
			} `json:"items"`
		}
		decodeInto(t, r, &page)
		found := false
		for _, e := range page.Items {
			if e.Actor.UserID != nil && *e.Actor.UserID == deleted.ID {
				found = true
				m := e.Actor.marks
				wantDeleted(t, "activity actor", &m)
			}
		}
		if !found {
			t.Fatalf("the event by the deleted account is not in the activity: %s", r.body)
		}
	})

	t.Run("space and project members", func(t *testing.T) {
		var short string
		if err := w.pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, w.f.SpaceA).Scan(&short); err != nil {
			t.Fatal(err)
		}
		for where, path := range map[string]string{
			"space members":   "/api/v1/spaces/" + short,
			"project members": "/api/v1/projects/" + members.ID.String() + "/members",
		} {
			r := w.get(reader, path)
			if r.status != http.StatusOK {
				t.Fatalf("%s: %d %s", where, r.status, r.body)
			}
			var withMembers struct {
				Members []struct {
					UserID uuid.UUID `json:"userId"`
					marks
				} `json:"members"`
				Items []struct {
					UserID uuid.UUID `json:"userId"`
					marks
				} `json:"items"`
			}
			if err := json.Unmarshal(r.body, &withMembers); err != nil {
				var flat []struct {
					UserID uuid.UUID `json:"userId"`
					marks
				}
				if err := json.Unmarshal(r.body, &flat); err != nil {
					t.Fatalf("decode %s %s: %v", where, r.body, err)
				}
				withMembers.Members = flat
			}
			rows := append(withMembers.Members, withMembers.Items...)
			found := false
			for _, m := range rows {
				if m.UserID == deleted.ID {
					found = true
					mk := m.marks
					wantDeleted(t, where, &mk)
				}
			}
			if !found {
				t.Errorf("%s does not list the deleted member: %s", where, r.body)
			}
		}
	})
}

// TestPeopleSpaces_DeletedMentionNeedsRelation — rows 120/122/123 (INV-9,
// E-22). Reference — the rule as the spec words
// it: a mention resolves to a person of the issue's context; a deleted
// account keeps its pseudonym where it was mentioned with a relation to the
// issue (E-22), and deletion alone grants nothing. A deleted account of
// another space with no relation to the issue is mentioned in the issue
// and in a comment: neither the card nor the comment resolves it — the
// answer equals the one for a name nobody has. Positive control: a deleted
// account of the same other space that acted on the issue resolves, with
// its pseudonym.
func TestPeopleSpaces_DeletedMentionNeedsRelation(t *testing.T) {
	w := newIsoWorld(t)
	ctx := context.Background()
	reader := testsupport.ActorSpaceOwner
	p := w.byKey[keyASpace]
	deletedInB := func(t *testing.T) (uuid.UUID, string) {
		t.Helper()
		id, name := testsupport.SeedUser(t, w.pool, "user")
		if _, err := w.pool.Exec(ctx, `INSERT INTO space_members (space_id, user_id, role) VALUES ($1, $2, 'member')`, w.f.SpaceB, id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), `DELETE FROM space_members WHERE user_id = $1`, id) })
		return id, name
	}
	stranger, strangerName := deletedInB(t)
	linked, linkedName := deletedInB(t)
	// linked acted on the issue (an event of his) before his deletion.
	if _, err := w.pool.Exec(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_type, actor_user_id, payload)
VALUES (now(), 'issue.updated', 'issue', $1, $2, $3, 'human', $4, '{}'::jsonb)`, p.IssueID, p.ID, linkedName, linked); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{stranger, linked} {
		if _, err := w.pool.Exec(ctx, `
UPDATE users SET deleted_at = now(), pseudonym = 'deleted-' || substr(md5(id::text), 1, 8) WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	nobody := "nobody" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	text := "ask @" + strangerName + " @" + linkedName + " @" + nobody
	if _, err := w.pool.Exec(ctx, `UPDATE issues SET description = $2 WHERE id = $1`, p.IssueID, text); err != nil {
		t.Fatal(err)
	}
	testsupport.SeedComment(t, w.pool, p.IssueID, w.f.User(t, reader).ID, text)
	base := "/api/v1/projects/" + p.ID.String() + "/issues/" + p.IssueID

	type mention struct {
		UserID   uuid.UUID `json:"userId"`
		Username string    `json:"username"`
	}
	check := func(t *testing.T, where string, ms []mention, body []byte) {
		t.Helper()
		var gotLinked bool
		for _, m := range ms {
			if m.UserID == stranger {
				t.Errorf("%s: the deleted account without a relation to the issue resolves", where)
			}
			if m.UserID == linked {
				gotLinked = true
			}
		}
		if bytes.Contains(body, []byte(stranger.String())) {
			t.Errorf("%s: the id of the deleted account without a relation appears", where)
		}
		if !gotLinked {
			t.Errorf("%s: the deleted account that acted on the issue does not resolve (positive control)", where)
		}
	}
	t.Run("issue card", func(t *testing.T) {
		r := w.get(reader, base+"/")
		if r.status != http.StatusOK {
			t.Fatalf("card: %d %s", r.status, r.body)
		}
		var card struct {
			Mentions []mention `json:"mentions"`
		}
		decodeInto(t, r, &card)
		check(t, "card", card.Mentions, r.body)
	})
	t.Run("comments", func(t *testing.T) {
		r := w.get(reader, base+"/comments")
		if r.status != http.StatusOK {
			t.Fatalf("comments: %d %s", r.status, r.body)
		}
		var comments []struct {
			Mentions []mention `json:"mentions"`
		}
		if err := json.Unmarshal(r.body, &comments); err != nil {
			t.Fatalf("decode comments %s: %v", r.body, err)
		}
		if len(comments) == 0 {
			t.Fatalf("the comment with the mentions is not listed")
		}
		var all []mention
		for _, c := range comments {
			all = append(all, c.Mentions...)
		}
		check(t, "comments", all, r.body)
	})
}
