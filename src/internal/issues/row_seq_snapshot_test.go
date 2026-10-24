package issues

// row_seq_snapshot_test.go — sk-622.21, T026, matrix row No. 55 (the read-path
// part), regression found in review
// (decision R26 item 2).
//
// Reference (spec US-15, plan K16, R26 item 2): a REST answer hands out a row's
// `rowSeq` together with the fields that the phase-2 event row replaces —
// labels and components among them — and the client substitutes an event
// row only if its rowSeq is greater than the shown one. So the answer must
// be ONE snapshot: its labels are exactly the committed labels at its
// rowSeq. If the labels were read later than rowSeq, a label commit landing
// in between would give an answer with newer labels and an older rowSeq; an
// intermediate event row (already captured by the outbox) then has a
// greater rowSeq, passes the strict-greater rule and rolls the shown labels
// back — "a repeated or reordered delivery does not bring back old marks" is
// broken.
//
// How: the reader runs on a pool whose tracer parks it right after the
// statement that reads rowSeq. While it is parked the test commits label B,
// captures the event row the outbox would publish (NeutralRowsForIDs),
// removes B and adds C, then releases the reader. Every committed state is
// recorded as (row_seq → labels). The answer is consistent iff its labels
// equal the recorded labels at its rowSeq.
//
// What turns it red: any of the card / list / tree paths that reads labels
// or components in a statement other than the one that reads rowSeq.

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
)

