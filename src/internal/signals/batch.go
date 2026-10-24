package signals

// batch.go — sk-622.21 T014 (decision K9, INV-12): the skein-api side of the
// personal signal. Every write path names the users whose header counters or
// personal screens it may have changed, and the areas; the names are
// collected in a per-ATTEMPT collector that travels with the transaction and
// is published only after a successful COMMIT.
//
// Why the collector rides on the transaction rather than on context.Context:
// the write paths run inside closures `func(tx pgx.Tx) error` that capture
// the caller's ctx, so a retry wrapper has no way to hand a fresh per-attempt
// collector to the closure through ctx. The tx, on the other hand, is created
// anew for every attempt and passed to the closure by the wrapper itself —
// so a collector attached to it is reset on every retry by construction, and
// a rolled-back attempt's collector is simply dropped.

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// areaSet is a bit set over the seven [Area] values, in canonical order.
type areaSet uint8

func areaBit(a Area) areaSet {
	for i, v := range Areas() {
		if v == a {
			return 1 << i
		}
	}
	return 0
}

func (s areaSet) areas() []Area {
	out := make([]Area, 0, len(Areas()))
	for i, v := range Areas() {
		if s&(1<<i) != 0 {
			out = append(out, v)
		}
	}
	return out
}

// Batch collects the personal-signal addressees of ONE transaction attempt:
// for every user, the union of the areas the attempt named for them.
//
// A Batch is in-memory only: adding to it and publishing it never touch the
// database, so a path's statement count does not depend on how many
// addressees it names. The zero value is not usable; use [NewBatch].
type Batch struct {
	mu    sync.Mutex
	users map[uuid.UUID]areaSet
	// spaces — the space-level events of the attempt per user, in order
	// (the payload of the `spaces` area, K-18).
	spaces map[uuid.UUID][]SpaceEvent
	// access — the access changes of the attempt per user (the
	// access_changed control event, K-15).
	access map[uuid.UUID]*accessDelta
}

// accessDelta is one user's net access change in a batch: per project, the
// last call wins (lost after gained in the same attempt is lost), and the
// spaces the user is no longer in.
type accessDelta struct {
	projects   map[uuid.UUID]bool // true = gained, false = lost
	lostSpaces map[string]struct{}
}

// NewBatch returns an empty collector. Write paths that run in a transaction
// do not call it: [BeginTxFunc] creates one per attempt. It exists for the
// rare path that changes data in autocommit statements and publishes with
// [Flush] after the statement succeeded.
func NewBatch() *Batch {
	return &Batch{
		users:  make(map[uuid.UUID]areaSet),
		spaces: make(map[uuid.UUID][]SpaceEvent),
		access: make(map[uuid.UUID]*accessDelta),
	}
}

// AddSpaceEvent names user uid as an addressee of the `spaces` area with
// the event ev (K-18): the user's signal of this transaction carries ev in
// its `spaces` list, after the events added before it. An event with an
// invalid reason or without a space id is a programming error: logged and
// ignored.
func (b *Batch) AddSpaceEvent(uid uuid.UUID, ev SpaceEvent) {
	if uid == uuid.Nil {
		return
	}
	if !ev.Reason.Valid() || ev.SpaceID == "" {
		slog.Error("signals: space event outside the contract ignored",
			"reason", string(ev.Reason), "userId", uid.String())
		return
	}
	b.Add(uid, AreaSpaces)
	b.mu.Lock()
	b.spaces[uid] = append(b.spaces[uid], ev)
	b.mu.Unlock()
}

// AddAccessChange records that user uid lost the projects lost, gained the
// projects gained and is no longer in the spaces lostSpaces (short ids):
// after commit the user gets ONE access_changed event with the net sets of
// the transaction (K-15). Within a transaction the last change of a project
// wins.
func (b *Batch) AddAccessChange(uid uuid.UUID, lost, gained []uuid.UUID, lostSpaces []string) {
	if uid == uuid.Nil || len(lost)+len(gained)+len(lostSpaces) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	d := b.access[uid]
	if d == nil {
		d = &accessDelta{projects: make(map[uuid.UUID]bool), lostSpaces: make(map[string]struct{})}
		b.access[uid] = d
	}
	for _, id := range lost {
		d.projects[id] = false
	}
	for _, id := range gained {
		d.projects[id] = true
	}
	for _, s := range lostSpaces {
		d.lostSpaces[s] = struct{}{}
	}
}

