package nudges

// observability_test.go — sk-303.14, task T021, coverage-matrix
// row 118: the observability gate of the reminder worker.
//
// The spec requires not "there are metrics", but DISTINGUISHABILITY:
//
//	"The worker's lag behind the schedule is measurable: otherwise
//	 'reminders aren't arriving' is indistinguishable from 'nobody to
//	 remind'."
//
// Distinguishability is a property of the SET, not of a single
// measurement, and it is checked in the state in which it vanishes: on an
// EMPTY selection. So there are two different assertions here, and neither
// replaces the other:
//
//  1. behavioural — the three states ("alive and empty", "alive and
//     blind", "there is someone") move DIFFERENT counters, and an empty
//     selection does not make the picture flat;
//  2. taxonomic — the documented label values are derived from the same
//     registries the values in the code are taken from, not written next
//     to them.
//
// The second is a direct consequence of finding eleven handwritten lists
// in the run that stay silent on an omission. A list of label values is
// exactly such a list: it is read by the author of an alert rule as a
// promise about cardinality, and it goes stale in silence.

import (
	"context"
	"go/ast"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"skein-api/internal/astguard"
	"skein-api/internal/notifications"
	"skein-api/internal/review"
)

// ---------------------------------------------------------------------------
// Distinguishability
// ---------------------------------------------------------------------------

// TestObservability_EmptySweepIsTellableFromADeadWorker — the gate,
// verbatim.
//
// A walk with a moment in the distant past gives an EMPTY selection
// deterministically and on a shared database: a candidate is a row whose
// frozen deadline has expired or whose countdown base is not after "now",
// and in 2001 neither exists on any row of any package.
//
// It is in this state that the previous set of measurements was flat: lag
// is not observed (there is nothing to observe), there are no formed, there
// are no lost segments. It looked exactly the same when the worker was not
// running at all. The assertion below requires that a distinction EXISTS:
// the tick counter grew, and everything else did not.
func TestObservability_EmptySweepIsTellableFromADeadWorker(t *testing.T) {
	e := newEnv(t)
	sc := NewScheduler(e.store, Config{})

	ticksBefore := counterValue(t, nudgeTicksTotal)
	formedBefore := map[string]float64{}
	candsBefore := map[string]float64{}
	for _, cat := range review.Categories() {
		formedBefore[cat.String()] = counterVecValue(t, nudgesFormedTotal, cat.String())
		candsBefore[cat.String()] = counterVecValue(t, nudgeCandidatesSelectedTotal, cat.String())
	}

	// Distant past: nobody's deadline has expired, the countdown base has
	// not arrived.
	sc.tickAt(context.Background(), time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))

	if got := counterValue(t, nudgeTicksTotal) - ticksBefore; got != 1 {
		t.Errorf("skein_nudge_ticks_total grew by %v, expected 1 — "+
			"the liveness counter must move even when there is nobody to remind: "+
			"otherwise \"the worker is not running\" and \"nobody to remind\" are indistinguishable", got)
	}
	for _, cat := range review.Categories() {
		if got := counterVecValue(t, nudgeCandidatesSelectedTotal, cat.String()) - candsBefore[cat.String()]; got != 0 {
			t.Errorf("category %s: %v candidates selected on a certainly empty selection — "+
				"a walk in 2001 cannot see candidates, either the deadline predicate or the counter is broken",
				cat, got)
		}
		if got := counterVecValue(t, nudgesFormedTotal, cat.String()) - formedBefore[cat.String()]; got != 0 {
			t.Errorf("category %s: %v reminders formed on an empty selection", cat, got)
		}
	}

	// A POSITIVE CONTROL, without which everything above is a tautology.
	//
	// "The counter did not grow" proves an empty selection ONLY if this
	// counter is capable of growing on this path at all. A counter that
	// nobody increments gives exactly the same zeros, and the
	// distinguishability assertion would pass green on a measurement that
	// does not exist. So the same walk is run over a real candidate, and
	// a non-zero growth of the same series is required from it.
	author, _ := e.user(t)
	_, workerName := e.user(t)
	e.seed(t, issue{suffix: "obsunacc", status: "open", assignee: workerName,
		assigneeSetAt: ago(90 * time.Minute), createdBy: author})

	cat := review.CategoryUnaccepted
	candsBeforeLive := counterVecValue(t, nudgeCandidatesSelectedTotal, cat.String())
	formedBeforeLive := counterVecValue(t, nudgesFormedTotal, cat.String())
	ticksBeforeLive := counterValue(t, nudgeTicksTotal)

	dbNow, err := e.store.Now(context.Background())
	if err != nil {
		t.Fatalf("database clock: %v", err)
	}
	sc.tickAt(context.Background(), dbNow)

	if got := counterValue(t, nudgeTicksTotal) - ticksBeforeLive; got != 1 {
		t.Errorf("skein_nudge_ticks_total grew by %v on a non-empty walk, expected 1", got)
	}
	if got := counterVecValue(t, nudgeCandidatesSelectedTotal, cat.String()) - candsBeforeLive; got < 1 {
		t.Errorf("category %s: %v candidates selected with a certainly ready candidate — "+
			"the selected counter does not move AT ALL, and the zeros of the previous half mean nothing",
			cat, got)
	}
	if got := counterVecValue(t, nudgesFormedTotal, cat.String()) - formedBeforeLive; got < 1 {
		t.Errorf("category %s: %v reminders formed with a ready candidate — "+
			"the formed counter does not move AT ALL", cat, got)
	}
}

