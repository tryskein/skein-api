package spaces_test

// membership_test.go — sk-674.18 T018, matrix rows 43, 47. The reference
// is the US-17 mutation table, INV-6, INV-7 and SC-017 of the spec,
// written before the primitives:
//
//   - row 43: every operation of the US-17 table × the initial state of the
//     target's membership row (none, "manually", "from space", both, an
//     explicit owner; and the implicit owner of the space) gives the access,
//     role and sources of the table; "remove" a member who has the source
//     "from space" is member_via_space with nothing changed (E-27); an
//     owner by appointment stays after space → members (E-28);
//   - row 47: joining a space with N ≥ 2 `space` projects, leaving it and a
//     visibility change write exactly one space-level event (with the
//     projects) and no project membership event.

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/authz"
	"skein-api/internal/projects"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

// world is one space S (owner sOwner) with a project P (owner pOwner, an
// explicit owner row) and a target user u, a participant of S.
type world struct {
	pool                *pgxpool.Pool
	store               *spaces.Store
	pstore              *projects.Store
	space               uuid.UUID
	sOwner, pOwner, u   uuid.UUID
	uName               string
	p                   uuid.UUID
	actor               spaces.Actor
	projectActor        projects.Actor
	assignedIssue       string
	assignedIssueClosed string
}

func newWorld(t *testing.T, visibility string) *world {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	w := &world{pool: pool, store: spaces.NewStore(pool), pstore: projects.NewStore(pool)}
	var sName, pName string
	w.sOwner, sName = testsupport.SeedUser(t, pool, "")
	w.pOwner, pName = testsupport.SeedUser(t, pool, "")
	w.u, w.uName = testsupport.SeedUser(t, pool, "")
	w.space = testsupport.SeedSpace(t, pool, w.sOwner)
	testsupport.AddSpaceMember(t, pool, w.space, w.pOwner)
	testsupport.AddSpaceMember(t, pool, w.space, w.u)
	var prefix string
	w.p, prefix = testsupport.SeedProjectInSpace(t, pool, w.pOwner, w.space, "mbr")
	if _, err := pool.Exec(ctx, `UPDATE projects SET visibility = $2 WHERE id = $1`, w.p, visibility); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	w.actor = spaces.Actor{UserID: w.sOwner, Username: sName, Type: "human"}
	w.projectActor = projects.Actor{UserID: w.pOwner, Username: pName, Role: "human"}
	w.assignedIssue, w.assignedIssueClosed = prefix+"-1", prefix+"-2"
	testsupport.SeedIssue(t, pool, w.p, w.pOwner, w.assignedIssue, "mbr assigned", "open", 2)
	testsupport.SeedIssue(t, pool, w.p, w.pOwner, w.assignedIssueClosed, "mbr assigned closed", "closed", 2)
	testsupport.SetAssignee(t, pool, w.assignedIssue, w.uName)
	testsupport.SetAssignee(t, pool, w.assignedIssueClosed, w.uName)
	return w
}

