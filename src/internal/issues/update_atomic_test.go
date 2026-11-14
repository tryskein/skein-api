package issues

// update_atomic_test.go — matrix row 38 (US-14, SC-020).
//
// Reference — the spec, written before the code: "a change of one issue is
// applied whole or not at all; a refusal at any step leaves no written part
// and does not move the version"; on success events and notifications as
// before; a serialization conflict still answers serialization_retry.
//
// The refusal is injected by a test trigger on the table of the step (a
// fixture of the environment, removed in t.Cleanup), never by altering
// product code. Every subtest checks every table of the chain, so a step
// left in a transaction of its own shows up whichever step fails.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

const atomicRepoURL = "https://example.com/atomic.git"
const atomicHash = "0123456789abcdef0123456789abcdef01234567"

// atomicSnapshot is everything one change may write about the issue.
type atomicSnapshot struct {
	version       int
	title         string
	status        string
	outcome       string
	labels        []string
	components    []uuid.UUID
	commits       int
	events        []string
	notifications int
}

func readAtomicSnapshot(t *testing.T, pool *pgxpool.Pool, id string) atomicSnapshot {
	t.Helper()
	ctx := context.Background()
	var s atomicSnapshot
	var outcome *string
	if err := pool.QueryRow(ctx, `SELECT version, title, status, close_outcome FROM issues WHERE id = $1`, id).
		Scan(&s.version, &s.title, &s.status, &outcome); err != nil {
		t.Fatalf("read issue %s: %v", id, err)
	}
	if outcome != nil {
		s.outcome = *outcome
	}
	var label string
	labelRows := `SELECT label FROM issue_labels WHERE issue_id = $1 ORDER BY label`
	rows, err := pool.Query(ctx, labelRows, id)
	if err != nil {
		t.Fatalf("labels: %v", err)
	}
	for rows.Next() {
		if err := rows.Scan(&label); err != nil {
			t.Fatalf("scan label: %v", err)
		}
		s.labels = append(s.labels, label)
	}
	rows.Close()
	var comp uuid.UUID
	rows, err = pool.Query(ctx, `SELECT component_id FROM issue_components WHERE issue_id = $1 ORDER BY component_id`, id)
	if err != nil {
		t.Fatalf("components: %v", err)
	}
	for rows.Next() {
		if err := rows.Scan(&comp); err != nil {
			t.Fatalf("scan component: %v", err)
		}
		s.components = append(s.components, comp)
	}
	rows.Close()
	var ev string
	rows, err = pool.Query(ctx, `SELECT type FROM events WHERE entity_type = 'issue' AND entity_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for rows.Next() {
		if err := rows.Scan(&ev); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		s.events = append(s.events, ev)
	}
	rows.Close()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issue_commits WHERE issue_id = $1`, id).Scan(&s.commits); err != nil {
		t.Fatalf("commits: %v", err)
	}
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM notifications n JOIN events e ON e.id = n.event_id
 WHERE e.entity_type = 'issue' AND e.entity_id = $1`, id).Scan(&s.notifications); err != nil {
		t.Fatalf("notifications: %v", err)
	}
	return s
}

func assertNothingWritten(t *testing.T, before, after atomicSnapshot) {
	t.Helper()
	if after.version != before.version {
		t.Errorf("version %d → %d, want unchanged", before.version, after.version)
	}
	if after.title != before.title {
		t.Errorf("title %q → %q, want unchanged (field patch step)", before.title, after.title)
	}
	if after.status != before.status || after.outcome != before.outcome {
		t.Errorf("status/outcome %s/%s → %s/%s, want unchanged (close step)",
			before.status, before.outcome, after.status, after.outcome)
	}
	if after.commits != before.commits {
		t.Errorf("commit refs %d → %d, want unchanged (commits step)", before.commits, after.commits)
	}
	if !slices.Equal(after.components, before.components) {
		t.Errorf("components %v → %v, want unchanged (components step)", before.components, after.components)
	}
	if !slices.Equal(after.labels, before.labels) {
		t.Errorf("labels %v → %v, want unchanged (labels steps)", before.labels, after.labels)
	}
	if !slices.Equal(after.events, before.events) {
		t.Errorf("events %v → %v, want none written", before.events, after.events)
	}
	if after.notifications != before.notifications {
		t.Errorf("notifications %d → %d, want none written", before.notifications, after.notifications)
	}
}

// installFailingTrigger makes every statement of the given kind on table,
// for rows of issue id, raise `errcode` until the test ends.
func installFailingTrigger(t *testing.T, pool *pgxpool.Pool, table, timing, when, errcode string) {
	t.Helper()
	ctx := context.Background()
	name := "t015_fail_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $f$
BEGIN
  RAISE EXCEPTION 'injected failure on %s' USING ERRCODE = '%s';
END $f$`, name, table, errcode)); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, name, table))
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
	})
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
CREATE TRIGGER %s %s ON %s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION %s()`,
		name, timing, table, when, name)); err != nil {
		t.Fatalf("create trigger on %s: %v", table, err)
	}
}

type atomicFixture struct {
	pool      *pgxpool.Pool
	store     *Store
	pid       uuid.UUID
	prefix    string
	owner     uuid.UUID
	actor     Actor
	component uuid.UUID
	n         int
}

func newAtomicFixture(t *testing.T) *atomicFixture {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	testsupport.SeedRepo(t, pool, pid, "atomic", atomicRepoURL)
	return &atomicFixture{
		pool:      pool,
		store:     NewStore(pool),
		pid:       pid,
		prefix:    prefix,
		owner:     owner,
		actor:     Actor{UserID: owner, Username: testsupport.Username(t, pool, owner), Role: "human"},
		component: seedComponent(t, pool, pid, owner, "atomic-comp"),
	}
}

// seed creates an open issue carrying the label "old" at version 1.
func (f *atomicFixture) seed(t *testing.T) string {
	t.Helper()
	f.n++
	id := fmt.Sprintf("%s-%d", f.prefix, f.n)
	seedIssue(t, f.pool, f.pid, f.owner, id, "before")
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO issue_labels (issue_id, label) VALUES ($1, 'old')`, id); err != nil {
		t.Fatalf("seed label: %v", err)
	}
	return id
}

