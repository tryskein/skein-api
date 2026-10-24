package mentions_test

// resolve_isolation_test.go — sk-674.18 T005 (INV-9,
// INV-11): an @mention of a person who does not see the issue's project
// creates no notification; a person who sees it gets one.
//
// Fixture testsupport.SeedTenancy (T003); reference testsupport.TenancyVisible.
// Two levels, both with every actor of the legend mentioned at once:
//
//   - mentions.Resolve — the recipient gate itself, per project;
//   - the production write path (issues.Store.AddComment) — the
//     comment.mention notifications that actually land, per actor.
//
// The comment author is the project's owner; mentioning oneself never
// notifies (FilterOutSelf), so the owner's cell is "no notification" by that
// rule, not by access.
//
// Every cell is asserted since T017 (it closed the last one: an admin
// mentioned in another person's "Personal").
//
// sk-674.18 T027 (INV-9 "in the context of the issue — in its project", E-21): seeing
// the project is not enough — the person must also belong to the issue's
// context, a participant of the project's space or an external member of
// the project — or the instance admin in a non-personal project (the
// maintainers' decision 2026-10-17, sk-741, replacing the decision
// 2026-10-14 under which admin0 was mentioned nowhere and adminA not in B): both
// admins are mentioned in A and B, neither in another person's "Personal".
// inContext is that reference, written from the fixture's spaces.

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/issues"
	"skein-api/internal/mentions"
	"skein-api/internal/testsupport"
)

func TestMentionIsolation_ResolveAndNotify(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	f := testsupport.SeedTenancy(t, pool)
	store := issues.NewStore(pool)

	var projectIDs []uuid.UUID
	for _, p := range f.Projects() {
		projectIDs = append(projectIDs, p.ID)
	}
	// Runs before the fixture's own cleanup: the events the comments wrote
	// reference their authors (ON DELETE RESTRICT).
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE project_id = ANY($1)`, projectIDs)
	})

	var candidates, mentionsText []string
	for _, a := range testsupport.TenancyActors {
		u := f.User(t, a)
		candidates = append(candidates, u.Username)
		mentionsText = append(mentionsText, "@"+u.Username)
	}
	body := "isomatrix " + strings.Join(mentionsText, " ")

	for _, p := range f.Projects() {
		t.Run(p.Key(), func(t *testing.T) {
			resolved, err := mentions.Resolve(ctx, pool, p.ID, candidates)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			got := map[uuid.UUID]bool{}
			for _, r := range resolved {
				got[r.UserID] = true
			}

			ownerName := testsupport.Username(t, pool, p.OwnerID)
			c, err := store.AddComment(ctx, p.IssueID, body, p.OwnerID, ownerName,
				issues.CommentAuthorTypeHuman, issues.CommentAuthorKindHuman,
				issues.HumanActor(p.OwnerID, ownerName))
			if err != nil {
				t.Fatalf("AddComment: %v", err)
			}
			rows, err := pool.Query(ctx, `
SELECT n.user_id FROM notifications n
  JOIN events e ON e.id = n.event_id
 WHERE e.entity_id = $1 AND n.type = 'comment.mention'`, c.ID.String())
			if err != nil {
				t.Fatalf("read mention notifications: %v", err)
			}
			notified := map[uuid.UUID]bool{}
			for rows.Next() {
				var uid uuid.UUID
				if err := rows.Scan(&uid); err != nil {
					t.Fatalf("scan: %v", err)
				}
				notified[uid] = true
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}

			for _, a := range testsupport.TenancyActors {
				t.Run(string(a), func(t *testing.T) {
					u := f.User(t, a)
					sees := f.Sees(a, p) && inContext(a, p)
					if got[u.ID] != sees {
						t.Errorf("Resolve: resolved = %v, want %v", got[u.ID], sees)
					}
					want := sees && u.ID != p.OwnerID
					if notified[u.ID] != want {
						t.Errorf("comment.mention notification = %v, want %v", notified[u.ID], want)
					}
				})
			}
		})
	}
}

// inContext reports whether actor a belongs to the context of project p by
// the fixture (testsupport.SeedTenancy): a participant of p's space — A:
// spaceOwner, onlyA, both, projectOwner, participantNoMembers, adminA, deactivated; B: onlyB, both; a "Personal": its owner —
// or the external member of A.members, or an instance admin (adminA, admin0) in
// a project of A or B (sk-741).
func inContext(a testsupport.Actor, p testsupport.TenancyProject) bool {
	spaceA := []testsupport.Actor{testsupport.ActorSpaceOwner, testsupport.ActorOnlyA, testsupport.ActorBoth,
		testsupport.ActorProjectOwner, testsupport.ActorParticipantNoMembers, testsupport.ActorAdminA,
		testsupport.ActorDeactivated}
	spaceB := []testsupport.Actor{testsupport.ActorOnlyB, testsupport.ActorBoth}
	switch {
	case p.Kind == testsupport.KindPersonal:
		return a == p.PersonalOf
	case a.IsAdmin():
		return true
	case p.Tenant == "A":
		return slices.Contains(spaceA, a) || (a == testsupport.ActorExternal && p.Kind == testsupport.KindMembers)
	default:
		return slices.Contains(spaceB, a)
	}
}
