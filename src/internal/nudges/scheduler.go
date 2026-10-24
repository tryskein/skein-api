package nudges

// scheduler.go — sk-303.14, task T017: the SHARED LOOP of repeat
// reminders.
//
// One worker, two categories (R-4): a shared loop, different predicates,
// different schedules, different unsubscribe categories. There is deliberately
// no second worker and no second binary — the categories differ in their
// inputs, not in the walk mechanics, and split across different loops they
// would drift in batch bounds, observability and the order of segment
// consumption.

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"skein-api/internal/outbox"
	"skein-api/internal/review"
)

// ---------------------------------------------------------------------------
// Factory values of the batch limits
// ---------------------------------------------------------------------------

const (
	// DefaultInterval — the worker tick period when Config.Interval is not
	// set.
	//
	// A minute is chosen from product precision, not "to tick more often":
	// the tightest segment that can be configured at all is ten minutes
	// (six burst reminders in the first hour), and a one-minute tick gives
	// an error of one tenth of the narrowest segment. A more frequent tick
	// would shrink an already-invisible error at the cost of a constant walk
	// of the sets.
	DefaultInterval = time.Minute

	// DefaultBatchSize — BATCH PER TICK: how many addressees of one category
	// one selection query takes.
	//
	// The limit exists not for the database's sake but for tick
	// predictability: without it the first tick after a long idle would
	// read every addressee in the system in one query and hold the
	// connection for as long as the aggregation takes.
	DefaultBatchSize = 200

	// DefaultBatchesPerTick — BATCHES PER TICK: how many consecutive batches
	// one tick processes before yielding until the next.
	//
	// The second limit does not duplicate the first: the batch bounds ONE
	// query, the batch count bounds the work of ONE TICK. Without the second
	// a tick with a large accumulated lag would spin batches until the queue
	// was empty, i.e. the batch bound would bound nothing. Ten batches of
	// two hundred is two thousand addressees per category per tick.
	//
	// ⚠ CORRECTED CLAIM (repair A/8). This used to say "whatever accumulates
	// past this is picked up by later ticks". That was FALSE: the walk
	// started every tick from the nil id, so the next tick took the same
	// head of the list, and the tail past the budget bound was never
	// processed at all. The claim is now true by construction: the walk
	// cursor SURVIVES THE TICK (Scheduler.cursor) — having exhausted the
	// budget, the tick remembers where it stopped, and the next one
	// continues from there and, reaching the end, returns to the head.
	// The tick, not a restart: the cursor lives in process memory, and the
	// bounds of that promise are written on the Scheduler.cursor field.
	//
	// Absence of catch-up in the schedule is a separate decision and lives
	// separately (review.Settings.NextReminderAt): a lagging worker does not
	// fire a volley for every slept-through segment.
	DefaultBatchesPerTick = 10
)

// ---------------------------------------------------------------------------
// Sweep stages
// ---------------------------------------------------------------------------

// sweepStage — the place in the sweep where it stumbled. The value of the
// `stage` label on the failure counter.
type sweepStage string

// sweepStageSet — the closed set of stages, in declaration order. Filled
// ONLY by registerSweepStage.
//
// ⚠ A registry, not an enumeration next to a list, for the same reason the
// category set became one (repair A/8) and the delivery-outcome set became
// one (repair A/7): a handwritten list stays silent on an omission. A stage
// added as a constant and forgotten from the documented label-value list
// would produce a label value that is not in its own documentation — and the
// value documentation is what alert rules are written against.
var sweepStageSet []sweepStage

func registerSweepStage(name string) sweepStage {
	if name == "" {
		panic("nudges: sweep stage must have a name")
	}
	for _, s := range sweepStageSet {
		if string(s) == name {
			panic("nudges: sweep stage registered twice: " + name)
		}
	}
	st := sweepStage(name)
	sweepStageSet = append(sweepStageSet, st)
	return st
}

