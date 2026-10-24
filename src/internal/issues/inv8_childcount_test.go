package issues

import (
	"context"
	"slices"
	"testing"
	"time"

	"skein-api/internal/authz"
	"skein-api/internal/childcount"
	"skein-api/internal/testsupport"
)

// inv8_childcount_test.go — sk-282 T030, matrix row 54.
//
// "INV-8: with the flag off, the execution plan of each of the three
// queries matches the current one; with it on, median growth on the
// reference set is at most 20%."
//
// # THIS FILE MEASURES. IT DOES NOT GATE. Read that before trusting green.
//
// The name of the test says so, and so does this paragraph, because the
// previous version of this file said the opposite in the only way that
// matters: it carried an `if percent > 20 { t.Errorf }` that could not
// fire. Its two halves were not comparable — «before» was timed on a
// 500-row page and «after» on a 25-row one — so the ratio was decided by
// the page size, not by the counters, and a page twenty times smaller wins
// against ANY cost of the thing being priced. An empty gate is worse than
// no gate: it reports protection that is not there.
//
// A gate has to compare LIKE FOR LIKE — one variable, the same page size
// on both sides, counters on versus counters off. That comparison is the
// one below, and it cannot be gated either, for the opposite and honest
// reason: on UNCHANGED code its spread is wider than the budget it would
// be checked against. Interleaved runs on the throwaway database produced
// 18.9 / 20.5 / 35.1 / 30.7 / 24.9 percent against a 20% budget — a gate
// there would fail on a slow neighbour and could not tell a real
// regression from its own spread, and the project guidelines are
// explicit that flaky is fixed or quarantined, never retried into green.
//
// So it prints. The number is on every run (INV8_CLASS_MARGINAL), so a
// regression that doubles it is visible in the log to whoever reads it.
//
// # What is left WITHOUT automatic regression protection — plainly
//
//   - The marginal cost of the child counters on a page. Nothing fails if
//     it grows: only the printed number moves.
//   - The tool-level INV-8 verdict. It is NOT re-derived here — that
//     measurement is the spec's own ("tool response time on this set,
//     median of five runs"), it was taken on this corpus for T030, and
//     it came out at −82…−85% (27–30 ms → 4.4–4.9 ms: the default page fell
//     from the store's 500 rows to 25, counters included). It is recorded
//     in T030's report, and this file deliberately does not repeat it —
//     re-running an incomparable pair here is exactly what produced the
//     empty gate.
//
// What this file DOES fail on is its own validity: if the measured page
// stops being the worst realistic page (parents with the corpus fan-out),
// the number below stops being about the cost it claims to be about, and
// the sanity checks below are fatal for that reason.
//
// # The corpus
//
// The counters walk the dependency graph for every row of the page, so
// they are measured on the worst realistic page: a full page of top-level
// issues, each with the corpus fan-out of twenty children. Measured on the
// default page — which on this corpus is almost all leaves — the growth
// came out at 9.9% with exactly ONE row of twenty-five having any children
// at all, and a number taken that way would be near zero on any
// implementation, including a per-row subquery.
func TestINV8_ChildCountMarginalCost_MeasuredNotGated(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	corpus := testsupport.SeedINV8Corpus(t, pool)
	store := NewStore(pool)
	ctx := context.Background()

	args := ListAllIssuesArgs{
		Scope:          authz.ForUser(corpus.CallerID),
		Caller:         corpus.CallerID,
		CallerUsername: corpus.CallerUsername,
		Limit:          25,
	}

	// The page the counters are computed for is a page of PARENTS, not
	// whatever the default ordering happens to surface.
	//
	// This is the difference between a measurement and a formality. The
	// corpus is 100 parents and 2000 children, so an unfiltered page of 25
	// is almost all children — leaves, zero children each — and the
	// counters have essentially nothing to count: measured that way the
	// growth came out at 9.9% with exactly ONE row of the twenty-five
	// having any children at all. A number taken that way is near zero on
	// any implementation, including a per-row subquery.
	//
	// So the page is the worst realistic one: a full page of top-level
	// issues, each with a fan-out of twenty. That is what a client
	// actually lists, and it is where the cost lives.
	ids := corpus.ParentIDs[:args.Limit]

	// The two halves are timed INTERLEAVED, and that is a correction to
	// the method rather than a preference.
	//
	// Measured the obvious way — five "off" runs, then five "on" runs,
	// ratio of the two medians — the number swung across repeated runs of
	// the same unchanged code: 12.7%, 19.0%, 22.0%. Nothing about the code
	// differed between those runs. The throwaway database is shared with
	// whatever else the suite is doing, so anything that drifts during the
	// measurement (cache warming, a neighbouring package, the host) lands
	// entirely on whichever block ran second — which is always "on". A
	// number a slow neighbour can move is not measuring the work.
	//
	// Alternating the two puts that drift on both sides equally. The
	// per-half medians are then comparable, and the ratio is a property of
	// the work rather than of the running order.
	//
	// Nine rounds, not the spec's five. The spec's "median of five runs"
	// is stated for the tool-level measurement, which is taken separately
	// (see the file comment); this half is a printed number whose whole
	// problem is spread, so it takes an odd sample larger than five — the
	// median stays a real observation and one outlier round cannot move
	// it. Nine is where that stops costing noticeable suite time.
	const rounds = 9
	offRuns := make([]time.Duration, 0, rounds)
	onRuns := make([]time.Duration, 0, rounds)
	for range rounds {
		offRuns = append(offRuns, timeOnce(t, func() error {
			_, err := store.ListAllIssues(ctx, args)
			return err
		}))
		onRuns = append(onRuns, timeOnce(t, func() error {
			if _, err := store.ListAllIssues(ctx, args); err != nil {
				return err
			}
			_, err := childcount.For(ctx, pool, ids, authz.ForUser(corpus.CallerID))
			return err
		}))
	}
	offMedian := medianDuration(offRuns)
	onMedian := medianDuration(onRuns)

	// Sanity: the counters must actually have something to count, or the
	// printed number is measuring zero work.
	counts, err := childcount.For(ctx, pool, ids, authz.ForUser(corpus.CallerID))
	if err != nil {
		t.Fatalf("childcount.For: %v", err)
	}
	withChildren := 0
	for _, c := range counts {
		if c.Total > 0 {
			withChildren++
		}
	}
	// Every row of a parents page must have children, and the fan-out must
	// be the corpus's. Anything less and the printed number is measuring a
	// cheaper shape than the one this measurement is about.
	if withChildren != len(ids) {
		t.Fatalf("only %d of %d rows on the measured page have children — the page is not "+
			"the parents page this measurement is stated on", withChildren, len(ids))
	}
	for id, c := range counts {
		if c.Total < testsupport.INV8ChildrenPerParent-1 {
			t.Fatalf("%s has %d children, the corpus fan-out is %d — the measured page is "+
				"not the corpus's worst realistic shape",
				id, c.Total, testsupport.INV8ChildrenPerParent)
		}
	}

	marginalPercent := float64(onMedian-offMedian) / float64(offMedian) * 100
	t.Logf("INV8_CLASS_MARGINAL off=%v on=%v rounds=%d rows=%d rows_with_children=%d "+
		"growth=%.1f%% — MEASURED, NOT GATED: nothing fails if this grows "+
		"(see the file comment)",
		offMedian, onMedian, rounds, len(ids), withChildren, marginalPercent)
	t.Logf("INV8_RUNS_NS_OFF %v", offRuns)
	t.Logf("INV8_RUNS_NS_ON %v", onRuns)

	// ---- what is NOT measured here, and why ----
	//
	// The tool-level comparison — the store's old 500-row default page
	// against today's 25 rows plus counters — used to sit here behind an
	// `if percent > 20` and could not fail: it changes two variables at
	// once, and the smaller page decides the ratio whatever the counters
	// cost. It is gone rather than relabelled, because re-running an
	// incomparable pair on every integration run is what made the empty
	// gate look like protection in the first place. That measurement was
	// taken once for T030, by the method the spec names, and its result
	// (−82…−85%) is recorded there — see the file comment.
}

// timeOnce runs f and returns how long it took, failing the test if f does.
func timeOnce(t *testing.T, f func() error) time.Duration {
	t.Helper()
	start := time.Now()
	if err := f(); err != nil {
		t.Fatalf("measured call failed: %v", err)
	}
	return time.Since(start)
}

// medianDuration returns the median of a non-empty sample.
func medianDuration(samples []time.Duration) time.Duration {
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}
