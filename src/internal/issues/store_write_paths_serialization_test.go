package issues

// store_write_paths_serialization_test.go — sk-622.21, T012, matrix row No. 22a
// (INV-7, edge 1, K2, decision R17): every public issues path that R17 moved
// onto withSerializableRetry absorbs a serialization failure ITSELF and
// exhausts ITS OWN budget. One test per path and per half:
//
//   - (a) absorption: a concurrent writer touches the issue row the path
//     reads and writes WITHOUT moving version (`UPDATE issues SET updated_at
//     = now()` — the sk-202 / sk-354 technique), the path blocks on it
//     (waitForBlocked is the positive control that the race was built), the
//     writer commits, the path's first attempt takes 40001 — and the call
//     returns success with its result in place. The rolled-back first
//     attempt left no duplicate: the operation's event is written exactly
//     once and its fan-out reached the fixture's recipient exactly once
//     (paths without a fan-out — labels, the delegation decline — wrote no
//     notification at all);
//   - (b) exhaustion: the writer is renewed for every attempt
//     (forceSerializationExhaustion with the path's own *MaxAttempts) — the
//     path answers with its previous classification (ErrVersionConflict and
//     the versionConflictOriginTotal series of that path, or
//     ErrNotLockHolder for Unclaim), and nothing of the operation was
//     written.
//
// Expected values are the contract of each path (spec K2/R17, plan T012),
// never read back from the code under test. dependencies.Store#Reparent and
// attachments.Store#Confirm live in their own packages (sk-634.14,
// sk-634.15).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"skein-api/internal/notifications"
)

// writePathEnv is one project: the creator (who acts unless a case says
// otherwise), a member who watches the issue by hand (the fan-out's
// recipient), and a second member for role-specific paths.
type writePathEnv struct {
	pool        *pgxpool.Pool
	store       *Store
	creator     uuid.UUID
	creatorName string
	watcher     uuid.UUID
	other       uuid.UUID
	otherName   string
	pid         uuid.UUID
	prefix      string
}

func newWritePathEnv(t *testing.T) *writePathEnv {
	t.Helper()
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	watcher := seedUser(t, pool)
	other := seedUser(t, pool)
	addProjectMember(t, pool, pid, watcher, creator)
	addProjectMember(t, pool, pid, other, creator)
	return &writePathEnv{
		pool: pool, store: NewStore(pool),
		creator: creator, creatorName: readUsername(t, pool, creator),
		watcher: watcher, other: other, otherName: readUsername(t, pool, other),
		pid: pid, prefix: prefix,
	}
}

// newIssue creates an issue (optionally assigned) and subscribes the
// watcher by hand.
func (e *writePathEnv) newIssue(t *testing.T, assignee *string) *IssueDetail {
	t.Helper()
	ctx := context.Background()
	d, err := e.store.CreateIssue(ctx, e.pid, e.prefix, "counter",
		CreateIssueInput{Title: "sk-634.13 write path", Type: "task", Priority: 2, Assignee: assignee},
		e.creator, HumanActor(e.creator, e.creatorName))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if err := notifications.NewStore(e.pool).AddIssueWatch(ctx, d.ID, e.watcher, false); err != nil {
		t.Fatalf("watch: %v", err)
	}
	return d
}

