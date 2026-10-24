package issues

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// sk-303.73 — a write of an issue must not answer version_conflict because
// the SAME user is creating issues in OTHER projects at the same time.
//
// The defect this pins was seen on the live stack: a person closed an issue
// through the UI while other sessions were creating issues elsewhere, and
// the close came back 422 version_conflict carrying the very version the
// caller had sent (currentVersion 1, expected 1 — nobody had modified the
// issue). The write paths and CreateIssue all run SERIALIZABLE; SSI aborts
// one side of a read/write dependency with SQLSTATE 40001 although the two
// transactions share no issue. The write path replays such an abort through
// withSerializableRetry, and when a burst of concurrent creates outlasts
// its budget, degradeSerializationAbort turns the exhausted budget into the
// false conflict.
//
// The expected outcome is the contract of each operation, not a reading of
// the code: every write of an issue whose version the caller holds — and
// that nobody else is modifying — succeeds and is applied, and every
// concurrent create succeeds too. A false version_conflict, any other
// error, or a write that "succeeded" without its effect fails the test.
//
// Contention shape: one user creates issues in creatorProjects projects
// from as many sessions at once while writers sessions of the same user
// each write their own issues of another project — one person (or one
// agent) working in several tabs or sessions. No issue is shared between
// the two sides, so every 40001 here is SSI contention, never a genuine
// concurrent edit.
func TestWritePaths_ConcurrentCreatesElsewhere_NoFalseVersionConflict(t *testing.T) {
	cases := []struct {
		name string
		// write performs the operation on d, which holds the caller's version.
		write func(ctx context.Context, s *Store, d *IssueDetail, actor Actor) error
		// applied reports whether the operation's effect on d is in the row.
		applied func(d *IssueDetail, status, title string) bool
	}{
		{
			name: "updateStatus",
			write: func(ctx context.Context, s *Store, d *IssueDetail, actor Actor) error {
				_, err := s.UpdateStatus(ctx, d.ID, UpdateStatusInput{ExpectedVersion: d.Version, Status: StatusClosed}, actor)
				return err
			},
			applied: func(_ *IssueDetail, status, _ string) bool { return status == StatusClosed },
		},
		{
			name: "close",
			write: func(ctx context.Context, s *Store, d *IssueDetail, actor Actor) error {
				_, err := s.Close(ctx, CloseInput{ID: d.ID, ExpectedVersion: d.Version, Reason: "done", Actor: actor})
				return err
			},
			applied: func(_ *IssueDetail, status, _ string) bool { return status == StatusClosed },
		},
		{
			name: "patchIssue",
			write: func(ctx context.Context, s *Store, d *IssueDetail, actor Actor) error {
				title := d.Title + " renamed"
				_, err := s.PatchIssue(ctx, d.ID, PatchIssueInput{ExpectedVersion: d.Version, Title: &title}, actor)
				return err
			},
			applied: func(d *IssueDetail, _, title string) bool { return title == d.Title+" renamed" },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runConcurrentCreatesElsewhere(t, c.write, c.applied)
		})
	}
}

func runConcurrentCreatesElsewhere(
	t *testing.T,
	write func(ctx context.Context, s *Store, d *IssueDetail, actor Actor) error,
	applied func(d *IssueDetail, status, title string) bool,
) {
	pool := newTestPool(t)
	ctx := context.Background()
	user := seedUser(t, pool)
	actor := HumanActor(user, readUsername(t, pool, user))
	store := NewStore(pool)

	const (
		creatorProjects   = 8
		createsPerProject = 4
		writers           = 4
		writesPerWriter   = 3
	)

	type project struct {
		id     uuid.UUID
		prefix string
	}
	seedOwnProject := func() project {
		pid, prefix := seedProject(t, pool, user)
		t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
		return project{pid, prefix}
	}

	target := seedOwnProject()
	toWrite := make([][]*IssueDetail, writers)
	for w := range writers {
		for i := range writesPerWriter {
			d, err := store.CreateIssue(ctx, target.id, target.prefix, "counter",
				CreateIssueInput{Title: fmt.Sprintf("to write %d.%d", w, i), Type: "task", Priority: 2},
				user, actor)
			if err != nil {
				t.Fatalf("seed issue to write: %v", err)
			}
			toWrite[w] = append(toWrite[w], d)
		}
	}
	others := make([]project, creatorProjects)
	for i := range others {
		others[i] = seedOwnProject()
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		writeErrs []error
		createErr []error
	)
	start := make(chan struct{})
	for _, p := range others {
		wg.Add(1)
		go func(p project) {
			defer wg.Done()
			<-start
			for i := range createsPerProject {
				_, err := store.CreateIssue(context.Background(), p.id, p.prefix, "counter",
					CreateIssueInput{Title: fmt.Sprintf("elsewhere %d", i), Type: "task", Priority: 2},
					user, actor)
				if err != nil {
					mu.Lock()
					createErr = append(createErr, err)
					mu.Unlock()
				}
			}
		}(p)
	}
	for w := range writers {
		wg.Add(1)
		go func(issues []*IssueDetail) {
			defer wg.Done()
			<-start
			for _, d := range issues {
				if err := write(context.Background(), store, d, actor); err != nil {
					mu.Lock()
					writeErrs = append(writeErrs, fmt.Errorf("write %s (version %d): %w", d.ID, d.Version, err))
					mu.Unlock()
				}
			}
		}(toWrite[w])
	}
	close(start)
	wg.Wait()

	for _, err := range writeErrs {
		var verr *ErrVersionConflict
		if errors.As(err, &verr) {
			t.Errorf("false version_conflict — nobody else modified the issue: %v", err)
			continue
		}
		t.Errorf("write failed: %v", err)
	}
	for _, err := range createErr {
		t.Errorf("concurrent create failed: %v", err)
	}
	for _, issues := range toWrite {
		for _, d := range issues {
			var status, title string
			if err := pool.QueryRow(ctx, `SELECT status, title FROM issues WHERE id = $1`, d.ID).Scan(&status, &title); err != nil {
				t.Fatalf("read %s: %v", d.ID, err)
			}
			if !applied(d, status, title) {
				t.Errorf("%s: the write is not applied (status %q, title %q)", d.ID, status, title)
			}
		}
	}
}
