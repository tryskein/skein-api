package issues

// acceptance_cardinality_test — sk-303.13, coverage-matrix row 35.
//
// Cardinality of acceptance events and the absence of double delivery,
// against real PostgreSQL. Row 34 proves the decision; this row proves
// that all four write paths actually apply it, not just one of them.
//
// SC-010: "There is no path on which the acceptance mark is set without
// an acceptance event". SC-011: "Claiming an unaccepted issue produces
// NO MORE THAN ONE notification for the creator". US-11 is the reverse
// transition.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// acceptanceFacts is what the journal and the mailbox say about one issue
// after a write.
type acceptanceFacts struct {
	// events is how many issue.accepted events exist on the issue.
	events int
	// reasons is the distinct set of their events.reason values, sorted.
	reasons []string
	// actors is how many of them carry an actor_user_id (an acceptance
	// event with no actor is an audit record that cannot answer "by whom").
	actors int
	// deliveredFromAccepted is how many notifications those events produced.
	deliveredFromAccepted int
	// stamped says whether issues.accepted_at is currently set.
	stamped bool
}

func readAcceptanceFacts(t *testing.T, pool *pgxpool.Pool, issueID string) acceptanceFacts {
	t.Helper()
	ctx := context.Background()
	var f acceptanceFacts
	if err := pool.QueryRow(ctx, `
SELECT count(*),
       COALESCE(array_agg(DISTINCT COALESCE(reason, '<null>')) FILTER (WHERE true), '{}'),
       count(*) FILTER (WHERE actor_user_id IS NOT NULL)
  FROM events
 WHERE entity_type = 'issue' AND entity_id = $1 AND type = 'issue.accepted'`,
		issueID).Scan(&f.events, &f.reasons, &f.actors); err != nil {
		t.Fatalf("read acceptance events: %v", err)
	}
	if err := pool.QueryRow(ctx, `
SELECT count(*)
  FROM notifications n
  JOIN events e ON e.id = n.event_id
 WHERE e.entity_type = 'issue' AND e.entity_id = $1 AND e.type = 'issue.accepted'`,
		issueID).Scan(&f.deliveredFromAccepted); err != nil {
		t.Fatalf("read acceptance notifications: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT accepted_at IS NOT NULL FROM issues WHERE id = $1`, issueID).Scan(&f.stamped); err != nil {
		t.Fatalf("read acceptance stamp: %v", err)
	}
	return f
}

// TestAcceptanceCardinality_EveryPath (SC-010): each of the four writes that
// can stamp acceptance produces EXACTLY ONE event, with an actor, and — for
// the three automatic ones — no delivery.
//
// One table, four paths, because the whole risk this covers is that a path
// gets forgotten: the pre-T021 code stamped on four and audited on one, and
// nothing in the code said so.
func TestAcceptanceCardinality_EveryPath(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)

	type path struct {
		name       string
		wantReason string
		wantDeliv  func(int) bool
		delivWhy   string
		// do performs the write and returns the issue id it acted on.
		do func(t *testing.T, creator uuid.UUID, creatorName string, worker uuid.UUID, workerName string, pid uuid.UUID, prefix string) string
	}

	paths := []path{
		{
			name:       "explicit accept",
			wantReason: "<null>",
			wantDeliv:  func(n int) bool { return n == 1 },
			delivWhy:   "the explicit tool is the ONE path that carries a delivery reason: the creator is waiting for the acknowledgement",
			do: func(t *testing.T, creator uuid.UUID, creatorName string, worker uuid.UUID, workerName string, pid uuid.UUID, prefix string) string {
				dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
					CreateIssueInput{Title: "explicit", Type: "task", Priority: 2, Assignee: ptrStr(workerName)},
					creator, HumanActor(creator, creatorName))
				if err != nil {
					t.Fatalf("CreateIssue: %v", err)
				}
				if _, err := store.Accept(t.Context(), AcceptInput{
					ID: dto.ID, Actor: HumanActor(worker, workerName),
				}); err != nil {
					t.Fatalf("Accept: %v", err)
				}
				return dto.ID
			},
		},
		{
			name:       "claim",
			wantReason: "auto-accept",
			wantDeliv:  func(n int) bool { return n == 0 },
			delivWhy:   "the claim already notifies the creator through issue.status_changed (INV-6)",
			do: func(t *testing.T, creator uuid.UUID, creatorName string, worker uuid.UUID, workerName string, pid uuid.UUID, prefix string) string {
				dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
					CreateIssueInput{Title: "claimed", Type: "task", Priority: 2, Assignee: ptrStr(workerName)},
					creator, HumanActor(creator, creatorName))
				if err != nil {
					t.Fatalf("CreateIssue: %v", err)
				}
				if _, err := store.Claim(t.Context(), ClaimInput{
					ID: dto.ID, SessionID: "sess-card-claim", Actor: HumanActor(worker, workerName),
				}); err != nil {
					t.Fatalf("Claim: %v", err)
				}
				return dto.ID
			},
		},
		{
			name:       "create on self",
			wantReason: "auto-accept",
			wantDeliv:  func(n int) bool { return n == 0 },
			delivWhy:   "an issue you filed on yourself has nobody to notify",
			do: func(t *testing.T, creator uuid.UUID, creatorName string, worker uuid.UUID, workerName string, pid uuid.UUID, prefix string) string {
				dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
					CreateIssueInput{Title: "self", Type: "task", Priority: 2, Assignee: ptrStr(creatorName)},
					creator, HumanActor(creator, creatorName))
				if err != nil {
					t.Fatalf("CreateIssue: %v", err)
				}
				return dto.ID
			},
		},
		{
			name:       "self-assign on patch",
			wantReason: "auto-accept",
			wantDeliv:  func(n int) bool { return n == 0 },
			delivWhy:   "the patch already notifies through issue.updated / \"Assigned issue\"",
			do: func(t *testing.T, creator uuid.UUID, creatorName string, worker uuid.UUID, workerName string, pid uuid.UUID, prefix string) string {
				dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
					CreateIssueInput{Title: "patched", Type: "task", Priority: 2},
					creator, HumanActor(creator, creatorName))
				if err != nil {
					t.Fatalf("CreateIssue: %v", err)
				}
				if _, err := store.PatchIssue(t.Context(), dto.ID, PatchIssueInput{
					ExpectedVersion: dto.Version, Assignee: ptrStr(workerName),
				}, HumanActor(worker, workerName)); err != nil {
					t.Fatalf("PatchIssue: %v", err)
				}
				return dto.ID
			},
		},
	}

	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			creator := seedUser(t, pool)
			creatorName := readUsername(t, pool, creator)
			worker := seedUser(t, pool)
			workerName := readUsername(t, pool, worker)
			pid, prefix := seedProject(t, pool, creator)
			if _, err := pool.Exec(t.Context(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3) ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, worker, creator); err != nil {
				t.Fatalf("seed member: %v", err)
			}

			issueID := p.do(t, creator, creatorName, worker, workerName, pid, prefix)
			f := readAcceptanceFacts(t, pool, issueID)

			if !f.stamped {
				t.Fatalf("precondition: this path is supposed to stamp acceptance")
			}
			if f.events != 1 {
				t.Errorf("issue.accepted events = %d, want exactly 1 (SC-010: no path stamps without an event, and none writes two)", f.events)
			}
			if f.actors != f.events {
				t.Errorf("%d of %d acceptance events carry no actor — the journal cannot answer \"by whom\"", f.events-f.actors, f.events)
			}
			if len(f.reasons) != 1 || f.reasons[0] != p.wantReason {
				t.Errorf("acceptance event reasons = %v, want [%s]", f.reasons, p.wantReason)
			}
			if !p.wantDeliv(f.deliveredFromAccepted) {
				t.Errorf("acceptance events delivered %d notification(s); %s", f.deliveredFromAccepted, p.delivWhy)
			}
		})
	}
}

