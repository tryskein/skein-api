package migrations

// migration_000050_test.go — integration class for migration 000050 (sk-489).
//
// The migration introduces the close outcome: a column with a closed set of values, a backfill
// of history, a consistency CHECK "outcome is present <=> the issue is closed" (INV-1) and
// a normalizer trigger that keeps this invariant under a write that does not
// know about the column.
//
// THE LIMIT OF THE PATTERN THIS FILE IS WRITTEN AFTER. The pattern is
// migration_000049_test.go#TestMigration000049_UpDownUpRoundTrip: it checks
// the appearance and disappearance of a COLUMN and the backfill of its values. Here the subject is wider by
// two objects that 000049 does not have at all — the consistency constraint and the
// trigger — and they carry the main load: US-10 rests on them
// (an old api version on the new schema), and its failure would look not like a red
// test but like a failure to close issues in the deploy window.
//
// WHY THE WRITE HERE IS RAW AND NOT THROUGH Store. Exactly the opposite decision to the
// pattern's, and for the same reason — the subject. 000049 checked the backfill FROM THE JOURNAL,
// which only a real writer produces, so the claim there goes through Store.
// Here the subject is the behaviour of the schema under a write THAT DOES NOT KNOW ABOUT THE COLUMN:
// an old pod and raw fixtures. The new version's Store knows the column and would fill
// it itself, that is, it would check itself and not the trigger.
//
// THE INSERT BRANCH is not symmetry for symmetry's sake. Closed issues in this tree
// are seeded by a raw INSERT (internal/testsupport#SeedIssue and the local seedIssue
// next to it), and without the INSERT branch such an INSERT would hit the consistency CHECK.
// The defect would show up not here but as a scatter of reds in OTHER packages — outside the
// scope of the task's gate, that is, after it is closed.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migration000050Below — the version the rollback goes TO in order to remove 000050.
// Absolute, not Steps(-1): the appearance of 000051 would make a step back a rollback of
// someone else's migration (the same reason its pin stands in the 000049 pattern).
const migration000050Below = 49