// setRow puts u's membership row of P into a state: "none", "manual",
// "space", "both", "owner" (an explicit owner, "manually").
func (w *world) setRow(t *testing.T, state string) {
	t.Helper()
	ctx := context.Background()
	if _, err := w.pool.Exec(ctx, `DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, w.p, w.u); err != nil {
		t.Fatal(err)
	}
	role, manual, space := "member", false, false
	switch state {
	case "none":
		return
	case "manual":
		manual = true
	case "space":
		space = true
	case "both":
		manual, space = true, true
	case "owner":
		role, manual = "owner", true
	default:
		t.Fatalf("unknown state %q", state)
	}
	if _, err := w.pool.Exec(ctx, `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by, via_manual, via_space)
VALUES ($1, $2, $3, now(), $4, $5, $6)`, w.p, w.u, role, w.pOwner, manual, space); err != nil {
		t.Fatalf("seed row %s: %v", state, err)
	}
}

// row is u's membership row of P after an operation.
type row struct {
	exists         bool
	role           string
	manual, space  bool
	seesP, inSpace bool
}

func (w *world) read(t *testing.T, user uuid.UUID) row {
	t.Helper()
	var r row
	err := w.pool.QueryRow(context.Background(), `
SELECT role, via_manual, via_space FROM project_members WHERE project_id = $1 AND user_id = $2`,
		w.p, user).Scan(&r.role, &r.manual, &r.space)
	r.exists = err == nil
	if err := w.pool.QueryRow(context.Background(), `
SELECT EXISTS (SELECT 1 FROM skein_project_access_set($1) WHERE project_id = $2),
       EXISTS (SELECT 1 FROM space_members WHERE space_id = $3 AND user_id = $1)`,
		user, w.p, w.space).Scan(&r.seesP, &r.inSpace); err != nil {
		t.Fatal(err)
	}
	return r
}

func want(exists bool, role string, manual, space, sees bool) row {
	return row{exists: exists, role: role, manual: manual, space: space, seesP: sees, inSpace: true}
}

// Row 43 — the US-17 table, operation × initial state.
func TestMembership_US17Table(t *testing.T) {
	ctx := context.Background()
	none := row{inSpace: true}

	type cell struct {
		vis, state string
		want       row
		wantErr    error
	}
	ops := []struct {
		name  string
		run   func(t *testing.T, w *world) error
		cells []cell
	}{
		{"members→space", func(t *testing.T, w *world) error {
			return w.store.SetVisibility(ctx, w.p, spaces.VisibilitySpace, w.actor)
		}, []cell{
			{vis: "members", state: "none", want: want(true, "member", false, true, true)},
			{vis: "members", state: "manual", want: want(true, "member", true, true, true)},
			{vis: "members", state: "owner", want: want(true, "owner", true, true, true)},
		}},
		{"space→members", func(t *testing.T, w *world) error {
			return w.store.SetVisibility(ctx, w.p, spaces.VisibilityMembers, w.actor)
		}, []cell{
			{vis: "space", state: "space", want: none},
			{vis: "space", state: "both", want: want(true, "member", true, false, true)},
			{vis: "space", state: "owner", want: want(true, "owner", true, false, true)}, // E-28
		}},
		{"add to project", func(t *testing.T, w *world) error {
			_, err := w.pstore.UpsertMember(ctx, w.p, w.u, "member", w.pOwner, w.projectActor)
			return err
		}, []cell{
			{vis: "members", state: "none", want: want(true, "member", true, false, true)},
			// US-17: adding a participant to a project sets "manually", on a
			// row that had only "from space" too.
			{vis: "space", state: "space", want: want(true, "member", true, true, true)},
		}},
		{"appoint owner", func(t *testing.T, w *world) error {
			_, err := w.pstore.UpsertMember(ctx, w.p, w.u, "owner", w.pOwner, w.projectActor)
			return err
		}, []cell{
			{vis: "space", state: "space", want: want(true, "owner", true, true, true)},
			{vis: "members", state: "manual", want: want(true, "owner", true, false, true)},
		}},
		{"appoint member", func(t *testing.T, w *world) error {
			_, err := w.pstore.UpsertMember(ctx, w.p, w.u, "member", w.pOwner, w.projectActor)
			return err
		}, []cell{
			{vis: "space", state: "owner", want: want(true, "member", true, false, true)},
		}},
		{"remove from project", func(t *testing.T, w *world) error {
			return w.pstore.RemoveMember(ctx, w.p, w.u, w.projectActor)
		}, []cell{
			{vis: "members", state: "manual", want: none},
			{vis: "space", state: "space", want: want(true, "member", false, true, true), wantErr: projects.ErrMemberViaSpace},
			{vis: "space", state: "both", want: want(true, "member", true, true, true), wantErr: projects.ErrMemberViaSpace},
			{vis: "members", state: "owner", want: none}, // the last explicit owner but one: P stays under sOwner
		}},
		{"leave the space", func(t *testing.T, w *world) error {
			return w.store.Leave(ctx, w.space, w.u, "left_space", false, w.actor)
		}, []cell{
			{vis: "space", state: "both", want: row{}},
			{vis: "members", state: "owner", want: row{}},
		}},
	}
	for _, op := range ops {
		for _, c := range op.cells {
			t.Run(op.name+"/"+c.vis+"/"+c.state, func(t *testing.T) {
				w := newWorld(t, c.vis)
				w.setRow(t, c.state)
				err := op.run(t, w)
				switch {
				case c.wantErr != nil && !errors.Is(err, c.wantErr):
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				case c.wantErr == nil && err != nil:
					t.Fatalf("operation: %v", err)
				}
				if got := w.read(t, w.u); got != c.want {
					t.Errorf("u after the operation = %+v, want %+v", got, c.want)
				}
			})
		}
	}

	// US-17: an explicitly added participant keeps the
	// project when it is switched to `members` — the addition set "manually".
	t.Run("explicit add survives space→members", func(t *testing.T) {
		w := newWorld(t, "space")
		w.setRow(t, "space")
		if _, err := w.pstore.UpsertMember(ctx, w.p, w.u, "member", w.pOwner, w.projectActor); err != nil {
			t.Fatalf("explicit add: %v", err)
		}
		if err := w.store.SetVisibility(ctx, w.p, spaces.VisibilityMembers, w.actor); err != nil {
			t.Fatalf("space→members: %v", err)
		}
		if got, want := w.read(t, w.u), want(true, "member", true, false, true); got != want {
			t.Errorf("explicitly added member after space→members = %+v, want %+v", got, want)
		}
	})

	t.Run("join/space project gets the space source", func(t *testing.T) {
		w := newWorld(t, "space")
		if err := w.store.Leave(ctx, w.space, w.u, "left_space", false, w.actor); err != nil {
			t.Fatalf("leave first: %v", err)
		}
		if err := w.store.Join(ctx, w.space, w.u, w.actor); err != nil {
			t.Fatalf("join: %v", err)
		}
		if got, wantRow := w.read(t, w.u), want(true, "member", false, true, true); got != wantRow {
			t.Errorf("u after joining = %+v, want %+v", got, wantRow)
		}
	})
	t.Run("join/members project gets nothing", func(t *testing.T) {
		w := newWorld(t, "members")
		if err := w.store.Leave(ctx, w.space, w.u, "left_space", false, w.actor); err != nil {
			t.Fatalf("leave first: %v", err)
		}
		if err := w.store.Join(ctx, w.space, w.u, w.actor); err != nil {
			t.Fatalf("join: %v", err)
		}
		if got := w.read(t, w.u); got != none {
			t.Errorf("u after joining = %+v, want %+v", got, none)
		}
	})
	t.Run("implicit owner/sees every project without a row, cannot leave", func(t *testing.T) {
		w := newWorld(t, "members")
		if got, wantRow := w.read(t, w.sOwner), (row{seesP: true, inSpace: true}); got != wantRow {
			t.Errorf("space owner = %+v, want %+v", got, wantRow)
		}
		if err := w.store.Leave(ctx, w.space, w.sOwner, "left_space", false, w.actor); !errors.Is(err, spaces.ErrOwnerCannotLeave) {
			t.Errorf("owner leaving: %v, want ErrOwnerCannotLeave", err)
		}
	})
}

// Row 47 — one space-level event per mass operation, none per project.
func TestMembership_OneSpaceEventPerMassOperation(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, "space")
	second, _ := testsupport.SeedProjectInSpace(t, w.pool, w.pOwner, w.space, "mbr2")
	if _, err := w.pool.Exec(ctx, `UPDATE projects SET visibility = 'space' WHERE id = $1`, second); err != nil {
		t.Fatal(err)
	}
	if err := w.store.Leave(ctx, w.space, w.u, "left_space", false, w.actor); err != nil {
		t.Fatalf("leave: %v", err)
	}

	count := func(t *testing.T) (spaceEvents int, projects []string, memberEvents int) {
		t.Helper()
		if err := w.pool.QueryRow(ctx, `
SELECT count(*) FROM events WHERE entity_type = 'space' AND entity_id = $1`, w.space.String()).Scan(&spaceEvents); err != nil {
			t.Fatal(err)
		}
		if err := w.pool.QueryRow(ctx, `
SELECT count(*) FROM events WHERE project_id = ANY($1) AND type LIKE 'project.member%'`,
			[]uuid.UUID{w.p, second}).Scan(&memberEvents); err != nil {
			t.Fatal(err)
		}
		return spaceEvents, nil, memberEvents
	}
	base, _, baseMember := count(t)

	steps := []struct {
		name string
		run  func() error
		// projects the event must list
		want []uuid.UUID
	}{
		{"join", func() error { return w.store.Join(ctx, w.space, w.u, w.actor) }, []uuid.UUID{w.p, second}},
		{"leave", func() error { return w.store.Leave(ctx, w.space, w.u, "left_space", false, w.actor) }, []uuid.UUID{w.p, second}},
		{"visibility", func() error { return w.store.SetVisibility(ctx, w.p, spaces.VisibilityMembers, w.actor) }, []uuid.UUID{w.p}},
	}
	for i, s := range steps {
		if err := s.run(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		n, _, m := count(t)
		if n != base+i+1 {
			t.Errorf("%s: space events = %d, want exactly one more (%d)", s.name, n-base-i, 1)
		}
		if m != baseMember {
			t.Errorf("%s: %d project membership events, want none", s.name, m-baseMember)
		}
		var listed []string
		if err := w.pool.QueryRow(ctx, `
SELECT ARRAY(SELECT jsonb_array_elements_text(payload->'projects') ORDER BY 1)
  FROM events WHERE entity_type = 'space' AND entity_id = $1 ORDER BY id DESC LIMIT 1`,
			w.space.String()).Scan(&listed); err != nil {
			t.Fatal(err)
		}
		wantIDs := make([]string, len(s.want))
		for j, id := range s.want {
			wantIDs[j] = id.String()
		}
		sort.Strings(wantIDs)
		if len(listed) != len(wantIDs) {
			t.Errorf("%s: event lists %v, want %v", s.name, listed, wantIDs)
			continue
		}
		for j := range listed {
			if listed[j] != wantIDs[j] {
				t.Errorf("%s: event lists %v, want %v", s.name, listed, wantIDs)
				break
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Row 45 — the consequences of a loss of access, cause by cause
// ---------------------------------------------------------------------------
//
// sk-674.18 T018/T037, matrix row 45 (SC-022, SC-010, US-7, E-29, E-14).
// The reference is INV-12 of the spec, written before the code: for every
// cause — leaving, being removed, removal from the project, space → members,
// a change of the space's owner, the admin demoted — the user who stopped
// seeing a project is left with no assignment on its issues in a status
// other than closed (a closed issue keeps him), no claim of his or his
// agents, no watch of its issues or of it, no reminder; the projects he
// still sees, the projects of his other spaces and his own spaces are not
// touched; a demoted admin stays the owner of the common space.

// lossProject is one project of the row with the user's stake in it.
type lossProject struct {
	id                         uuid.UUID
	open, closed, locked, w, r string
}

// lossStake gives user, on project p, an open and a closed issue assigned
// to him, an issue claimed by his agent, a watch on an issue and on the
// project and a reminder.
func lossStake(t *testing.T, pool *pgxpool.Pool, p uuid.UUID, prefix string, owner, user uuid.UUID, username string) lossProject {
	t.Helper()
	ctx := context.Background()
	lp := lossProject{id: p, open: prefix + "-11", closed: prefix + "-12", locked: prefix + "-13", w: prefix + "-14", r: prefix + "-15"}
	testsupport.SeedIssue(t, pool, p, owner, lp.open, "loss open", "open", 2)
	testsupport.SeedIssue(t, pool, p, owner, lp.closed, "loss closed", "closed", 2)
	testsupport.SeedIssue(t, pool, p, owner, lp.locked, "loss claimed", "in_progress", 2)
	testsupport.SeedIssue(t, pool, p, owner, lp.w, "loss watched", "open", 2)
	testsupport.SeedIssue(t, pool, p, owner, lp.r, "loss reminded", "open", 2)
	for _, id := range []string{lp.open, lp.closed, lp.locked} {
		testsupport.SetAssignee(t, pool, id, username)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE issues SET locked_by = 'agent-session-of-user', locked_by_user_id = $2, locked_at = now() WHERE id = $1`, []any{lp.locked, user}},
		{`INSERT INTO issue_watchers (issue_id, user_id, reasons) VALUES ($1, $2, '{manual}')`, []any{lp.w, user}},
		{`INSERT INTO project_watchers (project_id, user_id) VALUES ($1, $2)`, []any{p, user}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("stake: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM project_watchers WHERE project_id = $1`, p)
	})
	testsupport.SeedReminder(t, pool, lp.r, user)
	return lp
}

