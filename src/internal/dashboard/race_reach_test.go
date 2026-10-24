package dashboard

// race_reach_test.go — sk-674.18 T030 (US-13, INV-23). Reference — US-13 as the spec words it: the projects outside the
// caller's HOT ZONE reach contribute nothing to it. The race's window is
// bounded by the first event (firstEventInDb, effectiveStart, isColdStart),
// so that boundary is read inside the reach too: an early event that exists
// only in project B changes nothing of a race over project A, and a race
// over B does see it (the positive control).

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRace_FirstEventBoundaryStaysInTheReach(t *testing.T) {
	pool := newTestPool(t)
	store := New(pool)
	ctx := context.Background()
	owner, ownerName := seedUser(t, pool)
	projectA := seedProject(t, pool, owner)
	projectB := seedProject(t, pool, owner)
	now := time.Now().UTC()
	inA, early := now.Add(-3*24*time.Hour), now.Add(-60*24*time.Hour)

	pre := uniqueIDPrefix(t, "race")
	idA, idB := pre+"-1", pre+"-2"
	seedIssueRow(t, pool, projectA, idA, "human", owner, nil)
	seedIssueRow(t, pool, projectB, idB, "human", owner, nil)
	seedEventForIssue(t, pool, "issue.created", idA, projectA, ownerName, owner, "human", inA)
	seedEventForIssue(t, pool, "issue.created", idB, projectB, ownerName, owner, "human", early)

	race := func(t *testing.T, p uuid.UUID) *RaceResult {
		t.Helper()
		set := map[uuid.UUID]struct{}{p: {}}
		res, err := store.Race(ctx, RaceArgs{Metric: RaceMetricClosed, Window: RaceWindow30d, VisibleProjects: set, Reach: set})
		if err != nil {
			t.Fatalf("Race: %v", err)
		}
		return res
	}
	day := func(t time.Time) time.Time { return t.Truncate(24 * time.Hour) }

	t.Run("the reach of A: the boundary is A's first event", func(t *testing.T) {
		res := race(t, projectA)
		if res.FirstEventInDB == nil || !day(*res.FirstEventInDB).Equal(day(inA)) {
			t.Errorf("firstEventInDb %v, want the day of A's event %v — B's earlier event leaks in", res.FirstEventInDB, day(inA))
		}
		if !res.EffectiveStart.Equal(day(inA)) {
			t.Errorf("effectiveStart %v, want %v", res.EffectiveStart, day(inA))
		}
		if !res.IsColdStart {
			t.Errorf("isColdStart false: A's history starts inside the 30-day window")
		}
	})
	t.Run("positive control: the reach of B sees its early event", func(t *testing.T) {
		res := race(t, projectB)
		if res.FirstEventInDB == nil || !day(*res.FirstEventInDB).Equal(day(early)) {
			t.Errorf("firstEventInDb %v, want the day of B's event %v", res.FirstEventInDB, day(early))
		}
		if res.IsColdStart {
			t.Errorf("isColdStart true: B's history starts before the 30-day window")
		}
	})
}