// TestAcceptanceCardinality_ClaimNotifiesCreatorOnce (SC-011): "Claiming
// an unaccepted issue produces no more than one notification for the creator".
//
// The claim emits a PAIR of events — the status change and the acceptance —
// and the whole point of making the second journal-only is that the pair does
// not become a pair of notifications. Counted from the creator's mailbox, not
// from the classifier.
func TestAcceptanceCardinality_ClaimNotifiesCreatorOnce(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	worker := seedUser(t, pool)
	workerName := readUsername(t, pool, worker)
	pid, prefix := seedProject(t, pool, creator)
	if _, err := pool.Exec(t.Context(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3) ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, worker, creator); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "claim once", Type: "task", Priority: 2, Assignee: ptrStr(workerName)},
		creator, HumanActor(creator, creatorName))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	// Whatever the creation delivered is another action's business.
	if _, err := pool.Exec(t.Context(), `DELETE FROM notifications WHERE user_id = $1`, creator); err != nil {
		t.Fatalf("clear: %v", err)
	}

	if _, err := store.Claim(t.Context(), ClaimInput{
		ID: dto.ID, SessionID: "sess-card-once", Actor: HumanActor(worker, workerName),
	}); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// The pair of events exists…
	var evTypes []string
	rows, err := pool.Query(t.Context(), `
SELECT type FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type IN ('issue.accepted','issue.status_changed')
 ORDER BY id`, dto.ID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		evTypes = append(evTypes, typ)
	}
	rows.Close()
	if len(evTypes) != 2 {
		t.Fatalf("events from the claim = %v, want both issue.accepted and issue.status_changed", evTypes)
	}

	// …and the creator got ONE notification out of it.
	var delivered int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM notifications WHERE user_id = $1`, creator).Scan(&delivered); err != nil {
		t.Fatalf("count: %v", err)
	}
	if delivered != 1 {
		t.Errorf("the creator got %d notifications from one claim, want exactly 1 (SC-011 / INV-6)", delivered)
	}
}

// TestAcceptanceCardinality_IdempotencyAndReset: repeating the write does not
// duplicate the event, and clearing the stamp neither writes a new one nor
// deletes the old one (US-11 — the invariant is one-directional).
//
// What this test can and cannot attribute, measured rather than assumed.
// Idempotency here is protected TWICE: by ClassifyAcceptance (no transition →
// no event) and, independently, by the `AND accepted_at IS NULL` predicate on
// the UPDATE plus the `old.AcceptedAt == nil` guard on the automatic paths.
// A mutation that makes the classifier claim a transition on an
// already-accepted issue therefore does NOT turn this test red — the SQL
// still matches no rows and the write never happens. So the assertion below
// proves the OUTCOME (one event for one transition, however many writes), not
// which of the two guards produced it. The classifier itself is pinned by the
// unit gate in acceptance_test.go, where it is the only thing running.
func TestAcceptanceCardinality_IdempotencyAndReset(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	worker := seedUser(t, pool)
	workerName := readUsername(t, pool, worker)
	third := seedUser(t, pool)
	thirdName := readUsername(t, pool, third)
	pid, prefix := seedProject(t, pool, creator)
	if _, err := pool.Exec(t.Context(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3) ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, worker, creator); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "idempotent", Type: "task", Priority: 2, Assignee: ptrStr(workerName)},
		creator, HumanActor(creator, creatorName))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	// Accept twice, then claim on top: three writes, one transition.
	for range 2 {
		if _, err := store.Accept(t.Context(), AcceptInput{
			ID: dto.ID, Actor: HumanActor(worker, workerName),
		}); err != nil {
			t.Fatalf("Accept: %v", err)
		}
	}
	if _, err := store.Claim(t.Context(), ClaimInput{
		ID: dto.ID, SessionID: "sess-card-idem", Actor: HumanActor(worker, workerName),
	}); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if f := readAcceptanceFacts(t, pool, dto.ID); f.events != 1 {
		t.Errorf("issue.accepted events after accept+accept+claim = %d, want 1 (one transition, three writes)", f.events)
	}

	// Reassign to a third person: the stamp clears, the journal keeps its
	// one historical event, and no new one appears.
	cur, err := store.GetIssue(t.Context(), dto.ID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if _, err := store.PatchIssue(t.Context(), dto.ID, PatchIssueInput{
		ExpectedVersion: cur.Version,
		Assignee:        ptrStr(thirdName),
		Force:           true,
	}, HumanActor(creator, creatorName)); err != nil {
		t.Fatalf("PatchIssue (reassign): %v", err)
	}
	f := readAcceptanceFacts(t, pool, dto.ID)
	if f.stamped {
		t.Errorf("precondition: reassigning to somebody else clears the acceptance stamp")
	}
	if f.events != 1 {
		t.Errorf("issue.accepted events after the reset = %d, want still 1 — the reverse transition writes nothing and erases nothing", f.events)
	}
}