// seedIssueWithOutcome inserts an issue by a RAW INSERT, as the tree fixtures do:
// listing columns that do not know about close_outcome — except when
// the outcome is passed explicitly (outcome != ""), which the INSERT branch of the trigger needs.
//
// Local and not the neighbouring seedIssue: that one can only do an open issue, and
// the subject here is precisely a closed one.
func seedIssueWithOutcome(t *testing.T, pool *pgxpool.Pool, id string,
	projectID, creator uuid.UUID, status, outcome string,
) {
	t.Helper()
	var closedAt any
	var closedReason, closeOutcome *string
	if status == "closed" {
		closedAt = time.Now().UTC()
		empty := ""
		closedReason = &empty
	}
	if outcome != "" {
		closeOutcome = &outcome
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version, title, description, type, priority,
                    status, pinned, source, source_context, assignee, design, acceptance_criteria,
                    notes, estimated_minutes, due_at, defer_until, started_at, closed_at,
                    closed_reason, close_outcome, external_ref, created_by, created_at, updated_at,
                    metadata)
VALUES ($1, $2, NULL, 1, $3, '', 'task', 2, $4, false, 'human', NULL, NULL, '', '', '',
        NULL, NULL, NULL, NULL, $5, $6, $7,
        NULL, $8, now(), now(), '{}'::jsonb)`,
		id, projectID, "seed "+id, status, closedAt, closedReason, closeOutcome, creator); err != nil {
		t.Fatalf("seed issue %s (%s): %v", id, status, err)
	}
}

// closeOutcomeOf reads issues.close_outcome (nil = no outcome).
func closeOutcomeOf(t *testing.T, pool *pgxpool.Pool, id string) *string {
	t.Helper()
	var out *string
	if err := pool.QueryRow(context.Background(),
		`SELECT close_outcome FROM issues WHERE id = $1`, id).Scan(&out); err != nil {
		t.Fatalf("read close_outcome of %s: %v", id, err)
	}
	return out
}

// wantOutcome compares the issue's outcome with the expected one; want == "" means "no outcome".
func wantOutcome(t *testing.T, pool *pgxpool.Pool, id, want, why string) {
	t.Helper()
	got := closeOutcomeOf(t, pool, id)
	switch {
	case want == "" && got != nil:
		t.Errorf("%s: outcome = %q, want none — %s", id, *got, why)
	case want != "" && got == nil:
		t.Errorf("%s: no outcome, want %q — %s", id, want, why)
	case want != "" && got != nil && *got != want:
		t.Errorf("%s: outcome = %q, want %q — %s", id, *got, want, why)
	}
}

// migrator opens the migrator and registers the RETURN TO HEAD before the first step
// down: a failure of any assertion would otherwise leave the shared base at version 49 and
// break the fixtures of all following packages of the sequential run.
func migrator000050(t *testing.T, url string) *migrate.Migrate {
	t.Helper()
	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up: %v", err)
		}
		_, _ = m.Close()
	})
	return m
}

// TestMigration000050_UpDownUpRoundTrip — a forward and backward run over ALL
// four objects of the migration, not over the column alone.
//
// The objects are named one by one on purpose: a column that came back without the trigger is
// exactly the failure that US-10 says is not observable through the schema
// ("the trigger is built but not attached"), and the assertion "the column is in place" would
// miss it.
// constraintExists — existence of a named constraint of a table.
//
// WHY IT IS HERE AND NOT NEXT TO ITS SISTERS. columnExists / triggerExists /
// functionExists live in migration_000044_test.go — the file of someone else's migration;
// this helper is introduced here so that the edit does not spread to it.
//
// WHY IT IS NEEDED AT ALL, THOUGH constraintDef EXISTS. That one fails when
// the constraint is absent, and so does not suit the negative half: after the
// rollback the constraint must NOT be there, and that has to be asserted, not
// crash the test.
//
// WHAT EXACTLY IT COVERS. The review of wave sk-489/A/1 (critic self) showed by mutation
// that without it the consistency constraint was checked by NOTHING:
// removing ADD CONSTRAINT from the roll-forward left the whole harness green. The cause
// is not the author's oversight but the nature of the subject: the BEFORE ROW trigger
// brings the row to the invariant BEFORE the constraints are checked, so this guard
// is unreachable by a behavioural test while the trigger is alive — it is visible
// only by asking the catalog directly.
func constraintExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), `
SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = $1)`, name).Scan(&ok); err != nil {
		t.Fatalf("probe constraint %s: %v", name, err)
	}
	return ok
}

func TestMigration000050_UpDownUpRoundTrip(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)

	assertPresent := func(when string, want bool) {
		t.Helper()
		for what, got := range map[string]bool{
			"column issues.close_outcome":                    columnExists(t, pool, "issues", "close_outcome"),
			"function skein_normalize_close_outcome":         functionExists(t, pool, "skein_normalize_close_outcome"),
			"trigger issues_normalize_close_outcome":         triggerExists(t, pool, "issues_normalize_close_outcome"),
			"constraint issues_close_outcome_consistent_chk": constraintExists(t, pool, "issues_close_outcome_consistent_chk"),
		} {
			if got != want {
				t.Errorf("%s: %s present=%v, want %v", when, what, got, want)
			}
		}
	}

	assertPresent("before rollback", true)

	m := migrator000050(t, url)
	if err := m.Migrate(migration000050Below); err != nil {
		t.Fatalf("Migrate(%d) (000050 down): %v", migration000050Below, err)
	}
	assertPresent("after rollback", false)

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Up after down: %v", err)
	}
	assertPresent("after repeated roll-forward", true)
}

// TestMigration000050_SeedsEveryClosedIssueAndOnlyThose — the backfill (US-8) and the price of
// the rollback (US-10), in one run, because it is one down-up circle.
//
// The control — the number of closed issues — is taken by an INDEPENDENT query BEFORE the roll-forward.
// Comparing the backfill with a number counted after it over the same column would mean
// comparing the backfill with itself.
//
// The negative half is mandatory here: "done is set on all closed"
// is green for a backfill that set done on EVERYTHING too — and such a backfill would show
// an open issue as closed without work in all the progress numbers.
func TestMigration000050_SeedsEveryClosedIssueAndOnlyThose(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	owner := seedUser(t, pool, "m50a")
	pid, prefix := seedProject(t, pool, owner, "m50a")

	done := prefix + "-1"
	noWork := prefix + "-2"
	open := prefix + "-3"
	seedIssueWithOutcome(t, pool, done, pid, owner, "closed", "")
	seedIssueWithOutcome(t, pool, noWork, pid, owner, "closed", "")
	seedIssueWithOutcome(t, pool, open, pid, owner, "open", "")

	// An outcome CHOSEN by a human — it is what the rollback loses.
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET close_outcome = 'wont_do' WHERE id = $1`, noWork); err != nil {
		t.Fatalf("set wont_do: %v", err)
	}
	wantOutcome(t, pool, noWork, "wont_do", "precondition: the outcome is recorded at all")

	m := migrator000050(t, url)
	if err := m.Migrate(migration000050Below); err != nil {
		t.Fatalf("Migrate(%d): %v — the rollback with a live \"no work\" outcome must succeed",
			migration000050Below, err)
	}

	// The control is TAKEN BEFORE THE ROLL-FORWARD and not from the outcome column — it does not exist now.
	var closedBefore int64
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issues WHERE status = 'closed'`).Scan(&closedBefore); err != nil {
		t.Fatalf("control count: %v", err)
	}
	if closedBefore < 2 {
		t.Fatalf("the control counted %d closed issues — the fixture did not arrive, and any "+
			"assertion about the backfill below would become a tautology", closedBefore)
	}
	// The counter of the previous roll-forward went away with the rollback: the counter row is a part
	// of the evidence of THIS roll-forward, not an eternal record.
	var counterRows int64
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM migration_counters WHERE migration = '000050_close_outcome'`).
		Scan(&counterRows); err != nil {
		t.Fatalf("counter rows after down: %v", err)
	}
	if counterRows != 0 {
		t.Errorf("after the rollback %d 000050 counter rows remain — the rollback must remove its own "+
			"evidence, otherwise the next roll-forward would add a second one and the operator would see two numbers",
			counterRows)
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Up after down: %v", err)
	}

	var seeded, counter int64
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issues WHERE close_outcome = 'done'`).Scan(&seeded); err != nil {
		t.Fatalf("seeded count: %v", err)
	}
	if err := pool.QueryRow(context.Background(), `
