// Package issues — ListUserActivity redaction integration tests (war-room
// cross-project feed). Mirrors the fixtures in store_test.go: the
// TEST_DATABASE_URL gate via newTestPool, seedUser / seedProject /
// seedIssue. A local username lookup + event seeder are added here because
// the package's seedUser returns only the uuid and there is no shared
// event-seeding helper.
package issues

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/redact"
	"skein-api/internal/testsupport"
)

// usernameOf reads the username for a seeded user so the test can assert
// Actor.Username on the resulting ActivityEntry (seedUser returns only
// the uuid).
func usernameOf(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) string {
	t.Helper()
	var uname string
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, userID).Scan(&uname); err != nil {
		t.Fatalf("lookup username for %s: %v", userID, err)
	}
	return uname
}

// seedIssueEvent writes one issue-scoped events row. Mirrors the column
// set proven by the dashboard package's seedEventForIssue helper; the row
// is read back through activityListSQL (joined to issues + projects).
func seedIssueEvent(t *testing.T, pool *pgxpool.Pool, eventType, issueID string, projectID uuid.UUID, actor string, actorUserID uuid.UUID, occurredAt time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
VALUES ($1, $2, $3, $4, 'human', 'issue', $5, $6, '{}'::jsonb)`,
		occurredAt, eventType, actor, actorUserID, issueID, projectID)
	if err != nil {
		t.Fatalf("seed event %s/%s: %v", eventType, issueID, err)
	}
	// events have no FK CASCADE from issues; clean up explicitly so the
	// shared DB doesn't accumulate rows across parallel runs.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM events WHERE type=$1 AND entity_id=$2 AND project_id=$3`,
			eventType, issueID, projectID)
	})
}

// findActivity returns the ActivityEntry for a given issue id, or fails.
func findActivity(t *testing.T, items []ActivityEntry, issueID string) ActivityEntry {
	t.Helper()
	for _, e := range items {
		if e.Entity.ID == issueID {
			return e
		}
	}
	ids := make([]string, 0, len(items))
	for _, e := range items {
		ids = append(ids, e.Entity.ID)
	}
	t.Fatalf("activity for issue %q not found (had %v)", issueID, ids)
	return ActivityEntry{}
}

// TestListUserActivity_RedactInvisible seeds an issue.created lifecycle
// event in each of two projects — one the caller is a member of, one
// foreign. With redactInvisible=true (war-room /dashboard/feed contract)
// BOTH rows are returned, but the foreign-project row has its
// Title/Summary stripped and Redacted=true, while identity fields
// (entity id, project prefix, actor) survive. The member-project row is
// full and not redacted.
func TestListUserActivity_RedactInvisible(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedUser(t, pool)
	foreignOwner := seedUser(t, pool)
	memberPid, memberPrefix := seedProject(t, pool, caller)
	foreignPid, foreignPrefix := seedProject(t, pool, foreignOwner)

	callerUname := usernameOf(t, pool, caller)
	foreignUname := usernameOf(t, pool, foreignOwner)

	now := time.Now().UTC()
	memberID := memberPrefix + "-1"
	foreignID := foreignPrefix + "-1"
	seedIssue(t, pool, memberPid, caller, memberID, "member issue title")
	seedIssue(t, pool, foreignPid, foreignOwner, foreignID, "foreign issue title")
	seedIssueEvent(t, pool, "issue.created", memberID, memberPid, callerUname, caller, now.Add(-1*time.Hour))
	seedIssueEvent(t, pool, "issue.created", foreignID, foreignPid, foreignUname, foreignOwner, now.Add(-30*time.Minute))

	// Caller is a member of memberPid only.
	visible := map[uuid.UUID]struct{}{memberPid: {}}
	// The foreign project is in the caller's reach (sk-674.18 T030: a project
	// of his own space he does not see) — its rows come redacted.
	reach := map[uuid.UUID]struct{}{memberPid: {}, foreignPid: {}}
	items, _, err := store.ListUserActivity(ctx, caller, visible, false /*isAdmin*/, nil, nil, 100, reach, redact.Soft)
	if err != nil {
		t.Fatalf("ListUserActivity: %v", err)
	}

	member := findActivity(t, items, memberID)
	foreign := findActivity(t, items, foreignID)

	// Member-project row: full content, not redacted.
	if member.Redacted {
		t.Errorf("member-project row should NOT be redacted: %+v", member)
	}
	if member.Entity.Title != "member issue title" {
		t.Errorf("member-project row Title = %q, want %q", member.Entity.Title, "member issue title")
	}
	if member.Summary == "" {
		t.Errorf("member-project row Summary should be populated, got empty")
	}

	// Foreign-project row: content stripped, identity kept.
	if !foreign.Redacted {
		t.Errorf("foreign-project row should be redacted: %+v", foreign)
	}
	if foreign.Entity.Title != "" {
		t.Errorf("foreign-project row Title should be stripped, got %q", foreign.Entity.Title)
	}
	if foreign.Summary != "" {
		t.Errorf("foreign-project row Summary should be stripped, got %q", foreign.Summary)
	}
	if foreign.Entity.ID != foreignID {
		t.Errorf("foreign-project row entity id should survive, got %q want %q", foreign.Entity.ID, foreignID)
	}
	if foreign.Entity.ProjectIDPrefix != foreignPrefix {
		t.Errorf("foreign-project row project prefix should survive, got %q want %q", foreign.Entity.ProjectIDPrefix, foreignPrefix)
	}
	if foreign.Actor.Username != foreignUname {
		t.Errorf("foreign-project row actor should survive, got %q want %q", foreign.Actor.Username, foreignUname)
	}
}