// lossState is the user's stake in a project now.
type lossState struct {
	OpenAssigned, ClosedAssigned, Claimed bool
	IssueWatches, ProjectWatches, Reminds int
}

func readLoss(t *testing.T, pool *pgxpool.Pool, p lossProject, user uuid.UUID) lossState {
	t.Helper()
	var s lossState
	if err := pool.QueryRow(context.Background(), `
SELECT (SELECT assignee IS NOT NULL FROM issues WHERE id = $1),
       (SELECT assignee IS NOT NULL FROM issues WHERE id = $2),
       (SELECT locked_by_user_id IS NOT NULL FROM issues WHERE id = $3),
       (SELECT count(*) FROM issue_watchers w JOIN issues i ON i.id = w.issue_id WHERE i.project_id = $4 AND w.user_id = $5),
       (SELECT count(*) FROM project_watchers WHERE project_id = $4 AND user_id = $5),
       (SELECT count(*) FROM reminders r JOIN issues i ON i.id = r.issue_id WHERE i.project_id = $4 AND r.user_id = $5)`,
		p.open, p.closed, p.locked, p.id, user).Scan(&s.OpenAssigned, &s.ClosedAssigned, &s.Claimed,
		&s.IssueWatches, &s.ProjectWatches, &s.Reminds); err != nil {
		t.Fatalf("read the stake: %v", err)
	}
	return s
}

