package outbox

import (
	"strings"
	"testing"
	"time"

	"skein-api/internal/astguard"
	"skein-api/internal/notifications"
)

// push_machine_test.go — sk-303.13, coverage-matrix row 18: the UNIT gate
// on the delivery outcome machine and on the retention rule. No database.
//
// The behaviour of the runner is proven against Postgres elsewhere (rows
// 19–21, 23). What is proven HERE is the decision itself: which per-device
// answers make which verdict, which verdicts are final, when a transport
// failure stops being retryable, and which rows the retention sweep may take.
// A decision that is only ever exercised through I/O is a decision nobody can
// read back from a test.

// TestPushMachine_ClassifiesEveryCombinationOfDeviceAnswers — the whole truth
// table of one delivery attempt.
func TestPushMachine_ClassifiesEveryCombinationOfDeviceAnswers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                          string
		sent, gone, failed, forbidden int
		wantOutcome                   string
		wantCause                     string
		wantTerminal                  bool
		why                           string
	}{
		{"one device, delivered", 1, 0, 0, 0, outcomeSuccess, causeNone, true,
			"delivery to one active device is delivery"},
		{"three devices, one delivered", 1, 1, 1, 0, outcomeSuccess, causeNone, true,
			"a second device failing does not un-deliver the first"},
		{"every endpoint was dead", 0, 2, 0, 0, outcomeNoDevice, causeNone, true,
			"all endpoints pruned — the recipient has no active device, which is a legitimate final outcome"},
		{"one transport failure", 0, 0, 1, 0, "", causeNone, false,
			"a transport failure is not an outcome yet — the row stays owed"},
		{"dead endpoints and a transport failure", 0, 1, 1, 0, "", causeNone, false,
			"the live endpoint failed to answer, so there is still something to retry"},
		// sk-627: a 403 is the push service rejecting our credentials.
		{"the only device answered 403", 0, 0, 0, 1, outcomeConfigurationNoSend, causeProviderForbidden, true,
			"a 403 is a configuration refusal: a retry re-sends the same rejected JWT"},
		{"403 and a dead endpoint", 0, 1, 0, 1, outcomeConfigurationNoSend, causeProviderForbidden, true,
			"the device that refused is not dead — the recipient still has one, the configuration is what failed"},
		{"403 on one device, another delivered", 1, 0, 0, 1, outcomeSuccess, causeNone, true,
			"a refusal on one device does not un-deliver another"},
		{"403 and a transport failure", 0, 0, 1, 1, "", causeNone, false,
			"the transiently failing device is still worth a retry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outcome, cause, terminal := classifySendOutcome(sendTally{
				sent: tc.sent, gone: tc.gone, failed: tc.failed, forbidden: tc.forbidden,
			})
			if terminal != tc.wantTerminal {
				t.Fatalf("terminal = %v, want %v — %s", terminal, tc.wantTerminal, tc.why)
			}
			if outcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q — %s", outcome, tc.wantOutcome, tc.why)
			}
			if cause != tc.wantCause {
				t.Errorf("cause = %q, want %q — %s", cause, tc.wantCause, tc.why)
			}
		})
	}
}

// TestPushMachine_TransportFailureIsTheOnlyNonTerminalAnswer — SC-024 stated as
// a property rather than as a scenario. Every outcome but one ends the row on
// the spot; only the transport case leaves it alive, and only until its bounds
// are reached.
//
// The taxonomy is read from the package source (outcome_set_test.go). It was
// a hand-written list of six, and «six» stopped being true the moment
// sk-303.14 added the seventh — the list said nothing, because a hand-written
// list never does.
func TestPushMachine_TransportFailureIsTheOnlyNonTerminalAnswer(t *testing.T) {
	t.Parallel()
	// Every outcome the machine can NAME is final by construction: the runner
	// only ever writes one through finalise.
	for _, o := range allOutcomeValues(t) {
		if o == "" {
			t.Fatal("an outcome constant is empty")
		}
	}
	// And the one non-terminal answer names nothing — a "pending outcome"
	// would be a contradiction the schema's CHECK already refuses
	// ((push_state = 'final') = (push_outcome IS NOT NULL)).
	if outcome, _, terminal := classifySendOutcome(sendTally{failed: 1}); terminal || outcome != "" {
		t.Errorf("a transport failure produced outcome %q terminal=%v, want an unnamed non-terminal answer",
			outcome, terminal)
	}
}