func (e *writePathEnv) version(t *testing.T, id string) int {
	t.Helper()
	var v int
	if err := e.pool.QueryRow(context.Background(), `SELECT version FROM issues WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read version: %v", err)
	}
	return v
}

func (e *writePathEnv) lastEventID(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := e.pool.QueryRow(context.Background(), `SELECT COALESCE(max(id), 0) FROM events`).Scan(&id); err != nil {
		t.Fatalf("read last event id: %v", err)
	}
	return id
}

// eventsSince counts events of evType on issueID with id > after.
func (e *writePathEnv) eventsSince(t *testing.T, issueID, evType string, after int64) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `
SELECT count(*) FROM events WHERE entity_id = $1 AND type = $2 AND id > $3`, issueID, evType, after).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// notificationsSince counts notification rows of the operation's events
// (events of the issue with id > after, of the listed types), for one user
// or — userID = uuid.Nil — for everyone.
func (e *writePathEnv) notificationsSince(t *testing.T, issueID string, types []string, after int64, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `
SELECT count(*)
  FROM notifications n JOIN events ev ON ev.id = n.event_id
 WHERE ev.entity_id = $1 AND ev.type = ANY($2::text[]) AND ev.id > $3
   AND ($4::uuid = '00000000-0000-0000-0000-000000000000'::uuid OR n.user_id = $4)`,
		issueID, types, after, userID).Scan(&n); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return n
}

func (e *writePathEnv) enableReason(t *testing.T, userID uuid.UUID, key notifications.ReasonKey) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled)
VALUES ($1, $2, true, false)
ON CONFLICT (user_id, reason) DO UPDATE SET feed_enabled = true`, userID, string(key)); err != nil {
		t.Fatalf("enable %s: %v", key, err)
	}
}

// writePathCase is one R17 path.
type writePathCase struct {
	// op is the versionConflictOriginTotal op label; "" for Unclaim.
	op       string
	attempts int
	// setup prepares the issue and returns its id plus whatever call needs.
	setup func(t *testing.T, e *writePathEnv) *writePathState
	// call runs the public operation on the tagged store.
	call func(ctx context.Context, s *Store, e *writePathEnv, st *writePathState) error
	// events are the operation's own event types, each written exactly once
	// on success.
	events []string
	// recipient gets exactly one notification of the operation's events on
	// success; uuid.Nil means the operation has no fan-out and NOBODY gets a
	// notification of them.
	recipient func(e *writePathEnv) uuid.UUID
	// applied checks the operation's result after success.
	applied func(t *testing.T, e *writePathEnv, st *writePathState)
	// exhausted checks the error of an exhausted budget and returns the
	// expected origin label ("" when no metric applies). standVersion is the
	// issue's version read before the call: the colliding writer does not
	// move it, and an exhausted path writes nothing.
	exhausted func(t *testing.T, err error, standVersion int) string
	// notApplied checks the operation left no effect after exhaustion.
	notApplied func(t *testing.T, e *writePathEnv, st *writePathState)
}

type writePathState struct {
	id      string
	version int
	session string
}

const touchIssueSQL = `UPDATE issues SET updated_at = now() WHERE id = $1`

// expectVersionConflict checks row 22a(b): the previous classification,
// *ErrVersionConflict carrying the stand's live version.
func expectVersionConflict(t *testing.T, err error, standVersion int) {
	t.Helper()
	var verr *ErrVersionConflict
	if !errors.As(err, &verr) {
		t.Fatalf("error = %v, want *ErrVersionConflict (the path's previous classification)", err)
	}
	if verr.CurrentVersion != standVersion {
		t.Errorf("ErrVersionConflict.CurrentVersion = %d, want %d — the stand's version, which nothing moved",
			verr.CurrentVersion, standVersion)
	}
}