// untouched is the stake as seeded; revoked is INV-12 applied (the closed
// issue keeps its assignee).
var (
	untouched = lossState{OpenAssigned: true, ClosedAssigned: true, Claimed: true, IssueWatches: 1, ProjectWatches: 1, Reminds: 1}
	revoked   = lossState{ClosedAssigned: true}
)

// lossWorld: the user u in space S (owner sOwner) with a project lost by the
// cause and a project kept, a participant of another space T with a project
// there, and the owner of a space of his own with a project in it.
type lossWorld struct {
	pool             *pgxpool.Pool
	u, sOwner, other uuid.UUID
	uName            string
	s, own           uuid.UUID
	lost, kept       lossProject
	elsewhere, owned lossProject
}

// spaceRow sets the source of u's membership row on p: "space", "manual"
// or "none".
func spaceRow(t *testing.T, pool *pgxpool.Pool, p, u uuid.UUID, src string) {
	t.Helper()
	ctx := context.Background()
	switch src {
	case "space":
		if _, err := pool.Exec(ctx, `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by, via_manual, via_space)
VALUES ($1, $2, 'member', now(), $2, false, true)`, p, u); err != nil {
			t.Fatalf("space row: %v", err)
		}
	case "manual":
		if _, err := pool.Exec(ctx, `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by) VALUES ($1, $2, 'member', now(), $2)`, p, u); err != nil {
			t.Fatalf("manual row: %v", err)
		}
	}
}