// fullChange touches every step of the chain: fields, close with a commit
// ref, components, a label added and a label removed.
func (f *atomicFixture) fullChange(id string, version int) UpdateIssueInput {
	title := "after"
	outcome := "done"
	return UpdateIssueInput{
		ID:              id,
		ExpectedVersion: version,
		Actor:           f.actor,
		Patch:           &PatchIssueInput{Title: &title},
		Close: &CloseInput{
			Reason:       "shipped",
			CloseOutcome: &outcome,
			Commits:      []CloseCommitInput{{Hash: atomicHash, RepoURL: atomicRepoURL}},
		},
		ReplaceComponents: true,
		Components:        []uuid.UUID{f.component},
		LabelsAdd:         []string{"new"},
		LabelsRemove:      []string{"old"},
	}
}

func TestUpdateIssue_RefusalAtAnyStepWritesNothing(t *testing.T) {
	f := newAtomicFixture(t)

	steps := []struct {
		name, table, timing, when string
	}{
		{"fields", "issues", "BEFORE UPDATE", "NEW.title IS DISTINCT FROM OLD.title"},
		{"status and close", "issues", "BEFORE UPDATE", "NEW.status IS DISTINCT FROM OLD.status"},
		{"commits", "issue_commits", "BEFORE INSERT", "true"},
		{"components", "issue_components", "BEFORE INSERT", "true"},
		{"label added", "issue_labels", "BEFORE INSERT", "true"},
		{"label removed", "issue_labels", "BEFORE DELETE", "true"},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			id := f.seed(t)
			row := "NEW"
			if step.timing == "BEFORE DELETE" {
				row = "OLD"
			}
			idColumn := "issue_id"
			if step.table == "issues" {
				idColumn = "id"
			}
			installFailingTrigger(t, f.pool, step.table, step.timing,
				fmt.Sprintf("%s.%s = '%s' AND %s", row, idColumn, id, step.when), "P0001")
			before := readAtomicSnapshot(t, f.pool, id)

			_, err := f.store.UpdateIssue(context.Background(), f.fullChange(id, before.version))
			if err == nil {
				t.Fatalf("UpdateIssue succeeded although the %s step was made to fail", step.name)
			}
			assertNothingWritten(t, before, readAtomicSnapshot(t, f.pool, id))
		})
	}

	t.Run("success writes every step and its events after the commit", func(t *testing.T) {
		id := f.seed(t)
		before := readAtomicSnapshot(t, f.pool, id)

		res, err := f.store.UpdateIssue(context.Background(), f.fullChange(id, before.version))
		if err != nil {
			t.Fatalf("UpdateIssue: %v", err)
		}
		after := readAtomicSnapshot(t, f.pool, id)
		// Field patch, close and component replace each move the version;
		// labels do not (spec of the writers, unchanged by US-14).
		if after.version != before.version+3 || res.State.Version != after.version {
			t.Errorf("version %d → %d (answered %d), want %d", before.version, after.version,
				res.State.Version, before.version+3)
		}
		if after.title != "after" || after.status != StatusClosed || after.outcome != "done" {
			t.Errorf("issue = %q %s/%s, want after closed/done", after.title, after.status, after.outcome)
		}
		if after.commits != 1 {
			t.Errorf("commit refs = %d, want 1", after.commits)
		}
		if !slices.Equal(after.components, []uuid.UUID{f.component}) {
			t.Errorf("components = %v, want [%s]", after.components, f.component)
		}
		if !slices.Equal(after.labels, []string{"new"}) {
			t.Errorf("labels = %v, want [new]", after.labels)
		}
		written := after.events[len(before.events):]
		for _, want := range []string{EventIssueUpdated, EventIssueClosed, EventComponentsChanged, EventLabelAdded, EventLabelRemoved} {
			if !slices.Contains(written, want) {
				t.Errorf("events written %v lack %s", written, want)
			}
		}
	})

	// Review C5-01 (INV-7, US-6): a change made only of components or only
	// of labels checks the caller's version too.
	for _, only := range []struct {
		name   string
		change func(id string, version int) UpdateIssueInput
	}{
		{"components only", func(id string, version int) UpdateIssueInput {
			return UpdateIssueInput{ID: id, ExpectedVersion: version, Actor: f.actor,
				ReplaceComponents: true, Components: []uuid.UUID{f.component}}
		}},
		{"labels only", func(id string, version int) UpdateIssueInput {
			return UpdateIssueInput{ID: id, ExpectedVersion: version, Actor: f.actor,
				LabelsAdd: []string{"new"}, LabelsRemove: []string{"old"}}
		}},
	} {
		t.Run(only.name+" with a stale version is a version conflict and writes nothing", func(t *testing.T) {
			id := f.seed(t)
			before := readAtomicSnapshot(t, f.pool, id)

			_, err := f.store.UpdateIssue(context.Background(), only.change(id, before.version+1))
			var conflict *ErrVersionConflict
			if !errors.As(err, &conflict) || conflict.CurrentVersion != before.version {
				t.Fatalf("UpdateIssue with version %d on %d = %v, want ErrVersionConflict{%d}",
					before.version+1, before.version, err, before.version)
			}
			assertNothingWritten(t, before, readAtomicSnapshot(t, f.pool, id))
		})
		t.Run(only.name+": a serialization conflict answers serialization_retry", func(t *testing.T) {
			id := f.seed(t)
			table := "issue_labels"
			if only.name == "components only" {
				table = "issue_components"
			}
			installFailingTrigger(t, f.pool, table, "BEFORE INSERT",
				fmt.Sprintf("NEW.issue_id = '%s'", id), "40001")
			before := readAtomicSnapshot(t, f.pool, id)

			_, err := f.store.UpdateIssue(context.Background(), only.change(id, before.version))
			if !errors.Is(err, ErrSerializationRetry) {
				t.Fatalf("UpdateIssue = %v, want ErrSerializationRetry", err)
			}
			assertNothingWritten(t, before, readAtomicSnapshot(t, f.pool, id))
		})
	}

	t.Run("a serialization conflict answers serialization_retry and writes nothing", func(t *testing.T) {
		id := f.seed(t)
		installFailingTrigger(t, f.pool, "issue_labels", "BEFORE INSERT",
			fmt.Sprintf("NEW.issue_id = '%s'", id), "40001")
		before := readAtomicSnapshot(t, f.pool, id)

		_, err := f.store.UpdateIssue(context.Background(), f.fullChange(id, before.version))
		if !errors.Is(err, ErrSerializationRetry) {
			t.Fatalf("UpdateIssue = %v, want ErrSerializationRetry", err)
		}
		assertNothingWritten(t, before, readAtomicSnapshot(t, f.pool, id))
	})
}
