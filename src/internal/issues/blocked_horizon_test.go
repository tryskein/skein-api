package issues

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
)

// sk-423 — «blocked» is counted only over blockers the READER can see. A
// blocks / waits-for edge to an issue outside the reader's horizon is
// published as the fact of a link (unavailableBlockingLinks), whatever the
// hidden side's status — so nothing a reader outside Q gets back may depend
// on whether an issue in Q is closed.
//
// The main assertion of every test below is INDISTINGUISHABILITY: for a
// reader outside Q, the response for a subject whose hidden blocker is
// open is compared, on every field the rule touches, with the response for
// an otherwise identical subject whose hidden blocker is in_progress,
// deferred, closed or standing. On the code before sk-423 the open case
// reads «blocked» and the closed one does not, so these tests are red there.
//
// The visible half pins that a blocker the reader CAN see behaves as before,
// and the readiness test pins that readiness still counts every blocker.

// hiddenStatuses — the first is the reference every other one is compared to.
var hiddenStatuses = []string{StatusOpen, StatusInProgress, StatusDeferred, StatusClosed, StatusStanding}

// bhFixture: one horizonFixture (projects P and Q, the four readers) plus,
// for every status in hiddenStatuses, a subject in P blocked by an issue in
// Q in that status, and an epic in P whose only child is blocked the same
// way; plus two subjects blocked from inside P (open and closed blocker).
type bhFixture struct {
	*horizonFixture

	hidden     map[string]string // hidden blocker status → subject id
	hiddenEpic map[string]string // hidden blocker status → epic id
	visOpen    string            // subject blocked by an open blocker in P
	visOpenBy  string            // that blocker
	visClosed  string            // subject whose only blocker in P is closed
}