// TestObservability_BlindWorkerIsTellableFromAnEmptyOne — the third state.
//
// The worker is alive, but the database clock cannot be read: the tick is
// skipped entirely. Without a measurement of its own this state looks
// EXACTLY like "nobody to remind" — the walk did not reach a single
// selection, so not a single walk counter moved.
//
// A database is NOT needed here and is deliberately not used: a pool with
// an unreachable address connects lazily, so the refusal arrives on the
// first query — i.e. exactly on the clock read, and exactly as it would in
// production.
func TestObservability_BlindWorkerIsTellableFromAnEmptyOne(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("a pool with an unreachable address was not created: %v", err)
	}
	defer pool.Close()

	sc := NewScheduler(NewStore(pool), Config{})

	ticksBefore := counterValue(t, nudgeTicksTotal)
	clockFailsBefore := counterVecValue(t, nudgeSweepFailuresTotal, stageClock.String())

	sc.Tick(context.Background())

	if got := counterVecValue(t, nudgeSweepFailuresTotal, stageClock.String()) - clockFailsBefore; got != 1 {
		t.Errorf("skein_nudge_sweep_failures_total{stage=\"clock\"} grew by %v, expected 1 — "+
			"an unreadable clock must be a visible state, not silence", got)
	}
	if got := counterValue(t, nudgeTicksTotal) - ticksBefore; got != 0 {
		t.Errorf("skein_nudge_ticks_total grew by %v with an unreadable clock — "+
			"a tick that did not reach the walk does not count as liveness, otherwise \"blind\" merges with \"empty\"", got)
	}
}

// ---------------------------------------------------------------------------
// Sweep stages — behaviourally
// ---------------------------------------------------------------------------
//
// ⚠ WHY THESE FOUR ASSERTIONS, IF THERE IS ALREADY A STRUCTURAL GUARD
// ABOVE (observation of review A/9, finding B). The structural guard
// answers "is there a branch that puts the stage into the counter". It
// does NOT answer the question the counter exists for: "is the refusal
// visible from the outside". Until this fix, of five stages only ONE was
// held behaviourally — clock; deleting the increment of any of the other
// four did not turn a single test of the full run
// ./internal/nudges/... ./internal/outbox/... red.
//
// Worst of all was `select`: it is the one named as the motivating third
// state by both the spec and the commit message ("the worker is running,
// and selection is failing, from the outside it looks like 'nobody is
// waiting'"). The counter that makes this state visible was dropped
// without a single red test.
//
// The refusals are REAL, not planted: an unreachable pool answers with an
// error on the first query — exactly as it would in production.