var (
	// stageClock — the database clock could not be read, the tick is skipped
	// entirely.
	stageClock = registerSweepStage("clock")
	// stageSelect — candidate selection failed; the category is deferred to
	// the next tick from the same cursor.
	stageSelect = registerSweepStage("select")
	// stageSchedules — addressees' schedule settings could not be read;
	// the category is deferred, the segment is not consumed.
	stageSchedules = registerSweepStage("schedules")
	// stageSchedule — the schedule arithmetic refused to answer for one
	// addressee (the due predicate or the next-mark calculation).
	stageSchedule = registerSweepStage("schedule")
	// stageForm — the formation transaction rolled back; the segment is not
	// consumed, the addressee will return on the next tick.
	stageForm = registerSweepStage("form")
)

// SweepStages returns the closed set of stages as strings, in declaration
// order. It exists for those who must walk the set in full — label-value
// documentation is checked against it, not against a list written next to
// it.
func SweepStages() []string {
	out := make([]string, 0, len(sweepStageSet))
	for _, s := range sweepStageSet {
		out = append(out, string(s))
	}
	return out
}

func (s sweepStage) String() string { return string(s) }

// ---------------------------------------------------------------------------
// Measurements
// ---------------------------------------------------------------------------
//
// ⚠ WHAT IS DISTINGUISHED HERE AND WHY THERE ARE FOUR OF THESE, NOT ONE.
//
// The spec requires distinguishability: "the worker's lag behind the schedule
// is measurable: otherwise 'reminders aren't arriving' is indistinguishable
// from 'nobody to remind'". One formed-counter is not enough for that, and
// one lag histogram isn't either: BOTH stay silent on an empty selection —
// exactly when the question is asked. Until this task the set consisted of
// lag, formed, and lost segments, and all three were flat in THREE different
// cases at once:
//
//	the worker is not running at all              → flat
//	the worker is running, nobody to remind       → flat
//	the worker is running, selection is failing   → flat
//
// The third case is not invented: SelectDue fails, sweep exits the category,
// and from the outside it looks exactly like "nobody is waiting for a
// reminder". So the distinction is held by FOUR measurements, and what
// distinguishes them is precisely that they grow in non-overlapping cases:
//
//	ticks   grow    → the worker is alive and looking     (otherwise not running)
//	  + candidates 0 and failures 0 → nobody to remind
//	  + candidates 0 and failures grow → broken, not empty
//	  + candidates grow, formed 0 → segments are not being opened
//
// Lag remains what it was: a measure of HOW LATE an already-expired segment
// was picked up. It answers "how late is it", not "is it alive" — those are
// different questions, and one measurement does not stretch over both.

// nudgeTicksTotal — ticks that reached the sweep.
//
// @metric description Nudge worker ticks that read the database clock and went on to sweep, one increment per tick. Unlabelled and deliberately so: this is the LIVENESS signal, the number that separates "the worker is not running" from "the worker is running and there is nobody to remind". Both of those leave skein_nudges_formed_total and skein_nudge_schedule_lag_seconds perfectly flat, which is why neither of them can answer the question the spec asks. A tick whose clock read failed does NOT increment this — it increments skein_nudge_sweep_failures_total with stage="clock" instead, so "alive but blind" is a third state and not silence.
// @metric calculation Incremented in nudges.Scheduler.tickAt once per tick, at the head of the walk over the categories — after Store.Now has answered, because a tick whose clock read failed never reaches tickAt.
var nudgeTicksTotal = promauto.NewCounter(
	prometheus.CounterOpts{
		Name: "skein_nudge_ticks_total",
		Help: "Nudge worker ticks that read the clock and swept",
	},
)

