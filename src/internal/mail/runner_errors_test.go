package mail_test

// runner_errors_test.go — a failed pass of the mail worker (sk-674.18, T022;
// matrix row 77). No database: the pool is closed before the first pass.

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/mail"
)

type noIssuer struct{}

func (noIssuer) IssueForEmail(context.Context, uuid.UUID, int) (mail.Issued, error) {
	return mail.Issued{}, mail.ErrNotIssuable
}

func closedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	// pgxpool connects lazily: building it touches no network.
	pool, err := pgxpool.New(context.Background(), "postgres://nobody:nothing@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	return pool
}

func runnerErrors(t *testing.T) float64 {
	return counter(t, "skein_email_runner_errors_total", nil)
}

// Row 77 (K-24): a pass of the worker on a closed pool fails, adds exactly
// one to skein_email_runner_errors_total, and the worker's loop survives it
// — it keeps going until its context ends and then returns. The loop runs
// in a testing/synctest bubble: its back-off timers and the context
// deadline are virtual, so progress is proven by moving time, not measured
// on a live clock (§5). A closed pool fails at once,
// without I/O, so the loop is durably blocked only in its own waits.
func TestRunner_FailedPassIsCountedAndSurvived(t *testing.T) {
	pool := closedPool(t)
	run, err := mail.NewRunner(pool, &relay{}, noIssuer{}, linkBase, mail.Options{PollInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	before := runnerErrors(t)
	if _, err := run.Tick(context.Background()); err == nil {
		t.Fatal("precondition: a pass on a closed pool succeeded")
	}
	if got := runnerErrors(t) - before; got != 1 {
		t.Fatalf("skein_email_runner_errors_total grew by %v after one failed pass, want 1", got)
	}

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		before := runnerErrors(t)
		done := make(chan struct{})
		go func() {
			defer close(done)
			run.Run(ctx)
		}()

		// The first pass has failed and the loop waits in its back-off.
		synctest.Wait()
		if got := runnerErrors(t) - before; got != 1 {
			t.Fatalf("after the first pass the counter grew by %v, want 1", got)
		}

		// Back-off doubles from 1 s: within 59 virtual seconds the loop has
		// failed again and again, and is still running.
		time.Sleep(59 * time.Second)
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("the worker's loop returned on a failed pass, before its context ended")
		default:
		}
		if got := runnerErrors(t) - before; got < 2 {
			t.Fatalf("the loop counted %v failed passes, want at least 2 — it did not keep going after a failure", got)
		}

		// Past the context's deadline the loop returns.
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("the worker's loop did not return after its context ended")
		}
	})
}