// runWritePathAbsorbs is half (a).
func runWritePathAbsorbs(t *testing.T, c writePathCase) {
	ctx := context.Background()
	e := newWritePathEnv(t)
	st := c.setup(t, e)
	appName := "sk63413-a-" + uniqueTag()
	opStore := NewStore(newTaggedPool(t, appName))
	after := e.lastEventID(t)

	writer, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, touchIssueSQL, st.id); err != nil {
		t.Fatalf("writer touch: %v", err)
	}
	resCh := make(chan error, 1)
	go func() { resCh <- c.call(ctx, opStore, e, st) }()
	if !waitForBlocked(t, e.pool, appName, 10*time.Second) {
		t.Fatalf("the path never blocked on the writer's row lock — the race was not built")
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("writer commit: %v", err)
	}
	var callErr error
	select {
	case callErr = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("the path did not return within 30s")
	}
	if callErr != nil {
		t.Fatalf("the path surfaced %v after ONE concurrent writer — its retry (budget %d) did not absorb it",
			callErr, c.attempts)
	}
	c.applied(t, e, st)
	for _, ev := range c.events {
		if n := e.eventsSince(t, st.id, ev, after); n != 1 {
			t.Errorf("%s events written: %d, want exactly 1 (the rolled-back attempt left a duplicate or none)", ev, n)
		}
	}
	if c.recipient == nil {
		if n := e.notificationsSince(t, st.id, c.events, after, uuid.Nil); n != 0 {
			t.Errorf("a path without a fan-out wrote %d notifications", n)
		}
	} else if n := e.notificationsSince(t, st.id, c.events, after, c.recipient(e)); n != 1 {
		t.Errorf("recipient's notifications of the operation: %d, want exactly 1", n)
	}
}

// runWritePathExhausts is half (b).
func runWritePathExhausts(t *testing.T, c writePathCase) {
	ctx := context.Background()
	e := newWritePathEnv(t)
	st := c.setup(t, e)
	appName := "sk63413-b-" + uniqueTag()
	opStore := NewStore(newTaggedPool(t, appName))
	after := e.lastEventID(t)

	before := map[string]float64{}
	for _, origin := range []string{originSerializationDerived, originNoExpectedVersion, originGenuine} {
		if c.op != "" {
			before[origin] = testutil.ToFloat64(versionConflictOriginTotal.WithLabelValues(c.op, origin))
		}
	}

	standVersion := e.version(t, st.id)
	resCh := make(chan error, 1)
	start := func() { go func() { resCh <- c.call(ctx, opStore, e, st) }() }
	forceSerializationExhaustion(t, e.pool, appName, c.attempts, start, touchIssueSQL, st.id)

	var callErr error
	select {
	case callErr = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("the path did not return within 30s")
	}
	if callErr == nil {
		t.Fatalf("the path succeeded although every one of its %d attempts collided", c.attempts)
	}
	origin := c.exhausted(t, callErr, standVersion)
	if c.op != "" {
		for o, b := range before {
			want := 0.0
			if o == origin {
				want = 1
			}
			if got := testutil.ToFloat64(versionConflictOriginTotal.WithLabelValues(c.op, o)) - b; got != want {
				t.Errorf("versionConflictOriginTotal{op=%s,origin=%s} moved by %v, want %v", c.op, o, got, want)
			}
		}
	}
	for _, ev := range c.events {
		if n := e.eventsSince(t, st.id, ev, after); n != 0 {
			t.Errorf("%s events written by an exhausted path: %d, want 0", ev, n)
		}
	}
	if n := e.notificationsSince(t, st.id, c.events, after, uuid.Nil); n != 0 {
		t.Errorf("notifications written by an exhausted path: %d, want 0", n)
	}
	c.notApplied(t, e, st)
}

// ---------------------------------------------------------------- cases

