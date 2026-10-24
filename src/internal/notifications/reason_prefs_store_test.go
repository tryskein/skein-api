package notifications

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// reason_prefs_store_test.go — regressions on partial "reason × channel"
// settings updates (sk-303.13, contract C1, plan task T003).
//
// The contract's promise is one: the reasons and channels named — only they
// change, everything else keeps its state. Three ways to break that promise
// are checked here: a race of two requests, a repeated reason inside one
// body, and a composite PUT breaking in the middle.

func boolPtr(b bool) *bool { return &b }

// TestUpdateReasonPrefs_ConcurrentChannelUpdatesDoNotOverwrite — two
// concurrent requests changing DIFFERENT channels of one reason must
// compose, not overwrite each other.
//
// While the update was computed from a snapshot taken before the
// transaction, both requests read one state and the last commit undid the
// other's change — a classic lost update. The check runs in rounds: the
// race is not won every run, but accumulating rounds makes missing the
// defect unlikely, while it has no false positive — a correct
// implementation is green in any order.
func TestUpdateReasonPrefs_ConcurrentChannelUpdatesDoNotOverwrite(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	for round := range 25 {
		user := seedTestUser(t, pool, "user")

		// Initial state: both channels of the "mention" reason are on.
		if _, err := store.UpdateReasonPrefs(ctx, user, UpdateReasonPrefsInput{
			Reasons: []ReasonChannelUpdate{
				{Reason: string(ReasonKeyMention), Feed: boolPtr(true), Push: boolPtr(true)},
			},
		}); err != nil {
			t.Fatalf("round %d: seed state: %v", round, err)
		}

		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i, upd := range []ReasonChannelUpdate{
			{Reason: string(ReasonKeyMention), Feed: boolPtr(false)},
			{Reason: string(ReasonKeyMention), Push: boolPtr(false)},
		} {
			wg.Add(1)
			go func(i int, upd ReasonChannelUpdate) {
				defer wg.Done()
				<-start
				_, errs[i] = store.UpdateReasonPrefs(ctx, user,
					UpdateReasonPrefsInput{Reasons: []ReasonChannelUpdate{upd}})
			}(i, upd)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: concurrent update #%d: %v", round, i, err)
			}
		}

		states, err := store.GetReasonStates(ctx, user)
		if err != nil {
			t.Fatalf("round %d: read back: %v", round, err)
		}
		if got := states[ReasonKeyMention]; got.Feed || got.Push {
			t.Fatalf("round %d: mention = feed:%v push:%v, want both off — one concurrent update was overwritten",
				round, got.Feed, got.Push)
		}
	}
}