// nudgeCandidatesSelectedTotal — "there is someone to remind".
//
// @metric description Reminder candidates the nudge worker selected, one increment per row returned by the batched selection, by reminder category. Together with skein_nudge_ticks_total this is the pair that makes "reminders aren't arriving" tellable apart from "nobody to remind": ticks rising with this counter flat means the set is genuinely empty, while ticks rising with this counter rising and skein_nudges_formed_total flat means candidates are found and their segments are not being opened. A selected candidate is NOT yet a formed reminder — most selected rows are legitimately not due yet, so this counter is always at or above the formed one and the ratio is not an error rate.
// @metric calculation Incremented in nudges.Scheduler.sweep once per candidate in every batch Store.SelectDue returns, before the batch is considered.
// @label category Reminder category the candidate belongs to. Values: the closed set of review.Categories() — "unaccepted", "unreviewed".
var nudgeCandidatesSelectedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "skein_nudge_candidates_selected_total",
		Help: "Reminder candidates selected by the nudge worker, by category",
	},
	[]string{"category"},
)

// nudgeSweepFailuresTotal — "broken, not empty".
//
// @metric description Nudge worker sweep failures, by the stage that failed, one increment per failed step. This is the counter that stops a broken sweep from looking like an empty one: a failing candidate selection also leaves skein_nudge_candidates_selected_total and skein_nudges_formed_total flat, so without this metric a database outage and a quiet Friday are the same picture. The stages differ in blast radius, which is why the stage is a label and not a second metric: clock, select and schedules abandon a WHOLE category for this tick, while schedule and form lose ONE recipient and let the sweep continue. Nothing is lost in any of them — no segment is consumed, so every affected recipient returns on the next tick; the failure costs latency, not a reminder.
// @metric calculation Incremented in nudges.Scheduler.tick (stage="clock"), nudges.Scheduler.sweep (stage="select", stage="schedules") and nudges.Scheduler.consider (stage="schedule", stage="form"), each at the log call site that records the same failure.
// @label stage Where the sweep failed. Values: the closed set of nudges.SweepStages() — "clock", "select", "schedules", "schedule", "form".
var nudgeSweepFailuresTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "skein_nudge_sweep_failures_total",
		Help: "Nudge worker sweep failures, by the stage that failed",
	},
	[]string{"stage"},
)

// nudgesFormedTotal — formed reminders.
//
// @metric description Reminder digests FORMED by the nudge worker, one increment per notification row created, split by reminder category. Named after the verifiable fact (INV-10): the count is of notifications put into delivery, NOT of pushes that reached a device — delivery is asynchronous and its outcomes are counted separately by skein_outbox_push_outcomes_total. Because a formed digest is exactly what consumes a rate-limit segment, this counter is also the observable upper bound the spec promises: at most one increment per recipient and category per segment.
// @metric calculation Incremented in nudges.Scheduler.consider after the transaction that consumed the segment, wrote the audit event and inserted the push-only notification row commits. A candidate whose segment was consumed by a competing instance increments skein_nudge_segments_lost_total instead.
// @label category Reminder category the digest was formed for. Values: the closed set of review.Categories() — "unaccepted", "unreviewed".
var nudgesFormedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "skein_nudges_formed_total",
		Help: "Reminder digests formed by the nudge worker, by category",
	},
	[]string{"category"},
)

// nudgeSegmentsLostTotal — segments that went to a neighbouring instance.
//
// @metric description Rate-limit segments a nudge worker instance tried to consume and found already consumed by a competing instance, by reminder category. This is the visible side of INV-8's concurrency guarantee working: the conditional update returned zero rows, so nothing was written and no duplicate reminder exists. A steady low rate is normal on a multi-replica deployment; a rate comparable to skein_nudges_formed_total means the replicas are sweeping in lockstep and the tick phase is worth spreading.
// @metric calculation Incremented in nudges.Scheduler.consider when Store.Form reports the segment was not consumed by this call.
// @label category Reminder category whose segment was lost. Values: the closed set of review.Categories() — "unaccepted", "unreviewed".
var nudgeSegmentsLostTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "skein_nudge_segments_lost_total",
		Help: "Rate-limit segments lost to a competing nudge worker instance, by category",
	},
	[]string{"category"},
)