// Add names user uid as an addressee for areas. Repeated calls for the same
// user merge: the user still gets ONE signal per committed transaction,
// carrying the union of the areas. A nil uid and areas outside the contract
// set are ignored (the latter is a programming error and is logged).
func (b *Batch) Add(uid uuid.UUID, areas ...Area) {
	if uid == uuid.Nil || len(areas) == 0 {
		return
	}
	var set areaSet
	for _, a := range areas {
		bit := areaBit(a)
		if bit == 0 {
			slog.Error("signals: area outside the contract set ignored", "area", string(a), "userId", uid.String())
			continue
		}
		set |= bit
	}
	if set == 0 {
		return
	}
	b.mu.Lock()
	b.users[uid] |= set
	b.mu.Unlock()
}

// merge adds every addressee of o into b.
func (b *Batch) merge(o *Batch) {
	o.mu.Lock()
	snapshot := make(map[uuid.UUID]areaSet, len(o.users))
	for uid, set := range o.users {
		snapshot[uid] = set
	}

	spaces := make(map[uuid.UUID][]SpaceEvent, len(o.spaces))
	for uid, evs := range o.spaces {
		spaces[uid] = slices.Clone(evs)
	}
	access := make(map[uuid.UUID]accessDelta, len(o.access))
	for uid, d := range o.access {
		access[uid] = accessDelta{projects: maps.Clone(d.projects), lostSpaces: maps.Clone(d.lostSpaces)}
	}
	o.mu.Unlock()

	b.mu.Lock()
	for uid, set := range snapshot {
		b.users[uid] |= set
	}
	for uid, evs := range spaces {
		b.spaces[uid] = append(b.spaces[uid], evs...)
	}
	b.mu.Unlock()
	for uid, d := range access {
		var lost, gained []uuid.UUID
		for id, g := range d.projects {
			if g {
				gained = append(gained, id)
			} else {
				lost = append(lost, id)
			}
		}
		b.AddAccessChange(uid, lost, gained, slices.Collect(maps.Keys(d.lostSpaces)))
	}
}

// userAreas is one user's share of a batch, ready to publish.
type userAreas struct {
	userID uuid.UUID
	areas  []Area
	spaces []SpaceEvent
}

// userAccess is one user's access change, ready to publish.
type userAccess struct {
	userID uuid.UUID
	body   AccessChanged
}

// entries returns the batch content ordered by user id (deterministic
// publish order), each user's areas in canonical order.
func (b *Batch) entries() []userAreas {
	b.mu.Lock()
	out := make([]userAreas, 0, len(b.users))
	for uid, set := range b.users {
		out = append(out, userAreas{userID: uid, areas: set.areas(), spaces: slices.Clone(b.spaces[uid])})
	}
	b.mu.Unlock()
	slices.SortFunc(out, func(x, y userAreas) int { return compareUUID(x.userID, y.userID) })
	return out
}

// accessEntries returns the batch's access changes ordered by user id, each
// set sorted, the three arrays never nil (the contract: `[]` when empty).
func (b *Batch) accessEntries() []userAccess {
	b.mu.Lock()
	out := make([]userAccess, 0, len(b.access))
	for uid, d := range b.access {
		body := AccessChanged{LostProjectIDs: []uuid.UUID{}, GainedProjectIDs: []uuid.UUID{}, LostSpaceIDs: []string{}}
		for id, gained := range d.projects {
			if gained {
				body.GainedProjectIDs = append(body.GainedProjectIDs, id)
			} else {
				body.LostProjectIDs = append(body.LostProjectIDs, id)
			}
		}
		for s := range d.lostSpaces {
			body.LostSpaceIDs = append(body.LostSpaceIDs, s)
		}
		slices.SortFunc(body.LostProjectIDs, compareUUID)
		slices.SortFunc(body.GainedProjectIDs, compareUUID)
		slices.Sort(body.LostSpaceIDs)
		out = append(out, userAccess{userID: uid, body: body})
	}
	b.mu.Unlock()
	slices.SortFunc(out, func(x, y userAccess) int { return compareUUID(x.userID, y.userID) })
	return out
}