func readIssueCol[T any](t *testing.T, e *writePathEnv, id, col string) T {
	t.Helper()
	var v T
	if err := e.pool.QueryRow(context.Background(), `SELECT `+col+` FROM issues WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read %s: %v", col, err)
	}
	return v
}

func watcherRecipient(e *writePathEnv) uuid.UUID { return e.watcher }
func creatorRecipient(e *writePathEnv) uuid.UUID { return e.creator }

func (e *writePathEnv) creatorActor() Actor { return HumanActor(e.creator, e.creatorName) }

func conflictWith(origin string) func(t *testing.T, err error, standVersion int) string {
	return func(t *testing.T, err error, standVersion int) string {
		t.Helper()
		expectVersionConflict(t, err, standVersion)
		return origin
	}
}

// --- PatchIssue: a priority edit (issue.updated; the watcher's «other
// edits» feed is switched on so the fan-out has a recipient).

var patchIssueCase = writePathCase{
	op: "patchIssue", attempts: patchIssueMaxAttempts,
	setup: func(t *testing.T, e *writePathEnv) *writePathState {
		d := e.newIssue(t, nil)
		e.enableReason(t, e.watcher, notifications.ReasonKeyOtherEdits)
		return &writePathState{id: d.ID, version: e.version(t, d.ID)}
	},
	call: func(ctx context.Context, s *Store, e *writePathEnv, st *writePathState) error {
		p := 1
		_, err := s.PatchIssue(ctx, st.id, PatchIssueInput{ExpectedVersion: st.version, Priority: &p}, e.creatorActor())
		return err
	},
	events:    []string{EventIssueUpdated},
	recipient: watcherRecipient,
	applied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if p := readIssueCol[int](t, e, st.id, "priority"); p != 1 {
			t.Errorf("priority = %d, want 1", p)
		}
	},
	exhausted: conflictWith(originSerializationDerived),
	notApplied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if p := readIssueCol[int](t, e, st.id, "priority"); p != 2 {
			t.Errorf("priority = %d after exhaustion, want 2", p)
		}
	},
}

func TestWritePathRetry_PatchIssue_Absorbs(t *testing.T)  { runWritePathAbsorbs(t, patchIssueCase) }
func TestWritePathRetry_PatchIssue_Exhausts(t *testing.T) { runWritePathExhausts(t, patchIssueCase) }

// --- PatchIssue changing the outcome of a closed issue
// (close_outcome.go#writeOutcomeChangedInTx): another member edits it, the
// author is the outcome_changed recipient.

var patchOutcomeCase = writePathCase{
	op: "patchIssue", attempts: patchIssueMaxAttempts,
	setup: func(t *testing.T, e *writePathEnv) *writePathState {
		d := e.newIssue(t, nil)
		if _, err := e.store.Close(context.Background(), CloseInput{
			ID: d.ID, ExpectedVersion: d.Version, Reason: "done", Actor: e.creatorActor(),
		}); err != nil {
			t.Fatalf("Close: %v", err)
		}
		return &writePathState{id: d.ID, version: e.version(t, d.ID)}
	},
	call: func(ctx context.Context, s *Store, e *writePathEnv, st *writePathState) error {
		o := CloseOutcomeWontDo
		_, err := s.PatchIssue(ctx, st.id, PatchIssueInput{ExpectedVersion: st.version, CloseOutcome: &o},
			HumanActor(e.other, e.otherName))
		return err
	},
	events:    []string{EventIssueOutcomeChanged},
	recipient: creatorRecipient,
	applied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if o := readIssueCol[string](t, e, st.id, "close_outcome"); o != CloseOutcomeWontDo {
			t.Errorf("close_outcome = %q, want %q", o, CloseOutcomeWontDo)
		}
	},
	exhausted: conflictWith(originSerializationDerived),
	notApplied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if o := readIssueCol[string](t, e, st.id, "close_outcome"); o != CloseOutcomeDone {
			t.Errorf("close_outcome = %q after exhaustion, want %q", o, CloseOutcomeDone)
		}
	},
}

func TestWritePathRetry_PatchOutcome_Absorbs(t *testing.T) { runWritePathAbsorbs(t, patchOutcomeCase) }
func TestWritePathRetry_PatchOutcome_Exhausts(t *testing.T) {
	runWritePathExhausts(t, patchOutcomeCase)
}

// --- UpdateStatus: open → in_progress.

var updateStatusCase = writePathCase{
	op: "updateStatus", attempts: updateStatusMaxAttempts,
	setup: func(t *testing.T, e *writePathEnv) *writePathState {
		d := e.newIssue(t, nil)
		return &writePathState{id: d.ID, version: e.version(t, d.ID)}
	},
	call: func(ctx context.Context, s *Store, e *writePathEnv, st *writePathState) error {
		_, err := s.UpdateStatus(ctx, st.id, UpdateStatusInput{ExpectedVersion: st.version, Status: StatusInProgress},
			e.creatorActor())
		return err
	},
	events:    []string{EventIssueStatusChanged},
	recipient: watcherRecipient,
	applied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if s := readIssueCol[string](t, e, st.id, "status"); s != StatusInProgress {
			t.Errorf("status = %q, want %q", s, StatusInProgress)
		}
	},
	exhausted: conflictWith(originSerializationDerived),
	notApplied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if s := readIssueCol[string](t, e, st.id, "status"); s != StatusOpen {
			t.Errorf("status = %q after exhaustion, want %q", s, StatusOpen)
		}
	},
}

func TestWritePathRetry_UpdateStatus_Absorbs(t *testing.T) { runWritePathAbsorbs(t, updateStatusCase) }
func TestWritePathRetry_UpdateStatus_Exhausts(t *testing.T) {
	runWritePathExhausts(t, updateStatusCase)
}

// --- Close.

var closeCase = writePathCase{
	op: "close", attempts: closeMaxAttempts,
	setup: func(t *testing.T, e *writePathEnv) *writePathState {
		d := e.newIssue(t, nil)
		return &writePathState{id: d.ID, version: e.version(t, d.ID)}
	},
	call: func(ctx context.Context, s *Store, e *writePathEnv, st *writePathState) error {
		_, err := s.Close(ctx, CloseInput{ID: st.id, ExpectedVersion: st.version, Reason: "done", Actor: e.creatorActor()})
		return err
	},
	events:    []string{EventIssueStatusChanged, EventIssueClosed},
	recipient: watcherRecipient,
	applied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if s := readIssueCol[string](t, e, st.id, "status"); s != StatusClosed {
			t.Errorf("status = %q, want %q", s, StatusClosed)
		}
	},
	exhausted: conflictWith(originSerializationDerived),
	notApplied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if s := readIssueCol[string](t, e, st.id, "status"); s != StatusOpen {
			t.Errorf("status = %q after exhaustion, want %q", s, StatusOpen)
		}
	},
}

func TestWritePathRetry_Close_Absorbs(t *testing.T)  { runWritePathAbsorbs(t, closeCase) }
func TestWritePathRetry_Close_Exhausts(t *testing.T) { runWritePathExhausts(t, closeCase) }

// --- AddLabel / RemoveLabel: no fan-out.

func labelCount(t *testing.T, e *writePathEnv, id, label string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_labels WHERE issue_id = $1 AND label = $2`, id, label).Scan(&n); err != nil {
		t.Fatalf("count labels: %v", err)
	}
	return n
}

