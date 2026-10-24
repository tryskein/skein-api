package review_test

// set_value_form_test.go — sk-303.14, repair A/8, second round.
//
// VALUE FORM of the mechanic switch — the one used by the lens counter
// (handler/me.go), the list filter (issues/store.go, issues/list_by_user.go)
// and the re-check at delivery (outbox/digest.go). EXPRESSION form (a join
// column) is held by the background worker's selection and its own classes.
//
// # How this form was covered poorly
//
// A mutation "break mechanicEnabledFor in BOTH forms" went red widely — but
// the overwhelming share of that redness arrived as a 500 from a mismatch
// between the number of bound parameters and placeholders, that is on a
// PRECONDITION: the query never ran at all. Nothing about the expression
// answering CORRECTLY follows from that redness, and a mutation that keeps
// the parameter count (inverted default, negated column, a condition lifted
// out of the fragment) would have passed unnoticed.
//
// The assertions here run the query to completion and look at the VALUE.
//
// The package is external (review_test) on purpose: what is checked is the
// public contract — what consumers assemble — not the internals.

import (
	"context"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/review"
	"skein-api/internal/testsupport"
)

// binder is the same binding device every consumer of the fragment uses.
func binder(params *[]any) review.Binder {
	return func(v any) string {
		*params = append(*params, v)
		return "$" + strconv.Itoa(len(*params))
	}
}

// TestMechanicEnabledExpr_ValueFormReadsTheStoredSwitch — three states of
// the switch, all three observable.
//
// Mutations that keep the parameter count and so were NOT caught before:
//
//	COALESCE(..., false) instead of DefaultMechanicEnabled → "no row" goes red;
//	NOT p.review_mechanic_enabled                          → "on" and "off" go red.
func TestMechanicEnabledExpr_ValueFormReadsTheStoredSwitch(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := t.Context()

	noRow, _ := testsupport.SeedUser(t, pool, "user")
	on, _ := testsupport.SeedUser(t, pool, "user")
	off, _ := testsupport.SeedUser(t, pool, "user")

	for id, enabled := range map[uuid.UUID]bool{on: true, off: false} {
		if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_prefs (user_id, review_mechanic_enabled)
VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE SET review_mechanic_enabled = EXCLUDED.review_mechanic_enabled`,
			id, enabled); err != nil {
			t.Fatalf("saving the switch: %v", err)
		}
	}

	for _, tc := range []struct {
		name string
		user uuid.UUID
		want bool
	}{
		{"no settings row — built-in default", noRow, review.DefaultMechanicEnabled},
		{"switch on", on, true},
		{"switch off", off, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var params []any
			expr := review.MechanicEnabledExpr(binder(&params), tc.user)
			var got bool
			if err := pool.QueryRow(ctx, "SELECT "+expr, params...).Scan(&got); err != nil {
				t.Fatalf("evaluating expression: %v\nSQL: SELECT %s", err, expr)
			}
			if got != tc.want {
				t.Errorf("switch expression gave %v, want %v. VALUE form "+
					"reads the wrong source or the wrong default: the lens "+
					"counter, the list filter and the re-check at delivery will "+
					"drift from what the person chose in settings (INV-6)", got, tc.want)
			}
		})
	}
}

// TestUnreviewedPredicate_ValueFormObeysTheMechanicSwitch — the same switch,
// but inside the FRAGMENT, and by the count of selected issues.
//
// The assertion is two-sided: with the mechanic off the set is EMPTY, with
// it on — exactly the issue that was seeded. A one-sided half would also
// pass on a fragment that selects nothing at all.
//
// A mutation "drop `AND mechanicEnabledFor(callerPh)` from
// UnreviewedPredicate" does not change the parameter count (the identifier
// is bound without it), so it yields no 500 and would previously have gone
// green. Here it goes red on the substance: a person who turned the mechanic
// off still has unreviewed items.
func TestUnreviewedPredicate_ValueFormObeysTheMechanicSwitch(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := t.Context()

	author, _ := testsupport.SeedUser(t, pool, "user")
	_, assigneeName := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, author, "vform")
	testsupport.AddProjectMember(t, pool, pid, author, "member")

	id := prefix + "-1"
	if _, err := pool.Exec(ctx, `
INSERT INTO issues (id, project_id, version, title, description, type, priority, status,
                    source, assignee, created_by, created_at, updated_at,
                    closed_at, review_required, dismissed_by, previous_assignees)
VALUES ($1, $2, 1, 'v', '', 'task', 2, 'closed', 'human', $3, $4,
        now(), now(), now(), true, '{}'::uuid[], '{}'::uuid[])`,
		id, pid, assigneeName, author); err != nil {
		t.Fatalf("seeding issue: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, id)
	})

	count := func(t *testing.T) int {
		t.Helper()
		var params []any
		pred := review.UnreviewedPredicate(binder(&params), review.UnreviewedArgs{
			Alias: "i", CallerID: author})
		var n int
		sql := "SELECT COUNT(*) FROM issues i WHERE i.project_id = $" +
			strconv.Itoa(len(params)+1) + " AND " + pred
		params = append(params, pid)
		if err := pool.QueryRow(ctx, sql, params...).Scan(&n); err != nil {
			t.Fatalf("counting unreviewed: %v\nSQL: %s", err, sql)
		}
		return n
	}

	for _, tc := range []struct {
		name    string
		enabled bool
		want    int
	}{
		{"mechanic on", true, 1},
		{"mechanic off", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_prefs (user_id, review_mechanic_enabled)
VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE SET review_mechanic_enabled = EXCLUDED.review_mechanic_enabled`,
				author, tc.enabled); err != nil {
				t.Fatalf("saving the switch: %v", err)
			}
			if got := count(t); got != tc.want {
				t.Errorf("unreviewed count %d, want %d with switch %v. The fragment "+
					"in VALUE form does not consult the mechanic switch: a person "+
					"who turned it off still sees the set (INV-6)",
					got, tc.want, tc.enabled)
			}
		})
	}
}