SELECT value FROM migration_counters
 WHERE migration = '000050_close_outcome' AND metric = 'issues_seeded_done'`).
		Scan(&counter); err != nil {
		t.Fatalf("read migration counter: %v", err)
	}
	if seeded != closedBefore || counter != closedBefore {
		t.Errorf("seeded %d, migration counter %d, closed before the roll-forward %d — the US-8 acceptance "+
			"requires all three to be equal: the deploy checks by the counter with an SQL query, "+
			"and not by a sample read of the data", seeded, counter, closedBefore)
	}

	// Positive half: every closed issue got an outcome.
	wantOutcome(t, pool, done, "done", "US-8: the whole history is \"done\"")
	// The price of the rollback, recorded in the header of the down file: the chosen wont_do is not
	// returned, the repeated roll-forward issues the factory "done".
	wantOutcome(t, pool, noWork, "done",
		"the rollback loses outcomes, and the repeated roll-forward seeds the factory \"done\" and does not return the chosen one")
	// Negative half: an open issue gets no outcome.
	wantOutcome(t, pool, open, "", "INV-1: only a closed issue has an outcome")
}

// TestMigration000050_CheckRejectsUnknownOutcome — INV-2 at the storage level.
//
// The server check (ValidateCloseOutcome) lives a layer above and does not depend
// on this one: a write bypassing the application — raw SQL, a restore, an old pod —
// does not go through the validator at all, and then the closed set is held only by the CHECK.
func TestMigration000050_CheckRejectsUnknownOutcome(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	owner := seedUser(t, pool, "m50b")
	pid, prefix := seedProject(t, pool, owner, "m50b")

	id := prefix + "-1"
	seedIssueWithOutcome(t, pool, id, pid, owner, "closed", "")

	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET close_outcome = 'bogus' WHERE id = $1`, id); err == nil {
		t.Error("CHECK accepted the value 'bogus' — the set of outcomes is no longer closed, and anything " +
			"gets into the column past the application validator")
	}
	wantOutcome(t, pool, id, "done", "a rejected write must not change the row")

	// The other side: the CHECK lets each of the four values through. Without it
	// "rejects bogus" would pass for a constraint that rejects everything.
	for _, ok := range []string{"done", "wont_do", "duplicate", "superseded"} {
		if _, err := pool.Exec(context.Background(),
			`UPDATE issues SET close_outcome = $2 WHERE id = $1`, id, ok); err != nil {
			t.Errorf("CHECK rejected a legal value %q: %v", ok, err)
		}
	}
}