// newLossWorld builds the world; lostVis/lostRow and keptVis/keptRow set the
// visibility of the two projects of S and u's row on them.
func newLossWorld(t *testing.T, lostVis, lostRow, keptVis, keptRow string, uOwnsS bool) *lossWorld {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	w := &lossWorld{pool: pool}
	w.u, w.uName = testsupport.SeedUser(t, pool, "")
	w.sOwner, _ = testsupport.SeedUser(t, pool, "")
	w.other, _ = testsupport.SeedUser(t, pool, "")
	if uOwnsS {
		w.s = testsupport.SeedSpace(t, pool, w.u)
		testsupport.AddSpaceMember(t, pool, w.s, w.sOwner)
	} else {
		w.s = testsupport.SeedSpace(t, pool, w.sOwner)
		testsupport.AddSpaceMember(t, pool, w.s, w.u)
	}
	tSpace := testsupport.SeedSpace(t, pool, w.other)
	testsupport.AddSpaceMember(t, pool, tSpace, w.u)
	w.own = testsupport.SeedSpace(t, pool, w.u)

	seed := func(space, owner uuid.UUID, vis, row string) lossProject {
		p, prefix := testsupport.SeedProjectInSpace(t, pool, owner, space, "los")
		if _, err := pool.Exec(ctx, `UPDATE projects SET visibility = $2 WHERE id = $1`, p, vis); err != nil {
			t.Fatalf("visibility: %v", err)
		}
		spaceRow(t, pool, p, w.u, row)
		return lossStake(t, pool, p, prefix, owner, w.u, w.uName)
	}
	w.lost = seed(w.s, w.sOwner, lostVis, lostRow)
	w.kept = seed(w.s, w.sOwner, keptVis, keptRow)
	w.elsewhere = seed(tSpace, w.other, "space", "space")
	w.owned = seed(w.own, w.u, "members", "none")
	return w
}