// TestPushMachine_BudgetSpentHonoursBothBounds — the retryability rule. Two
// independent bounds, either of which ends the row.
func TestPushMachine_BudgetSpentHonoursBothBounds(t *testing.T) {
	t.Parallel()
	now := time.Now()
	soon := now.Add(time.Minute)
	far := now.Add(24 * time.Hour)

	cases := []struct {
		name      string
		attempts  int
		max       int
		next      time.Time
		deadline  *time.Time
		wantSpent bool
		why       string
	}{
		{"budget left, deadline far", 1, 5, now.Add(30 * time.Second), &far, false,
			"neither bound is reached"},
		{"attempts exhausted", 5, 5, now.Add(30 * time.Second), &far, true,
			"the attempt budget alone ends it"},
		{"attempts over budget", 9, 5, now.Add(30 * time.Second), &far, true,
			"an over-spent row must not keep going"},
		{"retry would land past the deadline", 1, 5, far, &soon, true,
			"scheduling past the deadline only buys one wake-up that retires the row"},
		{"retry lands exactly on the deadline", 1, 5, soon, &soon, true,
			"on the deadline is not before it"},
		{"no deadline at all", 1, 5, far, nil, false,
			"a row with no deadline is bounded by attempts only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := budgetSpent(tc.attempts, tc.max, tc.next, tc.deadline); got != tc.wantSpent {
				t.Errorf("budgetSpent = %v, want %v — %s", got, tc.wantSpent, tc.why)
			}
		})
	}
}

// TestPushMachine_BackoffIsBoundedAndGrowing — a retry schedule that does not
// grow hammers a failing provider; one that grows without a cap can outlive
// the deadline and turn "retry" into "never".
func TestPushMachine_BackoffIsBoundedAndGrowing(t *testing.T) {
	t.Parallel()
	prev := time.Duration(0)
	for attempt := 1; attempt <= 12; attempt++ {
		d := pushBackoff(attempt)
		if d < prev {
			t.Errorf("attempt %d: delay %v is shorter than the previous %v", attempt, d, prev)
		}
		if d > pushRetryMaxDelay {
			t.Errorf("attempt %d: delay %v exceeds the cap %v", attempt, d, pushRetryMaxDelay)
		}
		prev = d
	}
	if pushBackoff(1) != pushRetryBaseDelay {
		t.Errorf("first delay = %v, want the base %v", pushBackoff(1), pushRetryBaseDelay)
	}
	if pushBackoff(2) <= pushBackoff(1) {
		t.Error("the back-off does not actually grow")
	}
	// The whole budget must fit inside the delivery deadline, or the attempt
	// bound would be decorative — every row would die of the deadline instead.
	var total time.Duration
	for attempt := 1; attempt <= defaultPushMaxAttempts; attempt++ {
		total += pushBackoff(attempt)
	}
	if total >= notifications.PushDeliveryDeadline {
		t.Errorf("the full retry schedule (%v) does not fit inside the delivery deadline (%v) — the attempt budget can never be reached",
			total, notifications.PushDeliveryDeadline)
	}
}

// TestPushMachine_RetentionPredicateTakesOnlyFinishedInvisibleRows — SC-025 as
// a rule, checked on the statement that enforces it.
//
// The eligibility rule lives in SQL because the sweep is a set operation, so a
// Go mirror of it would be a second source of truth that drifts. What a unit
// test can do — and what a behavioural test on a seeded row cannot — is assert
// that every condition is PRESENT: a behavioural test proves the rows it seeded
// are handled correctly, this proves no condition was quietly dropped for the
// rows nobody seeded.
func TestPushMachine_RetentionPredicateTakesOnlyFinishedInvisibleRows(t *testing.T) {
	t.Parallel()
	sql := deleteEligibleSQL

	for _, required := range []struct{ fragment, why string }{
		{"NOT feed_channel", "a feed row is somebody's history and must never be swept"},
		{"push_state = 'final'", "anything pending, leased or awaiting a retry is still owed"},
		{"push_outcome IS NOT NULL", "a final row without an outcome would be a contradiction, and sweeping it would hide it"},
		{"push_finalized_at IS NOT NULL", "the window is counted from the outcome, so a row without that stamp has no age"},
		{"push_finalized_at < $1", "the retention window itself"},
		{"LIMIT $2", "the batch bound — an unbounded DELETE would lock the whole backlog at once"},
		{"ORDER BY push_finalized_at", "oldest first, so an interrupted sweep still made the progress that matters"},
	} {
		if !strings.Contains(sql, required.fragment) {
			t.Errorf("the retention predicate has no %q — %s", required.fragment, required.why)
		}
	}

	// The rule is «any final outcome», so the statement must NOT name
	// individual outcomes: exempting configuration_no_send or
	// prefs_unavailable would rebuild exactly the pile this sweep removes,
	// and those two are the ones that arrive in bulk during an incident.
	//
	// The set is read from the package source (outcome_set_test.go), and here
	// that matters most of all: a hand-written list can only prove the
	// predicate does not name the outcomes SOMEBODY REMEMBERED TO LIST. The
	// exemption a future wave writes by hand would be for the value that wave
	// just added — the one the list would not have.
	for _, outcome := range allOutcomeValues(t) {
		if strings.Contains(sql, "'"+outcome+"'") {
			t.Errorf("the retention predicate singles out the outcome %q — the rule is ANY final outcome", outcome)
		}
	}
}

