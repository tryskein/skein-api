package issues

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
)

// sk-421.2.8 T004 (US-4, US-2, INV-1, INV-5, INV-7, SC-006, SC-011) — the
// blockers list of an issue-tree node, assembled by Store.IssueTree with
// IncludeBlockers. Test-plan rows 14–17. Fixture: horizonFixture from
// blockers_visibility_test.go — the subject issue is the tree's root row.

// treeArgsFor is the IssueTreeArgs a handler would build for the reader:
// the same caller and scope as ctxFor's principal, rooted at the subject.
func (f *horizonFixture) treeArgsFor(t *testing.T, r reader) IssueTreeArgs {
	t.Helper()
	args := IssueTreeArgs{
		RootID:          &f.subject,
		Types:           []string{"parent-child"},
		IncludeBlockers: true,
	}
	switch r {
	case readerMemberP:
		args.Caller = f.pOwner
	case readerMemberBoth:
		args.Caller = f.memberBoth
	case readerExternalP:
		args.Caller = f.externalP
	case readerAdmin:
		args.Caller = f.admin
	default:
		t.Fatalf("unknown reader %q", r)
	}
	args.Scope = authz.ForUser(args.Caller)
	return args
}

// treeRoot runs the tree call and returns the subject's row.
func (f *horizonFixture) treeRoot(t *testing.T, ctx context.Context, args IssueTreeArgs) IssueTreeNode {
	t.Helper()
	res, err := f.store.IssueTree(ctx, args)
	if err != nil {
		t.Fatalf("IssueTree(%s): %v", f.subject, err)
	}
	if len(res.Items) == 0 || res.Items[0].Item.ID != f.subject {
		t.Fatalf("IssueTree(%s): first row is not the subject: %+v", f.subject, res.Items)
	}
	return res.Items[0]
}

// treeEntryKeys returns the sorted JSON keys an entry serialises to — the
// forms of US-4 are statements about KEYS, «absent» is not «empty».
func treeEntryKeys(t *testing.T, b TreeBlockerRef) string {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal tree blocker: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal tree blocker %s: %v", raw, err)
	}
	return sortedKeys(m)
}

// Row 14 — requests without a reader (R-5). An undeclared context sees no
// project, so no blocker counts — P's included — and, having no reader and
// so no own space, it counts no hidden link either (INV-28 (4), sk-674.18
// T017); a context declared server-internal sees every entry in full, even
// though the args are a member of P only.
func TestTreeBlockers_ContextsWithoutReader(t *testing.T) {
	f := newHorizonFixture(t)
	seeded := f.seed(t,
		blockerSeed{n: 2, status: StatusOpen, depType: "blocks"},
		blockerSeed{inQ: true, n: 3, status: StatusInProgress, depType: "waits-for"},
	)
	seeds := byID(seeded)
	args := f.treeArgsFor(t, readerMemberP)

	t.Run("undeclared context: nothing blocks, no hidden link counted", func(t *testing.T) {
		got := f.treeRoot(t, context.Background(), args)
		if got.Item.IsBlocked || len(got.Blockers) != 0 || got.UnavailableBlockingLinks != 0 {
			t.Errorf("isBlocked=%v, %d entries, %d hidden links; want false, 0, 0",
				got.Item.IsBlocked, len(got.Blockers), got.UnavailableBlockingLinks)
		}
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		for _, s := range seeded {
			if strings.Contains(string(raw), s.title) {
				t.Errorf("blocker %s leaked its title into %s", s.id, raw)
			}
		}
	})

	t.Run("declared server-internal context: every entry full", func(t *testing.T) {
		got := f.treeRoot(t, authz.WithUnrestricted(context.Background()), args)
		if !got.Item.IsBlocked || len(got.Blockers) != 2 || got.UnavailableBlockingLinks != 0 {
			t.Fatalf("isBlocked=%v with %d entries, %d hidden links; want true with 2, 0",
				got.Item.IsBlocked, len(got.Blockers), got.UnavailableBlockingLinks)
		}
		for i, b := range got.Blockers {
			s, ok := seeds[b.ID]
			if !ok {
				t.Errorf("entry %d: id %q is not a seeded blocker", i, b.ID)
				continue
			}
			if keys := treeEntryKeys(t, b); keys != "id,projectId,status,title" {
				t.Errorf("entry %s: wire keys = %s; want id,projectId,status,title — declared internal "+
					"work sees every project, whatever VisibleProjects the args carry", s.id, keys)
			}
			wantProject := f.pID
			if s.inQ {
				wantProject = f.qID
			}
			if b.Title != s.title || b.Status != s.status || b.ProjectID == nil || *b.ProjectID != wantProject {
				t.Errorf("entry %s = %+v; want title %q, status %s, projectId %s", s.id, b, s.title, s.status, wantProject)
			}
		}
	})
}