func newBHFixture(t *testing.T) *bhFixture {
	t.Helper()
	h := newHorizonFixture(t)
	f := &bhFixture{
		horizonFixture: h,
		hidden:         make(map[string]string, len(hiddenStatuses)),
		hiddenEpic:     make(map[string]string, len(hiddenStatuses)),
	}
	pid := func(n int) string { return fmt.Sprintf("%s-%d", h.pPrefix, n) }
	qid := func(n int) string { return fmt.Sprintf("%s-%d", h.qPrefix, n) }

	for k, st := range hiddenStatuses {
		subject, blocker := pid(100+k), qid(100+k)
		seedIssue(t, h.pool, h.pID, h.pOwner, subject, "bh subject")
		seedIssue(t, h.pool, h.qID, h.qOwner, blocker, "bh hidden blocker")
		if st != StatusOpen {
			setIssueStatus(t, h.pool, blocker, st)
		}
		seedDep(t, h.pool, subject, blocker, "blocks", h.pOwner)
		f.hidden[st] = subject

		epic, child, childBlocker := pid(200+k), pid(300+k), qid(300+k)
		seedIssue(t, h.pool, h.pID, h.pOwner, epic, "bh epic")
		if _, err := h.pool.Exec(context.Background(), `UPDATE issues SET type = 'epic' WHERE id = $1`, epic); err != nil {
			t.Fatalf("make %s an epic: %v", epic, err)
		}
		seedIssue(t, h.pool, h.pID, h.pOwner, child, "bh epic child")
		seedParentChild(t, h.pool, child, epic, h.pOwner)
		seedIssue(t, h.pool, h.qID, h.qOwner, childBlocker, "bh hidden child blocker")
		if st != StatusOpen {
			setIssueStatus(t, h.pool, childBlocker, st)
		}
		seedDep(t, h.pool, child, childBlocker, "waits-for", h.pOwner)
		f.hiddenEpic[st] = epic
	}

	f.visOpen, f.visOpenBy = pid(10), pid(11)
	seedIssue(t, h.pool, h.pID, h.pOwner, f.visOpen, "bh visible subject")
	seedIssue(t, h.pool, h.pID, h.pOwner, f.visOpenBy, "bh visible blocker")
	seedDep(t, h.pool, f.visOpen, f.visOpenBy, "blocks", h.pOwner)

	f.visClosed = pid(12)
	seedIssue(t, h.pool, h.pID, h.pOwner, f.visClosed, "bh visible subject")
	seedIssue(t, h.pool, h.pID, h.pOwner, pid(13), "bh visible closed blocker")
	setIssueStatus(t, h.pool, pid(13), StatusClosed)
	seedDep(t, h.pool, f.visClosed, pid(13), "blocks", h.pOwner)

	// Every subject gets the vector bhEmbedder returns, so the semantic
	// rerank of the cross-project search reaches them for a query that full
	// text cannot match.
	subjects := []string{f.visOpen, f.visClosed}
	for _, id := range f.hidden {
		subjects = append(subjects, id)
	}
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE issues SET embedding = $1::halfvec WHERE id = ANY($2)`, bhUnitVectorLiteral(), subjects); err != nil {
		t.Fatalf("seed embeddings: %v", err)
	}
	return f
}

// bhEmbedder is a deterministic QueryEmbedder: always the same unit vector,
// so a row stored with that vector scores similarity 1.
type bhEmbedder struct{}

func (bhEmbedder) Available() bool { return true }

func (bhEmbedder) EmbedSingle(context.Context, string) ([]float32, error) {
	v := make([]float32, 1024)
	v[0] = 1
	return v, nil
}

func bhUnitVectorLiteral() string {
	parts := make([]string, 1024)
	for i := range parts {
		parts[i] = "0"
	}
	parts[0] = "1"
	return "[" + strings.Join(parts, ",") + "]"
}

// outsideQ are the readers whose horizon does not include Q: two with a
// principal, and a request without a reader (everything is outside it).
type bhReader struct {
	name string
	ctx  func(t *testing.T) context.Context
	args reader // the principal the store args describe
}

func (f *bhFixture) outsideQ() []bhReader {
	return []bhReader{
		{name: string(readerMemberP), ctx: func(t *testing.T) context.Context { return f.ctxFor(t, readerMemberP) }, args: readerMemberP},
		{name: string(readerExternalP), ctx: func(t *testing.T) context.Context { return f.ctxFor(t, readerExternalP) }, args: readerExternalP},
		{name: "request without a reader", ctx: func(*testing.T) context.Context { return context.Background() }, args: readerMemberP},
	}
}

// pick marshals v and returns the named top-level keys as canonical JSON —
// the «affected fields» of one response. An absent key is recorded as
// absent, so «absent» and «zero» stay distinguishable.
func pick(t *testing.T, v any, keys ...string) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if val, ok := m[k]; ok {
			out[k] = val
		} else {
			out[k] = "<absent>"
		}
	}
	canon, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal pick: %v", err)
	}
	return string(canon)
}

// assertIndistinguishable compares the projection of every hidden status
// with the reference (open) one.
func assertIndistinguishable(t *testing.T, surface string, project func(status string) string) {
	t.Helper()
	ref := project(hiddenStatuses[0])
	for _, st := range hiddenStatuses[1:] {
		if got := project(st); got != ref {
			t.Errorf("%s: hidden blocker %s answers %s, hidden blocker %s answers %s — "+
				"the response reveals the hidden issue's status", surface, hiddenStatuses[0], ref, st, got)
		}
	}
}

// Detail: flag, list and the published fact of the link.
func TestBlockedHorizon_Detail(t *testing.T) {
	f := newBHFixture(t)
	for _, r := range f.outsideQ() {
		t.Run(r.name, func(t *testing.T) {
			ctx := r.ctx(t)
			assertIndistinguishable(t, "detail", func(st string) string {
				dto, err := f.store.GetIssue(ctx, f.hidden[st])
				if err != nil {
					t.Fatalf("GetIssue: %v", err)
				}
				return pick(t, dto, "isBlocked", "blockers", "unavailableBlockingLinks")
			})
			dto, err := f.store.GetIssue(ctx, f.hidden[StatusOpen])
			if err != nil {
				t.Fatalf("GetIssue: %v", err)
			}
			// INV-28 (4): the link is counted for a participant of the
			// space Q is in; an external reader and a request without a
			// reader have no space, and count none.
			want := `{"blockers":[],"isBlocked":false,"unavailableBlockingLinks":1}`
			if r.name != string(readerMemberP) {
				want = `{"blockers":[],"isBlocked":false,"unavailableBlockingLinks":0}`
			}
			if got := pick(t, dto, "isBlocked", "blockers", "unavailableBlockingLinks"); got != want {
				t.Errorf("hidden open blocker: %s; want %s", got, want)
			}
		})
	}
}

// A reader who sees Q gets the hidden-for-others blocker as before: counted
// while it gates, not once it is closed; no unavailable link.
func TestBlockedHorizon_DetailReaderWhoSeesQ(t *testing.T) {
	f := newBHFixture(t)
	for _, r := range []reader{readerMemberBoth, readerAdmin} {
		t.Run(string(r), func(t *testing.T) {
			ctx := f.ctxFor(t, r)
			for _, st := range hiddenStatuses {
				dto, err := f.store.GetIssue(ctx, f.hidden[st])
				if err != nil {
					t.Fatalf("GetIssue: %v", err)
				}
				gates := st != StatusClosed && st != StatusStanding
				if dto.IsBlocked != gates || len(dto.Blockers) != map[bool]int{true: 1, false: 0}[gates] {
					t.Errorf("blocker %s: isBlocked=%v with %d entries; want %v", st, dto.IsBlocked, len(dto.Blockers), gates)
				}
				if got := pick(t, dto, "unavailableBlockingLinks"); got != `{"unavailableBlockingLinks":0}` {
					t.Errorf("blocker %s: %s; want 0 — nothing is unavailable to this reader", st, got)
				}
			}
		})
	}
}

// Visible blockers: behaviour unchanged.
func TestBlockedHorizon_VisibleBlockerUnchanged(t *testing.T) {
	f := newBHFixture(t)
	for _, r := range allReaders {
		t.Run(string(r), func(t *testing.T) {
			ctx := f.ctxFor(t, r)
			open, err := f.store.GetIssue(ctx, f.visOpen)
			if err != nil {
				t.Fatalf("GetIssue: %v", err)
			}
			want := fmt.Sprintf(`{"blockers":[{"id":%q,"status":"open","title":"bh visible blocker"}],"isBlocked":true}`, f.visOpenBy)
			if got := pick(t, open, "isBlocked", "blockers"); got != want {
				t.Errorf("visible open blocker: %s; want %s", got, want)
			}
			closed, err := f.store.GetIssue(ctx, f.visClosed)
			if err != nil {
				t.Fatalf("GetIssue: %v", err)
			}
			if got := pick(t, closed, "isBlocked", "blockers"); got != `{"blockers":[],"isBlocked":false}` {
				t.Errorf("visible closed blocker: %s; want not blocked", got)
			}

			res, err := f.store.ListIssues(ctx, ListIssuesArgs{ProjectID: f.pID, Limit: 200})
			if err != nil {
				t.Fatalf("ListIssues: %v", err)
			}
			rows := rowsByID(res.Items)
			if !rows[f.visOpen].IsBlocked || rows[f.visClosed].IsBlocked {
				t.Errorf("list rows: visible-open isBlocked=%v, visible-closed isBlocked=%v; want true, false",
					rows[f.visOpen].IsBlocked, rows[f.visClosed].IsBlocked)
			}
		})
	}
}

func rowsByID(items []IssueSummary) map[string]IssueSummary {
	m := make(map[string]IssueSummary, len(items))
	for _, it := range items {
		m[it.ID] = it
	}
	return m
}

// Rows and the isBlocked filter of the three list paths.
func TestBlockedHorizon_ListRowsAndFilter(t *testing.T) {
	f := newBHFixture(t)
	yes, no := true, false

	type lister func(ctx context.Context, r reader, blocked *bool) ([]IssueSummary, error)
	listers := map[string]lister{
		"project list": func(ctx context.Context, _ reader, blocked *bool) ([]IssueSummary, error) {
			res, err := f.store.ListIssues(ctx, ListIssuesArgs{ProjectID: f.pID, IsBlocked: blocked, Limit: 200})
			if err != nil {
				return nil, err
			}
			return res.Items, nil
		},
		"project search": func(ctx context.Context, _ reader, blocked *bool) ([]IssueSummary, error) {
			text := "bh subject"
			res, err := f.store.ListIssues(ctx, ListIssuesArgs{ProjectID: f.pID, Text: &text, IsBlocked: blocked, Limit: 200})
			if err != nil {
				return nil, err
			}
			return res.Items, nil
		},
		"cross-project list": func(ctx context.Context, r reader, blocked *bool) ([]IssueSummary, error) {
			a := f.listAllArgs(r)
			a.IsBlocked = blocked
			res, err := f.store.ListAllIssues(ctx, a)
			if err != nil {
				return nil, err
			}
			return res.Items, nil
		},
		"cross-project search": func(ctx context.Context, r reader, blocked *bool) ([]IssueSummary, error) {
			a := f.listAllArgs(r)
			a.Q = "bh subject"
			a.IsBlocked = blocked
			res, err := f.store.ListAllIssues(ctx, a)
			if err != nil {
				return nil, err
			}
			return res.Items, nil
		},
		"cross-project semantic": func(ctx context.Context, r reader, blocked *bool) ([]IssueSummary, error) {
			a := f.listAllArgs(r)
			a.Q = "zzqxwvunmatchable" // full text finds nothing; only the rerank can
			a.Embedder = bhEmbedder{}
			a.IsBlocked = blocked
			res, err := f.store.ListAllIssues(ctx, a)
			if err != nil {
				return nil, err
			}
			return res.Items, nil
		},
		"by user": func(ctx context.Context, r reader, blocked *bool) ([]IssueSummary, error) {
			a := f.listAllArgs(r)
			res, err := f.store.ListByUser(ctx, ListByUserArgs{
				UserID: f.pOwner, Mode: ModeCreatedBy, Scope: a.Scope,
				IsBlocked: blocked, Limit: 200,
			})
			if err != nil {
				return nil, err
			}
			return res.Items, nil
		},
	}

	for _, r := range f.outsideQ() {
		for name, list := range listers {
			t.Run(r.name+"/"+name, func(t *testing.T) {
				ctx := r.ctx(t)
				all, err := list(ctx, r.args, nil)
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				onlyBlocked, err := list(ctx, r.args, &yes)
				if err != nil {
					t.Fatalf("list isBlocked=true: %v", err)
				}
				onlyUnblocked, err := list(ctx, r.args, &no)
				if err != nil {
					t.Fatalf("list isBlocked=false: %v", err)
				}
				rows := rowsByID(all)
				if name == "cross-project semantic" {
					// The rerank swallows its own SQL errors (FTS-only fallback);
					// without these rows the check below would pass vacuously.
					if _, ok := rows[f.hidden[StatusClosed]]; !ok {
						t.Fatalf("semantic rerank did not deliver the subjects — its query failed: %v", idsOf(all))
					}
				}
				inBlocked, inUnblocked := idsOf(onlyBlocked), idsOf(onlyUnblocked)
				assertIndistinguishable(t, name, func(st string) string {
					id := f.hidden[st]
					row, ok := rows[id]
					if !ok {
						t.Fatalf("subject %s missing from the unfiltered list", id)
					}
					return fmt.Sprintf("row=%s filter(true)=%v filter(false)=%v",
						pick(t, row, "isBlocked"), slices.Contains(inBlocked, id), slices.Contains(inUnblocked, id))
				})
			})
		}
	}
}

func (f *bhFixture) listAllArgs(r reader) ListAllIssuesArgs {
	a := ListAllIssuesArgs{Limit: 200}
	switch r {
	case readerMemberP:
		a.Caller, a.Scope = f.pOwner, authz.ForUser(f.pOwner)
	case readerExternalP:
		a.Caller, a.Scope = f.externalP, authz.ForUser(f.externalP)
	}
	return a
}

// Tree rows, with and without includeBlockers.
func TestBlockedHorizon_Tree(t *testing.T) {
	f := newBHFixture(t)
	for _, r := range f.outsideQ() {
		for _, include := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/includeBlockers=%v", r.name, include), func(t *testing.T) {
				ctx := r.ctx(t)
				assertIndistinguishable(t, "tree", func(st string) string {
					root := f.hidden[st]
					args := f.treeArgsFor(t, r.args)
					args.RootID = &root
					args.IncludeBlockers = include
					res, err := f.store.IssueTree(ctx, args)
					if err != nil {
						t.Fatalf("IssueTree: %v", err)
					}
					if len(res.Items) == 0 || res.Items[0].Item.ID != root {
						t.Fatalf("tree root is not %s: %+v", root, res.Items)
					}
					node := res.Items[0]
					return pick(t, node.Item, "isBlocked") + pick(t, node, "blockers", "unavailableBlockingLinks")
				})
			})
		}
	}
}

// Epic rollup: every counter, compared whole.
func TestBlockedHorizon_EpicRollup(t *testing.T) {
	f := newBHFixture(t)
	for _, r := range f.outsideQ() {
		t.Run(r.name, func(t *testing.T) {
			ctx := r.ctx(t)
			assertIndistinguishable(t, "epic rollup", func(st string) string {
				dto, err := f.store.GetIssue(ctx, f.hiddenEpic[st])
				if err != nil {
					t.Fatalf("GetIssue: %v", err)
				}
				return pick(t, dto, "epic")
			})
		})
	}
	// A reader who sees Q: blockedChildren counts the gating hidden-for-others
	// blocker exactly as before.
	ctx := f.ctxFor(t, readerMemberBoth)
	for _, st := range hiddenStatuses {
		dto, err := f.store.GetIssue(ctx, f.hiddenEpic[st])
		if err != nil {
			t.Fatalf("GetIssue: %v", err)
		}
		want := 0
		if st != StatusClosed && st != StatusStanding {
			want = 1
		}
		if dto.Epic == nil || dto.Epic.BlockedChildren != want {
			t.Errorf("member of both, blocker %s: epic = %+v; want blockedChildren %d", st, dto.Epic, want)
		}
	}
}

// Readiness keeps counting every blocker, the hidden ones included.
func TestBlockedHorizon_ReadinessUnchanged(t *testing.T) {
	f := newBHFixture(t)
	deps := dependencies.NewStore(f.pool)
	for _, r := range append(f.outsideQ(), bhReader{
		name: string(readerMemberBoth), ctx: func(t *testing.T) context.Context { return f.ctxFor(t, readerMemberBoth) },
	}) {
		t.Run(r.name, func(t *testing.T) {
			ready, err := deps.ListReady(r.ctx(t), f.pID, dependencies.ReadyFilters{}, 100)
			if err != nil {
				t.Fatalf("ListReady: %v", err)
			}
			ids := make([]string, 0, len(ready))
			for _, it := range ready {
				ids = append(ids, it.ID)
			}
			for _, st := range hiddenStatuses {
				gates := st != StatusClosed && st != StatusStanding
				if got := slices.Contains(ids, f.hidden[st]); got == gates {
					t.Errorf("hidden blocker %s: subject ready=%v; want %v — readiness counts every blocker", st, got, !gates)
				}
			}
			if slices.Contains(ids, f.visOpen) || !slices.Contains(ids, f.visClosed) {
				t.Errorf("visible blockers: ready = %v; want %s out and %s in", ids, f.visOpen, f.visClosed)
			}
		})
	}
}