// deadPool — a pool with an unreachable address. It connects lazily, so
// the refusal arrives on the first query, not on construction.
func deadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("a pool with an unreachable address was not created: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestObservability_FailingSelectionIsTellableFromAnEmptySweep — the
// `select` stage, the spec's third state verbatim.
//
// The walk is called PAST the clock read (sweep, not tick): the subject of
// the assertion is a selection refusal, and bringing a clock refusal here
// too would mean checking two stages with one assertion, and the red would
// be read ambiguously.
func TestObservability_FailingSelectionIsTellableFromAnEmptySweep(t *testing.T) {
	sc := NewScheduler(NewStore(deadPool(t)), Config{})

	cat := review.CategoryUnaccepted
	failsBefore := counterVecValue(t, nudgeSweepFailuresTotal, stageSelect.String())
	candsBefore := counterVecValue(t, nudgeCandidatesSelectedTotal, cat.String())
	formedBefore := counterVecValue(t, nudgesFormedTotal, cat.String())

	sc.sweep(context.Background(), cat, time.Now().UTC())

	if got := counterVecValue(t, nudgeSweepFailuresTotal, stageSelect.String()) - failsBefore; got != 1 {
		t.Errorf("skein_nudge_sweep_failures_total{stage=\"select\"} grew by %v, expected 1 — "+
			"a failing selection must be a visible state: without it \"the database is unreachable\" and "+
			"\"nobody is waiting for a reminder\" give the same flat picture from the outside", got)
	}
	if got := counterVecValue(t, nudgeCandidatesSelectedTotal, cat.String()) - candsBefore; got != 0 {
		t.Errorf("category %s: %v candidates selected on a failing selection", cat, got)
	}
	if got := counterVecValue(t, nudgesFormedTotal, cat.String()) - formedBefore; got != 0 {
		t.Errorf("category %s: %v reminders formed on a failing selection", cat, got)
	}
}

// TestObservability_UnreadableSchedulesAreVisible — the `schedules` stage.
//
// A REAL database is needed here: the assertion is that selection passed
// and the schedules could not be read, and this stage can be distinguished
// from `select` only by seeing a non-zero selected counter. So the
// selection pool is live, and the prefs pool is unreachable: exactly the
// dependency whose refusal is the subject of the check is substituted.
func TestObservability_UnreadableSchedulesAreVisible(t *testing.T) {
	e := newEnv(t)
	author, _ := e.user(t)
	_, workerName := e.user(t)
	e.seed(t, issue{suffix: "obsschd", status: "open", assignee: workerName,
		assigneeSetAt: ago(90 * time.Minute), createdBy: author})

	store := NewStore(e.pool)
	store.prefs = notifications.NewStore(deadPool(t))
	sc := NewScheduler(store, Config{})

	cat := review.CategoryUnaccepted
	dbNow, err := e.store.Now(context.Background())
	if err != nil {
		t.Fatalf("database clock: %v", err)
	}

	failsBefore := counterVecValue(t, nudgeSweepFailuresTotal, stageSchedules.String())
	candsBefore := counterVecValue(t, nudgeCandidatesSelectedTotal, cat.String())
	formedBefore := counterVecValue(t, nudgesFormedTotal, cat.String())

	sc.sweep(context.Background(), cat, dbNow)

	if got := counterVecValue(t, nudgeSweepFailuresTotal, stageSchedules.String()) - failsBefore; got != 1 {
		t.Errorf("skein_nudge_sweep_failures_total{stage=\"schedules\"} grew by %v, expected 1 — "+
			"unreadable schedule settings drop the WHOLE category for this tick, and from the outside "+
			"this must differ from an empty set", got)
	}
	// A positive control of the stage: if selection had already failed,
	// selected would be zero, and the red/green above would say nothing
	// about `schedules`.
	if got := counterVecValue(t, nudgeCandidatesSelectedTotal, cat.String()) - candsBefore; got < 1 {
		t.Errorf("category %s: %v candidates selected — the walk did not reach reading the schedules, "+
			"and the assertion checks the wrong stage", cat, got)
	}
	if got := counterVecValue(t, nudgesFormedTotal, cat.String()) - formedBefore; got != 0 {
		t.Errorf("category %s: %v reminders formed with unreadable schedules", cat, got)
	}
}

// TestObservability_UnschedulableRecipientIsVisible — the `schedule` stage,
// BOTH of its calls.
//
// There are two calls, and they are in different branches of consider: a
// refusal of the due predicate (before observing lag) and a refusal of the
// next-mark calculation (after it). One case is not enough: it would leave
// the second increment without an assertion, and that is exactly how
// refusals vanish — one at a time.
//
// The cases are distinguished by the LAG HISTOGRAM: it is observed exactly
// between the two calls. A candidate without a frozen deadline does not
// reach it, a candidate with an expired deadline does. That is the proof
// that two different places were checked, not one twice.
func TestObservability_UnschedulableRecipientIsVisible(t *testing.T) {
	sc := NewScheduler(NewStore(deadPool(t)), Config{})
	cat := review.CategoryUnaccepted
	now := time.Now().UTC()
	overdue := now.Add(-time.Hour)

	cases := []struct {
		name    string
		cand    Candidate
		wantLag float64
		why     string
	}{
		{
			name: "the due predicate does not answer",
			// There is no rate-limit state, and there is no countdown base
			// either: the schedule refuses to answer "is it due"
			// (review.ErrNoBase).
			cand:    Candidate{UserID: uuid.New(), IssueIDs: []string{"sk-1"}},
			wantLag: 0,
			why:     "the refusal arrives BEFORE observing lag",
		},
		{
			name: "the next-mark calculation does not answer",
			// The frozen deadline has expired — "due" is decided without
			// the schedule — but the countdown base is empty, and there is
			// nothing to count the next mark from.
			cand:    Candidate{UserID: uuid.New(), IssueIDs: []string{"sk-1"}, NextDueAt: &overdue},
			wantLag: 1,
			why:     "the refusal arrives AFTER observing lag",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failsBefore := counterVecValue(t, nudgeSweepFailuresTotal, stageSchedule.String())
			lagBefore := histogramCount(t, nudgeScheduleLagSeconds, cat.String())
			formedBefore := counterVecValue(t, nudgesFormedTotal, cat.String())

			sc.consider(context.Background(), cat, tc.cand, review.Defaults(), now)

			if got := counterVecValue(t, nudgeSweepFailuresTotal, stageSchedule.String()) - failsBefore; got != 1 {
				t.Errorf("skein_nudge_sweep_failures_total{stage=\"schedule\"} grew by %v, expected 1 — "+
					"an addressee the schedule refused to answer for must be visible: "+
					"they lose a reminder in silence, and the walk continues as if nothing happened", got)
			}
			if got := histogramCount(t, nudgeScheduleLagSeconds, cat.String()) - lagBefore; got != tc.wantLag {
				t.Errorf("lag observations %v, expected %v (%s) — the wrong call of the stage was checked",
					got, tc.wantLag, tc.why)
			}
			if got := counterVecValue(t, nudgesFormedTotal, cat.String()) - formedBefore; got != 0 {
				t.Errorf("%v reminders formed with a refusing schedule", got)
			}
		})
	}
}

