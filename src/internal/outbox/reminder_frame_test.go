package outbox

// reminder_frame_test.go — sk-622.21, test matrix row 37 (T018; US-11
// "Reminders", K14, INV-12).
//
// Subject: the outbox dispatcher publishing `reminder.*` events, and the
// personal reminder frame it sends on `skein.user.{owner}.reminder`, read on
// the wire as skein-web reads it. The events are written by the real
// reminder paths (reminders.Store, the scheduler's ring); the test reads the
// FRAME, never the event payload.
//
// Reference: spec US-11 — reminders (set, snoozed, rang, cancelled) reach the
// reminder's owner as a personal frame; INV-12 — only after the change is
// committed; plan K14 — at-least-once, firingId = reminderId@remindAt, so a
// redelivery of the same event repeats the same firingId. The expected
// firingId is composed here from the reminder id and the due time the test
// chose (whole seconds), not by calling the production helper.
//
// Absence is judged by a SENTINEL: a frame of another owner published on the
// same connection after the action; once it is in, every frame published
// before it on that connection is in too.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"skein-api/internal/reminders"
	"skein-api/internal/signals"
	"skein-api/internal/testsupport"
)

type gotFrame struct {
	owner uuid.UUID
	body  []byte
	frame struct {
		ReminderID string `json:"reminderId"`
		IssueID    string `json:"issueId"`
		Kind       string `json:"kind"`
		RemindAt   string `json:"remindAt"`
		FiringID   string `json:"firingId"`
	}
}

type frameSink struct {
	mu     sync.Mutex
	frames []gotFrame
	notify chan struct{}
}

// subscribeFrames listens on every personal reminder subject on its own
// connection to srvURL.
func subscribeFrames(t *testing.T, srvURL string) *frameSink {
	t.Helper()
	nc, err := nats.Connect(srvURL, nats.Name("reminder-frame-test-sub"))
	if err != nil {
		t.Fatalf("connect subscriber: %v", err)
	}
	t.Cleanup(nc.Close)
	s := &frameSink{notify: make(chan struct{}, 1)}
	if _, err := nc.Subscribe("skein.user.*.reminder", func(m *nats.Msg) {
		owner, err := uuid.Parse(strings.TrimSuffix(strings.TrimPrefix(m.Subject, "skein.user."), ".reminder"))
		if err != nil {
			t.Errorf("subject %q carries no owner: %v", m.Subject, err)
			return
		}
		f := gotFrame{owner: owner, body: append([]byte(nil), m.Data...)}
		if err := json.Unmarshal(m.Data, &f.frame); err != nil {
			t.Errorf("frame body %s: %v", m.Data, err)
			return
		}
		s.mu.Lock()
		s.frames = append(s.frames, f)
		s.mu.Unlock()
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}
	return s
}

// waitOwner blocks until a frame for owner arrived, then returns every frame
// received so far.
func (s *frameSink) waitOwner(t *testing.T, owner uuid.UUID) []gotFrame {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		s.mu.Lock()
		for _, f := range s.frames {
			if f.owner == owner {
				out := append([]gotFrame(nil), s.frames...)
				s.mu.Unlock()
				return out
			}
		}
		s.mu.Unlock()
		select {
		case <-s.notify:
		case <-deadline:
			t.Fatalf("no reminder frame for %s arrived", owner)
		}
	}
}

func framesOf(all []gotFrame, owner uuid.UUID) []gotFrame {
	var out []gotFrame
	for _, f := range all {
		if f.owner == owner {
			out = append(out, f)
		}
	}
	return out
}

// reminderWorld: owner and other each keep a reminder on one issue.
type reminderWorld struct {
	pid    uuid.UUID
	issue  string
	owner  uuid.UUID
	other  uuid.UUID
	store  *reminders.Store
	actorO reminders.Actor
	actorX reminders.Actor
}