var addLabelCase = writePathCase{
	op: "addLabel", attempts: addLabelMaxAttempts,
	setup: func(t *testing.T, e *writePathEnv) *writePathState {
		d := e.newIssue(t, nil)
		return &writePathState{id: d.ID}
	},
	call: func(ctx context.Context, s *Store, e *writePathEnv, st *writePathState) error {
		_, err := s.AddLabel(ctx, st.id, "urgent", e.creatorActor(), LockOpts{})
		return err
	},
	events: []string{EventLabelAdded},
	applied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if n := labelCount(t, e, st.id, "urgent"); n != 1 {
			t.Errorf("label rows = %d, want 1", n)
		}
	},
	exhausted: conflictWith(originNoExpectedVersion),
	notApplied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if n := labelCount(t, e, st.id, "urgent"); n != 0 {
			t.Errorf("label rows = %d after exhaustion, want 0", n)
		}
	},
}

func TestWritePathRetry_AddLabel_Absorbs(t *testing.T)  { runWritePathAbsorbs(t, addLabelCase) }
func TestWritePathRetry_AddLabel_Exhausts(t *testing.T) { runWritePathExhausts(t, addLabelCase) }

var removeLabelCase = writePathCase{
	op: "removeLabel", attempts: removeLabelMaxAttempts,
	setup: func(t *testing.T, e *writePathEnv) *writePathState {
		d := e.newIssue(t, nil)
		if _, err := e.store.AddLabel(context.Background(), d.ID, "urgent", e.creatorActor(), LockOpts{}); err != nil {
			t.Fatalf("seed label: %v", err)
		}
		return &writePathState{id: d.ID}
	},
	call: func(ctx context.Context, s *Store, e *writePathEnv, st *writePathState) error {
		_, err := s.RemoveLabel(ctx, st.id, "urgent", e.creatorActor(), LockOpts{})
		return err
	},
	events: []string{EventLabelRemoved},
	applied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if n := labelCount(t, e, st.id, "urgent"); n != 0 {
			t.Errorf("label rows = %d, want 0", n)
		}
	},
	exhausted: conflictWith(originNoExpectedVersion),
	notApplied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if n := labelCount(t, e, st.id, "urgent"); n != 1 {
			t.Errorf("label rows = %d after exhaustion, want 1", n)
		}
	},
}