// nudgeScheduleLagSeconds — the worker's lag behind the schedule.
//
// ⚠ CORRECTION OF THIS TASK (T021) AND A CORRECTED RECONSTRUCTION OF ITS
// CAUSE (review A/9; the previous explanation is not erased, because being
// wrong about the cause is more dangerous than not knowing it: the next such
// case is then "fixed" for the wrong reason).
//
// WHAT WAS HERE. This metric's annotations existed since an earlier change, but the
// doc-block was written NOT ABOVE ITS OWN DECLARATION: it sat above
// nudgesFormedTotal, and `var nudgeScheduleLagSeconds` lived two declarations
// below — and a blank line stood in front of it, i.e. it had no doc-comment
// AT ALL. go/ast attributes a doc only to the declaration that follows the
// comment immediately; a declaration separated by a blank line is left
// without Doc. Hence the symptom: a histogram with not a single directive.
//
// WHAT WAS WRITTEN HERE EARLIER AND WHY IT IS WRONG. The previous edition
// explained the symptom as: "there was no blank line between them, THEREFORE
// go/ast attributed the whole block to the next declaration, and the
// histogram was left without directives". The absence of a blank line was
// real, the conclusion was not. Checked with a probe against the parse tree
// on the defective source itself:
//
//	as it was:                 nudgesFormedTotal desc=2  …ScheduleLagSeconds doc=0 bytes
//	a blank line inserted:     nudgesFormedTotal desc=1  …ScheduleLagSeconds doc=0 bytes
//
// I.e. the blank line explains A DIFFERENT, then-unnoticed symptom —
// nudgesFormedTotal glued both blocks into one group and carried two
// directives of each kind. It does not explain the histogram's emptiness and
// inserting one does not fix the defect: what fixes it is moving the block
// to its own declaration, which is what was done.
//
// A mutation of "remove the blank line so the block sticks to the neighbour"
// does NOT reproduce the A/8 defect — on the current tree attribution does
// not change from it at all (2077 bytes of doc before and after), and such a
// mutation is green by right.
//
// The second defect of the same period still stands and is fixed separately:
// the annotation guard walked TWO handwritten catalogues, and internal/nudges
// was not in the list, so nobody saw the empty histogram. There the walk now
// follows the fact of the metric's declaration.
//
// @metric description Histogram of reminder schedule lag in seconds: the scheduler-supplied "now" minus the frozen next_due_at of a candidate whose segment has expired, per reminder category. Zero-ish on a healthy deployment (a segment is picked up within one tick of falling due); a growing tail means the worker is behind its own schedule. This measures HOW LATE a due segment was picked up, not whether the worker is alive — the liveness question belongs to skein_nudge_ticks_total, because an idle worker with nothing due observes nothing here at all. Candidates with no rate-limit state yet contribute nothing either: they have no frozen deadline, so they have nothing to be late against.
// @metric calculation Observed in nudges.Scheduler.consider once per selected candidate that is due and carries a stored next_due_at, before the segment is consumed.
// @label category Reminder category of the late segment. Values: the closed set of review.Categories() — "unaccepted", "unreviewed".
var nudgeScheduleLagSeconds = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "skein_nudge_schedule_lag_seconds",
		Help:    "Lag between a reminder segment falling due and the scheduler picking it up, in seconds",
		Buckets: []float64{1, 5, 15, 60, 300, 900, 3600, 21600},
	},
	[]string{"category"},
)

// ---------------------------------------------------------------------------
// Configuration and the loop
// ---------------------------------------------------------------------------

// Config bounds the worker goroutine. Filled in main() from config.Config;
// the package itself does not know about the config package — the same move
// as reminders.Config and lockreap.Config.
type Config struct {
	// Interval — period between ticks. <= 0 → DefaultInterval.
	Interval time.Duration
	// BatchSize — batch per tick. <= 0 → DefaultBatchSize.
	BatchSize int
	// BatchesPerTick — batches per tick. <= 0 → DefaultBatchesPerTick.
	BatchesPerTick int
}