// TestListUserActivity_RedactInvisible_Admin asserts that the unrestricted
// branch (isAdmin=true — declared internal work, authz.VisibleProjectIDs'
// `all`; an instance admin is NOT this branch since sk-674.18 T017, his
// rows go through his reach: TestListUserActivity_InstanceAdminReach) sees
// every row fully populated — never redacted.
func TestListUserActivity_RedactInvisible_Admin(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedUser(t, pool)
	foreignOwner := seedUser(t, pool)
	memberPid, memberPrefix := seedProject(t, pool, caller)
	foreignPid, foreignPrefix := seedProject(t, pool, foreignOwner)

	callerUname := usernameOf(t, pool, caller)
	foreignUname := usernameOf(t, pool, foreignOwner)

	now := time.Now().UTC()
	memberID := memberPrefix + "-1"
	foreignID := foreignPrefix + "-1"
	seedIssue(t, pool, memberPid, caller, memberID, "member issue title")
	seedIssue(t, pool, foreignPid, foreignOwner, foreignID, "foreign issue title")
	seedIssueEvent(t, pool, "issue.created", memberID, memberPid, callerUname, caller, now.Add(-1*time.Hour))
	seedIssueEvent(t, pool, "issue.created", foreignID, foreignPid, foreignUname, foreignOwner, now.Add(-30*time.Minute))

	// Admin caller — visible set is irrelevant, nothing redacted.
	visible := map[uuid.UUID]struct{}{memberPid: {}}
	items, _, err := store.ListUserActivity(ctx, caller, visible, true /*isAdmin*/, nil, nil, 100,
		map[uuid.UUID]struct{}{memberPid: {}, foreignPid: {}} /*reach*/, redact.Soft)
	if err != nil {
		t.Fatalf("ListUserActivity admin: %v", err)
	}

	for _, id := range []string{memberID, foreignID} {
		e := findActivity(t, items, id)
		if e.Redacted {
			t.Errorf("admin row %s should NOT be redacted: %+v", id, e)
		}
		if e.Entity.Title == "" {
			t.Errorf("admin row %s Title should be populated, got empty", id)
		}
	}
}