func newReminderWorld(t *testing.T) reminderWorld {
	t.Helper()
	pool := newTestPool(t)
	owner, ownerName := testsupport.SeedUser(t, pool, "")
	other, otherName := testsupport.SeedUser(t, pool, "")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "obrem")
	// The project's events — and with them their notifications, which owe
	// pushes to the shared push queue — go with the fixture, so they do not
	// sit in the queue ahead of other packages' rows.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE project_id = $1`, pid)
	})
	testsupport.AddProjectMember(t, pool, pid, other, "member")
	issue := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1", "reminded", "open", 2)
	resetOutbox(t, pool, pid)
	return reminderWorld{
		pid: pid, issue: issue, owner: owner, other: other,
		store:  reminders.NewStore(pool),
		actorO: reminders.Actor{UserID: owner, Username: ownerName, Role: "human"},
		actorX: reminders.Actor{UserID: other, Username: otherName, Role: "human"},
	}
}

// firing composes the expected firingId by the contract's words: reminder id,
// "@", the due time in UTC as RFC 3339 with the fractional seconds it has.
func firing(id uuid.UUID, at time.Time) string {
	return id.String() + "@" + at.UTC().Format(time.RFC3339Nano)
}

// TestReminderFrame_LifecycleReachesOnlyTheOwner — set, snoozed, rang and
// dismissed each give the reminder's owner one frame with that kind and the
// firingId of the due time; the other user's reminder frames never reach
// the owner, and the owner's never reach the other user.
func TestReminderFrame_LifecycleReachesOnlyTheOwner(t *testing.T) {
	pool := newTestPool(t)
	srv, nc := startEmbeddedNATS(t)
	w := newReminderWorld(t)
	sink := subscribeFrames(t, srv.ClientURL())
	d := New(pool, nc, Options{BatchSize: 100})
	ctx := context.Background()

	t1 := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	t2 := t1.Add(time.Hour)

	mine, err := w.store.Create(ctx, w.issue, w.owner, t1, "", w.actorO)
	if err != nil {
		t.Fatalf("Create (owner): %v", err)
	}
	theirs, err := w.store.Create(ctx, w.issue, w.other, t1, "", w.actorX)
	if err != nil {
		t.Fatalf("Create (other): %v", err)
	}
	if _, err := w.store.Snooze(ctx, mine.ID, w.owner, t2, "later", w.actorO); err != nil {
		t.Fatalf("Snooze: %v", err)
	}
	// Due now: the scheduler rings it.
	past := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	if _, err := pool.Exec(ctx, `UPDATE reminders SET remind_at = $2 WHERE id = $1`, mine.ID, past); err != nil {
		t.Fatalf("make reminder due: %v", err)
	}
	if _, err := w.store.ClaimAndNotifyDue(ctx, 1000, reminders.Actor{Username: "system", Role: "system"}); err != nil {
		t.Fatalf("ClaimAndNotifyDue: %v", err)
	}
	if err := w.store.Dismiss(ctx, mine.ID, w.owner, "", w.actorO); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}

	drainProject(t, d, pool, w.pid)
	sink.waitOwner(t, w.other)
	all := sink.waitOwner(t, w.owner)
	// The owner's dismissed frame is the last event of the project; wait
	// until it is in so the set is complete.
	deadline := time.Now().Add(10 * time.Second)
	for len(framesOf(all, w.owner)) < 4 && time.Now().Before(deadline) {
		select {
		case <-sink.notify:
		case <-time.After(100 * time.Millisecond):
		}
		sink.mu.Lock()
		all = append([]gotFrame(nil), sink.frames...)
		sink.mu.Unlock()
	}

	wantOwner := []struct{ kind, firingID string }{
		{"set", firing(mine.ID, t1)},
		{"snoozed", firing(mine.ID, t2)},
		{"rang", firing(mine.ID, past)},
		{"dismissed", firing(mine.ID, past)},
	}
	gotOwner := framesOf(all, w.owner)
	if len(gotOwner) != len(wantOwner) {
		t.Fatalf("owner got %d frames, want %d: %+v", len(gotOwner), len(wantOwner), gotOwner)
	}
	for i, want := range wantOwner {
		f := gotOwner[i].frame
		if f.Kind != want.kind || f.FiringID != want.firingID || f.ReminderID != mine.ID.String() || f.IssueID != w.issue {
			t.Errorf("owner frame %d = {kind %q, firingId %q, reminder %s, issue %s}, want {kind %q, firingId %q, reminder %s, issue %s}",
				i, f.Kind, f.FiringID, f.ReminderID, f.IssueID, want.kind, want.firingID, mine.ID, w.issue)
		}
	}
	gotOther := framesOf(all, w.other)
	if len(gotOther) != 1 || gotOther[0].frame.Kind != "set" || gotOther[0].frame.FiringID != firing(theirs.ID, t1) {
		t.Errorf("other user frames = %+v, want exactly their own «set» with firingId %s", gotOther, firing(theirs.ID, t1))
	}
}