func TestWritePathRetry_RemoveLabel_Absorbs(t *testing.T)  { runWritePathAbsorbs(t, removeLabelCase) }
func TestWritePathRetry_RemoveLabel_Exhausts(t *testing.T) { runWritePathExhausts(t, removeLabelCase) }

// --- Accept: the assignee (the second member) accepts with a declared
// version; the watcher gets the «accepted» feed.

var acceptCase = writePathCase{
	op: "accept", attempts: acceptMaxAttempts,
	setup: func(t *testing.T, e *writePathEnv) *writePathState {
		d := e.newIssue(t, &e.otherName)
		return &writePathState{id: d.ID, version: e.version(t, d.ID)}
	},
	call: func(ctx context.Context, s *Store, e *writePathEnv, st *writePathState) error {
		v := st.version
		_, err := s.Accept(ctx, AcceptInput{ID: st.id, ExpectedVersion: &v, Actor: HumanActor(e.other, e.otherName)})
		return err
	},
	events:    []string{EventIssueAccepted},
	recipient: watcherRecipient,
	applied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if at := readIssueCol[*time.Time](t, e, st.id, "accepted_at"); at == nil {
			t.Errorf("accepted_at is NULL after a successful accept")
		}
	},
	exhausted: conflictWith(originSerializationDerived),
	notApplied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if at := readIssueCol[*time.Time](t, e, st.id, "accepted_at"); at != nil {
			t.Errorf("accepted_at = %v after exhaustion, want NULL", *at)
		}
	},
}

func TestWritePathRetry_Accept_Absorbs(t *testing.T)  { runWritePathAbsorbs(t, acceptCase) }
func TestWritePathRetry_Accept_Exhausts(t *testing.T) { runWritePathExhausts(t, acceptCase) }

// --- Unclaim: the lock is the caller session's; nextStatus = open differs
// from in_progress, so the status change (and its fan-out) happens.

var unclaimCase = writePathCase{
	attempts: unclaimMaxAttempts,
	setup: func(t *testing.T, e *writePathEnv) *writePathState {
		d := e.newIssue(t, nil)
		session := "sess-" + uniqueTag()
		if _, err := e.store.Claim(context.Background(), ClaimInput{ID: d.ID, SessionID: session, Actor: e.creatorActor()}); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		return &writePathState{id: d.ID, session: session}
	},
	call: func(ctx context.Context, s *Store, e *writePathEnv, st *writePathState) error {
		_, err := s.Unclaim(ctx, UnclaimInput{ID: st.id, SessionID: st.session, NextStatus: StatusOpen,
			Reason: "handing back", Actor: e.creatorActor()})
		return err
	},
	events:    []string{EventIssueUnclaimed, EventIssueStatusChanged},
	recipient: watcherRecipient,
	applied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if l := readIssueCol[*string](t, e, st.id, "locked_by"); l != nil {
			t.Errorf("locked_by = %q, want NULL", *l)
		}
		if s := readIssueCol[string](t, e, st.id, "status"); s != StatusOpen {
			t.Errorf("status = %q, want %q", s, StatusOpen)
		}
	},
	exhausted: func(t *testing.T, err error, _ int) string {
		t.Helper()
		if !errors.Is(err, ErrNotLockHolder) {
			t.Fatalf("error = %v, want ErrNotLockHolder (Unclaim's previous classification)", err)
		}
		return ""
	},
	notApplied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if l := readIssueCol[*string](t, e, st.id, "locked_by"); l == nil || *l != st.session {
			t.Errorf("lock after exhaustion = %v, want the caller's session %q", l, st.session)
		}
	},
}

