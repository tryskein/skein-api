package projects

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

// sk-423 — the «blocked» counters of the project row, the project card and
// the component row (blocked_count, my_outbox_blocked_count,
// on_me_blocked_count, Stats.Blocked) and their disjoint complements (open /
// in_progress) count only blockers the READER can see; the lens HAVING
// clauses follow the same rule. For a reader outside the blocker's project
// nothing on these surfaces may depend on whether the hidden blocker is
// closed.
//
// One project P per hidden status, identical except for that status: three
// issues, each blocked by its own issue in Q (Q is invisible to the
// readers) — an unassigned open one, an in_progress one the reader created
// and delegated (my-outbox), and a deferred one assigned to the reader by
// someone else and accepted (on-me) — all three in one component. On the
// code before sk-423 the open-blocker project reads «3 blocked» and the
// closed-blocker project «0 blocked», and only the first survives the on-me
// lens.

var bhHiddenStatuses = []string{"open", "in_progress", "deferred", "closed", "standing"}

type bhProjectsFixture struct {
	pool     *pgxpool.Pool
	store    *Store
	reader   uuid.UUID
	readerUN string
	external uuid.UUID

	projectOf   map[string]uuid.UUID // hidden status → project P
	componentOf map[string]uuid.UUID // hidden status → component in P
}

func newBHProjectsFixture(t *testing.T) *bhProjectsFixture {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	reader, readerUN := testsupport.SeedUser(t, pool, "user")
	other, otherUN := testsupport.SeedUser(t, pool, "user")
	external, _ := testsupport.SeedUser(t, pool, "user")
	qOwner, _ := testsupport.SeedUser(t, pool, "user")
	qID, qPrefix := testsupport.SeedProject(t, pool, qOwner, "bhq")

	f := &bhProjectsFixture{
		pool: pool, store: NewStore(pool),
		reader: reader, readerUN: readerUN, external: external,
		projectOf:   make(map[string]uuid.UUID, len(bhHiddenStatuses)),
		componentOf: make(map[string]uuid.UUID, len(bhHiddenStatuses)),
	}
	for k, st := range bhHiddenStatuses {
		pid, prefix := testsupport.SeedProject(t, pool, reader, "bhp")
		testsupport.AddProjectMember(t, pool, pid, other, "member")
		testsupport.AddProjectMember(t, pool, pid, external, "member")
		if _, err := pool.Exec(context.Background(),
			`UPDATE projects SET embedding = $1::halfvec WHERE id = $2`, literalUnitVector(), pid); err != nil {
			t.Fatalf("seed project embedding: %v", err)
		}
		comp := testsupport.SeedComponent(t, pool, pid, reader, "bhcomp", "")

		open := testsupport.SeedIssue(t, pool, pid, reader, prefix+"-1", "bh open", "open", 2)
		delegated := testsupport.SeedIssue(t, pool, pid, reader, prefix+"-2", "bh delegated", "in_progress", 2)
		testsupport.SetAssignee(t, pool, delegated, otherUN)
		onMe := testsupport.SeedIssue(t, pool, pid, other, prefix+"-3", "bh on me", "deferred", 2)
		testsupport.SetAssignee(t, pool, onMe, readerUN)
		testsupport.Accept(t, pool, onMe, reader, true)

		for j, subject := range []string{open, delegated, onMe} {
			blocker := testsupport.SeedIssue(t, pool, qID, qOwner,
				fmt.Sprintf("%s-%d", qPrefix, 10*k+j+1), "bh hidden blocker", st, 2)
			testsupport.SeedDep(t, pool, subject, blocker, "blocks", reader)
			if _, err := pool.Exec(context.Background(),
				`INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)`, subject, comp); err != nil {
				t.Fatalf("seed issue component: %v", err)
			}
		}
		f.projectOf[st] = pid
		f.componentOf[st] = comp
	}
	return f
}

type bhProjectsReader struct {
	name string
	id   uuid.UUID
	ctx  context.Context
}

func (f *bhProjectsFixture) readers(t *testing.T) []bhProjectsReader {
	principal := func(id uuid.UUID, external bool) context.Context {
		ctx := auth.WithUserRole(auth.WithUserID(t.Context(), id), "user")
		if external {
			ctx = auth.WithIsExternal(ctx, true)
		}
		return ctx
	}
	return []bhProjectsReader{
		{name: "member of P, not of Q", id: f.reader, ctx: principal(f.reader, false)},
		{name: "external member of P", id: f.external, ctx: principal(f.external, true)},
		{name: "request without a reader", id: f.reader, ctx: context.Background()},
	}
}

// bhJSON marshals v and returns the named keys, dropping nothing else —
// an absent key is recorded as absent.
func bhJSON(t *testing.T, v any, keys ...string) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if val, ok := m[k]; ok {
			out[k] = val
		} else {
			out[k] = "<absent>"
		}
	}
	canon, _ := json.Marshal(out)
	return string(canon)
}

var bhRowCounters = []string{
	"openCount", "inProgressCount", "blockedCount", "standingCount",
	"myOutboxOpenCount", "myOutboxInProgressCount", "myOutboxBlockedCount", "myOutboxStandingCount",
	"onMeOpenCount", "onMeInProgressCount", "onMeBlockedCount",
}