// Row 15 — the args and the context's principal must describe one reader
// (R-6, INV-7). Under a member-of-P principal, each subcase moves exactly
// one of the three compared inputs; each must fail with no rows. The same
// args without IncludeBlockers pass: the check belongs to the flag.
func TestTreeBlockers_ArgsMustMatchPrincipal(t *testing.T) {
	f := newHorizonFixture(t)
	f.seed(t,
		blockerSeed{n: 2, status: StatusOpen, depType: "blocks"},
		blockerSeed{inQ: true, n: 3, status: StatusOpen, depType: "blocks"},
	)
	ctx := f.ctxFor(t, readerMemberP)

	if res, err := f.store.IssueTree(ctx, f.treeArgsFor(t, readerMemberP)); err != nil || len(res.Items) == 0 {
		t.Fatalf("control: matching args must pass, got err=%v", err)
	}

	cases := []struct {
		name   string
		mutate func(a *IssueTreeArgs)
	}{
		{name: "caller differs", mutate: func(a *IssueTreeArgs) { a.Caller = f.memberBoth }},
		{name: "scope differs", mutate: func(a *IssueTreeArgs) { a.Scope = authz.ForUser(f.memberBoth) }},
		{name: "unrestricted scope", mutate: func(a *IssueTreeArgs) { a.Scope = authz.Unrestricted() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := f.treeArgsFor(t, readerMemberP)
			tc.mutate(&args)

			res, err := f.store.IssueTree(ctx, args)
			if !errors.Is(err, errTreeHorizonMismatch) {
				t.Errorf("with IncludeBlockers: err = %v; want errTreeHorizonMismatch — nodes and blocker "+
					"entries would be classified for two different readers", err)
			}
			if res != nil {
				t.Errorf("with IncludeBlockers: got %d rows alongside the error; want none", len(res.Items))
			}

			args.IncludeBlockers = false
			if _, err := f.store.IssueTree(ctx, args); err != nil {
				t.Errorf("without IncludeBlockers: err = %v; want nil — the check applies only with the flag", err)
			}
		})
	}
}

// orderFixture backs row 16. Project prefixes are random, so the literal
// «hidden after visible» would pass trivially whenever the hidden project's
// prefix happened to sort AFTER the visible one. The fixture therefore picks
// the project with the SMALLER prefix as the hidden one (h) and puts the
// subject in the other (v):
//
//   - vOwner   — internal member of v only (the «internal non-member of Q»);
//   - external — external collaborator, member of v only;
//   - both     — internal member of v and h.
type orderFixture struct {
	pool  *pgxpool.Pool
	store *Store

	vID, hID         uuid.UUID
	vPrefix, hPrefix string
	vOwner, hOwner   uuid.UUID
	external, both   uuid.UUID

	subject string
}

func newOrderFixture(t *testing.T) *orderFixture {
	t.Helper()
	pool := newTestPool(t)
	f := &orderFixture{pool: pool, store: NewStore(pool)}

	aOwner, bOwner := seedUser(t, pool), seedUser(t, pool)
	aID, aPrefix := seedProject(t, pool, aOwner)
	bID, bPrefix := seedProject(t, pool, bOwner)
	if aPrefix == bPrefix {
		t.Fatalf("two seeded projects share prefix %q", aPrefix)
	}
	if aPrefix < bPrefix {
		f.hID, f.hPrefix, f.hOwner = aID, aPrefix, aOwner
		f.vID, f.vPrefix, f.vOwner = bID, bPrefix, bOwner
	} else {
		f.hID, f.hPrefix, f.hOwner = bID, bPrefix, bOwner
		f.vID, f.vPrefix, f.vOwner = aID, aPrefix, aOwner
	}

	f.external = seedUser(t, pool)
	addMember(t, pool, f.vID, f.external, f.vOwner)
	f.both = seedUser(t, pool)
	addMember(t, pool, f.vID, f.both, f.vOwner)
	addMember(t, pool, f.hID, f.both, f.hOwner)

	f.subject = f.vPrefix + "-1"
	seedIssue(t, pool, f.vID, f.vOwner, f.subject, "subject")
	return f
}

// orderSeed is one blocks edge of the subject: n in h (hidden) or in v.
type orderSeed struct {
	hidden bool
	n      int
}

func (f *orderFixture) seed(t *testing.T, seeds ...orderSeed) {
	t.Helper()
	for _, s := range seeds {
		pid, owner, prefix := f.vID, f.vOwner, f.vPrefix
		if s.hidden {
			pid, owner, prefix = f.hID, f.hOwner, f.hPrefix
		}
		id := prefix + "-" + strconv.Itoa(s.n)
		seedIssue(t, f.pool, pid, owner, id, "blocker "+id)
		seedDep(t, f.pool, f.subject, id, "blocks", f.vOwner)
	}
}