// TestReminderFrame_OnlyAfterCommit — INV-12, K14: the reminder frame goes
// out only through the outbox, i.e. only after the reminder write committed
// and the dispatcher picked its event up. The real write path
// (reminders.Store.Create) runs while NO dispatcher ticks, with the process
// publisher of personal messages wired to the same connection the frames are
// read from — as main() wires it. A frame published by the write path itself
// (the rejected K14 alternative: straight from the store, before or around
// COMMIT) would therefore reach the sink ahead of the sentinel and turn this
// red. Only after the dispatcher drains the project does the frame arrive.
func TestReminderFrame_OnlyAfterCommit(t *testing.T) {
	pool := newTestPool(t)
	srv, nc := startEmbeddedNATS(t)
	w := newReminderWorld(t)
	sink := subscribeFrames(t, srv.ClientURL())
	signals.Configure(nc)
	t.Cleanup(func() { signals.Configure(nil) })
	d := New(pool, nc, Options{BatchSize: 100})
	ctx := context.Background()
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)

	r, err := w.store.Create(ctx, w.issue, w.owner, at, "", w.actorO)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Sentinel: a frame of a stranger published on the same connection
	// after the write returned. Once it is in, anything the write path
	// published on this connection is in too.
	stranger := uuid.New()
	if err := nc.Publish("skein.user."+stranger.String()+".reminder",
		[]byte(`{"reminderId":"`+uuid.NewString()+`","kind":"set"}`)); err != nil {
		t.Fatalf("publish sentinel: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush sentinel: %v", err)
	}
	all := sink.waitOwner(t, stranger)
	if got := framesOf(all, w.owner); len(got) != 0 {
		t.Fatalf("owner got %d frame(s) before the outbox delivered the event: %+v", len(got), got)
	}

	drainProject(t, d, pool, w.pid)
	all = sink.waitOwner(t, w.owner)
	got := framesOf(all, w.owner)
	if len(got) != 1 || got[0].frame.Kind != "set" || got[0].frame.FiringID != firing(r.ID, at) {
		t.Errorf("owner frames after the outbox tick = %+v, want one «set» with firingId %s", got, firing(r.ID, at))
	}
}