// TestObservability_FailedFormationIsVisible — the `form` stage.
//
// The candidate reached the write: the deadline has expired, the schedule
// answered, — and the formation transaction rolled back. The assertion
// requires not only visibility of the refusal, but also that it is NOT
// counted as a lost segment: a lost segment is the expected outcome of
// neighbouring instances coordinating, and mixing a database refusal with
// it would mean declaring a breakage the norm.
func TestObservability_FailedFormationIsVisible(t *testing.T) {
	sc := NewScheduler(NewStore(deadPool(t)), Config{})
	cat := review.CategoryUnaccepted
	now := time.Now().UTC()
	overdue := now.Add(-time.Hour)
	cand := Candidate{
		UserID:    uuid.New(),
		Base:      now.Add(-24 * time.Hour),
		IssueIDs:  []string{"sk-1"},
		NextDueAt: &overdue,
	}

	failsBefore := counterVecValue(t, nudgeSweepFailuresTotal, stageForm.String())
	schedFailsBefore := counterVecValue(t, nudgeSweepFailuresTotal, stageSchedule.String())
	formedBefore := counterVecValue(t, nudgesFormedTotal, cat.String())
	lostBefore := counterVecValue(t, nudgeSegmentsLostTotal, cat.String())

	sc.consider(context.Background(), cat, cand, review.Defaults(), now)

	if got := counterVecValue(t, nudgeSweepFailuresTotal, stageForm.String()) - failsBefore; got != 1 {
		t.Errorf("skein_nudge_sweep_failures_total{stage=\"form\"} grew by %v, expected 1 — "+
			"a rolled-back formation transaction must be visible: the segment is not consumed, "+
			"the addressee will return on the next tick, and from the outside this is indistinguishable from \"nothing to form\"", got)
	}
	// A positive control of the stage: the walk must have REACHED the
	// write.
	if got := counterVecValue(t, nudgeSweepFailuresTotal, stageSchedule.String()) - schedFailsBefore; got != 0 {
		t.Errorf("the schedule stage grew by %v — the refusal happened before formation, "+
			"and the assertion checks the wrong stage", got)
	}
	if got := counterVecValue(t, nudgesFormedTotal, cat.String()) - formedBefore; got != 0 {
		t.Errorf("%v reminders formed with a rolled-back transaction", got)
	}
	if got := counterVecValue(t, nudgeSegmentsLostTotal, cat.String()) - lostBefore; got != 0 {
		t.Errorf("%v segments lost — a database refusal was counted as a normal segment loss to a "+
			"neighbouring instance; then a breakage looks like normal cluster operation", got)
	}
}