// TestMigration000050_TriggerNormalizesRawWrites — US-10 as a whole: how the schema
// behaves under a write that does not know about the column.
//
// Each case is named by its failure. What is checked is not "the trigger exists" but what
// it does — a trigger that is built but not attached would pass the existence check
// and break issue closing on every old pod.
func TestMigration000050_TriggerNormalizesRawWrites(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	ctx := context.Background()
	owner := seedUser(t, pool, "m50c")
	pid, prefix := seedProject(t, pool, owner, "m50c")

	// (1) UPDATE: a raw close with THE SAME columns that the old Store.Close writes
	// (status, closed_at, closed_reason — without close_outcome).
	id := prefix + "-1"
	seedIssueWithOutcome(t, pool, id, pid, owner, "open", "")
	if _, err := pool.Exec(ctx, `
UPDATE issues SET status = 'closed', closed_at = now(), closed_reason = 'raw close'
 WHERE id = $1`, id); err != nil {
		t.Fatalf("the raw close was rejected (%v) — this is exactly how the old api version closes issues "+
			"in the deploy window, and a refusal here means a refusal to close in production", err)
	}
	wantOutcome(t, pool, id, "done", "US-1: a close without an outcome gives \"done\"")

	// (2) UPDATE: a raw reopen erases the outcome.
	if _, err := pool.Exec(ctx, `
UPDATE issues SET status = 'open', closed_at = NULL, closed_reason = NULL WHERE id = $1`,
		id); err != nil {
		t.Fatalf("the raw reopen was rejected: %v", err)
	}
	wantOutcome(t, pool, id, "", "US-4: a reopen erases the outcome — INV-1 is not violated for a moment")

	// (3) UPDATE: a raw write of an outcome into an OPEN issue leaves no outcome.
	if _, err := pool.Exec(ctx,
		`UPDATE issues SET close_outcome = 'wont_do' WHERE id = $1`, id); err != nil {
		t.Fatalf("the raw write of an outcome into an open issue was rejected: %v", err)
	}
	wantOutcome(t, pool, id, "", "INV-1: an unclosed issue has no outcome, whatever is written into it")

	// (4) UPDATE: an outcome that was set survives the old pod overwriting the reason.
	// This is what distinguishes the normalizer from "set done on every close":
	// the latter would overwrite a human's choice on every repeated close.
	kept := prefix + "-2"
	seedIssueWithOutcome(t, pool, kept, pid, owner, "closed", "")
	if _, err := pool.Exec(ctx,
		`UPDATE issues SET close_outcome = 'wont_do' WHERE id = $1`, kept); err != nil {
		t.Fatalf("set wont_do: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE issues SET status = 'closed', closed_at = now(), closed_reason = 're-close by old api'
 WHERE id = $1`, kept); err != nil {
		t.Fatalf("the raw repeated close was rejected: %v", err)
	}
	wantOutcome(t, pool, kept, "wont_do",
		"INV-10: a repeated close without an outcome does not change the outcome — otherwise the old pod would overwrite a human's choice")

	// (5) INSERT: a closed issue without an outcome gets "done". That is how
	// the tree fixtures are seeded, and without this branch they would hit the CHECK.
	ins := prefix + "-3"
	seedIssueWithOutcome(t, pool, ins, pid, owner, "closed", "")
	wantOutcome(t, pool, ins, "done",
		"INSERT branch: a raw insert of a closed issue without an outcome must succeed and get \"done\"")

	// (6) INSERT: an open issue with a passed outcome gets no outcome.
	// It discriminates a trigger attached only to UPDATE.
	insOpen := prefix + "-4"
	seedIssueWithOutcome(t, pool, insOpen, pid, owner, "open", "wont_do")
	wantOutcome(t, pool, insOpen, "",
		"INSERT branch: an outcome passed to an unclosed issue must be stripped, not kept")
}