func bhAssertIndistinguishable(t *testing.T, surface string, project func(status string) string) {
	t.Helper()
	ref := project(bhHiddenStatuses[0])
	for _, st := range bhHiddenStatuses[1:] {
		if got := project(st); got != ref {
			t.Errorf("%s: hidden blocker open answers %s, hidden blocker %s answers %s — "+
				"the response reveals the hidden issue's status", surface, ref, st, got)
		}
	}
}

func TestBlockedHorizon_ProjectRows(t *testing.T) {
	f := newBHProjectsFixture(t)
	paths := map[string]func(ListProjectsArgs) ListProjectsArgs{
		"alphabetic": func(a ListProjectsArgs) ListProjectsArgs { return a },
		"search":     func(a ListProjectsArgs) ListProjectsArgs { a.Query = "bhp"; return a },
		"semantic": func(a ListProjectsArgs) ListProjectsArgs {
			a.Query = "zzqxwvunmatchable"
			a.Embedder = fakeEmbedder{}
			return a
		},
	}
	for _, r := range f.readers(t) {
		for pathName, shape := range paths {
			for _, lens := range []string{"", "on-me", "delegated"} {
				t.Run(fmt.Sprintf("%s/%s/lens=%q", r.name, pathName, lens), func(t *testing.T) {
					res, err := f.store.ListProjects(r.ctx, shape(ListProjectsArgs{
						CallerID: r.id, Lens: lens, Limit: 100,
					}))
					if err != nil {
						t.Fatalf("ListProjects: %v", err)
					}
					rows := make(map[uuid.UUID]ProjectListItem, len(res.Items))
					for _, it := range res.Items {
						rows[it.ID] = it
					}
					if lens == "" {
						if _, ok := rows[f.projectOf["closed"]]; !ok {
							t.Fatalf("control: project missing from the %s list", pathName)
						}
					}
					bhAssertIndistinguishable(t, "project row", func(st string) string {
						row, ok := rows[f.projectOf[st]]
						if !ok {
							return "row absent"
						}
						return bhJSON(t, row, bhRowCounters...)
					})
				})
			}
		}
	}
}

func TestBlockedHorizon_ProjectCard(t *testing.T) {
	f := newBHProjectsFixture(t)
	for _, r := range f.readers(t) {
		t.Run(r.name, func(t *testing.T) {
			bhAssertIndistinguishable(t, "project card stats", func(st string) string {
				dto, err := f.store.GetProjectWithStats(r.ctx, f.projectOf[st], r.id)
				if err != nil {
					t.Fatalf("GetProjectWithStats: %v", err)
				}
				return bhJSON(t, dto.Stats, "open", "inProgress", "blocked", "deferred", "closed",
					"standingCount", "total", "myAssigned", "myCreated")
			})
		})
	}
}

func TestBlockedHorizon_ComponentRows(t *testing.T) {
	f := newBHProjectsFixture(t)
	for _, r := range f.readers(t) {
		for _, lens := range []string{"", "on-me", "delegated"} {
			t.Run(fmt.Sprintf("%s/lens=%q", r.name, lens), func(t *testing.T) {
				bhAssertIndistinguishable(t, "component row", func(st string) string {
					list, err := f.store.ListComponentsWithLens(r.ctx, ListComponentsArgs{
						ProjectID: f.projectOf[st], Archived: "false", CallerID: r.id, Lens: lens,
					})
					if err != nil {
						t.Fatalf("ListComponentsWithLens: %v", err)
					}
					for _, c := range list {
						if c.ID == f.componentOf[st] {
							return bhJSON(t, c, bhRowCounters...)
						}
					}
					return "component absent"
				})
			})
		}
	}
}

// A reader who can see the blocker's project gets the counters as before:
// the open-blocker project counts all three as blocked, the closed one none.
func TestBlockedHorizon_ProjectCountersVisibleBlocker(t *testing.T) {
	f := newBHProjectsFixture(t)
	// The reader is an admin in users: since sk-674.18 T007 the horizon reads
	// the role from the access relation, not from the request context (D-2).
	// MineOnly is off, so the list holds every project the relation grants.
	if _, err := f.pool.Exec(t.Context(), `UPDATE users SET role = 'admin' WHERE id = $1`, f.reader); err != nil {
		t.Fatalf("make the reader an admin: %v", err)
	}
	admin := auth.WithUserRole(auth.WithUserID(t.Context(), f.reader), "admin")
	res, err := f.store.ListProjects(admin, ListProjectsArgs{
		CallerID: f.reader, Limit: 100,
	})
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	for _, it := range res.Items {
		switch it.ID {
		case f.projectOf["open"]:
			if it.BlockedCount != 3 || it.MyOutboxBlockedCount != 1 || it.OnMeBlockedCount != 1 || it.OpenCount != 0 {
				t.Errorf("open blocker, reader who sees Q: %s", bhJSON(t, it, bhRowCounters...))
			}
		case f.projectOf["closed"]:
			if it.BlockedCount != 0 || it.OpenCount != 1 || it.InProgressCount != 1 {
				t.Errorf("closed blocker, reader who sees Q: %s", bhJSON(t, it, bhRowCounters...))
			}
		}
	}
}