// TestListUserActivity_NoRedact_Unchanged is the regression guard for the
// /me/activity contract: redactInvisible=false, non-admin caller → the
// feed is CUT to the visible set (foreign rows absent entirely), and no
// returned row carries Redacted=true.
func TestListUserActivity_NoRedact_Unchanged(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedUser(t, pool)
	foreignOwner := seedUser(t, pool)
	memberPid, memberPrefix := seedProject(t, pool, caller)
	foreignPid, foreignPrefix := seedProject(t, pool, foreignOwner)

	callerUname := usernameOf(t, pool, caller)
	foreignUname := usernameOf(t, pool, foreignOwner)

	now := time.Now().UTC()
	memberID := memberPrefix + "-1"
	foreignID := foreignPrefix + "-1"
	seedIssue(t, pool, memberPid, caller, memberID, "member issue title")
	seedIssue(t, pool, foreignPid, foreignOwner, foreignID, "foreign issue title")
	seedIssueEvent(t, pool, "issue.created", memberID, memberPid, callerUname, caller, now.Add(-1*time.Hour))
	seedIssueEvent(t, pool, "issue.created", foreignID, foreignPid, foreignUname, foreignOwner, now.Add(-30*time.Minute))

	visible := map[uuid.UUID]struct{}{memberPid: {}}
	items, _, err := store.ListUserActivity(ctx, caller, visible, false /*isAdmin*/, nil, nil, 100, nil /*reach*/, redact.None)
	if err != nil {
		t.Fatalf("ListUserActivity: %v", err)
	}

	for _, e := range items {
		if e.Entity.ID == foreignID || (e.Entity.ProjectID != nil && *e.Entity.ProjectID == foreignPid) {
			t.Errorf("foreign-project row leaked into scoped feed: %+v", e)
		}
		if e.Redacted {
			t.Errorf("scoped feed must not carry Redacted rows: %+v", e)
		}
	}
	// Member row must still be present and full.
	member := findActivity(t, items, memberID)
	if member.Entity.Title != "member issue title" {
		t.Errorf("member-project row Title = %q, want %q", member.Entity.Title, "member issue title")
	}
}

// seedDiffEvent writes an events row carrying a scalar diff (field +
// old_data/new_data), like issue.status_changed. Used to prove the
// redaction helper strips the diff fields (OldValue/NewValue/Field), not
// just the title — that field set is where a foreign project's
// status-change values or renamed-title diff would otherwise leak.
func seedDiffEvent(t *testing.T, pool *pgxpool.Pool, eventType, issueID string, projectID uuid.UUID, actor string, actorUserID uuid.UUID, field, oldData, newData string, occurredAt time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, field,
                    old_data, new_data, payload)
VALUES ($1, $2, $3, $4, 'human', 'issue', $5, $6, $7,
        $8::jsonb, $9::jsonb, '{}'::jsonb)`,
		occurredAt, eventType, actor, actorUserID, issueID, projectID,
		field, oldData, newData)
	if err != nil {
		t.Fatalf("seed diff event %s/%s: %v", eventType, issueID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM events WHERE type=$1 AND entity_id=$2 AND project_id=$3`,
			eventType, issueID, projectID)
	})
}

// TestListUserActivity_RedactInvisible_StripsDiffFields — I2 from the
// sk-94.1 code review: the redaction integration tests only seeded
// issue.created events with empty payloads, so OldValue/NewValue/Changes/
// Field were nil before redaction and the stripping of those fields was
// never asserted. This seeds an issue.status_changed event (populated
// old_data/new_data/field) in BOTH a member and a foreign project and
// asserts the diff fields survive on the member row but are stripped on
// the redacted foreign row.
func TestListUserActivity_RedactInvisible_StripsDiffFields(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedUser(t, pool)
	foreignOwner := seedUser(t, pool)
	memberPid, memberPrefix := seedProject(t, pool, caller)
	foreignPid, foreignPrefix := seedProject(t, pool, foreignOwner)

	callerUname := usernameOf(t, pool, caller)
	foreignUname := usernameOf(t, pool, foreignOwner)

	now := time.Now().UTC()
	memberID := memberPrefix + "-1"
	foreignID := foreignPrefix + "-1"
	seedIssue(t, pool, memberPid, caller, memberID, "member issue title")
	seedIssue(t, pool, foreignPid, foreignOwner, foreignID, "foreign issue title")
	seedDiffEvent(t, pool, "issue.status_changed", memberID, memberPid, callerUname, caller,
		"status", `"open"`, `"in_progress"`, now.Add(-1*time.Hour))
	seedDiffEvent(t, pool, "issue.status_changed", foreignID, foreignPid, foreignUname, foreignOwner,
		"status", `"open"`, `"in_progress"`, now.Add(-30*time.Minute))

	visible := map[uuid.UUID]struct{}{memberPid: {}}
	// The foreign project is in the caller's reach (sk-674.18 T030: a project
	// of his own space he does not see) — its rows come redacted.
	reach := map[uuid.UUID]struct{}{memberPid: {}, foreignPid: {}}
	items, _, err := store.ListUserActivity(ctx, caller, visible, false /*isAdmin*/, nil, nil, 100, reach, redact.Soft)
	if err != nil {
		t.Fatalf("ListUserActivity: %v", err)
	}

	member := findActivity(t, items, memberID)
	foreign := findActivity(t, items, foreignID)

	// Member-project row: diff fields populated (proves the seed actually
	// carries a diff, so the foreign-row assertions below are meaningful).
	if member.Redacted {
		t.Errorf("member-project row should NOT be redacted: %+v", member)
	}
	if member.OldValue == nil || member.NewValue == nil {
		t.Errorf("member-project row should carry OldValue/NewValue, got old=%v new=%v",
			member.OldValue, member.NewValue)
	}
	if member.Field == nil || *member.Field != "status" {
		t.Errorf("member-project row Field = %v, want \"status\"", member.Field)
	}

	// Foreign-project row: every diff field stripped alongside the title.
	if !foreign.Redacted {
		t.Errorf("foreign-project row should be redacted: %+v", foreign)
	}
	if foreign.OldValue != nil || foreign.NewValue != nil {
		t.Errorf("foreign-project row diff must be stripped, got old=%v new=%v",
			foreign.OldValue, foreign.NewValue)
	}
	if foreign.Changes != nil {
		t.Errorf("foreign-project row Changes must be nil, got %v", foreign.Changes)
	}
	if foreign.Field != nil {
		t.Errorf("foreign-project row Field must be nil, got %v", *foreign.Field)
	}
	if foreign.Entity.Title != "" || foreign.Summary != "" {
		t.Errorf("foreign-project row title/summary must be stripped: title=%q summary=%q",
			foreign.Entity.Title, foreign.Summary)
	}
}