// stageIdent — the Go identifier the code names the stage with the given
// label value by. The ONLY place this convention is written: both
// structural guards below ask it, rather than each deriving the name its
// own way.
//
// ⚠ CORRECTED FALSE REFUSAL (review A/9, finding P-2; not erased). Both
// guards derived the name rigidly — `"stage" + Title(name[:1]) +
// name[1:]` — and therefore accepted ONLY one-word label values. A legal
// two-word stage `registerSweepStage("clock_read")`, declared, documented
// and actually incremented, turned BOTH red: the first demanded a
// non-existent `stageClock_read`, the second declared the real
// `stageClockRead` as having arrived past the registry. The bound was
// named nowhere, and the person who would introduce a two-word stage
// would run into it — i.e. at the most inconvenient moment.
//
// The form is supported, not forbidden: the label value remains snake_case
// (that is how it is read in Prometheus), the identifier is CamelCase
// (that is what Go requires).
//
// ⚠ The BOUND that REMAINS, and it is named. The convention is exactly
// this: the stage name in the label is written as lowercase words
// separated by underscores. A value with a different separator (hyphen,
// dot, space) will not map here, and the guards will go red — rightly:
// such a label value is not introduced in this package.
func stageIdent(stage string) string {
	var b strings.Builder
	b.WriteString("stage")
	for _, word := range strings.Split(stage, "_") {
		if word == "" {
			continue
		}
		b.WriteString(strings.ToUpper(word[:1]))
		b.WriteString(word[1:])
	}
	return b.String()
}