// check asserts the row: the lost project revoked, everything else as it was.
func (w *lossWorld) check(t *testing.T, keptToo bool) {
	t.Helper()
	if got := readLoss(t, w.pool, w.lost, w.u); got != revoked {
		t.Errorf("the lost project: %+v, want %+v", got, revoked)
	}
	wantKept := untouched
	if keptToo {
		wantKept = revoked
	}
	if got := readLoss(t, w.pool, w.kept, w.u); got != wantKept {
		t.Errorf("the second project of the space: %+v, want %+v", got, wantKept)
	}
	if got := readLoss(t, w.pool, w.elsewhere, w.u); got != untouched {
		t.Errorf("the project of another space: %+v, want it untouched", got)
	}
	if got := readLoss(t, w.pool, w.owned, w.u); got != untouched {
		t.Errorf("the project of his own space: %+v, want it untouched", got)
	}
	var owner uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `SELECT owner_id FROM spaces WHERE id = $1`, w.own).Scan(&owner); err != nil {
		t.Fatalf("read his space: %v", err)
	}
	if owner != w.u {
		t.Errorf("his own space changed owner")
	}
}

// ownCommon makes user the owner of the common space for the test (created
// when the database has none) and restores it afterwards.
func ownCommon(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var common, prev uuid.UUID
	err := pool.QueryRow(ctx, `SELECT id, owner_id FROM spaces WHERE kind = 'common'`).Scan(&common, &prev)
	if err != nil {
		common = testsupport.SeedSpaceOfKind(t, pool, user, "common")
		return common
	}
	var wasMember bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM space_members WHERE space_id = $1 AND user_id = $2)`,
		common, user).Scan(&wasMember); err != nil {
		t.Fatalf("read the common space: %v", err)
	}
	swap := func(to uuid.UUID) error {
		for _, st := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO space_members (space_id, user_id, role) VALUES ($1, $2, 'member') ON CONFLICT DO NOTHING`, []any{common, to}},
			{`UPDATE space_members SET role = 'member' WHERE space_id = $1 AND role = 'owner'`, []any{common}},
			{`UPDATE space_members SET role = 'owner' WHERE space_id = $1 AND user_id = $2`, []any{common, to}},
			{`UPDATE spaces SET owner_id = $2 WHERE id = $1`, []any{common, to}},
		} {
			if _, err := pool.Exec(context.Background(), st.sql, st.args...); err != nil {
				return err
			}
		}
		return nil
	}
	if err := swap(user); err != nil {
		t.Fatalf("own the common space: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if err := swap(prev); err != nil {
			t.Errorf("restore the common space: %v", err)
		}
		if !wasMember {
			_, _ = pool.Exec(ctx, `DELETE FROM space_members WHERE space_id = $1 AND user_id = $2`, common, user)
		}
	})
	return common
}