// Scheduler — the background worker of repeat reminders. Construction is
// cheap; Run starts the loop and blocks until the context is cancelled.
type Scheduler struct {
	store *Store
	cfg   Config

	// mu protects cursor. Run spins the loop in one goroutine, but Tick is
	// open to the outside, and a second caller must not be able to tear the
	// map.
	mu sync.Mutex
	// cursor — where the walk of each category stopped.
	//
	// SURVIVES THE TICK, and that is a load-bearing property, not an
	// optimisation. The walk is keyset by addressee id; having exhausted the
	// batch budget, the tick remembers the last id it looked at, and the
	// next tick continues from it. Reaching the end of the list, the walk
	// returns to the head (uuid.Nil).
	//
	// Without this the tail of the list was NEVER processed under an
	// overflowing budget: every tick started from nil and spent the budget
	// on the same head. The head could fail to drain indefinitely — see the
	// first-mark lower bound in store.selectDueSQL. The two measures do not
	// replace each other: the bound removes the ones that are certainly not
	// due from the selection, the cursor guarantees progress over those who
	// remain in the selection legitimately.
	//
	// ⚠ BOUND OF THE PROMISE (recorded by the second round of review A/8).
	// The cursor lives ONLY IN PROCESS MEMORY. So "the tail waits no longer
	// than one round" is true inside an instance's lifetime, not
	// unconditionally: a restart — a rollout, a pod restart, a crash —
	// zeroes the cursor, and the walk starts from the head. With restarts
	// more frequent than a walk round, the tail can again fail to get its
	// turn.
	//
	// Putting the cursor in the database is NOT needed now, and that is a
	// decision, not an oversight: a walk round at factory limits is two
	// thousand addressees per category per tick, i.e. at any plausible
	// installation size a round takes a handful of ticks (minutes), and
	// restarts are orders of magnitude rarer. The measure becomes meaningful
	// if a deployment appears where a round is comparable to the interval
	// between restarts; then the cursor belongs in a state table, not in a
	// struct field.
	cursor map[review.Category]uuid.UUID
}

// NewScheduler builds a worker on top of the store.
//
// Empty limits are replaced with factory values HERE, not in the environment
// parse: the worker must be operational under any construction path, including
// from a test that passed only what it is checking.
func NewScheduler(store *Store, cfg Config) *Scheduler {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.BatchesPerTick <= 0 {
		cfg.BatchesPerTick = DefaultBatchesPerTick
	}
	return &Scheduler{
		store:  store,
		cfg:    cfg,
		cursor: make(map[review.Category]uuid.UUID, len(review.Categories())),
	}
}

// cursorFor returns the resume point of the category's walk. The nil id is
// the start: it is less than any id the system issues (all user ids are
// UUIDv7).
func (sc *Scheduler) cursorFor(cat review.Category) uuid.UUID {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.cursor[cat]
}

// setCursor remembers the resume point of the category's walk.
func (sc *Scheduler) setCursor(cat review.Category, after uuid.UUID) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.cursor[cat] = after
}

// Run starts the worker loop. The first tick fires immediately on start —
// after the same pattern as the other workers in the repository: what
// accumulated during idle must not wait a whole interval.
//
// The loop does not depend on bus availability: reminders live as rows in
// the database, and the live feed and push are different transports. A
// worker bound to NATS would stay silent with an empty NATS_URL, and that
// would be exactly the silent breakage the earlier change moved the push path out from
// under the bus branch to eliminate.
func (sc *Scheduler) Run(ctx context.Context) {
	slog.Info("nudge scheduler started",
		"interval", sc.cfg.Interval,
		"batchSize", sc.cfg.BatchSize,
		"batchesPerTick", sc.cfg.BatchesPerTick)

	sc.tick(ctx)

	t := time.NewTicker(sc.cfg.Interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("nudge scheduler stopping")
			return
		case <-t.C:
			sc.tick(ctx)
		}
	}
}