// TestObservability_EveryRegisteredStageHasACallSite — the stage registry
// against the code that fills it.
//
// A stage that is declared and never incremented is a documented label
// value the label will never take: an alert on it will never fire, and
// that cannot be understood from the metric. The reverse is equally
// harmful: a vanished call means a refusal that has become silent.
//
// ⚠ CORRECTED MISTAKE (review A/9, finding A.1; not erased, because it is
// more instructive than the check itself). The first edition COUNTED
// OCCURRENCES OF THE IDENTIFIER IN THE TEXT of the concatenated source and
// required two: "the declaration plus at least one call". The threshold
// could NEVER fire: the package's house style is a doc-comment that
// begins with the name of the declared,
//
//	// stageForm — the formation transaction rolled back …
//	stageForm = registerSweepStage("form")
//
// i.e. the second occurrence was collected by the comment, before any
// code. Checked by mutations in both directions: deleting the ONLY
// increment of stageForm left the package green, and a sixth stage
// declared without a call passed PASS. The author of the wave saw the
// second of these mutations green and explained it as the mutation itself
// being spoiled — the explanation was true on the fact and false on the
// conclusion: the next question ("and is the check capable of going red
// at all?") was not asked.
//
// Raising the threshold would have been replacing blindness with a false
// refusal: an identifier mentioned in two comments would pass any
// threshold. What is counted now is not the text, but the TREE
// (internal/astguard): comments are not expression nodes at all.
//
// And what is asked is not "is it used", but what the test's name asserts:
// does the stage stand as an ARGUMENT of a failure-counter increment. A
// stage named only in its own list or in String() is used — but there is
// no branch behind it, and from the outside a refusal of this stage is
// still invisible.
func TestObservability_EveryRegisteredStageHasACallSite(t *testing.T) {
	t.Parallel()
	pkg, err := astguard.Parse(".")
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}

	// The arguments of all calls of nudgeSweepFailuresTotal.WithLabelValues(...)
	// — i.e. exactly the identifiers the code puts into the `stage` label.
	incremented := map[string]bool{}
	for _, name := range pkg.CallArgIdents("nudgeSweepFailuresTotal", "WithLabelValues") {
		incremented[name] = true
	}
	if len(incremented) == 0 {
		t.Fatal("no call of nudgeSweepFailuresTotal.WithLabelValues was found — " +
			"either the parse is broken or the counter was renamed; in both cases the check below " +
			"would accuse EVERY stage, not find the real one")
	}

	for _, stage := range SweepStages() {
		ident := stageIdent(stage)
		if !incremented[ident] {
			t.Errorf("stage %q is declared, but no branch puts it into the failure counter "+
				"(identifier %s does not occur in any argument of "+
				"nudgeSweepFailuresTotal.WithLabelValues) — a documented label value "+
				"the label will never take",
				stage, ident)
		}
	}
}

// TestObservability_StageIdentifiersAreNotDeadLetters — the reverse half:
// a stage name standing in an increment argument must be a DECLARED stage.
//
// Without it the previous assertion can be fulfilled by a typo: `stageFrom`
// instead of `stageForm` would not compile, but
// `nudgeSweepFailuresTotal.WithLabelValues("form")` with a string literal
// would compile perfectly — and the stage registry would stop being the
// only source of label values, remaining green.
func TestObservability_StageIdentifiersAreNotDeadLetters(t *testing.T) {
	t.Parallel()
	pkg, err := astguard.Parse(".")
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}

	known := map[string]bool{}
	for _, stage := range SweepStages() {
		known[stageIdent(stage)] = true
	}

	args := pkg.CallArgIdents("nudgeSweepFailuresTotal", "WithLabelValues")
	if len(args) == 0 {
		t.Fatal("no increment arguments were found — the parse is broken, not the code")
	}
	for _, name := range args {
		// String is the method by which a stage turns into a label value;
		// it is not a stage, and its presence in the argument is legal.
		if name == "String" || known[name] {
			continue
		}
		t.Errorf("the failure counter is given %q — this is not one of the stages of the "+
			"nudges.SweepStages() registry; a label value that arrived past the registry "+
			"will land neither in the value documentation nor in the coverage check", name)
	}
}

// ---------------------------------------------------------------------------
// Taxonomy: documented label values
// ---------------------------------------------------------------------------