// orderReader is one of the three readers of row 16.
type orderReader string

const (
	orderInternalNonMember orderReader = "internal non-member of h"
	orderExternal          orderReader = "external member of v"
	orderMemberBoth        orderReader = "member of v and h"
)

func (f *orderFixture) ctxAndArgs(t *testing.T, r orderReader) (context.Context, IssueTreeArgs) {
	t.Helper()
	args := IssueTreeArgs{
		RootID:          &f.subject,
		Types:           []string{"parent-child"},
		IncludeBlockers: true,
	}
	switch r {
	case orderInternalNonMember:
		args.Caller, args.Scope = f.vOwner, authz.ForUser(f.vOwner)
		return callerCtx(t, f.vOwner, "user"), args
	case orderExternal:
		args.Caller, args.Scope = f.external, authz.ForUser(f.external)
		return auth.WithIsExternal(callerCtx(t, f.external, "user"), true), args
	case orderMemberBoth:
		args.Caller, args.Scope = f.both, authz.ForUser(f.both)
		return callerCtx(t, f.both, "user"), args
	}
	t.Fatalf("unknown reader %q", r)
	return nil, args
}

// renderEntries spells a node's blockers as their ids plus the number of
// hidden links: «a,b|hidden=N».
func renderEntries(node IssueTreeNode) string {
	parts := make([]string, 0, len(node.Blockers))
	for _, b := range node.Blockers {
		parts = append(parts, b.ID)
	}
	return strings.Join(parts, ",") + "|hidden=" + strconv.Itoa(node.UnavailableBlockingLinks)
}

// Row 16 — the acceptance literals of US-4 "order of entries", as sk-423
// left them: the entries are the blockers the reader can see, in the
// detail's order (the id-order rule of US-6 since sk-426); edges to a project
// outside the horizon are not entries, only counted. The hidden project's
// prefix sorts BEFORE the visible one, so a hidden entry that survived would
// show up first.
func TestTreeBlockers_EntryOrderAcceptanceLiterals(t *testing.T) {
	check := func(t *testing.T, f *orderFixture, r orderReader, want string) {
		t.Helper()
		ctx, args := f.ctxAndArgs(t, r)
		res, err := f.store.IssueTree(ctx, args)
		if err != nil {
			t.Fatalf("%s: IssueTree: %v", r, err)
		}
		if len(res.Items) == 0 || res.Items[0].Item.ID != f.subject {
			t.Fatalf("%s: first row is not the subject: %+v", r, res.Items)
		}
		if got := renderEntries(res.Items[0]); got != want {
			t.Errorf("%s: blockers = %s; want %s", r, got, want)
		}
	}
	precondition := func(t *testing.T, f *orderFixture) {
		t.Helper()
		if !(f.hPrefix < f.vPrefix) {
			t.Fatalf("precondition: hidden prefix %q must sort before visible prefix %q", f.hPrefix, f.vPrefix)
		}
	}
	v := func(f *orderFixture, n int) string { return f.vPrefix + "-" + strconv.Itoa(n) }
	h := func(f *orderFixture, n int) string { return f.hPrefix + "-" + strconv.Itoa(n) }

	t.Run("(a) p-9, q-1 hidden, p-2", func(t *testing.T) {
		f := newOrderFixture(t)
		f.seed(t, orderSeed{n: 9}, orderSeed{hidden: true, n: 1}, orderSeed{n: 2})
		precondition(t, f)

		check(t, f, orderInternalNonMember, v(f, 2)+","+v(f, 9)+"|hidden=1")
		check(t, f, orderExternal, v(f, 2)+","+v(f, 9)+"|hidden=1")
		check(t, f, orderMemberBoth, h(f, 1)+","+v(f, 2)+","+v(f, 9)+"|hidden=0")
	})

	t.Run("(b) q-3 hidden, p-2, q-1 hidden", func(t *testing.T) {
		f := newOrderFixture(t)
		f.seed(t, orderSeed{hidden: true, n: 3}, orderSeed{n: 2}, orderSeed{hidden: true, n: 1})
		precondition(t, f)

		check(t, f, orderInternalNonMember, v(f, 2)+"|hidden=2")
		check(t, f, orderMemberBoth, h(f, 1)+","+h(f, 3)+","+v(f, 2)+"|hidden=0")
	})

	t.Run("(c) q-9, q-10 hidden, p-9, p-10", func(t *testing.T) {
		f := newOrderFixture(t)
		f.seed(t, orderSeed{hidden: true, n: 9}, orderSeed{hidden: true, n: 10}, orderSeed{n: 9}, orderSeed{n: 10})
		precondition(t, f)

		// sk-426: the detail's order is the id-order rule of US-6, so 9
		// comes before 10 (string order put 10 first).
		check(t, f, orderInternalNonMember, v(f, 9)+","+v(f, 10)+"|hidden=2")
	})
}