func TestWritePathRetry_Unclaim_Absorbs(t *testing.T)  { runWritePathAbsorbs(t, unclaimCase) }
func TestWritePathRetry_Unclaim_Exhausts(t *testing.T) { runWritePathExhausts(t, unclaimCase) }

// --- DismissDelegated: the creator declines; the subscription is stopped
// with an opt-out and the creator's Watching card of the issue is cleared
// (T011). Audit-only event, no fan-out.

func (e *writePathEnv) seedCreatorCard(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	if err := notifications.NewStore(e.pool).AddIssueWatch(ctx, id, e.creator, false); err != nil {
		t.Fatalf("creator watch: %v", err)
	}
	var eid int64
	if err := e.pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_type, payload)
VALUES (now(), 'issue.status_changed', 'issue', $1, $2, 'tester', 'human', '{}'::jsonb)
RETURNING id`, id, e.pid).Scan(&eid); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `
INSERT INTO notifications (user_id, event_id, type, project_id, reason, feed_channel, card_issue_id, card_resolved)
VALUES ($1, $2, 'issue.status_changed', $3, 'delegated_status', true, $4, true)`,
		e.creator, eid, e.pid, id); err != nil {
		t.Fatalf("seed card: %v", err)
	}
}

func (e *writePathEnv) creatorOptOuts(t *testing.T, id string) (optouts, cards int) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), `
SELECT (SELECT count(*) FROM issue_watch_optouts WHERE issue_id = $1 AND user_id = $2),
       (SELECT count(*) FROM notifications WHERE user_id = $2 AND card_issue_id = $1)`,
		id, e.creator).Scan(&optouts, &cards); err != nil {
		t.Fatalf("read opt-outs/cards: %v", err)
	}
	return optouts, cards
}

var dismissDelegatedCase = writePathCase{
	op: "dismissDelegated", attempts: dismissDelegatedMaxAttempts,
	setup: func(t *testing.T, e *writePathEnv) *writePathState {
		d := e.newIssue(t, nil)
		e.seedCreatorCard(t, d.ID)
		return &writePathState{id: d.ID}
	},
	call: func(ctx context.Context, s *Store, e *writePathEnv, st *writePathState) error {
		_, err := s.DismissDelegated(ctx, DelegationActionInput{ID: st.id, Actor: e.creatorActor()})
		return err
	},
	events: []string{EventIssueDelegationDropped},
	applied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if optouts, cards := e.creatorOptOuts(t, st.id); optouts != 1 || cards != 0 {
			t.Errorf("after the decline: opt-outs = %d (want 1), cards = %d (want 0)", optouts, cards)
		}
	},
	exhausted: conflictWith(originNoExpectedVersion),
	notApplied: func(t *testing.T, e *writePathEnv, st *writePathState) {
		if optouts, cards := e.creatorOptOuts(t, st.id); optouts != 0 || cards != 1 {
			t.Errorf("after exhaustion: opt-outs = %d (want 0), cards = %d (want 1)", optouts, cards)
		}
	},
}

func TestWritePathRetry_DismissDelegated_Absorbs(t *testing.T) {
	runWritePathAbsorbs(t, dismissDelegatedCase)
}
func TestWritePathRetry_DismissDelegated_Exhausts(t *testing.T) {
	runWritePathExhausts(t, dismissDelegatedCase)
}