func compareUUID(x, y uuid.UUID) int {
	for i := range x {
		if x[i] != y[i] {
			if x[i] < y[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// collectingTx is a transaction attempt that carries the attempt's collector.
// Every pgx.Tx method is the embedded transaction's, except Begin (a nested
// pseudo-transaction gets its own collector, see savepointTx).
type collectingTx struct {
	pgx.Tx
	batch *Batch
}

// Begin opens a savepoint whose addressees reach the enclosing collector only
// if the savepoint is released: a rolled-back savepoint's changes are not
// committed, so neither are its signals.
func (t *collectingTx) Begin(ctx context.Context) (pgx.Tx, error) {
	nested, err := t.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &savepointTx{collectingTx: collectingTx{Tx: nested, batch: NewBatch()}, parent: t.batch}, nil
}

// savepointTx is a pseudo-nested transaction (SAVEPOINT) of a collecting
// transaction.
type savepointTx struct {
	collectingTx
	parent *Batch
	merged bool
}

// Commit releases the savepoint and, on success, hands its addressees to the
// enclosing collector — once: the space events are a list, so the repeated
// Commit pgx allows must not hand them over twice.
func (t *savepointTx) Commit(ctx context.Context) error {
	if err := t.Tx.Commit(ctx); err != nil {
		return err
	}
	if !t.merged {
		t.parent.merge(t.batch)
		t.merged = true
	}
	return nil
}

// Add names user uid as an addressee of the personal signal for areas, in the
// transaction attempt tx. The signal is published only if this attempt
// commits (INV-12); a retried or rolled-back attempt publishes nothing of
// what it added.
//
// tx must be a transaction opened by [BeginTxFunc] (or a savepoint of one).
// Any other transaction cannot carry the collector: the call is a
// programming error — a write path that was not moved onto [BeginTxFunc] —
// and is logged at error level, never silently dropped.
func Add(tx pgx.Tx, uid uuid.UUID, areas ...Area) {
	switch t := tx.(type) {
	case *collectingTx:
		t.batch.Add(uid, areas...)
	case *savepointTx:
		t.batch.Add(uid, areas...)
	default:
		slog.Error("signals: transaction does not collect personal signals; open it with signals.BeginTxFunc",
			"userId", uid.String())
	}
}

// AddSpaceEvent is [Batch.AddSpaceEvent] in the transaction attempt tx:
// published only if this attempt commits.
func AddSpaceEvent(tx pgx.Tx, uid uuid.UUID, ev SpaceEvent) {
	if b := batchOf(tx, uid); b != nil {
		b.AddSpaceEvent(uid, ev)
	}
}

// AddAccessChange is [Batch.AddAccessChange] in the transaction attempt tx:
// the access_changed event is published only if this attempt commits
// (K-15: «only after the final commit of the change»).
func AddAccessChange(tx pgx.Tx, uid uuid.UUID, lost, gained []uuid.UUID, lostSpaces []string) {
	if b := batchOf(tx, uid); b != nil {
		b.AddAccessChange(uid, lost, gained, lostSpaces)
	}
}

// batchOf is the collector tx carries; nil (logged) for a transaction not
// opened by BeginTxFunc.
func batchOf(tx pgx.Tx, uid uuid.UUID) *Batch {
	switch t := tx.(type) {
	case *collectingTx:
		return t.batch
	case *savepointTx:
		return t.batch
	default:
		slog.Error("signals: transaction does not collect personal signals; open it with signals.BeginTxFunc",
			"userId", uid.String())
		return nil
	}
}

// TxBeginner is the part of *pgxpool.Pool (and *pgx.Conn) that BeginTxFunc
// needs.
type TxBeginner interface {
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
}

// BeginTxFunc is pgx.BeginTxFunc with a personal-signal collector: it opens a
// transaction on db with opts, runs fn with a tx that collects the
// addressees fn names through [Add], and — only when the transaction
// committed — publishes one signal per addressee with the union of its areas
// through the process publisher (see [Configure]).
//
// Each call is ONE attempt with a fresh collector, so a retry wrapper that
// calls BeginTxFunc once per attempt resets the collector on every retry:
// an aborted attempt's addressees are dropped together with its tx. The
// returned error is exactly pgx.BeginTxFunc's; publishing never fails the
// call (delivery is best-effort, decision D6).
func BeginTxFunc(ctx context.Context, db TxBeginner, opts pgx.TxOptions, fn func(pgx.Tx) error) error {
	batch := NewBatch()
	err := pgx.BeginTxFunc(ctx, db, opts, func(tx pgx.Tx) error {
		return fn(&collectingTx{Tx: tx, batch: batch})
	})
	if err != nil {
		return err
	}
	Flush(ctx, batch)
	return nil
}
