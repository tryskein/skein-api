package issues

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
)

// sk-421.1 T001 (US-14, INV-2, SC-003) — the blockers list of an issue
// detail used to carry the real title and status of every blocker, whether
// or not the reader could see the blocker's project. sk-421.1 redacted such
// an entry; sk-423 removed it from the list altogether (its presence said
// the hidden issue was not closed) and counts the edge in
// UnavailableBlockingLinks, whatever the hidden issue's status. The rule
// lives in the one place the card is assembled (hydrateIssueDetail →
// blockersFor), with the same horizon as the epic rollup on that card.
//
// Test-plan rows 1–3. The fixtures below are shared with
// blocked_consistency_test.go (rows 4–5).

// horizonFixture is a subject issue in project P plus two projects' worth
// of readers:
//
//   - pOwner      — member of P, not of Q;
//   - memberBoth  — member of P and of Q;
//   - externalP   — external collaborator, member of P, not of Q;
//   - admin       — internal org admin, member of neither.
type horizonFixture struct {
	pool  *pgxpool.Pool
	store *Store

	pID, qID         uuid.UUID
	pPrefix, qPrefix string
	pOwner, qOwner   uuid.UUID
	memberBoth       uuid.UUID
	externalP        uuid.UUID
	admin            uuid.UUID

	subject string
}

func newHorizonFixture(t *testing.T) *horizonFixture {
	t.Helper()
	pool := newTestPool(t)
	f := &horizonFixture{pool: pool, store: NewStore(pool)}

	f.pOwner = seedUser(t, pool)
	f.qOwner = seedUser(t, pool)
	f.pID, f.pPrefix = seedProject(t, pool, f.pOwner)
	f.qID, f.qPrefix = seedProject(t, pool, f.qOwner)

	f.memberBoth = seedUser(t, pool)
	addMember(t, pool, f.pID, f.memberBoth, f.pOwner)
	addMember(t, pool, f.qID, f.memberBoth, f.qOwner)

	f.externalP = seedUser(t, pool)
	// External in the data, not only in the context: the access relation
	// reads users.is_external (D-2), and since sk-674.18 T017 an external
	// collaborator is outside spaces (INV-1 v, E-16).
	if _, err := pool.Exec(context.Background(), `UPDATE users SET is_external = true WHERE id = $1`, f.externalP); err != nil {
		t.Fatalf("mark externalP external: %v", err)
	}
	addMember(t, pool, f.pID, f.externalP, f.pOwner)

	f.admin = seedAdminUser(t, pool)

	f.subject = f.pPrefix + "-1"
	seedIssue(t, pool, f.pID, f.pOwner, f.subject, "subject")
	return f
}

// blockerSeed describes one outgoing edge of the subject.
type blockerSeed struct {
	inQ     bool   // the target lives in Q (invisible to pOwner and externalP)
	n       int    // counter part of the target's hierarchical id
	status  string // target status
	depType string // blocks | waits-for | related | parent-child
}

// seededBlocker is what a seed produced — the values a reader who CAN see
// the target must get back.
type seededBlocker struct {
	blockerSeed
	id    string
	title string
}

// blocking reports whether the seed is a blocks / waits-for edge at all.
func (s seededBlocker) blocking() bool {
	return s.depType == "blocks" || s.depType == "waits-for"
}

// gates is the status half of the rule: a blocks / waits-for edge to a
// target that is neither closed nor standing.
func (s seededBlocker) gates() bool {
	return s.blocking() && s.status != StatusClosed && s.status != StatusStanding
}

// counts is THE rule of counting a blocker for one reader, written once for
// every expectation in rows 1–5 (sk-423): it gates AND the reader can see
// its project. When the rule changes, this function changes, and the
// consistency guard (row 5) keeps holding the list and the flag together.
func (s seededBlocker) counts(visible bool) bool {
	return s.gates() && visible
}

// hiddenLink is the companion rule (sk-423): a blocks / waits-for edge to a
// target the reader cannot see, WHATEVER its status — the edges
// UnavailableBlockingLinks counts.
func (s seededBlocker) hiddenLink(visible bool) bool {
	return s.blocking() && !visible
}

