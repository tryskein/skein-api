package notifications

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

// unwatch_on_close_prefs_test.go — sk-622.21, decision R23 (found in
// review), the settings half.
//
// Reference: the US-2 acceptance is an invariant of the state — «on closed
// issues there are no subscriptions made only of automatic reasons for users
// with the setting on» — and R23: turning `unwatchOnClose` on removes the
// person's automatic reasons on issues that are ALREADY closed, in the same
// operation, the same way a close does (StripAutoWatchersOnClose: an
// automatic-only row is deleted, a mixed row keeps its manual reason, no
// opt-out); turning it off gives
// nothing back. Open issues and other people are not touched.
//
// Counterexample it catches: the setting write touching only the prefs row
// (the contract before R23, «acts on the next close»).
func TestUpdateReasonPrefs_TurningUnwatchOnCloseOnStripsClosedAutoWatches(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	user, _ := testsupport.SeedUser(t, pool, "user")
	other, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, user, "uoc-prefs")

	// The person starts with the setting OFF: automatic subscriptions on
	// closed issues are then legitimate state.
	if _, err := store.UpdateReasonPrefs(ctx, user, UpdateReasonPrefsInput{UnwatchOnClose: boolPtr(false)}); err != nil {
		t.Fatalf("seed setting off: %v", err)
	}

	type row struct {
		name    string
		status  string
		who     uuid.UUID
		reasons []string
		wantOn  []string // after switching on; nil = no row
	}
	rows := []row{
		{name: "closed, creator only", status: "closed", who: user, reasons: []string{"creator"}, wantOn: nil},
		{name: "closed, assignee only", status: "closed", who: user, reasons: []string{"assignee"}, wantOn: nil},
		{name: "closed, assignee + manual", status: "closed", who: user, reasons: []string{"assignee", "manual"}, wantOn: []string{"manual"}},
		{name: "closed, manual only", status: "closed", who: user, reasons: []string{"manual"}, wantOn: []string{"manual"}},
		{name: "open, creator only", status: "open", who: user, reasons: []string{"creator"}, wantOn: []string{"creator"}},
		{name: "closed, another person's creator", status: "closed", who: other, reasons: []string{"creator"}, wantOn: []string{"creator"}},
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = testsupport.SeedIssue(t, pool, pid, user, fmt.Sprintf("%s-%d", prefix, 9100+i), "uoc "+r.name, r.status, 2)
		if _, err := pool.Exec(ctx, `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since)
VALUES ($1, $2, $3::text[], now())`, ids[i], r.who, r.reasons); err != nil {
			t.Fatalf("seed watch %s: %v", r.name, err)
		}
	}

	check := func(when string) {
		t.Helper()
		for i, r := range rows {
			got, present := readReasons(t, pool, ids[i], r.who)
			if r.wantOn == nil {
				if present {
					t.Errorf("%s / %s: subscription %v kept, want none", when, r.name, got)
				}
				continue
			}
			want := slices.Clone(r.wantOn)
			slices.Sort(want)
			if !present || !slices.Equal(got, want) {
				t.Errorf("%s / %s: reasons %v (present=%v), want %v", when, r.name, got, present, want)
			}
		}
	}

	dto, err := store.UpdateReasonPrefs(ctx, user, UpdateReasonPrefsInput{UnwatchOnClose: boolPtr(true)})
	if err != nil {
		t.Fatalf("turn the setting on: %v", err)
	}
	if !dto.UnwatchOnClose {
		t.Fatalf("setting reads off after turning it on")
	}
	check("after turning on")

	var optouts int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM issue_watch_optouts WHERE user_id = $1`, user).Scan(&optouts); err != nil {
		t.Fatalf("count opt-outs: %v", err)
	}
	if optouts != 0 {
		t.Errorf("turning the setting on wrote %d opt-outs, want 0", optouts)
	}

	// Turning it off gives nothing back.
	if _, err := store.UpdateReasonPrefs(ctx, user, UpdateReasonPrefsInput{UnwatchOnClose: boolPtr(false)}); err != nil {
		t.Fatalf("turn the setting off: %v", err)
	}
	check("after turning off again")
}

// readReasons answers the sorted reasons of a subscription row and whether the
// row exists.
func readReasons(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID) ([]string, bool) {
	t.Helper()
	var reasons []string
	err := pool.QueryRow(context.Background(),
		`SELECT reasons FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`, issueID, userID).Scan(&reasons)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("read reasons %s/%s: %v", issueID, userID, err)
	}
	slices.Sort(reasons)
	return reasons, true
}
