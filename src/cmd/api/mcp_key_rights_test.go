package main

// mcp_key_rights_test.go — matrix rows 31, 32 and 132 (SC-002, SC-013,
// SC-015, SC-017, INV-1, US-2, US-13). Reference — the spec, written before
// the code: the rights of a key apply in /mcp ("MCP changes"); a call the
// key's right does not cover answers insufficient_scope with the required
// right and writes nothing, and the right is checked before the object is
// looked up; force=true on a guarded tool needs manage and the owner's
// force_unlock role; force on skein_create stays write; a backfilled key
// keeps the previous behaviour; an object outside the key's scope answers as
// a nonexistent one even where the owner's role would refuse it.
//
// Every call goes through the real /mcp of the production router, so the
// grant rides in the per-RPC context exactly as for an agent.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

// mintGrantKey is mintScopedKey with the whole grant chosen by the test.
func mintGrantKey(t *testing.T, s *parityStack, a testsupport.Actor, g apikeys.Grant) *scopedKey {
	t.Helper()
	u := s.f.User(t, a)
	plain, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	c, err := apikeys.New(s.pool).Create(context.Background(), apikeys.NewKey{
		UserID: u.ID, Label: "mcp key rights", Prefix: prefix, Hash: hash, Grant: g,
	}, testsupport.KeyTTLPolicy)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, c.ID)
	})
	k := &scopedKey{s: s, user: u, id: c.ID, plain: plain, grant: g}
	k.sid = k.initialize(t)
	return k
}

// mintBackfilledKey inserts a key the way the grant migration backfilled the
// keys that existed before grants: manage, admin when the owner is an
// instance admin, scope all, no expiry.
func mintBackfilledKey(t *testing.T, s *parityStack, a testsupport.Actor) *scopedKey {
	t.Helper()
	u := s.f.User(t, a)
	plain, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	id := uuid.New()
	if _, err := s.pool.Exec(context.Background(), `
INSERT INTO api_keys (id, user_id, key_hash, prefix, label, created_at, level, is_admin, scope_mode)
VALUES ($1, $2, $3, $4, 'backfilled key', now(), 'manage', (SELECT role = 'admin' FROM users WHERE id = $2), 'all')`,
		id, u.ID, hash, prefix); err != nil {
		t.Fatalf("insert backfilled key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, id)
	})
	k := &scopedKey{s: s, user: u, id: id, plain: plain}
	k.sid = k.initialize(t)
	return k
}

func grantAll(level apikeys.Level) apikeys.Grant {
	return apikeys.Grant{Level: level, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}}
}

// toolErr decodes an error envelope: the code and the `required` right.
func toolErr(t *testing.T, res toolResult) (code, required string) {
	t.Helper()
	if !res.isError {
		t.Fatalf("want an error, got success: %s", res.text)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(res.text), &m); err != nil {
		t.Fatalf("decode error %s: %v", res.text, err)
	}
	code, _ = m["error"].(string)
	required, _ = m["required"].(string)
	return code, required
}