// TestReminderFrame_RedeliveryRepeatsFiringID — the dispatcher delivers the
// same reminder event twice (at-least-once); both frames carry the same
// firingId.
func TestReminderFrame_RedeliveryRepeatsFiringID(t *testing.T) {
	pool := newTestPool(t)
	srv, nc := startEmbeddedNATS(t)
	w := newReminderWorld(t)
	sink := subscribeFrames(t, srv.ClientURL())
	d := New(pool, nc, Options{BatchSize: 100})
	ctx := context.Background()
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)

	r, err := w.store.Create(ctx, w.issue, w.owner, at, "", w.actorO)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	drainProject(t, d, pool, w.pid)
	first := framesOf(sink.waitOwner(t, w.owner), w.owner)

	// A redelivery: the event goes back to the queue, as after a lost
	// stamp, and is published again.
	if _, err := pool.Exec(ctx, `
UPDATE events SET published_at = NULL
 WHERE project_id = $1 AND type = 'reminder.set' AND payload->>'reminderId' = $2`, w.pid, r.ID.String()); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	drainProject(t, d, pool, w.pid)
	deadline := time.Now().Add(10 * time.Second)
	var again []gotFrame
	for {
		sink.mu.Lock()
		again = framesOf(sink.frames, w.owner)
		sink.mu.Unlock()
		if len(again) >= 2 || time.Now().After(deadline) {
			break
		}
		select {
		case <-sink.notify:
		case <-time.After(100 * time.Millisecond):
		}
	}
	if len(first) != 1 || len(again) != 2 {
		t.Fatalf("frames: first delivery %d, after redelivery %d; want 1 and 2", len(first), len(again))
	}
	want := firing(r.ID, at)
	for i, f := range again {
		if f.frame.FiringID != want {
			t.Errorf("delivery %d firingId = %q, want %q (the same on every delivery)", i+1, f.frame.FiringID, want)
		}
	}
}

// TestReminderFrame_FiringIDKeepsStoredFraction — found in review:
// the contract's firingId is `{reminderId}@{remindAt}` with
// the fractional seconds the due time has. A due time with a sub-second part
// (given here with nanoseconds, stored by Postgres in microseconds) reaches
// the wire with the STORED value's fraction — on set and on snooze — not cut
// to whole seconds and not the caller's unstored nanoseconds.
func TestReminderFrame_FiringIDKeepsStoredFraction(t *testing.T) {
	pool := newTestPool(t)
	srv, nc := startEmbeddedNATS(t)
	w := newReminderWorld(t)
	sink := subscribeFrames(t, srv.ClientURL())
	d := New(pool, nc, Options{BatchSize: 100})
	ctx := context.Background()

	base := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	t1 := base.Add(123456789 * time.Nanosecond)
	t2 := base.Add(time.Hour + 250*time.Millisecond)

	r, err := w.store.Create(ctx, w.issue, w.owner, t1, "", w.actorO)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var storedSet time.Time
	if err := pool.QueryRow(ctx, `SELECT remind_at FROM reminders WHERE id = $1`, r.ID).Scan(&storedSet); err != nil {
		t.Fatalf("read stored remind_at: %v", err)
	}
	if _, err := w.store.Snooze(ctx, r.ID, w.owner, t2, "later", w.actorO); err != nil {
		t.Fatalf("Snooze: %v", err)
	}
	var storedSnooze time.Time
	if err := pool.QueryRow(ctx, `SELECT remind_at FROM reminders WHERE id = $1`, r.ID).Scan(&storedSnooze); err != nil {
		t.Fatalf("read stored remind_at after snooze: %v", err)
	}
	if storedSet.Nanosecond() == 0 || storedSnooze.Nanosecond() == 0 {
		t.Fatalf("precondition: stored due times %s / %s carry no fraction", storedSet, storedSnooze)
	}

	drainProject(t, d, pool, w.pid)
	deadline := time.Now().Add(10 * time.Second)
	var got []gotFrame
	for {
		sink.mu.Lock()
		got = framesOf(sink.frames, w.owner)
		sink.mu.Unlock()
		if len(got) >= 2 || time.Now().After(deadline) {
			break
		}
		select {
		case <-sink.notify:
		case <-time.After(100 * time.Millisecond):
		}
	}
	want := []struct{ kind, firingID string }{
		{"set", firing(r.ID, storedSet)},
		{"snoozed", firing(r.ID, storedSnooze)},
	}
	if len(got) != len(want) {
		t.Fatalf("owner got %d frames, want %d: %+v", len(got), len(want), got)
	}
	for i, wnt := range want {
		if got[i].frame.Kind != wnt.kind || got[i].frame.FiringID != wnt.firingID {
			t.Errorf("frame %d = {kind %q, firingId %q}, want {kind %q, firingId %q}",
				i, got[i].frame.Kind, got[i].frame.FiringID, wnt.kind, wnt.firingID)
		}
	}
}