// Tick runs one sweep of both categories. Open for tests, so the worker can
// be driven deterministically, without a timer.
func (sc *Scheduler) Tick(ctx context.Context) { sc.tick(ctx) }

// tick walks both categories "now", where "now" is the DATABASE CLOCK, not
// this process's clock.
//
// The process clock is unfit here in principle (repair A/8): INV-8 is
// promised with several instances running at once, and the instances' clocks
// diverge. While "now" came from each process's time.Now(), the promise
// rested on an unwritten and unchecked assumption of replica clock
// agreement: a replica that had run ahead by more than a segment formed a
// second reminder for the same segment (the threshold is three hours at
// factory settings, fifteen minutes at the minimum setting). The database
// clock is one for everyone, so the assumption is not softened, it
// disappears.
//
// Could not be read — the tick is skipped entirely. There is nothing to
// decide "is it due" from, and substituting the process clock "just in case"
// would return exactly the hole this fix closes.
func (sc *Scheduler) tick(ctx context.Context) {
	now, err := sc.store.Now(ctx)
	if err != nil {
		nudgeSweepFailuresTotal.WithLabelValues(stageClock.String()).Inc()
		slog.WarnContext(ctx, "nudge scheduler: database clock unreadable, tick skipped",
			"error", err.Error())
		return
	}
	sc.tickAt(ctx, now)
}

// tickAt walks both categories in one loop from a given moment.
//
// "Now" is taken ONCE per tick and passed on down the whole chain: into the
// query's deadline predicate and into the schedule arithmetic. Two different
// "now"s in one decision would give an unrepeatable answer on the segment
// boundary — the selection would consider the deadline expired, and the
// schedule would recompute it into the future.
//
// The moment is a parameter, not a clock read inside, precisely for this:
// the tick has one, and uniqueness is expressed by the signature. The
// schedule test class uses the same property to drive the worker in the past
// and the future without substituting the process clock.
func (sc *Scheduler) tickAt(ctx context.Context, now time.Time) {
	// The liveness mark is placed HERE, not in tick: the walk begins at this
	// line, and it is its absence that means "the worker is not running".
	// In tick it would sit after the clock read, i.e. in a place the
	// schedule class — the only one that drives the walk deterministically
	// — never enters at all, and the liveness assertion would be checked
	// against the real clock and a shared database where the selection is
	// never predictably empty.
	nudgeTicksTotal.Inc()
	for _, cat := range review.Categories() {
		if ctx.Err() != nil {
			return
		}
		sc.sweep(ctx, cat, now)
	}
}