// TestPushMachine_RetentionWindowIsTheDocumentedNumber — the spec names thirty
// days on purpose: "a 'bounded lifetime' without a value cannot be told from
// storing forever". A test that reads the constant back is what keeps the
// number from drifting into a comment.
func TestPushMachine_RetentionWindowIsTheDocumentedNumber(t *testing.T) {
	t.Parallel()
	if want := 30 * 24 * time.Hour; PushRetention != want {
		t.Errorf("PushRetention = %v, want %v (SC-025 names the number)", PushRetention, want)
	}
	// A retention window shorter than the delivery deadline would delete rows
	// that are still being retried.
	if PushRetention <= notifications.PushDeliveryDeadline {
		t.Errorf("retention (%v) is not longer than the delivery deadline (%v) — the sweep could reach a row that is still owed",
			PushRetention, notifications.PushDeliveryDeadline)
	}
}

// TestPushMachine_EveryDeclaredOutcomeIsReadSomewhere — sk-303.14 / T021,
// row 118: the machine half of the taxonomy assertion.
//
// The outcome set is closed and lives as one list of constants. The matrix
// row requires a NEW outcome to be present in the outcome machine, not only
// in the schema — and requires it precisely because the list is hand-written:
// a value declared and read by nobody gives a state no row will ever enter,
// and a label — a documented value an alert will never fire on. Neither is
// visible in any other gate: TestOutcomeSet_CodeAndSchemaAgree checks the
// code against the SCHEMA and accepts such an outcome as matching, and the
// value-documentation guard checks against the annotation text and also
// passes.
//
// ⚠ CORRECTED MISTAKE (review A/9, finding A.2; not erased). The first
// edition counted IDENTIFIER OCCURRENCES IN THE TEXT and required two —
// "declaration plus at least one read". The threshold was met by the doc
// comment, which in this repository by style starts with the declared name,
// i.e. BEFORE any code. Proven by mutation: three reads of
// `outcomeSuperseded` in supersede.go were replaced with the string literal
// "superseded" — behaviour and the metric snapshot identical, the constant
// declared and read by nobody, Go allows that — the package stayed green
// because the comment and the declaration already gave two.
//
// What is counted now is the TREE (internal/astguard): *ast.Ident nodes
// outside the declaration position. Comments do not enter the expression
// tree at all, so the threshold became one and means exactly what it says
// — "someone reads it".
//
// ⚠ What the assertion does NOT promise — written here so it does not
// promise more than the check holds: it does not prove that the branch
// reading the constant is actually reachable. Whether the constant is read
// by anyone at all is its subject. A re-export (`const OutcomeSuperseded =
// outcomeSuperseded`) counts as a read, because an external write hangs
// off it, and telling it apart here would mean guessing at intent.
func TestPushMachine_EveryDeclaredOutcomeIsReadSomewhere(t *testing.T) {
	t.Parallel()

	pkg, err := astguard.Parse(".")
	if err != nil {
		t.Fatalf("parsing package: %v", err)
	}
	names := allOutcomeConstNames(t)
	values := allOutcomeValues(t)
	if len(names) != len(values) {
		t.Fatalf("constant names %d, values %d — the package parse disagreed with itself",
			len(names), len(values))
	}

	for i, name := range names {
		if n := pkg.CountUses(name); n < 1 {
			t.Errorf("outcome %q (constant %s) is declared but is not read anywhere in the package: "+
				"a state no row will ever enter, and a documented "+
				"label value no alert will ever fire on",
				values[i], name)
		}
	}
}