func issueVersion(t *testing.T, s *parityStack, id string) int {
	t.Helper()
	var v int
	if err := s.pool.QueryRow(context.Background(), `SELECT version FROM issues WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read version of %s: %v", id, err)
	}
	return v
}

func issueCommitCount(t *testing.T, s *parityStack, id string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM issue_commits WHERE issue_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count commits of %s: %v", id, err)
	}
	return n
}

func TestMCPKeyRights_RefusedBeforeTheTool(t *testing.T) {
	s := newParityStack(t)
	f := s.f
	P := f.ASpace
	member := f.User(t, testsupport.ActorParticipantNoMembers) // participant of A, no force_unlock in A.space
	missing := P.IDPrefix + "-999999"

	t.Run("read key on a writing tool: insufficient_scope before the lookup, nothing written", func(t *testing.T) {
		k := mintGrantKey(t, s, testsupport.ActorParticipantNoMembers, grantAll(apikeys.LevelRead))
		before := issueVersion(t, s, P.IssueID)
		for _, id := range []string{P.IssueID, missing} {
			res := k.tool(t, "skein_update", map[string]any{"id": id, "expectedVersion": before, "title": "renamed by a read key"})
			code, required := toolErr(t, res)
			if code != "insufficient_scope" || required != "write" {
				t.Errorf("skein_update %s with a read key = %s/%s, want insufficient_scope/write: %s", id, code, required, res.text)
			}
		}
		if after := issueVersion(t, s, P.IssueID); after != before {
			t.Errorf("version of %s = %d after the refusal, want %d", P.IssueID, after, before)
		}
	})

	t.Run("grouped tool: inbox passes, mark_read_all is refused", func(t *testing.T) {
		k := mintGrantKey(t, s, testsupport.ActorParticipantNoMembers, grantAll(apikeys.LevelRead))
		if res := k.tool(t, "skein_notifications", map[string]any{"action": "inbox"}); res.isError {
			t.Errorf("inbox with a read key: %s", res.text)
		}
		res := k.tool(t, "skein_notifications", map[string]any{"action": "mark_read_all"})
		if code, required := toolErr(t, res); code != "insufficient_scope" || required != "write" {
			t.Errorf("mark_read_all with a read key = %s/%s, want insufficient_scope/write", code, required)
		}
	})

	t.Run("write key with force=true on a guarded tool: insufficient_scope/manage, nothing applied", func(t *testing.T) {
		k := mintGrantKey(t, s, testsupport.ActorParticipantNoMembers, grantAll(apikeys.LevelWrite))
		second := testsupport.SeedIssue(t, s.pool, P.ID, member.ID, P.IDPrefix+"-951", "second of a batch", "open", 2)
		v1, v2 := issueVersion(t, s, P.IssueID), issueVersion(t, s, second)
		cases := []struct {
			tool string
			args map[string]any
		}{
			{"skein_update", map[string]any{"id": P.IssueID, "expectedVersion": v1, "title": "forced", "force": true}},
			{"skein_close", map[string]any{"id": P.IssueID, "expectedVersion": v1, "reason": "forced", "closeOutcome": "done", "force": true}},
			{"skein_unclaim", map[string]any{"id": P.IssueID, "nextStatus": "open", "reason": "forced", "force": true}},
			{"skein_commits_attach", map[string]any{"issueId": P.IssueID, "commits": []any{map[string]any{"hash": "abcdef1"}}, "force": true}},
			{"skein_commits_detach", map[string]any{"issueId": P.IssueID, "hashes": []any{"abcdef1"}, "force": true}},
			{"skein_update_many", map[string]any{"items": []any{
				map[string]any{"id": P.IssueID, "expectedVersion": v1, "title": "forced"},
				map[string]any{"id": second, "expectedVersion": v2, "title": "forced"},
			}, "force": true}},
			{"skein_close_many", map[string]any{"items": []any{
				map[string]any{"id": P.IssueID, "expectedVersion": v1, "reason": "forced", "closeOutcome": "done"},
				map[string]any{"id": second, "expectedVersion": v2, "reason": "forced", "closeOutcome": "done"},
			}, "force": true}},
		}
		for _, tc := range cases {
			t.Run(tc.tool, func(t *testing.T) {
				res := k.tool(t, tc.tool, tc.args)
				if code, required := toolErr(t, res); code != "insufficient_scope" || required != "manage" {
					t.Errorf("%s force=true with a write key = %s/%s, want insufficient_scope/manage: %s", tc.tool, code, required, res.text)
				}
				if got := issueVersion(t, s, P.IssueID); got != v1 {
					t.Errorf("version of %s = %d, want %d", P.IssueID, got, v1)
				}
				if got := issueVersion(t, s, second); got != v2 {
					t.Errorf("version of %s = %d, want %d", second, got, v2)
				}
				if n := issueCommitCount(t, s, P.IssueID); n != 0 {
					t.Errorf("%s has %d commit refs, want 0", P.IssueID, n)
				}
			})
		}
	})

	t.Run("manage key without the owner's force_unlock role: force_unlock_forbidden", func(t *testing.T) {
		k := mintGrantKey(t, s, testsupport.ActorParticipantNoMembers, grantAll(apikeys.LevelManage))
		v := issueVersion(t, s, P.IssueID)
		res := k.tool(t, "skein_update", map[string]any{"id": P.IssueID, "expectedVersion": v, "title": "forced", "force": true})
		if code, _ := toolErr(t, res); code != "force_unlock_forbidden" {
			t.Errorf("skein_update force=true, manage key, no role = %s, want force_unlock_forbidden: %s", code, res.text)
		}
		if got := issueVersion(t, s, P.IssueID); got != v {
			t.Errorf("version of %s = %d, want %d", P.IssueID, got, v)
		}
	})

	t.Run("manage key with the owner's force_unlock role: force=true passes", func(t *testing.T) {
		k := mintGrantKey(t, s, testsupport.ActorSpaceOwner, grantAll(apikeys.LevelManage))
		v := issueVersion(t, s, P.IssueID)
		res := k.tool(t, "skein_update", map[string]any{"id": P.IssueID, "expectedVersion": v, "title": "forced by the space owner", "force": true})
		if res.isError {
			t.Fatalf("skein_update force=true, manage key, space owner: %s", res.text)
		}
		if got := issueVersion(t, s, P.IssueID); got <= v {
			t.Errorf("version of %s = %d, want above %d", P.IssueID, got, v)
		}
	})

	t.Run("skein_create force=true with a write key passes", func(t *testing.T) {
		k := mintGrantKey(t, s, testsupport.ActorParticipantNoMembers, grantAll(apikeys.LevelWrite))
		res := k.tool(t, "skein_create", map[string]any{
			"project": P.ID.String(), "title": "created with force by a write key", "type": "task", "force": true, "kind": "autonomous",
		})
		if res.isError {
			t.Fatalf("skein_create force=true with a write key: %s", res.text)
		}
	})
}

// TestMCPKeyRights_BackfilledKeyKeepsThePreviousBehaviour — row 32: a key
// that existed before grants (backfilled to manage, admin for an admin
// owner, scope all, no expiry) still reads, writes, forces and manages users.
func TestMCPKeyRights_BackfilledKeyKeepsThePreviousBehaviour(t *testing.T) {
	s := newParityStack(t)
	P := s.f.ASpace
	k := mintBackfilledKey(t, s, testsupport.ActorAdminA)

	if res := k.tool(t, "skein_show", map[string]any{"id": P.IssueID}); res.isError {
		t.Errorf("read (skein_show): %s", res.text)
	}
	if res := k.tool(t, "skein_comment", map[string]any{"issueId": P.IssueID, "body": "written by a backfilled key", "kind": "autonomous"}); res.isError {
		t.Errorf("write (skein_comment): %s", res.text)
	}
	v := issueVersion(t, s, P.IssueID)
	if res := k.tool(t, "skein_update", map[string]any{"id": P.IssueID, "expectedVersion": v, "title": "forced by a backfilled key", "force": true}); res.isError {
		t.Errorf("force=true (skein_update): %s", res.text)
	}
	if res := k.tool(t, "skein_user", map[string]any{"action": "list"}); res.isError {
		t.Errorf("users (skein_user list) by an admin owner: %s", res.text)
	}
}

// TestMCPKeyRights_OutsideTheScopeBeforeTheRole — row 132: with a key of
// scope P, an object of Q answers as a nonexistent one, even where the
// owner's role would refuse it with forbidden — the role is checked after the
// scope and never reveals the object.
func TestMCPKeyRights_OutsideTheScopeBeforeTheRole(t *testing.T) {
	s := newParityStack(t)
	f := s.f
	P, Q := f.ASpace, f.AMembers
	other := f.User(t, testsupport.ActorProjectOwner) // owner of A.members: someone else's comments in Q
	k := mintScopedKey(t, s, testsupport.ActorBoth, apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{P.ID}})
	foreign := testsupport.SeedComment(t, s.pool, Q.IssueID, other.ID, "a comment of someone else in Q")
	missingComment := uuid.NewString()
	missingIssue := Q.IDPrefix + "-999999"

	t.Run("skein_update of an issue of Q", func(t *testing.T) {
		v := issueVersion(t, s, Q.IssueID)
		inQ := k.tool(t, "skein_update", map[string]any{"id": Q.IssueID, "expectedVersion": v, "title": "renamed outside the scope"})
		none := k.tool(t, "skein_update", map[string]any{"id": missingIssue, "expectedVersion": v, "title": "renamed outside the scope"})
		if !inQ.isError || errShape(t, []byte(inQ.text)) != errShape(t, []byte(none.text)) {
			t.Errorf("skein_update of %s = %s, want the answer for a nonexistent issue %s", Q.IssueID, inQ.text, none.text)
		}
		if got := issueVersion(t, s, Q.IssueID); got != v {
			t.Errorf("version of %s = %d, want %d", Q.IssueID, got, v)
		}
	})

	t.Run("skein_comment_delete of someone else's comment in Q", func(t *testing.T) {
		inQ := k.tool(t, "skein_comment_delete", map[string]any{"commentId": foreign.String()})
		none := k.tool(t, "skein_comment_delete", map[string]any{"commentId": missingComment})
		if !inQ.isError || errShape(t, []byte(inQ.text)) != errShape(t, []byte(none.text)) {
			t.Errorf("skein_comment_delete in Q = %s, want the answer for a nonexistent comment %s", inQ.text, none.text)
		}
		if code, _ := toolErr(t, inQ); code == "forbidden" {
			t.Errorf("skein_comment_delete in Q = forbidden: the role check revealed the comment")
		}
	})

	t.Run("skein_comment_update of someone else's comment in Q", func(t *testing.T) {
		inQ := k.tool(t, "skein_comment_update", map[string]any{"commentId": foreign.String(), "body": "edited outside the scope", "expectedVersion": 1})
		none := k.tool(t, "skein_comment_update", map[string]any{"commentId": missingComment, "body": "edited outside the scope", "expectedVersion": 1})
		if !inQ.isError || errShape(t, []byte(inQ.text)) != errShape(t, []byte(none.text)) {
			t.Errorf("skein_comment_update in Q = %s, want the answer for a nonexistent comment %s", inQ.text, none.text)
		}
		if code, _ := toolErr(t, inQ); code == "forbidden" {
			t.Errorf("skein_comment_update in Q = forbidden: the authorship check revealed the comment")
		}
	})
}

// TestMCPKeyRights_CreateProjectInsideTheKeysSpaces — review C4-01 (INV-2,
// createProject "manage, scope spaces or all, inside the chosen spaces"): a
// key limited to spaces creates a project only in its spaces. A space
// outside them answers as one the owner does not see, is never picked and
// is never listed in space_required. uAB is a participant of A, of B and of
// his own Personal.
func TestMCPKeyRights_CreateProjectInsideTheKeysSpaces(t *testing.T) {
	s := newParityStack(t)
	f := s.f
	ctx := context.Background()
	shortOf := func(id uuid.UUID) string {
		var short string
		if err := s.pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, id).Scan(&short); err != nil {
			t.Fatalf("short id of space %s: %v", id, err)
		}
		return short
	}
	aShort, bShort := shortOf(f.SpaceA), shortOf(f.SpaceB)
	personalShort := shortOf(f.PersonalSpace[testsupport.ActorBoth])
	spacesKey := func(ids ...uuid.UUID) apikeys.Grant {
		return apikeys.Grant{Level: apikeys.LevelManage, Scope: apikeys.Scope{Mode: apikeys.ScopeSpaces, Spaces: ids}}
	}
	newPrefix := func() string { return "c401" + strings.ReplaceAll(uuid.NewString()[:6], "-", "") }
	spaceOfProject := func(prefix string) (uuid.UUID, bool) {
		var space uuid.UUID
		err := s.pool.QueryRow(ctx, `SELECT space_id FROM projects WHERE id_prefix = $1`, prefix).Scan(&space)
		if err != nil {
			return uuid.Nil, false
		}
		return space, true
	}
	forgetPrefix := func(prefix string) {
		t.Cleanup(func() {
			c := context.Background()
			var id uuid.UUID
			if err := s.pool.QueryRow(c, `SELECT id FROM projects WHERE id_prefix = $1`, prefix).Scan(&id); err != nil {
				return
			}
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

	t.Run("a named space outside the key: as a nonexistent one, nothing created", func(t *testing.T) {
		k := mintGrantKey(t, s, testsupport.ActorBoth, spacesKey(f.SpaceA))
		prefix := newPrefix()
		forgetPrefix(prefix)
		outside := k.tool(t, "skein_project_create", map[string]any{"name": prefix, "idPrefix": prefix, "space": bShort})
		none := k.tool(t, "skein_project_create", map[string]any{"name": prefix, "idPrefix": prefix, "space": "aaaaaaaa"})
		if !outside.isError || errShape(t, []byte(outside.text)) != errShape(t, []byte(none.text)) {
			t.Errorf("create in B with a key of A = %s, want the answer for a nonexistent space %s", outside.text, none.text)
		}
		if code, _ := toolErr(t, outside); code != "space_not_found" {
			t.Errorf("create in B with a key of A = %s, want space_not_found", code)
		}
		if _, created := spaceOfProject(prefix); created {
			t.Errorf("a project %s was created outside the key's spaces", prefix)
		}
	})

	t.Run("no space named: the only space of the key is picked", func(t *testing.T) {
		k := mintGrantKey(t, s, testsupport.ActorBoth, spacesKey(f.SpaceA))
		prefix := newPrefix()
		forgetPrefix(prefix)
		res := k.tool(t, "skein_project_create", map[string]any{"name": prefix, "idPrefix": prefix})
		if res.isError {
			t.Fatalf("create without space with a key of A: %s", res.text)
		}
		if got, ok := spaceOfProject(prefix); !ok || got != f.SpaceA {
			t.Errorf("project %s went to space %s, want A %s", prefix, got, f.SpaceA)
		}
	})

	t.Run("space_required lists only the key's spaces", func(t *testing.T) {
		k := mintGrantKey(t, s, testsupport.ActorBoth, spacesKey(f.SpaceA, f.SpaceB))
		prefix := newPrefix()
		forgetPrefix(prefix)
		res := k.tool(t, "skein_project_create", map[string]any{"name": prefix, "idPrefix": prefix})
		if code, _ := toolErr(t, res); code != "space_required" {
			t.Fatalf("create without space with a key of A and B = %s, want space_required", res.text)
		}
		var body struct {
			Spaces []struct {
				ID string `json:"id"`
			} `json:"spaces"`
		}
		if err := json.Unmarshal([]byte(res.text), &body); err != nil {
			t.Fatalf("decode %s: %v", res.text, err)
		}
		listed := map[string]bool{}
		for _, sp := range body.Spaces {
			listed[sp.ID] = true
		}
		if !listed[aShort] || !listed[bShort] || len(listed) != 2 {
			t.Errorf("space_required lists %v, want exactly A %s and B %s", body.Spaces, aShort, bShort)
		}
		if listed[personalShort] {
			t.Errorf("space_required lists the Personal %s, outside the key's spaces", personalShort)
		}
		if _, created := spaceOfProject(prefix); created {
			t.Errorf("a project %s was created on space_required", prefix)
		}
	})
}