func TestMembership_LossOfAccessByCause(t *testing.T) {
	ctx := context.Background()

	t.Run("leaving the space", func(t *testing.T) {
		w := newLossWorld(t, "space", "space", "members", "manual", false)
		if err := spaces.NewStore(w.pool).Leave(ctx, w.s, w.u, authz.RevokeLeftSpace, false,
			spaces.Actor{UserID: w.u, Username: w.uName, Type: "human"}); err != nil {
			t.Fatalf("leave: %v", err)
		}
		w.check(t, true)
	})

	t.Run("being removed from the space", func(t *testing.T) {
		w := newLossWorld(t, "space", "space", "members", "manual", false)
		if err := spaces.NewStore(w.pool).Leave(ctx, w.s, w.u, authz.RevokeRemovedFromSpace, true,
			spaces.Actor{UserID: w.sOwner, Username: "s-owner", Type: "human"}); err != nil {
			t.Fatalf("remove: %v", err)
		}
		w.check(t, true)
	})

	t.Run("removal from the project", func(t *testing.T) {
		w := newLossWorld(t, "members", "manual", "space", "space", false)
		if err := projects.NewStore(w.pool).RemoveMember(ctx, w.lost.id, w.u,
			projects.Actor{UserID: w.sOwner, Username: "s-owner", Role: "human"}); err != nil {
			t.Fatalf("remove from the project: %v", err)
		}
		w.check(t, false)
	})

	t.Run("space to members", func(t *testing.T) {
		w := newLossWorld(t, "space", "space", "members", "manual", false)
		if err := spaces.NewStore(w.pool).SetVisibility(ctx, w.lost.id, spaces.VisibilityMembers,
			spaces.Actor{UserID: w.sOwner, Username: "s-owner", Type: "human"}); err != nil {
			t.Fatalf("visibility: %v", err)
		}
		w.check(t, false)
	})

	t.Run("a change of the space owner (E-29)", func(t *testing.T) {
		// u owns S: the `members` project without his row is his only as the
		// implicit owner; the `space` one he keeps as a participant.
		w := newLossWorld(t, "members", "none", "space", "space", true)
		admin, adminName := testsupport.SeedUser(t, w.pool, "admin")
		svc := spaces.NewService(w.pool, spaces.Options{Creation: spaces.CreationAll})
		var short string
		if err := w.pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, w.s).Scan(&short); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ChangeOwner(ctx, spaces.Caller{UserID: admin, Username: adminName,
			Actor: spaces.Actor{UserID: admin, Username: adminName, Type: "human"}}, short, w.sOwner); err != nil {
			t.Fatalf("change the owner: %v", err)
		}
		w.check(t, false)
		var member bool
		if err := w.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM space_members WHERE space_id = $1 AND user_id = $2)`,
			w.s, w.u).Scan(&member); err != nil {
			t.Fatal(err)
		}
		if !member {
			t.Errorf("the previous owner is no longer a participant of the space")
		}
	})

	t.Run("the admin demoted (E-14)", func(t *testing.T) {
		// u is an admin outside S: he sees its projects only by the admin's
		// reach — the demotion takes both away (they are «kept» by nothing).
		w := newLossWorld(t, "space", "none", "members", "none", false)
		if _, err := w.pool.Exec(ctx, `DELETE FROM space_members WHERE space_id = $1 AND user_id = $2`, w.s, w.u); err != nil {
			t.Fatal(err)
		}
		if _, err := w.pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE id = $1`, w.u); err != nil {
			t.Fatal(err)
		}
		common := ownCommon(t, w.pool, w.u)
		// Another active admin, so the demotion is not the last admin's.
		other, _ := testsupport.SeedUser(t, w.pool, "admin")
		ust := users.NewStore(w.pool, 4, apikeys.New(w.pool), "local", "", nil)
		role := "user"
		if _, err := ust.UpdateUser(ctx, w.u, users.UpdateUserInput{Role: &role},
			users.Actor{UserID: other, Username: "other-admin", Role: "human"}); err != nil {
			t.Fatalf("demote: %v", err)
		}
		w.check(t, true)
		var owner uuid.UUID
		if err := w.pool.QueryRow(ctx, `SELECT owner_id FROM spaces WHERE id = $1`, common).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		if owner != w.u {
			t.Errorf("the demoted admin is no longer the owner of the common space")
		}
	})
}