// sweep walks the addressees of one category in batches.
//
// The walk is KEYSET by addressee id and RESUMABLE between ticks. OFFSET is
// unfit here: an addressee whose segment legitimately remains unexpired in
// the selection would shift row numbers underfoot.
//
// The cursor survives the tick (see Scheduler.cursor). A tick that exhausted
// the batch budget leaves the cursor on the last id it looked at; a tick
// that reached the end of the list returns the cursor to the head. Before
// repair A/8 there was no cursor at all — every tick started from nil, and
// everything past the budget bound went unprocessed until the head drained.
// The head could fail to drain indefinitely.
func (sc *Scheduler) sweep(ctx context.Context, cat review.Category, now time.Time) {
	after := sc.cursorFor(cat)

	for batch := 0; batch < sc.cfg.BatchesPerTick; batch++ {
		if ctx.Err() != nil {
			// Stop mid-sentence: the cursor stays where the walk actually
			// finished work, so the next start does not redo what is done
			// and does not skip what is not.
			sc.setCursor(cat, after)
			return
		}

		cands, err := sc.store.SelectDue(ctx, cat, now, sc.cfg.BatchSize, after)
		if err != nil {
			nudgeSweepFailuresTotal.WithLabelValues(stageSelect.String()).Inc()
			slog.WarnContext(ctx, "nudge scheduler: candidate selection failed, "+
				"category deferred to the next tick from the same cursor",
				"category", cat.String(), "after", after.String(), "error", err.Error())
			sc.setCursor(cat, after)
			return
		}
		if len(cands) == 0 {
			// The list ended — the category walk is done, the next tick
			// starts from the head.
			sc.setCursor(cat, uuid.Nil)
			return
		}

		nudgeCandidatesSelectedTotal.WithLabelValues(cat.String()).Add(float64(len(cands)))

		ids := make([]uuid.UUID, 0, len(cands))
		for _, c := range cands {
			ids = append(ids, c.UserID)
		}
		schedules, err := sc.store.prefs.ReviewSchedules(ctx, ids)
		if err != nil {
			nudgeSweepFailuresTotal.WithLabelValues(stageSchedules.String()).Inc()
			// The schedule was not read — there is nothing to decide "is it
			// due" from.
			//
			// ⚠ CORRECTED MESSAGE (repair A/8): this used to say "batch
			// deferred", although the code exits sweep entirely, i.e. it
			// drops the WHOLE CATEGORY for this tick, not one batch. The
			// text is brought in line with the behaviour. The segment is
			// not consumed, so nothing is lost, and the cursor is left at
			// the START of the unread batch — the next tick will take
			// exactly that.
			slog.WarnContext(ctx, "nudge scheduler: schedules unreadable, "+
				"category deferred to the next tick from the same cursor",
				"category", cat.String(), "after", after.String(), "error", err.Error())
			sc.setCursor(cat, after)
			return
		}

		for _, c := range cands {
			if ctx.Err() != nil {
				sc.setCursor(cat, after)
				return
			}
			sc.consider(ctx, cat, c, schedules[c.UserID], now)
		}

		after = cands[len(cands)-1].UserID
		if len(cands) < sc.cfg.BatchSize {
			// A short batch — the list ended.
			sc.setCursor(cat, uuid.Nil)
			return
		}
	}

	// The batch budget is exhausted. WE REMEMBER where we stopped: that is
	// exactly what makes the promise "whatever accumulates past the budget
	// is picked up by later ticks" true.
	sc.setCursor(cat, after)
	slog.WarnContext(ctx, "nudge scheduler: per-tick batch budget reached, "+
		"the rest resumes from this cursor on the next tick",
		"category", cat.String(), "batchesPerTick", sc.cfg.BatchesPerTick,
		"after", after.String())
}