// addEdge adds one more outgoing edge of the subject to an already seeded
// target — the duplicate blocks + waits-for configuration of row 17.
func (f *horizonFixture) addEdge(t *testing.T, toID, depType string) {
	t.Helper()
	if depType == "waits-for" {
		if _, err := f.pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'waits-for', '{"gate":"all-children"}'::jsonb, 1, $3, now())`,
			f.subject, toID, f.pOwner); err != nil {
			t.Fatalf("add waits-for %s -> %s: %v", f.subject, toID, err)
		}
		return
	}
	seedDep(t, f.pool, f.subject, toID, depType, f.pOwner)
}

// Row 17 — SC-006 / INV-5: the tree node and the issue detail follow ONE
// rule. For every reader and configuration, the node is compared with the
// detail of the same issue read with the same context: the same entries in
// the same order with the detail's title and status, the same flag, the
// same number of hidden links (sk-423); a visible entry's projectId is its
// blocker's project.
func TestTreeBlockers_SameRuleAsDetail(t *testing.T) {
	type edge struct {
		n       int // seeded target n
		depType string
	}
	configs := []struct {
		name  string
		seeds []blockerSeed
		extra []edge
	}{
		{
			name: "closed, standing and deferred targets",
			seeds: []blockerSeed{
				{n: 2, status: StatusOpen, depType: "blocks"},
				{n: 3, status: StatusClosed, depType: "blocks"},
				{inQ: true, n: 4, status: StatusStanding, depType: "waits-for"},
				{inQ: true, n: 5, status: StatusDeferred, depType: "blocks"},
				{n: 6, status: StatusDeferred, depType: "waits-for"},
				{inQ: true, n: 7, status: StatusClosed, depType: "waits-for"},
			},
		},
		{
			name: "related and parent-child edges do not block",
			seeds: []blockerSeed{
				{n: 2, status: StatusOpen, depType: "related"},
				{inQ: true, n: 3, status: StatusOpen, depType: "parent-child"},
				{n: 4, status: StatusOpen, depType: "blocks"},
				{inQ: true, n: 5, status: StatusInProgress, depType: "related"},
			},
		},
		{
			name: "duplicate blocks and waits-for to one blocker",
			seeds: []blockerSeed{
				{n: 2, status: StatusOpen, depType: "waits-for"},
				{inQ: true, n: 3, status: StatusOpen, depType: "blocks"},
				{n: 4, status: StatusInProgress, depType: "blocks"},
			},
			extra: []edge{{n: 2, depType: "blocks"}, {n: 3, depType: "waits-for"}},
		},
	}

	for _, cfg := range configs {
		t.Run(cfg.name, func(t *testing.T) {
			f := newHorizonFixture(t)
			seeded := f.seed(t, cfg.seeds...)
			seeds := byID(seeded)
			idOf := make(map[int]string, len(seeded))
			for _, s := range seeded {
				idOf[s.n] = s.id
			}
			for _, e := range cfg.extra {
				f.addEdge(t, idOf[e.n], e.depType)
			}

			for _, r := range allReaders {
				t.Run(string(r), func(t *testing.T) {
					ctx := f.ctxFor(t, r)
					dto := f.detail(t, ctx)
					node := f.treeRoot(t, ctx, f.treeArgsFor(t, r))

					if node.Item.IsBlocked != dto.IsBlocked {
						t.Errorf("node isBlocked = %v; detail %v", node.Item.IsBlocked, dto.IsBlocked)
					}
					if node.Item.IsBlocked != (len(node.Blockers) > 0) {
						t.Errorf("node isBlocked = %v with %d entries — flag and list disagree (INV-2)",
							node.Item.IsBlocked, len(node.Blockers))
					}
					if node.UnavailableBlockingLinks != dto.UnavailableBlockingLinks {
						t.Errorf("hidden links: node %d; detail %d (INV-5)", node.UnavailableBlockingLinks, dto.UnavailableBlockingLinks)
					}
					if len(node.Blockers) != len(dto.Blockers) {
						t.Fatalf("entries: node %d; detail %d (INV-5)", len(node.Blockers), len(dto.Blockers))
					}
					for i, d := range dto.Blockers {
						n := node.Blockers[i]
						if n.ID != d.ID || n.Title != d.Title || n.Status != d.Status {
							t.Errorf("entry %d: node {%s %q %s}; detail {%s %q %s}",
								i, n.ID, n.Title, n.Status, d.ID, d.Title, d.Status)
						}
						wantProject := f.pID
						if seeds[d.ID].inQ {
							wantProject = f.qID
						}
						if n.ProjectID == nil || *n.ProjectID != wantProject {
							t.Errorf("entry %s: projectId = %v; want %s", n.ID, n.ProjectID, wantProject)
						}
					}
				})
			}
		})
	}
}
