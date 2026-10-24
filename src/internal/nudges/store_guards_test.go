package nudges

// store_guards_test.go — sk-303.14, repair A/8, second round.
//
// Two gates added by the FIRST round of the repair and held by nothing: their
// removal was green on the whole `go test ./...`.
//
//  1. The CALL of the splice-form validator in pushEnabledExpr. Checking the
//     function says nothing about whether it is called; the neighbours in
//     internal/review have such an assertion
//     (TestPredicates_RefuseAMalformedExpression), this splice did not.
//  2. Store.Now reads the DATABASE clock. This is half of the repair of the
//     third critical, living in the tick, and it was held by a comment.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/review"
	"skein-api/internal/testsupport"
)

// mustPanicHere calls f and requires a panic with the expected prefix.
//
// The TEXT is checked, not only the fact: a panic for another reason
// (nil dereference in changed code) would pass an "it crashed" check and
// declare the guard alive when it is already gone. The same move as in
// review/set_validation_test.go.
func mustPanicHere(t *testing.T, wantPrefix string, f func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Errorf("no panic — the form of the addressee-id splice into the query "+
				"text is no longer checked (expected prefix %q)", wantPrefix)
			return
		}
		msg, ok := r.(string)
		if !ok {
			t.Errorf("panic is not a string: %#v", r)
			return
		}
		if !strings.HasPrefix(msg, wantPrefix) {
			t.Errorf("panic %q, expected prefix %q — crashed for another reason, not "+
				"on the form check", msg, wantPrefix)
		}
	}()
	f()
}

// TestPushEnabledExpr_RefusesAMalformedUserExpression — gate 1.
//
// pushEnabledExpr splices the addressee-id expression into the query text and
// must run it through the same validator as the neighbouring splice in the
// same SQL string (review.ValidateUserExpr). The assertion addresses THE CALL
// ITSELF: a mutation of "remove review.ValidateUserExpr(userExpr) from
// pushEnabledExpr" compiles and until this round was green on the whole tree.
//
// ⚠ Why "substitute an invalid expression and see what goes red" was unfit.
// The red there came from a PANIC, taking down the package binary as a whole,
// and only because the validator itself is alive. There was no check that it
// is CALLED in that: remove the call — and there is no panic, and even less
// redness.
func TestPushEnabledExpr_RefusesAMalformedUserExpression(t *testing.T) {
	bind := func(any) string { return "$1" }

	// Each form catches its own breakage: a leftover tail of a condition, a
	// statement separator, a comment, a newline, a subquery, a literal.
	for _, expr := range []string{
		"", "ru.id OR true", "ru.id; DROP TABLE issues", "ru.id--", "ru.id\nOR 1=1",
		"(SELECT id FROM users)", "1", "RU.id", "ru.id ", "$1a",
	} {
		t.Run("refuses "+expr, func(t *testing.T) {
			mustPanicHere(t, "review: invalid SQL user expression", func() {
				_, _ = pushEnabledExpr(bind, review.CategoryUnaccepted, expr)
			})
		})
	}

	// The reverse half: legal forms must pass. Without it a mutation of
	// "panic always" would pass the check above entirely, and the selection
	// would stop building at all.
	for _, expr := range []string{"ru.id", "$1", "$12", "i.created_by"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("legal form %q refused: %v — the worker's selection "+
						"will stop building", expr, r)
				}
			}()
			if _, err := pushEnabledExpr(bind, review.CategoryUnaccepted, expr); err != nil {
				t.Errorf("legal form %q gave an error: %v", expr, err)
			}
		}()
	}
}

// TestStoreNow_AsksTheDatabaseAndRefusesWhenItCannot — gate 2.
//
// Store.Now must read the DATABASE clock. The assertion cannot compare
// values: in the run the process and Postgres sit on the same machine, and
// substituting time.Now().UTC() would give numerically the same magnitude.
// What distinguishes them is ANOTHER observable property — refusal: a read
// from the database can fail to happen, and the process clock never refuses.
//
// This is not a formality. The documented tick behaviour rests on the
// refusal: "the clock could not be read — the tick is skipped entirely"
// (Scheduler.tick). A mutation of "return time.Now().UTC()" makes the
// refusal unreachable, the skip branch dead, and the INV-8 promise with
// several instances again dependent on replica clock agreement.
//
// The mutation `return time.Now().UTC(), nil` goes red here: an unreachable
// database yields a moment instead of an error.
func TestStoreNow_AsksTheDatabaseAndRefusesWhenItCannot(t *testing.T) {
	ctx := context.Background()

	// Live database: the moment is read and it is in UTC.
	live := NewStore(testsupport.NewTestPool(t))
	now, err := live.Now(ctx)
	if err != nil {
		t.Fatalf("database clock could not be read: %v", err)
	}
	if now.IsZero() {
		t.Error("database clock gave a zero moment")
	}
	if _, off := now.Zone(); off != 0 {
		t.Errorf("moment is not in UTC (offset %d s): the schedule counts in UTC, and "+
			"a shifted moment would move segment bounds", off)
	}

	// OWN pool — the shared one must not be closed, the whole fixture sits
	// on it.
	own, err := pgxpool.New(ctx, os.Getenv(testsupport.TestDatabaseURL))
	if err != nil {
		t.Fatalf("own pool: %v", err)
	}
	own.Close()

	if got, err := NewStore(own).Now(ctx); err == nil {
		t.Errorf("unreachable database gave moment %s instead of a refusal. The clock "+
			"is read not from the database but from the process: then the branch "+
			"\"clock could not be read — the tick is skipped\" is unreachable, and "+
			"INV-8 with several instances again rests on replica clock agreement", got)
	}

	// The neighbouring half of the same property: the database moment is
	// fit for schedule arithmetic without a fix — it is not behind the
	// epoch and does not fly into the future.
	if now.Before(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("database moment %s is before 2020 — the schedule would count marks "+
			"from garbage", now)
	}
}
