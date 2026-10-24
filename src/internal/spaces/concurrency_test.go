package spaces_test

// concurrency_test.go — sk-674.18 T018, matrix row 48 (E-2, K-7): an
// assignment, an addition to a project and a visibility change started
// while a user's leave holds the row of the space wait for it (the wait is
// a lock wait in pg_stat_activity, blocked by the leave's backend), and the
// same operation in another space does not wait. After the leave commits,
// what the leave decides holds: a visibility change gives the departed user
// no row (INV-7 reads the participants after the leave).
//
// The halves that depend on later rules are named cells that skip (lead's
// decision 2026-10-13, sk-674.18): an assignment of the departed user after
// the leave is refused or left unresolved by INV-30 — T028; an addition of
// a non-participant to a project of the space is refused by INV-5 — T024.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
)

// heldLeave runs the leave of user from space in a transaction that stays
// open until release is called; pid is its backend. The leave goes through
// spaces.WithRetry like the production path: the membership primitives read
// the relation, so a SERIALIZABLE conflict with an operation in another
// space is possible and is absorbed by the retry (lead's decision on review
// api/4 #7). Only the first attempt is held.
type heldLeave struct {
	pid     int32
	release chan struct{}
	done    chan error
}

func holdLeave(t *testing.T, w *world) *heldLeave {
	t.Helper()
	h := &heldLeave{release: make(chan struct{}), done: make(chan error, 1)}
	started := make(chan error, 1)
	go func() {
		first := true
		h.done <- spaces.WithRetry(context.Background(), w.pool, func(tx pgx.Tx) error {
			held := first
			first = false
			if held {
				if err := tx.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&h.pid); err != nil {
					started <- err
					return err
				}
			}
			if err := spaces.LeaveInTx(context.Background(), tx, w.space, w.u, "left_space", false, w.actor); err != nil {
				if held {
					started <- err
				}
				return err
			}
			if held {
				started <- nil
				<-h.release
			}
			return nil
		})
	}()
	if err := <-started; err != nil {
		t.Fatalf("held leave: %v", err)
	}
	t.Cleanup(func() {
		select {
		case <-h.release:
		default:
			close(h.release)
		}
	})
	return h
}

func (h *heldLeave) commit(t *testing.T) {
	t.Helper()
	close(h.release)
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("leave commit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the held leave did not commit")
	}
}

// blocked reports whether some backend waits on a lock held by pid, within
// a few seconds.
func blocked(t *testing.T, pool *pgxpool.Pool, pid int32) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM pg_stat_activity
 WHERE wait_event_type = 'Lock' AND $1::int = ANY(pg_blocking_pids(pid))`, pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func async(fn func() error) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- fn() }()
	return ch
}

func awaitErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(15 * time.Second):
		t.Fatalf("%s did not return", what)
		return nil
	}
}

func TestMembership_LeaveHoldsTheSpace(t *testing.T) {
	ctx := context.Background()
	istore := func(w *world) *issues.Store { return issues.NewStore(w.pool) }

	t.Run("assignment waits", func(t *testing.T) {
		w := newWorld(t, "members")
		w.setRow(t, "manual")
		issue := w.assignedIssue
		testsupport.SetAssignee(t, w.pool, issue, "")
		var version int
		if err := w.pool.QueryRow(ctx, `SELECT version FROM issues WHERE id = $1`, issue).Scan(&version); err != nil {
			t.Fatal(err)
		}
		h := holdLeave(t, w)
		res := async(func() error {
			name := w.uName
			_, err := istore(w).PatchIssue(ctx, issue, issues.PatchIssueInput{ExpectedVersion: version, Assignee: &name},
				issues.HumanActor(w.pOwner, "owner"))
			return err
		})
		if !blocked(t, w.pool, h.pid) {
			t.Fatal("the assignment did not wait for the leave holding the space")
		}
		h.commit(t)
		err := awaitErr(t, res, "the assignment")
		t.Run("after the leave the departed user is not the assignee", func(t *testing.T) {
			// INV-30 (T028): a username is free text — stored as typed, not
			// refused — and it resolves to nobody once its owner may not
			// work in the project: no assigneeUserId, no «assignee» watch.
			if err != nil {
				t.Fatalf("the assignment by username after the leave = %v, want it stored as text", err)
			}
			got, gerr := istore(w).GetIssue(ctx, issue)
			if gerr != nil {
				t.Fatal(gerr)
			}
			if got.Assignee == nil || *got.Assignee != w.uName {
				t.Errorf("assignee text = %v, want %q as typed", got.Assignee, w.uName)
			}
			if got.AssigneeUserID != nil {
				t.Errorf("assigneeUserId = %v: the departed user resolves for delivery", *got.AssigneeUserID)
			}
			var watches int
			if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`,
				issue, w.u).Scan(&watches); err != nil {
				t.Fatal(err)
			}
			if watches != 0 {
				t.Errorf("the departed user got %d watch rows from the assignment", watches)
			}
		})
	})

	t.Run("addition to the project waits", func(t *testing.T) {
		w := newWorld(t, "members")
		w.setRow(t, "manual")
		h := holdLeave(t, w)
		res := async(func() error {
			_, err := w.pstore.UpsertMember(ctx, w.p, w.u, "member", w.pOwner, w.projectActor)
			return err
		})
		if !blocked(t, w.pool, h.pid) {
			t.Fatal("the addition did not wait for the leave holding the space")
		}
		h.commit(t)
		err := awaitErr(t, res, "the addition")
		t.Run("after the leave the departed user is not a member", func(t *testing.T) {
			// INV-5: once he left, he is not a participant — the addition is
			// refused with the neutral «no such user» and leaves no row.
			if !errors.Is(err, projects.ErrUserNotFound) {
				t.Errorf("the addition after the leave = %v, want projects.ErrUserNotFound", err)
			}
			if got := w.read(t, w.u); got.exists || got.seesP {
				t.Errorf("the departed user after the addition: %+v, want no row and no access", got)
			}
		})
	})

	t.Run("visibility change waits and gives the departed no row", func(t *testing.T) {
		w := newWorld(t, "members")
		w.setRow(t, "manual")
		h := holdLeave(t, w)
		res := async(func() error { return w.store.SetVisibility(ctx, w.p, spaces.VisibilitySpace, w.actor) })
		if !blocked(t, w.pool, h.pid) {
			t.Fatal("the visibility change did not wait for the leave holding the space")
		}
		h.commit(t)
		if err := awaitErr(t, res, "the visibility change"); err != nil {
			t.Fatalf("visibility change: %v", err)
		}
		if got := w.read(t, w.u); got.exists || got.seesP {
			t.Errorf("the departed user after the visibility change: %+v, want no row and no access", got)
		}
	})

	t.Run("another space does not wait", func(t *testing.T) {
		w := newWorld(t, "members")
		other := newWorld(t, "members")
		h := holdLeave(t, w)
		err := awaitErr(t, async(func() error {
			_, err := other.pstore.UpsertMember(ctx, other.p, other.u, "member", other.pOwner, other.projectActor)
			return err
		}), "an addition in another space")
		if err != nil {
			t.Fatalf("addition in another space: %v", err)
		}
		h.commit(t)
		// Both outcomes hold after a possible retry of the leave.
		if got := w.read(t, w.u); got.exists || got.inSpace {
			t.Errorf("the departed user after the leave: %+v, want no row and no participation", got)
		}
		if got := other.read(t, other.u); !got.exists || !got.manual || !got.seesP {
			t.Errorf("the user added in the other space: %+v, want a manual row and access", got)
		}
	})
}