// seedReparentEvent writes an issue.reparented events row whose old_data
// (`{"parentIds":[...]}`) and new_data (`{"parentId":...}`) embed issue
// ids — a leak vector the Hard redaction must strip (via the base
// OldValue/NewValue nil-out). Mirrors dependencies.WriteReparentEvent's
// column shape.
func seedReparentEvent(t *testing.T, pool *pgxpool.Pool, issueID string, projectID uuid.UUID, actor string, actorUserID uuid.UUID, oldParentIDs []string, newParentID string, occurredAt time.Time) {
	t.Helper()
	old := map[string]any{"parentIds": oldParentIDs}
	nw := map[string]any{"parentId": newParentID}
	oldJSON, _ := json.Marshal(old)
	newJSON, _ := json.Marshal(nw)
	_, err := pool.Exec(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, old_data, new_data, payload)
VALUES ($1, 'issue.reparented', $2, $3, 'human', 'issue', $4, $5,
        $6::jsonb, $7::jsonb, '{}'::jsonb)`,
		occurredAt, actor, actorUserID, issueID, projectID, oldJSON, newJSON)
	if err != nil {
		t.Fatalf("seed reparent event %s: %v", issueID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM events WHERE type='issue.reparented' AND entity_id=$1 AND project_id=$2`,
			issueID, projectID)
	})
}

// seedDepEvent writes a dependency.* events row whose entity_id is the
// composite "fromID::toID::depType" (two issue ids). The Hard redaction
// must blank entity.id, dropping BOTH ids at once.
func seedDepEvent(t *testing.T, pool *pgxpool.Pool, eventType, fromID, toID, depType string, projectID uuid.UUID, actor string, actorUserID uuid.UUID, occurredAt time.Time) {
	t.Helper()
	entityID := fromID + "::" + toID + "::" + depType
	_, err := pool.Exec(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
VALUES ($1, $2, $3, $4, 'human', 'dep', $5, $6, '{}'::jsonb)`,
		occurredAt, eventType, actor, actorUserID, entityID, projectID)
	if err != nil {
		t.Fatalf("seed dep event %s: %v", entityID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM events WHERE type=$1 AND entity_id=$2 AND project_id=$3`,
			eventType, entityID, projectID)
	})
}