// committedLabels reads the issue's labels straight from the table, in the
// REST order.
func committedLabels(t *testing.T, pool *pgxpool.Pool, id string) []string {
	t.Helper()
	var out []string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(array_agg(label ORDER BY label), '{}') FROM issue_labels WHERE issue_id = $1`, id).
		Scan(&out); err != nil {
		t.Fatalf("read labels of %s: %v", id, err)
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// snapshotReader is one REST read path: it returns the issue's rowSeq and
// labels as the answer carries them.
type snapshotReader struct {
	name string
	// marker is a fragment of the statement that reads the row's rowSeq —
	// present before and after the fix, and in no earlier statement of the
	// same read.
	marker string
	read   func(ctx context.Context, s *Store, f rowSeqFixture) (int64, []string, error)
}

func findSummary(items []IssueSummary, id string) (IssueSummary, bool) {
	for _, it := range items {
		if it.ID == id {
			return it, true
		}
	}
	return IssueSummary{}, false
}

// TestRowSeq_AnswerIsOneSnapshot — see the file header.
func TestRowSeq_AnswerIsOneSnapshot(t *testing.T) {
	if os.Getenv(testDatabaseURL) == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	const listMarker = "AS i_assigned"
	readers := []snapshotReader{
		{"GetIssue (card)", "previous_assignee_usernames", func(ctx context.Context, s *Store, f rowSeqFixture) (int64, []string, error) {
			card, err := s.GetIssue(ctx, f.issueID)
			if err != nil {
				return 0, nil, err
			}
			return card.RowSeq, card.Labels, nil
		}},
		{"ListIssues (project list)", listMarker, func(ctx context.Context, s *Store, f rowSeqFixture) (int64, []string, error) {
			r, err := s.ListIssues(ctx, ListIssuesArgs{ProjectID: f.project, Limit: 50})
			if err != nil {
				return 0, nil, err
			}
			row, ok := findSummary(r.Items, f.issueID)
			if !ok {
				return 0, nil, errNotInAnswer
			}
			return row.RowSeq, row.Labels, nil
		}},
		{"ListAllIssues (cross-project list)", listMarker, func(ctx context.Context, s *Store, f rowSeqFixture) (int64, []string, error) {
			r, err := s.ListAllIssues(ctx, ListAllIssuesArgs{
				Scope:  authz.ForUser(f.owner),
				Caller: f.owner, Limit: 50,
			})
			if err != nil {
				return 0, nil, err
			}
			row, ok := findSummary(r.Items, f.issueID)
			if !ok {
				return 0, nil, errNotInAnswer
			}
			return row.RowSeq, row.Labels, nil
		}},
		{"ListByUser (/me list)", listMarker, func(ctx context.Context, s *Store, f rowSeqFixture) (int64, []string, error) {
			r, err := s.ListByUser(ctx, ListByUserArgs{
				Scope:  authz.ForUser(f.owner),
				UserID: f.owner, Mode: ModeCreatedBy, Limit: 50,
			})
			if err != nil {
				return 0, nil, err
			}
			row, ok := findSummary(r.Items, f.issueID)
			if !ok {
				return 0, nil, errNotInAnswer
			}
			return row.RowSeq, row.Labels, nil
		}},
		{"IssueTree (tree rows)", listMarker, func(ctx context.Context, s *Store, f rowSeqFixture) (int64, []string, error) {
			r, err := s.IssueTree(ctx, IssueTreeArgs{
				Scope:     authz.ForUser(f.owner),
				Caller:    f.owner,
				ProjectID: &f.project, Types: []string{"parent-child"},
			})
			if err != nil {
				return 0, nil, err
			}
			for _, n := range r.Items {
				if n.Item.ID == f.issueID {
					return n.Item.RowSeq, n.Item.Labels, nil
				}
			}
			return 0, nil, errNotInAnswer
		}},
	}

	for _, rd := range readers {
		t.Run(rd.name, func(t *testing.T) {
			f := newRowSeqFixture(t)
			ctx := callerCtx(t, f.owner, "user")
			actor := HumanActor(f.owner, f.name)

			// Committed history of the row: row_seq → labels.
			history := map[int64][]string{}
			record := func() {
				seq, _ := rowState(t, f.pool, f.issueID)
				history[seq] = committedLabels(t, f.pool, f.issueID)
			}
			record()

			cfg, err := pgxpool.ParseConfig(os.Getenv(testDatabaseURL))
			if err != nil {
				t.Fatalf("parse %s: %v", testDatabaseURL, err)
			}
			tracer := &rowSeqHoldTracer{}
			cfg.ConnConfig.Tracer = tracer
			heldPool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(heldPool.Close)
			reader := NewStore(heldPool)

			reached, release := tracer.arm(rd.marker)
			released := false
			t.Cleanup(func() {
				if !released {
					close(release)
				}
			})

			type answer struct {
				seq    int64
				labels []string
				err    error
			}
			done := make(chan answer, 1)
			go func() {
				seq, labels, err := rd.read(context.WithoutCancel(ctx), reader, f)
				done <- answer{seq, labels, err}
			}()
			select {
			case <-reached:
			case <-time.After(10 * time.Second):
				t.Fatalf("could not orchestrate: %s never ran the statement with %q", rd.name, rd.marker)
			}

			// Between the rowSeq read and the rest of the answer: B is
			// committed and its event row captured, then B is replaced by C.
			if _, err := f.store.AddLabelNoLoad(ctx, f.issueID, "snap-b", actor, LockOpts{}); err != nil {
				t.Fatalf("add label B: %v", err)
			}
			record()
			events, err := NeutralRowsForIDs(ctx, f.pool, []string{f.issueID})
			if err != nil {
				t.Fatalf("capture event row: %v", err)
			}
			event, ok := events[f.issueID]
			if !ok {
				t.Fatalf("capture event row: no row for %s", f.issueID)
			}
			if _, err := f.store.RemoveLabelNoLoad(ctx, f.issueID, "snap-b", actor, LockOpts{}); err != nil {
				t.Fatalf("remove label B: %v", err)
			}
			record()
			if _, err := f.store.AddLabelNoLoad(ctx, f.issueID, "snap-c", actor, LockOpts{}); err != nil {
				t.Fatalf("add label C: %v", err)
			}
			record()

			close(release)
			released = true
			var got answer
			select {
			case got = <-done:
			case <-time.After(30 * time.Second):
				t.Fatalf("%s did not return within 30s", rd.name)
			}
			if got.err != nil {
				t.Fatalf("%s: %v", rd.name, got.err)
			}
			if got.labels == nil {
				got.labels = []string{}
			}

			want, known := history[got.seq]
			if !known {
				t.Fatalf("%s: answer rowSeq = %d is none of the committed orders %v", rd.name, got.seq, history)
			}
			if !slices.Equal(got.labels, want) {
				t.Errorf("%s: answer rowSeq = %d with labels %v, but the committed labels at that order are %v — "+
					"two snapshots. The captured event row (rowSeq = %d, labels %v) passes the strict-greater "+
					"rule against this answer and rolls its labels back (US-15)",
					rd.name, got.seq, got.labels, want, event.RowSeq, event.Labels)
			}
		})
	}
}

// errNotInAnswer — the fixture issue is missing from a list answer.
var errNotInAnswer = errors.New("the issue is not in the answer")