// labelValueSources — where the closed set of values of each label of this
// package is taken from. Not a list of values: a FUNCTION that yields the
// very set the code puts into the label.
//
// The key difference from a handwritten list: an eighth category or a
// sixth stage lands in the check at the same moment it lands in the code,
// and no edit of this file is needed for that. One omitted from the
// annotation — goes red.
//
// ⚠ The BOUND the promise was held by in silence (review A/9, finding
// P-5). This map itself is a handwritten map, and its key is the LABEL
// NAME. While the label is literally named `category` or `stage`, the
// header's promise is true; rename the label — and the promise quietly
// stops applying. Mutation: on nudgeSegmentsLostTotal the `category` label
// is renamed to `cat`, an incomplete old list is left in the annotation —
// the run is GREEN, because the taxonomic gate skipped an unknown name
// (`known == false → continue`), i.e. the form of the enumeration was
// checked without the truth.
//
// The silence is closed: a label name that stands in NEITHER of the two
// registries below is red. An unknown label no longer means "nothing to
// check".
var labelValueSources = map[string]func() []string{
	"category": func() []string {
		out := make([]string, 0, len(review.Categories()))
		for _, c := range review.Categories() {
			out = append(out, c.String())
		}
		return out
	},
	"stage": SweepStages,
}

// labelsWithoutClosedSet — labels of this package that have no closed set
// of values in substance, and the reason they do not.
//
// It exists so that "there is no set" is a DECISION, written here, not a
// consequence of the label name not matching a key of the neighbouring
// map. The registry is empty: both labels of the package have a closed
// set. A label with a free value appearing will be written here with a
// reason, and that will be visible in review.
var labelsWithoutClosedSet = map[string]string{}

// TestObservability_EveryLabelOfThePackageIsClassified — no label of the
// package is left without an answer to "is its set of values closed".
//
// Without this assertion the taxonomic gate below stayed silent about any
// label whose name was not in labelValueSources — and renaming a label
// took it out from under the check, leaving the run green (finding P-5).
func TestObservability_EveryLabelOfThePackageIsClassified(t *testing.T) {
	t.Parallel()
	docs := packageMetricLabelDocs(t)
	if len(docs) == 0 {
		t.Fatal("no metric was found in the package — the walk is broken, not the code")
	}
	seen := map[string]bool{}
	for metric, byLabel := range docs {
		for label := range byLabel {
			seen[label] = true
			_, closed := labelValueSources[label]
			_, open := labelsWithoutClosedSet[label]
			switch {
			case closed && open:
				t.Errorf("label %q is declared both with a closed set and without one — "+
					"the registries contradict each other", label)
			case !closed && !open:
				t.Errorf("%s: label %q is listed neither in labelValueSources (the set of values "+
					"is closed and where it is taken from) nor in labelsWithoutClosedSet (there is no set, "+
					"and here is why) — which means NOTHING is checked about its values. "+
					"That is exactly how renaming a label takes it out from under the taxonomic "+
					"gate, leaving the run green", metric, label)
			}
		}
	}
	// The reverse side: a record whose label is no longer in the package
	// must leave with it, not guard emptiness.
	for label := range labelValueSources {
		if !seen[label] {
			t.Errorf("labelValueSources names label %q, but no metric of the package "+
				"declares it — the record has outlived its subject", label)
		}
	}
	for label := range labelsWithoutClosedSet {
		if !seen[label] {
			t.Errorf("labelsWithoutClosedSet names label %q, but no metric of the package "+
				"declares it — the record has outlived its subject", label)
		}
	}
}