func (f *horizonFixture) seed(t *testing.T, seeds ...blockerSeed) []seededBlocker {
	t.Helper()
	out := make([]seededBlocker, 0, len(seeds))
	for _, s := range seeds {
		pid, owner, prefix, where := f.pID, f.pOwner, f.pPrefix, "p"
		if s.inQ {
			pid, owner, prefix, where = f.qID, f.qOwner, f.qPrefix, "q"
		}
		id := prefix + "-" + strconv.Itoa(s.n)
		title := where + " blocker title " + strconv.Itoa(s.n)
		seedIssue(t, f.pool, pid, owner, id, title)
		if s.status != StatusOpen {
			setIssueStatus(t, f.pool, id, s.status)
		}
		switch s.depType {
		case "waits-for":
			if _, err := f.pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'waits-for', '{"gate":"all-children"}'::jsonb, 1, $3, now())`,
				f.subject, id, f.pOwner); err != nil {
				t.Fatalf("seed waits-for %s -> %s: %v", f.subject, id, err)
			}
		case "parent-child":
			seedParentChild(t, f.pool, f.subject, id, f.pOwner)
		default:
			seedDep(t, f.pool, f.subject, id, s.depType, f.pOwner)
		}
		out = append(out, seededBlocker{blockerSeed: s, id: id, title: title})
	}
	return out
}

// reader names the four readers of SC-003 / SC-017.
type reader string

const (
	readerMemberP    reader = "member of P, not of Q"
	readerMemberBoth reader = "member of P and Q"
	readerExternalP  reader = "external member of P"
	readerAdmin      reader = "internal admin"
)

var allReaders = []reader{readerMemberP, readerMemberBoth, readerExternalP, readerAdmin}

func (f *horizonFixture) ctxFor(t *testing.T, r reader) context.Context {
	t.Helper()
	switch r {
	case readerMemberP:
		return callerCtx(t, f.pOwner, "user")
	case readerMemberBoth:
		return callerCtx(t, f.memberBoth, "user")
	case readerExternalP:
		return auth.WithIsExternal(callerCtx(t, f.externalP, "user"), true)
	case readerAdmin:
		return callerCtx(t, f.admin, auth.RoleAdmin)
	}
	t.Fatalf("unknown reader %q", r)
	return nil
}

// seesQ reports whether the reader's horizon includes project Q.
func seesQ(r reader) bool { return r == readerMemberBoth || r == readerAdmin }

func (f *horizonFixture) detail(t *testing.T, ctx context.Context) *IssueDetail {
	t.Helper()
	dto, err := f.store.GetIssue(ctx, f.subject)
	if err != nil {
		t.Fatalf("GetIssue(%s): %v", f.subject, err)
	}
	return dto
}

// wireKeys returns the JSON object an entry serialises to — the test looks
// at KEYS, because «absent» and «empty» are different contracts.
func wireKeys(t *testing.T, b BlockerRef) map[string]any {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal blocker: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal blocker %s: %v", raw, err)
	}
	return m
}

func sortedKeys(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// assertFullEntry: exactly {id, title, status} with the real values.
func assertFullEntry(t *testing.T, got BlockerRef, want seededBlocker) {
	t.Helper()
	m := wireKeys(t, got)
	if keys := sortedKeys(m); keys != "id,status,title" {
		t.Errorf("entry %s: wire keys = %s; want id,status,title (no redacted)", want.id, keys)
	}
	if m["id"] != want.id || m["title"] != want.title || m["status"] != want.status {
		t.Errorf("entry = %v; want {id:%s title:%q status:%s}", m, want.id, want.title, want.status)
	}
}

// expectedOrder is the list a reader must get: the blockers that reader
// counts (visible decides what the reader sees), by id.
func expectedOrder(seeded []seededBlocker, visible func(seededBlocker) bool) []string {
	ids := make([]string, 0, len(seeded))
	for _, s := range seeded {
		if s.counts(visible(s)) {
			ids = append(ids, s.id)
		}
	}
	sort.Strings(ids)
	return ids
}

// expectedHiddenLinks is the UnavailableBlockingLinks a reader must get.
func expectedHiddenLinks(seeded []seededBlocker, visible func(seededBlocker) bool) int {
	n := 0
	for _, s := range seeded {
		if s.hiddenLink(visible(s)) {
			n++
		}
	}
	return n
}

func entryIDs(bs []BlockerRef) []string {
	ids := make([]string, 0, len(bs))
	for _, b := range bs {
		ids = append(ids, b.ID)
	}
	return ids
}

func byID(seeded []seededBlocker) map[string]seededBlocker {
	m := make(map[string]seededBlocker, len(seeded))
	for _, s := range seeded {
		m[s.id] = s
	}
	return m
}

// checkEntries asserts what a reader must get: exactly the blockers that
// reader counts, each in the full form, in order; the flag; the number of
// hidden links; and no hidden title anywhere in the list.
func checkEntries(t *testing.T, dto *IssueDetail, seeded []seededBlocker, visible func(seededBlocker) bool) {
	t.Helper()
	checkEntriesCounting(t, dto, seeded, visible, expectedHiddenLinks(seeded, visible))
}

// hiddenFor is the number of hidden links reader r is told about: every
// hidden link for a participant of the space Q is in, none for the external
// collaborator — he is outside spaces and gets no unavailable* numbers
// (INV-28 (4), sk-674.18 T017).
func hiddenFor(r reader, seeded []seededBlocker, visible func(seededBlocker) bool) int {
	if r == readerExternalP {
		return 0
	}
	return expectedHiddenLinks(seeded, visible)
}

// checkEntriesCounting is checkEntries with the expected number of hidden
// links given explicitly — for a reader whose hidden links count nowhere
// (INV-28 (4): no reader, no own space).
func checkEntriesCounting(t *testing.T, dto *IssueDetail, seeded []seededBlocker, visible func(seededBlocker) bool, wantHidden int) {
	t.Helper()
	want := expectedOrder(seeded, visible)
	if got := entryIDs(dto.Blockers); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("blocker ids = %v; want %v (the blockers this reader counts, by id)", got, want)
	}
	if dto.IsBlocked != (len(want) > 0) {
		t.Errorf("IsBlocked = %v; want %v", dto.IsBlocked, len(want) > 0)
	}
	if dto.UnavailableBlockingLinks != wantHidden {
		t.Errorf("UnavailableBlockingLinks = %d; want %d", dto.UnavailableBlockingLinks, wantHidden)
	}
	seeds := byID(seeded)
	for _, b := range dto.Blockers {
		assertFullEntry(t, b, seeds[b.ID])
	}
	// Belt and braces over the key checks: no hidden title reaches the
	// serialised list at all.
	raw, err := json.Marshal(dto.Blockers)
	if err != nil {
		t.Fatalf("marshal blockers: %v", err)
	}
	for _, s := range seeded {
		if !visible(s) && (strings.Contains(string(raw), s.title) || strings.Contains(string(raw), s.id)) {
			t.Errorf("hidden blocker %s leaked its id or title into %s", s.id, raw)
		}
	}
}

// Row 1 — mixed blockers from P and Q, each reader.
func TestBlockersHorizon_MixedBlockersPerReader(t *testing.T) {
	f := newHorizonFixture(t)
	seeded := f.seed(t,
		blockerSeed{n: 2, status: StatusOpen, depType: "blocks"},
		blockerSeed{inQ: true, n: 3, status: StatusInProgress, depType: "blocks"},
		blockerSeed{n: 4, status: StatusDeferred, depType: "waits-for"},
		blockerSeed{inQ: true, n: 5, status: StatusOpen, depType: "waits-for"},
	)

	for _, r := range allReaders {
		t.Run(string(r), func(t *testing.T) {
			dto := f.detail(t, f.ctxFor(t, r))
			visible := func(s seededBlocker) bool { return !s.inQ || seesQ(r) }
			checkEntriesCounting(t, dto, seeded, visible, hiddenFor(r, seeded, visible))
		})
	}
}

// Row 1, variant «only Q»: for a reader outside Q nothing blocks — both
// edges are hidden links (sk-423); a reader who sees Q gets both blockers.
func TestBlockersHorizon_AllBlockersHidden(t *testing.T) {
	f := newHorizonFixture(t)
	seeded := f.seed(t,
		blockerSeed{inQ: true, n: 2, status: StatusOpen, depType: "blocks"},
		blockerSeed{inQ: true, n: 3, status: StatusDeferred, depType: "waits-for"},
	)

	for _, r := range allReaders {
		t.Run(string(r), func(t *testing.T) {
			dto := f.detail(t, f.ctxFor(t, r))
			if dto.IsBlocked != seesQ(r) {
				t.Errorf("IsBlocked = %v; want %v — only blockers the reader can see count", dto.IsBlocked, seesQ(r))
			}
			visible := func(seededBlocker) bool { return seesQ(r) }
			checkEntriesCounting(t, dto, seeded, visible, hiddenFor(r, seeded, visible))
		})
	}
}

// Row 2 — defensive rule: a synthetic principal with role=admin AND
// is_external (impossible in the data, see users_external_not_admin) gets
// the external ceiling, not the admin horizon.
func TestBlockersHorizon_ExternalAdminCeiling(t *testing.T) {
	f := newHorizonFixture(t)
	seeded := f.seed(t,
		blockerSeed{n: 2, status: StatusOpen, depType: "blocks"},
		blockerSeed{inQ: true, n: 3, status: StatusOpen, depType: "blocks"},
	)

	ctx := auth.WithIsExternal(callerCtx(t, f.externalP, auth.RoleAdmin), true)
	dto := f.detail(t, ctx)
	visible := func(s seededBlocker) bool { return !s.inQ }
	checkEntriesCounting(t, dto, seeded, visible, hiddenFor(readerExternalP, seeded, visible))
}

// Row 3 — the default is fail-closed: an undeclared context sees nothing
// (no blocker counts, P's included), a context declared server-internal
// sees everything. One test on purpose: both halves guard one choice.
// Since sk-674.18 T017 the undeclared context's hidden links are not
// counted either: "there is something hidden, how many" belongs to a participant of the
// space the hidden issue is in (INV-28 (4)), and a context without a
// reader has no space.
func TestBlockersHorizon_UndeclaredContextFailsClosed(t *testing.T) {
	f := newHorizonFixture(t)
	seeded := f.seed(t,
		blockerSeed{n: 2, status: StatusOpen, depType: "blocks"},
		blockerSeed{inQ: true, n: 3, status: StatusInProgress, depType: "waits-for"},
	)

	t.Run("undeclared context counts no blocker", func(t *testing.T) {
		dto := f.detail(t, context.Background())
		checkEntriesCounting(t, dto, seeded, func(seededBlocker) bool { return false }, 0)
	})
	t.Run("declared unrestricted context sees every entry", func(t *testing.T) {
		dto := f.detail(t, authz.WithUnrestricted(context.Background()))
		checkEntries(t, dto, seeded, func(seededBlocker) bool { return true })
	})
}