// consider decides the fate of one candidate.
func (sc *Scheduler) consider(
	ctx context.Context, cat review.Category, c Candidate, s review.Settings, now time.Time,
) {
	due, err := sc.due(cat, c, s, now)
	if err != nil {
		nudgeSweepFailuresTotal.WithLabelValues(stageSchedule.String()).Inc()
		slog.WarnContext(ctx, "nudge scheduler: schedule refused to answer",
			"category", cat.String(), "userId", c.UserID.String(), "error", err.Error())
		return
	}
	if !due {
		return
	}
	if c.NextDueAt != nil {
		nudgeScheduleLagSeconds.WithLabelValues(cat.String()).
			Observe(now.Sub(*c.NextDueAt).Seconds())
	}

	// The new frozen deadline is computed with CURRENT settings — that is
	// "a setting change takes effect from the next segment". From the
	// countdown base and the formation moment, and only that way: a shift
	// from the previous deadline would replay every slept-through segment.
	//
	// The tick moment is NOT passed into Form (repair A/8, second round):
	// the tick's "now" was read at its start and is stale by the time of
	// the write, and the deadline guard must compare against the same value
	// as the segment gate. So the guard lives in Form's transaction and
	// asks its own now(). Here the moment is needed only by the schedule —
	// it counts marks from it, it does not decide "has the segment
	// expired".
	next, err := s.NextReminderAt(cat, c.Base, now)
	if err != nil {
		nudgeSweepFailuresTotal.WithLabelValues(stageSchedule.String()).Inc()
		slog.WarnContext(ctx, "nudge scheduler: next reminder unschedulable",
			"category", cat.String(), "userId", c.UserID.String(), "error", err.Error())
		return
	}

	res, err := sc.store.Form(ctx, cat, c, next)
	if err != nil {
		// Nothing written: the transaction is rolled back entirely. The
		// addressee will return on the next tick — their segment is not
		// consumed.
		//
		// ⚠ WHAT THIS COSTS is written here because it is written nowhere
		// else (observation of repair A/8, third round; behaviour did not
		// change). A refusal of Form's deadline guard is not a free
		// defence: it costs the addressee THIS TICK'S SLOT. The tick
		// moment was read at its start; if the tick ran longer than the
		// schedule measured to the next mark, the deadline is no longer
		// future by the time of the write, the guard refuses, and the
		// candidate is skipped — while the walk cursor moves on through
		// the batch, i.e. they are not returned to within this tick.
		//
		// Permanent starvation does not follow: the segment is not
		// consumed and the frozen deadline is not rewritten, so the next
		// walk hands the same addressee back, already with a fresh moment.
		// The worst outcome is a delay of one walk, and that is strictly
		// better than the previous behaviour, in which the same stale
		// moment opened an expired segment and produced a DUPLICATE.
		// Delay instead of a duplicate is a conscious trade-off, not an
		// oversight.
		nudgeSweepFailuresTotal.WithLabelValues(stageForm.String()).Inc()
		slog.WarnContext(ctx, "nudge scheduler: reminder not formed",
			"category", cat.String(), "userId", c.UserID.String(), "error", err.Error())
		return
	}
	if !res.Formed {
		// Segment eaten by a neighbouring instance between selection and
		// write. This is the expected outcome of coordination, not an
		// error (INV-8).
		nudgeSegmentsLostTotal.WithLabelValues(cat.String()).Inc()
		return
	}

	nudgesFormedTotal.WithLabelValues(cat.String()).Inc()
	outbox.ObserveSuperseded(res.Reason, res.Superseded)

	slog.InfoContext(ctx, "reminder digest formed",
		"category", cat.String(),
		"userId", c.UserID.String(),
		"issues", len(c.IssueIDs),
		"superseded", res.Superseded,
		"nextDueAt", next.Format(time.RFC3339))
}

// due answers whether this addressee's segment has expired.
//
// TWO branches, and the difference between them is load-bearing:
//
//   - rate-limit state EXISTS: the deadline is READ and is never
//     recomputed. It is frozen at the settings that were in force when the
//     segment was opened, and recomputing with current ones would cut a
//     started segment when the interval is shortened and stretch it when
//     lengthened — and a setting change must take effect only from the next
//     segment. The query has already selected rows by this same condition
//     with this same "now"; it is written here again because the rule
//     belongs to the decision, not to the query form.
//
//     ⚠ The previous edition of this paragraph promised the rule "would
//     survive any rewrite of the selection", and the promise was held by
//     NOTHING: a `return true, nil` mutation was green — on live data the
//     branch is always true, because SelectDue already selected the rows
//     with the same condition. It is now held by
//     TestDue_FrozenDeadlineIsReadNotRecomputed: it calls due directly,
//     with candidates the selection would not have returned, and on
//     settings under which a recompute would give a different answer;
//
//   - there is NO state: a reminder of this category has never been formed
//     for this person, and the first deadline is computed by the schedule
//     from the countdown base. It is this branch that fulfils INV-11 — the
//     first reminder of an unaccepted issue is not earlier than one burst
//     segment after assignment: an empty "last" yields the first mark of
//     the grid, not "now".
//
// The non-strict comparison "deadline not after now" is the same as
// review.IsDue, and for the same reason: when the clock goes backwards the
// moment simply has not arrived, the segment is not declared expired and no
// flood occurs.
func (sc *Scheduler) due(
	cat review.Category, c Candidate, s review.Settings, now time.Time,
) (bool, error) {
	if c.NextDueAt != nil {
		return !now.Before(*c.NextDueAt), nil
	}
	return s.IsDue(cat, c.Base, time.Time{}, now)
}