// TestObservability_DocumentedLabelValuesCoverWhatTheCodeProduces — the
// promise about cardinality, checked in the direction in which it breaks.
//
// It breaks not by someone inventing a value at runtime, but by someone
// putting it into the registry and not returning to the annotation. So the
// check goes FROM THE REGISTRY to the text, not the other way.
func TestObservability_DocumentedLabelValuesCoverWhatTheCodeProduces(t *testing.T) {
	t.Parallel()
	docs := packageMetricLabelDocs(t)
	if len(docs) == 0 {
		t.Fatal("no metric was found in the package — the walk is broken, not the code")
	}

	checked := 0
	for metric, byLabel := range docs {
		for label, text := range byLabel {
			source, known := labelValueSources[label]
			if !known {
				continue
			}
			values := source()
			if len(values) == 0 {
				t.Fatalf("the value registry of label %q is empty — there is nothing to compare with, "+
					"and the check would become tautologically green", label)
			}
			for _, v := range values {
				if !strings.Contains(text, `"`+v+`"`) {
					t.Errorf("%s: label %q takes value %q, but `@label %s` does not enumerate it: %q",
						metric, label, v, label, text)
				}
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no label of the package was checked against the registry — either the metrics lost their labels, " +
			"or labelValueSources drifted from their names; in both cases the check is not looking")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// metricReceiver — all three supported forms of a metric constructor:
//
//	prometheus.NewCounterVec(...)         // X — an identifier
//	promauto.NewCounterVec(...)           // X — an identifier
//	promauto.With(reg).NewCounterVec(...)  // X — a CALL of promauto.With(reg)
//
// ⚠ THE THIRD FORM (review A/9, finding P-6 — the same blindness as the
// metrics gate in internal/outbox, and it is closed here at the same
// time). The requirement "X is *ast.Ident" dropped it in silence, and a
// metric of this package declared that way landed neither in the
// taxonomic gate nor in the label-classification check: NOTHING would be
// checked about its label values. The form is named as supported in
// the project guidelines.
func metricReceiver(x ast.Expr) bool {
	switch recv := x.(type) {
	case *ast.Ident:
		return recv.Name == "promauto" || recv.Name == "prometheus"
	case *ast.CallExpr:
		sel, ok := recv.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "With" {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "promauto"
	}
	return false
}

// packageMetricLabelDocs reads the doc-comments of the package's metric
// declarations and returns metricVar → label → text of the `@label` line.
//
// The parse is shared (internal/astguard): a third way of reading the
// package source is not introduced in this file.
func packageMetricLabelDocs(t *testing.T) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	pkg, err := astguard.Parse(".")
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, val := range vs.Values {
					call, ok := val.(*ast.CallExpr)
					if !ok {
						continue
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || !strings.HasPrefix(sel.Sel.Name, "New") {
						continue
					}
					if !metricReceiver(sel.X) {
						continue
					}
					var doc strings.Builder
					for _, g := range []*ast.CommentGroup{vs.Doc, gd.Doc, vs.Comment} {
						if g != nil {
							doc.WriteString(g.Text())
							doc.WriteString("\n")
						}
					}
					labels := map[string]string{}
					for _, line := range strings.Split(doc.String(), "\n") {
						rest, ok := strings.CutPrefix(strings.TrimSpace(line), "@label ")
						if !ok {
							continue
						}
						name, text, _ := strings.Cut(strings.TrimSpace(rest), " ")
						labels[name] = strings.TrimSpace(text)
					}
					if len(labels) > 0 {
						out[vs.Names[i].Name] = labels
					}
				}
			}
		}
	}
	return out
}

// counterValue reads the current value of an unlabelled counter.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("reading the counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// histogramCount reads the NUMBER OF OBSERVATIONS of one histogram series.
//
// Not a sum and not a quantile: the assertions above need "an observation
// was / was not", and only the count answers that, independent of the
// value.
func histogramCount(t *testing.T, v *prometheus.HistogramVec, labels ...string) float64 {
	t.Helper()
	obs, err := v.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("histogram series %v: %v", labels, err)
	}
	m, ok := obs.(prometheus.Metric)
	if !ok {
		t.Fatalf("histogram series %v does not yield a snapshot", labels)
	}
	var dtoMetric dto.Metric
	if err := m.Write(&dtoMetric); err != nil {
		t.Fatalf("reading histogram series %v: %v", labels, err)
	}
	return float64(dtoMetric.GetHistogram().GetSampleCount())
}

// counterVecValue reads the value of one labelled counter series.
func counterVecValue(t *testing.T, v *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := v.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("series %v: %v", labels, err)
	}
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("reading series %v: %v", labels, err)
	}
	return m.GetCounter().GetValue()
}