// TestUpdateReasonPrefs_RepeatedReasonInOneBodyMerges — one reason named
// twice in the body with different channels yields ONE final state, not a
// second write on top of the first.
//
// This is the same defect as the race, but without any parallelism: while
// each element was computed from one snapshot, the second restored the
// channel the first had switched off.
func TestUpdateReasonPrefs_RepeatedReasonInOneBodyMerges(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	user := seedTestUser(t, pool, "user")

	if _, err := store.UpdateReasonPrefs(ctx, user, UpdateReasonPrefsInput{
		Reasons: []ReasonChannelUpdate{
			{Reason: string(ReasonKeyMention), Feed: boolPtr(true), Push: boolPtr(true)},
		},
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	if _, err := store.UpdateReasonPrefs(ctx, user, UpdateReasonPrefsInput{
		Reasons: []ReasonChannelUpdate{
			{Reason: string(ReasonKeyMention), Feed: boolPtr(false)},
			{Reason: string(ReasonKeyMention), Push: boolPtr(false)},
		},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	states, err := store.GetReasonStates(ctx, user)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got := states[ReasonKeyMention]; got.Feed || got.Push {
		t.Fatalf("mention = feed:%v push:%v, want both off — the second entry of the body "+
			"was computed from a stale snapshot and undid the first", got.Feed, got.Push)
	}
}

// TestUpdateReasonPrefs_NoRowStartsFromRegistryDefault — updating ONE
// channel of a reason that has no row yet has no right to zero the other:
// an unnamed channel starts from the registry's narrow default, not from
// false.
func TestUpdateReasonPrefs_NoRowStartsFromRegistryDefault(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	user := seedTestUser(t, pool, "user")

	// A fresh account: there are no settings rows at all (the migration did not write them).
	var rows int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM user_notification_reason_prefs WHERE user_id = $1`, user).Scan(&rows); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if rows != 0 {
		t.Fatalf("fixture already has %d reason pref rows", rows)
	}

	// "Assignment" has both channels on by default; switch the feed off.
	if _, err := store.UpdateReasonPrefs(ctx, user, UpdateReasonPrefsInput{
		Reasons: []ReasonChannelUpdate{
			{Reason: string(ReasonKeyAssignment), Feed: boolPtr(false)},
		},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	states, err := store.GetReasonStates(ctx, user)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := states[ReasonKeyAssignment]
	if got.Feed {
		t.Error("assignment/feed stayed on after it was switched off")
	}
	if !got.Push {
		t.Error("assignment/push was silently switched off — an unnamed channel must keep the registry default")
	}
}

// TestUpdateReasonPrefs_ReservedReasonIsRefusedWithoutWriting — a reason
// reserved for a future wave is refused by the API: it has no mapping onto
// events, and a stored toggle would do nothing. The form does not show it
// for the same reason — the API must be the same surface.
func TestUpdateReasonPrefs_ReservedReasonIsRefusedWithoutWriting(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	user := seedTestUser(t, pool, "user")

	var reserved ReasonKey
	for _, r := range AllReasons() {
		if r.State == ReasonStateReserved {
			reserved = r.Key
			break
		}
	}
	if reserved == "" {
		t.Skip("the registry currently reserves no reason")
	}

	_, err := store.UpdateReasonPrefs(ctx, user, UpdateReasonPrefsInput{
		Reasons: []ReasonChannelUpdate{{Reason: string(reserved), Push: boolPtr(false)}},
	})
	if err == nil {
		t.Fatalf("reserved reason %s was accepted", reserved)
	}
	if !strings.Contains(err.Error(), string(reserved)) {
		t.Errorf("error does not name the refused reason: %v", err)
	}

	var rows int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM user_notification_reason_prefs WHERE user_id = $1`, user).Scan(&rows); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if rows != 0 {
		t.Errorf("the refused update wrote %d row(s)", rows)
	}
}

// TestUpdateReasonPrefs_CompositeUpdateIsAllOrNothing — a composite PUT
// ("reasons" + "suppress own-agent actions") is applied wholly or not at
// all.
//
// While the second half was written as a separate query after the first
// committed, its failure returned 500 to the client with the channels
// already applied — exactly the half-applied change the contract promises
// not to leave. The second write's refusal is simulated by a trigger that
// raises an exception only for this user.
func TestUpdateReasonPrefs_CompositeUpdateIsAllOrNothing(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	user := seedTestUser(t, pool, "user")

	before, err := store.GetReasonStates(ctx, user)
	if err != nil {
		t.Fatalf("read initial state: %v", err)
	}

	if _, err := pool.Exec(ctx, `
CREATE OR REPLACE FUNCTION test_break_own_agent_suppression() RETURNS trigger AS $$
BEGIN
    IF NEW.user_id = '`+user.String()+`'::uuid THEN
        RAISE EXCEPTION 'test: own-agent suppression write refused';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER test_break_own_agent_suppression
    BEFORE INSERT OR UPDATE ON user_notification_prefs
    FOR EACH ROW EXECUTE FUNCTION test_break_own_agent_suppression()`); err != nil {
		t.Fatalf("install failing trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DROP TRIGGER IF EXISTS test_break_own_agent_suppression ON user_notification_prefs`)
		_, _ = pool.Exec(context.Background(),
			`DROP FUNCTION IF EXISTS test_break_own_agent_suppression()`)
	})

	if _, err := store.UpdateReasonPrefs(ctx, user, UpdateReasonPrefsInput{
		Reasons: []ReasonChannelUpdate{
			{Reason: string(ReasonKeyMention), Feed: boolPtr(false), Push: boolPtr(false)},
		},
		SuppressAgentActions: boolPtr(true),
	}); err == nil {
		t.Fatal("the composite update reported success although its second half failed")
	}

	after, err := store.GetReasonStates(ctx, user)
	if err != nil {
		t.Fatalf("read state after the failure: %v", err)
	}
	if after[ReasonKeyMention] != before[ReasonKeyMention] {
		t.Errorf("mention = %+v, want the pre-request %+v — the failed PUT left its first half applied",
			after[ReasonKeyMention], before[ReasonKeyMention])
	}
	var rows int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM user_notification_reason_prefs WHERE user_id = $1`, user).Scan(&rows); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if rows != 0 {
		t.Errorf("the failed composite update wrote %d reason row(s)", rows)
	}
}