// seedCommentEvent writes a comments row on issueID plus a comment.created
// events row pointing at it. activityListSQL resolves the comment's parent
// issue id (issueID) into entity.id — which the Hard redaction must blank.
func seedCommentEvent(t *testing.T, pool *pgxpool.Pool, issueID string, projectID uuid.UUID, actor string, actorUserID uuid.UUID, occurredAt time.Time) {
	t.Helper()
	commentID := uuid.Must(uuid.NewRandom())
	if _, err := pool.Exec(context.Background(), `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, body)
VALUES ($1, $2, $3, $4, 'human', 'secret comment body')`,
		commentID, issueID, actor, actorUserID); err != nil {
		t.Fatalf("seed comment row %s: %v", commentID, err)
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
VALUES ($1, 'comment.created', $2, $3, 'human', 'comment', $4, $5, '{}'::jsonb)`,
		occurredAt, actor, actorUserID, commentID.String(), projectID); err != nil {
		t.Fatalf("seed comment event %s: %v", commentID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM events WHERE type='comment.created' AND entity_id=$1 AND project_id=$2`,
			commentID.String(), projectID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM comments WHERE id=$1`, commentID)
	})
}

// TestListUserActivity_HardRedact_AirtightLeak is the airtight (§8) leak
// test for the EXTERNAL feed contract. A caller who is a member of NOTHING
// requests the global war-room feed at redact.Hard. Every seeded row of an
// inaccessible project — an issue.created, an issue.reparented (old_data
// embeds a parent issue id), a dependency.created (entity_id = a "from::to"
// composite), and a comment.created (entity.id resolves to the parent issue
// id) — must come back PRESENT but Hard-redacted, and the marshaled JSON of
// those rows must contain NO substring of the project prefix, any issue id,
// or either projectId field.
func TestListUserActivity_HardRedact_AirtightLeak(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedUser(t, pool) // member of NOTHING
	foreignOwner := seedUser(t, pool)
	foreignPid, foreignPrefix := seedProject(t, pool, foreignOwner)
	foreignUname := usernameOf(t, pool, foreignOwner)

	now := time.Now().UTC()
	createdID := foreignPrefix + "-1"
	reparentedID := foreignPrefix + "-2"
	oldParentID := foreignPrefix + "-9"
	newParentID := foreignPrefix + "-3"
	const createdTitle = "SECRET-created-title"
	const reparentedTitle = "SECRET-reparented-title"
	seedIssue(t, pool, foreignPid, foreignOwner, createdID, createdTitle)
	seedIssue(t, pool, foreignPid, foreignOwner, reparentedID, reparentedTitle)

	seedIssueEvent(t, pool, "issue.created", createdID, foreignPid, foreignUname, foreignOwner, now.Add(-40*time.Minute))
	seedReparentEvent(t, pool, reparentedID, foreignPid, foreignUname, foreignOwner,
		[]string{oldParentID}, newParentID, now.Add(-30*time.Minute))
	seedDepEvent(t, pool, "dependency.created", createdID, reparentedID, "blocks",
		foreignPid, foreignUname, foreignOwner, now.Add(-20*time.Minute))
	seedCommentEvent(t, pool, createdID, foreignPid, foreignUname, foreignOwner, now.Add(-10*time.Minute))

	// Member of nothing → visible empty; the foreign project is in the reach
	// (sk-674.18 T030), so its rows come back; level=Hard redacts every one
	// of them (all are inaccessible).
	visible := map[uuid.UUID]struct{}{}
	items, _, err := store.ListUserActivity(ctx, caller, visible, false /*isAdmin*/, nil, nil, 100,
		map[uuid.UUID]struct{}{foreignPid: {}} /*reach*/, redact.Hard)
	if err != nil {
		t.Fatalf("ListUserActivity: %v", err)
	}

	// Isolate my four seeded rows by their (kept) actor — id/projectId are
	// all stripped, so the actor username is the only stable handle.
	mine := make([]ActivityEntry, 0, 4)
	for _, e := range items {
		if e.Actor.Username == foreignUname {
			mine = append(mine, e)
		}
	}
	if len(mine) != 4 {
		t.Fatalf("expected 4 rows for actor %q, got %d (%+v)", foreignUname, len(mine), mine)
	}

	for _, e := range mine {
		if !e.Redacted {
			t.Errorf("row must be redacted: %+v", e)
		}
		if e.Entity.ID != "" {
			t.Errorf("Hard-redacted entity.id must be blank, got %q", e.Entity.ID)
		}
		if e.Entity.ProjectIDPrefix != "" {
			t.Errorf("Hard-redacted entity.projectIdPrefix must be blank, got %q", e.Entity.ProjectIDPrefix)
		}
		if e.Entity.ProjectID != nil {
			t.Errorf("Hard-redacted entity.projectId must be nil, got %v", *e.Entity.ProjectID)
		}
		if e.ProjectID != nil {
			t.Errorf("Hard-redacted top-level projectId must be nil, got %v", *e.ProjectID)
		}
		if e.Entity.Title != "" || e.Summary != "" {
			t.Errorf("Hard-redacted content must be blank: title=%q summary=%q", e.Entity.Title, e.Summary)
		}
		if e.OldValue != nil || e.NewValue != nil {
			t.Errorf("Hard-redacted diff must be nil: old=%v new=%v", e.OldValue, e.NewValue)
		}
		// Actor is the accepted "who acted" exposure and MUST survive.
		if e.Actor.Username == "" {
			t.Errorf("actor username must survive Hard redaction: %+v", e)
		}
	}

	blob, err := json.Marshal(mine)
	if err != nil {
		t.Fatalf("marshal redacted rows: %v", err)
	}
	js := string(blob)
	// foreignPrefix ("ist"+hex) can never be a coincidental substring of a
	// kept field: the actor username is "tu"+hex (all hex, no 'i'/'s'/'t'),
	// and event/entity type strings contain no "ist". So a hit is a real leak.
	for _, secret := range []string{
		foreignPrefix,
		createdID, reparentedID, oldParentID, newParentID,
		foreignPid.String(),
		createdTitle, reparentedTitle,
	} {
		if strings.Contains(js, secret) {
			t.Errorf("Hard-redacted feed leaked %q in JSON: %s", secret, js)
		}
	}
	// Neither projectId field may be present on the wire at all.
	if strings.Contains(js, "\"projectId\"") {
		t.Errorf("Hard-redacted feed must not carry any projectId field: %s", js)
	}
}

// TestListUserActivity_InstanceAdminReach — sk-674.18, row 203 (T030, K-6,
// INV-23, SC-005). Reference — the admin's HOT ZONE as US-13 words it,
// written before the code: the set of his column — every non-personal
// project plus his own "Personal" — and never another person's "Personal". The
// reach is the production one (authz.ReachFor for the admin), the feed the
// production read: an event in another person's "Personal" is absent, an
// event of a non-personal project he is not a member of is present (the
// positive control).
func TestListUserActivity_InstanceAdminReach(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	admin, _ := testsupport.SeedUser(t, pool, "admin")
	person := seedUser(t, pool)
	personName := usernameOf(t, pool, person)

	personal := testsupport.SeedSpaceOfKind(t, pool, person, "personal")
	privatePid, privatePrefix := testsupport.SeedProjectInSpace(t, pool, person, personal, "advp")
	sharedPid, sharedPrefix := seedProject(t, pool, person) // the shared test space, non-personal

	now := time.Now().UTC()
	privateID, sharedID := privatePrefix+"-1", sharedPrefix+"-1"
	seedIssue(t, pool, privatePid, person, privateID, "private issue")
	seedIssue(t, pool, sharedPid, person, sharedID, "shared issue")
	seedIssueEvent(t, pool, "issue.created", privateID, privatePid, personName, person, now.Add(-time.Minute))
	seedIssueEvent(t, pool, "issue.created", sharedID, sharedPid, personName, person, now.Add(-time.Minute))

	hz, err := authz.ReachFor(ctx, pool, authz.ForUser(admin), "")
	if err != nil {
		t.Fatalf("ReachFor: %v", err)
	}
	if _, ok := hz.Reach[privatePid]; ok {
		t.Fatalf("fixture: another person's Personal is in the admin's reach")
	}
	items, _, err := store.ListUserActivity(ctx, admin, hz.Visible, false, nil, nil, 100, hz.Reach, redact.Soft)
	if err != nil {
		t.Fatalf("ListUserActivity: %v", err)
	}
	sawShared := false
	for _, it := range items {
		if (it.ProjectID != nil && *it.ProjectID == privatePid) || it.Entity.ID == privateID {
			t.Errorf("the admin's feed carries an event of another person's Personal: %+v", it)
		}
		if it.Entity.ID == sharedID {
			sawShared = true
		}
	}
	if !sawShared {
		t.Errorf("the admin's feed misses the event of a non-personal project (control)")
	}
}
