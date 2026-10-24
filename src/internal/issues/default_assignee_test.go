package issues

// default_assignee_test.go — sk-565, T029, test matrix rows 16–32 (US-1,
// US-2, US-3, US-5, US-6 with its "Composition norm", INV-1, INV-3, INV-5,
// INV-6, INV-9, INV-10, INV-11, D1, D4, D6, D9, SC-001, FOR-PLAN #1).
// References are verbatim from spec sk-565: assignee is the owner's username,
// the issue is accepted by them, status open, no lock (US-1); "under someone else's" has no
// assignee and is not accepted (US-6 item 4); an invisible ancestor counts as someone else's (item 2);
// someone else's is an assignee that does not denote the owner (item 3).
//
// P1 is set on the store's direct input: the MCP layer that sets it in
// production is T031. Nothing here depends on the subscription rule of D7
// (T030).

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// daEnv: Alice creates through her agent in Bob's project (Alice is a
// member); Carol owns a second project that Alice does not see unless a
// test adds her.
type daEnv struct {
	t      *testing.T
	pool   *pgxpool.Pool // traced
	plain  *pgxpool.Pool // untraced, for out-of-band writes from hooks
	tr     *statementTracer
	store  *Store
	alice  uuid.UUID
	bob    uuid.UUID
	carol  uuid.UUID
	aName  string
	aEmail string
	bName  string
	pid    uuid.UUID // Bob's project, the creation project
	prefix string
	pid2   uuid.UUID // Carol's project
	pref2  string
}

func newDAEnv(t *testing.T) *daEnv {
	t.Helper()
	pool, tr := newStatementTracedPool(t)
	plain := newTestPool(t)
	e := &daEnv{t: t, pool: pool, plain: plain, tr: tr, store: NewStore(pool)}
	e.alice = seedUser(t, plain)
	e.bob = seedUser(t, plain)
	e.carol = seedUser(t, plain)
	e.aName = readUsername(t, plain, e.alice)
	e.bName = readUsername(t, plain, e.bob)
	if err := plain.QueryRow(context.Background(),
		`SELECT email FROM users WHERE id = $1`, e.alice).Scan(&e.aEmail); err != nil {
		t.Fatalf("read alice email: %v", err)
	}
	e.pid, e.prefix = seedProject(t, plain, e.bob)
	addMember(t, plain, e.pid, e.alice, e.bob)
	e.pid2, e.pref2 = seedProject(t, plain, e.carol)
	return e
}

// agentOf is the actor an agent of the user acts as: the actor label is
// the agent's, not the person's username (D6 trap).
func agentOf(user uuid.UUID) Actor {
	return Actor{UserID: user, Username: "agent:ska_" + user.String()[:8], Role: "agent"}
}

func (e *daEnv) setFlag(user uuid.UUID, on bool) {
	e.t.Helper()
	if _, err := e.plain.Exec(context.Background(), `
INSERT INTO user_notification_prefs (user_id, assign_agent_issues_to_me, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (user_id) DO UPDATE SET assign_agent_issues_to_me = EXCLUDED.assign_agent_issues_to_me`,
		user, on); err != nil {
		e.t.Fatalf("set flag: %v", err)
	}
}

func (e *daEnv) caller(user uuid.UUID, role string) *DependenciesCaller {
	return &DependenciesCaller{UserID: user, Role: role}
}

// mcpInput is an MCP-shaped create input from Alice: P1 on, no assignee.
func (e *daEnv) mcpInput(title string) CreateIssueInput {
	return CreateIssueInput{
		Title: title, Type: "task", Priority: 2,
		Source:                 SourceAgentDirected,
		AssignCreatorByDefault: true,
		DependenciesCaller:     e.caller(e.alice, "user"),
	}
}

func (e *daEnv) createAs(ctx context.Context, creator uuid.UUID, project uuid.UUID, prefix, mode string, in CreateIssueInput) (*IssueDetail, error) {
	source := in.Source
	if source == "" {
		source = SourceHuman
	}
	in.Source = source
	return e.store.CreateIssue(ctx, project, prefix, mode, in, creator, agentOf(creator))
}

// create is Alice creating in Bob's project.
func (e *daEnv) create(in CreateIssueInput) *IssueDetail {
	e.t.Helper()
	dto, err := e.createAs(e.t.Context(), e.alice, e.pid, e.prefix, "counter", in)
	if err != nil {
		e.t.Fatalf("CreateIssue %q: %v", in.Title, err)
	}
	return dto
}

// seedAssigned creates, without P1, an issue whose assignee is exactly the
// given entry ("" stores an empty string), optionally under a parent, in
// the given project, by the given creator.
func (e *daEnv) seedAssigned(creator, project uuid.UUID, prefix, assignee string, parent *string) string {
	e.t.Helper()
	in := CreateIssueInput{Title: "seed", Type: "task", Priority: 2, ParentID: parent,
		DependenciesCaller: e.caller(creator, "user")}
	a := assignee
	in.Assignee = &a
	dto, err := e.store.CreateIssue(e.t.Context(), project, prefix, "counter", in, creator, HumanActor(creator, "seed"))
	if err != nil {
		e.t.Fatalf("seed issue (assignee %q): %v", assignee, err)
	}
	return dto.ID
}

// linkParent writes a parent-child edge child → parent directly: the
// fixtures need hierarchies across projects that no single caller sees.
func (e *daEnv) linkParent(child, parent string) {
	e.t.Helper()
	if _, err := e.plain.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'parent-child', '{}'::jsonb, 1, $3, now())`, child, parent, e.carol); err != nil {
		e.t.Fatalf("link %s → %s: %v", child, parent, err)
	}
}

type daState struct {
	assignee   *string
	acceptedAt *time.Time
	acceptedBy *uuid.UUID
	status     string
	lockedBy   *string
}

func (e *daEnv) state(id string) daState {
	e.t.Helper()
	var s daState
	if err := e.plain.QueryRow(context.Background(), `
SELECT assignee, accepted_at, accepted_by, status, locked_by FROM issues WHERE id = $1`, id).
		Scan(&s.assignee, &s.acceptedAt, &s.acceptedBy, &s.status, &s.lockedBy); err != nil {
		e.t.Fatalf("read %s: %v", id, err)
	}
	return s
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return "\"" + *p + "\""
}

// wantOwner: US-1 — assignee is exactly the creator's username, accepted by
// the creator, open, not locked.
func (e *daEnv) wantOwner(t *testing.T, id string, ownerName string, owner uuid.UUID) {
	t.Helper()
	s := e.state(id)
	if s.assignee == nil || *s.assignee != ownerName {
		t.Errorf("%s: assignee = %s, want %q (the creator's username)", id, str(s.assignee), ownerName)
	}
	if s.acceptedAt == nil {
		t.Errorf("%s: accepted_at is empty, want stamped", id)
	}
	if s.acceptedBy == nil || *s.acceptedBy != owner {
		t.Errorf("%s: accepted_by = %v, want the creator %s", id, s.acceptedBy, owner)
	}
	if s.status != StatusOpen {
		t.Errorf("%s: status = %q, want open", id, s.status)
	}
	if s.lockedBy != nil {
		t.Errorf("%s: locked_by = %q, want no lock", id, *s.lockedBy)
	}
}

// wantUnassigned: no assignee and not accepted.
func (e *daEnv) wantUnassigned(t *testing.T, id string) {
	t.Helper()
	s := e.state(id)
	if s.assignee != nil && *s.assignee != "" {
		t.Errorf("%s: assignee = %s, want none", id, str(s.assignee))
	}
	if s.acceptedAt != nil || s.acceptedBy != nil {
		t.Errorf("%s: accepted_at = %v, accepted_by = %v, want both empty", id, s.acceptedAt, s.acceptedBy)
	}
}

func ptrTo(s string) *string { return &s }

// ---------------------------------------------------------------------------
// Rows 16–23: the rule on visible chains
// ---------------------------------------------------------------------------

// TestDefaultAssignee_OwnerByUsernameAndAccepted — row 16.
func TestDefaultAssignee_OwnerByUsernameAndAccepted(t *testing.T) {
	e := newDAEnv(t)
	for _, tc := range []struct {
		name    string
		source  string
		prefRow bool
	}{
		{"directed, no prefs row", SourceAgentDirected, false},
		{"directed, prefs row true", SourceAgentDirected, true},
		{"autonomous, no prefs row", SourceAgentAutonomous, false},
		{"autonomous, prefs row true", SourceAgentAutonomous, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.prefRow {
				e.setFlag(e.alice, true)
			} else {
				if _, err := e.plain.Exec(context.Background(), `DELETE FROM user_notification_prefs WHERE user_id = $1`, e.alice); err != nil {
					t.Fatalf("drop prefs row: %v", err)
				}
			}
			in := e.mcpInput("default " + tc.name)
			in.Source = tc.source
			dto := e.create(in)
			e.wantOwner(t, dto.ID, e.aName, e.alice)
		})
	}
}

// TestDefaultAssignee_SettingIsTheCreators — row 17.
func TestDefaultAssignee_SettingIsTheCreators(t *testing.T) {
	e := newDAEnv(t)

	e.setFlag(e.alice, false)
	e.setFlag(e.bob, true)
	off := e.create(e.mcpInput("alice off, project owner on"))
	e.wantUnassigned(t, off.ID)

	e.setFlag(e.alice, true)
	e.setFlag(e.bob, false)
	on := e.create(e.mcpInput("alice on, project owner off"))
	e.wantOwner(t, on.ID, e.aName, e.alice)
}

// TestDefaultAssignee_DecidedByP1NotBySource — row 18.
func TestDefaultAssignee_DecidedByP1NotBySource(t *testing.T) {
	e := newDAEnv(t)
	e.setFlag(e.alice, true)

	human := e.mcpInput("P1 on, source human")
	human.Source = SourceHuman
	dto := e.create(human)
	e.wantOwner(t, dto.ID, e.aName, e.alice)

	agent := e.mcpInput("P1 off, source agent_directed")
	agent.AssignCreatorByDefault = false
	dto = e.create(agent)
	e.wantUnassigned(t, dto.ID)
}

// TestDefaultAssignee_ExplicitAssigneeKeptVerbatim — row 19.
func TestDefaultAssignee_ExplicitAssigneeKeptVerbatim(t *testing.T) {
	e := newDAEnv(t)
	bobTask := e.seedAssigned(e.bob, e.pid, e.prefix, e.bName, nil)
	for _, flag := range []bool{true, false} {
		e.setFlag(e.alice, flag)
		for _, explicit := range []string{e.bName, "external-bot", e.aEmail} {
			for _, under := range []*string{nil, &bobTask} {
				in := e.mcpInput("explicit")
				in.Assignee = ptrTo(explicit)
				in.ParentID = under
				dto := e.create(in)
				s := e.state(dto.ID)
				if s.assignee == nil || *s.assignee != explicit {
					t.Errorf("flag=%v explicit=%q under=%v: assignee = %s, want %q verbatim",
						flag, explicit, under != nil, str(s.assignee), explicit)
				}
			}
		}
	}
}

// TestDefaultAssignee_StandingIsNeverAssigned — row 20.
func TestDefaultAssignee_StandingIsNeverAssigned(t *testing.T) {
	e := newDAEnv(t)
	e.setFlag(e.alice, true)
	ownersTask := e.seedAssigned(e.alice, e.pid, e.prefix, e.aName, nil)

	root := e.mcpInput("standing root")
	root.Standing = true
	dto := e.create(root)
	e.wantUnassigned(t, dto.ID)

	child := e.mcpInput("standing under the owner's task")
	child.Standing = true
	child.ParentID = &ownersTask
	dto = e.create(child)
	e.wantUnassigned(t, dto.ID)
}

// TestDefaultAssignee_ForeignIsAnIdentityComparison — row 21.
func TestDefaultAssignee_ForeignIsAnIdentityComparison(t *testing.T) {
	e := newDAEnv(t)
	e.setFlag(e.alice, true)

	for _, owner := range []string{e.aName, e.aEmail, e.alice.String()} {
		parent := e.seedAssigned(e.alice, e.pid, e.prefix, owner, nil)
		in := e.mcpInput("under the owner as " + owner)
		in.ParentID = &parent
		e.wantOwner(t, e.create(in).ID, e.aName, e.alice)
	}

	// The parent assigned by Bob's own setting: Bob's agent, P1 on.
	e.setFlag(e.bob, true)
	byBobsSetting, err := e.createAs(t.Context(), e.bob, e.pid, e.prefix, "counter", CreateIssueInput{
		Title: "bob's default", Type: "task", Priority: 2, Source: SourceAgentDirected,
		AssignCreatorByDefault: true, DependenciesCaller: e.caller(e.bob, "user"),
	})
	if err != nil {
		t.Fatalf("Bob's create: %v", err)
	}
	if s := e.state(byBobsSetting.ID); s.assignee == nil || *s.assignee != e.bName {
		t.Fatalf("precondition: Bob's default did not assign Bob: %s", str(s.assignee))
	}
	foreign := map[string]string{
		"bob":                       e.seedAssigned(e.bob, e.pid, e.prefix, e.bName, nil),
		"external-bot":              e.seedAssigned(e.bob, e.pid, e.prefix, "external-bot", nil),
		"unregistered e-mail":       e.seedAssigned(e.bob, e.pid, e.prefix, "ghost-"+e.alice.String()[:8]+"@nowhere.test", nil),
		"assigned by bob's setting": byBobsSetting.ID,
	}
	for name, parent := range foreign {
		in := e.mcpInput("under " + name)
		in.ParentID = &parent
		e.wantUnassigned(t, e.create(in).ID)
	}
}

// TestDefaultAssignee_ParentDeclaredEitherWay — row 22.
func TestDefaultAssignee_ParentDeclaredEitherWay(t *testing.T) {
	e := newDAEnv(t)
	e.setFlag(e.alice, true)
	bobTask := e.seedAssigned(e.bob, e.pid, e.prefix, e.bName, nil)

	viaParentID := e.mcpInput("parentId under bob")
	viaParentID.ParentID = &bobTask
	e.wantUnassigned(t, e.create(viaParentID).ID)

	viaEdge := e.mcpInput("inline parent-child under bob")
	viaEdge.Dependencies = []DependencyEdgeInput{{ToID: bobTask, Type: "parent-child"}}
	e.wantUnassigned(t, e.create(viaEdge).ID)
}

// TestDefaultAssignee_NearestAssignedAncestorDecides — row 23.
func TestDefaultAssignee_NearestAssignedAncestorDecides(t *testing.T) {
	e := newDAEnv(t)
	e.setFlag(e.alice, true)

	// «empty → Bob»
	bobTop := e.seedAssigned(e.bob, e.pid, e.prefix, e.bName, nil)
	emptyOverBob := e.seedAssigned(e.bob, e.pid, e.prefix, "", &bobTop)
	in := e.mcpInput("empty → bob")
	in.ParentID = &emptyOverBob
	e.wantUnassigned(t, e.create(in).ID)

	// «empty → owner → Bob»
	ownerOverBob := e.seedAssigned(e.bob, e.pid, e.prefix, e.aName, &bobTop)
	emptyOverOwner := e.seedAssigned(e.bob, e.pid, e.prefix, "", &ownerOverBob)
	in = e.mcpInput("empty → owner → bob")
	in.ParentID = &emptyOverOwner
	e.wantOwner(t, e.create(in).ID, e.aName, e.alice)

	// «empty → empty → no ancestors»
	emptyTop := e.seedAssigned(e.bob, e.pid, e.prefix, "", nil)
	emptyMid := e.seedAssigned(e.bob, e.pid, e.prefix, "", &emptyTop)
	in = e.mcpInput("empty → empty → none")
	in.ParentID = &emptyMid
	e.wantOwner(t, e.create(in).ID, e.aName, e.alice)
}

// ---------------------------------------------------------------------------
// Rows 24–26: the visibility boundary
// ---------------------------------------------------------------------------

// boundaryLayout: P in the creation project, unassigned unless pAssignee is
// set; G, P's parent, in Carol's project with gAssignee; above G optionally
// H (Carol's project) with hAssignee.
func (e *daEnv) boundaryLayout(pAssignee, gAssignee string, hAssignee *string) (p, g string) {
	e.t.Helper()
	var gParent *string
	if hAssignee != nil {
		h := e.seedAssigned(e.carol, e.pid2, e.pref2, *hAssignee, nil)
		gParent = &h
	}
	g = e.seedAssigned(e.carol, e.pid2, e.pref2, gAssignee, gParent)
	p = e.seedAssigned(e.bob, e.pid, e.prefix, pAssignee, nil)
	e.linkParent(p, g)
	return p, g
}

// TestDefaultAssignee_VisibilityBoundary — row 24.
func TestDefaultAssignee_VisibilityBoundary(t *testing.T) {
	e := newDAEnv(t)
	e.setFlag(e.alice, true)

	cases := []struct {
		name      string
		pAssignee string
		gAssignee string
		hAssignee *string
		wantOwner bool
	}{
		{"(a) invisible G on the owner", "", e.aName, nil, false},
		{"(b) invisible G empty, its parent on the owner", "", "", &e.aName, false},
		{"(c) invisible G on Bob", "", e.bName, nil, false},
		{"(d) P on the owner, invisible G on Bob above", e.aName, e.bName, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := e.boundaryLayout(tc.pAssignee, tc.gAssignee, tc.hAssignee)
			in := e.mcpInput(tc.name)
			in.ParentID = &p
			dto := e.create(in)
			if tc.wantOwner {
				e.wantOwner(t, dto.ID, e.aName, e.alice)
			} else {
				e.wantUnassigned(t, dto.ID)
			}
		})
	}
}

// TestDefaultAssignee_InvisibleAncestorDataIsNotRead — row 25.
func TestDefaultAssignee_InvisibleAncestorDataIsNotRead(t *testing.T) {
	e := newDAEnv(t)
	e.setFlag(e.alice, true)
	for _, tc := range []struct {
		name      string
		gAssignee string
		hAssignee *string
	}{
		{"24(a)", e.aName, nil},
		{"24(b)", "", &e.aName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, g := e.boundaryLayout("", tc.gAssignee, tc.hAssignee)
			in := e.mcpInput(tc.name)
			in.ParentID = &p
			var created *IssueDetail
			stmts := e.tr.record(func() { created = e.create(in) })
			e.wantUnassigned(t, created.ID)

			touchingG := 0
			for _, s := range stmts {
				hasG := false
				for _, a := range s.args {
					if v, ok := a.(string); ok && v == g {
						hasG = true
					}
				}
				if !hasG {
					continue
				}
				touchingG++
				if strings.Contains(s.sql, "assignee") || strings.Contains(s.sql, "dependencies") {
					t.Errorf("a statement with the invisible G (%s) as argument reads its assignee or parent link:\n%s", g, s.sql)
				}
			}
			if touchingG == 0 {
				t.Errorf("no executed statement had G as argument — the walk never reached the boundary, the check above proves nothing")
			}
		})
	}
}

// TestDefaultAssignee_AdminBoundary — row 26.
func TestDefaultAssignee_AdminBoundary(t *testing.T) {
	e := newDAEnv(t)
	admin := seedUser(t, e.plain)
	adminName := readUsername(t, e.plain, admin)
	if _, err := e.plain.Exec(context.Background(), `UPDATE users SET role = 'admin' WHERE id = $1`, admin); err != nil {
		t.Fatalf("make admin: %v", err)
	}
	addMember(t, e.plain, e.pid, admin, e.bob)
	e.setFlag(admin, true)

	adminInput := func(title string, parent *string) CreateIssueInput {
		return CreateIssueInput{Title: title, Type: "task", Priority: 2, Source: SourceAgentDirected,
			AssignCreatorByDefault: true, ParentID: parent, DependenciesCaller: e.caller(admin, "admin")}
	}

	t.Run("external admin stops at the boundary", func(t *testing.T) {
		// Since sk-674.18 T008 visibility reads the caller from users, so
		// the external collaborator is a real row (an external admin cannot
		// be stored: CHECK users_external_not_admin); the admin claim rides
		// on the request only — and grants nothing (D-2).
		ext := seedUser(t, e.plain)
		extName := readUsername(t, e.plain, ext)
		if _, err := e.plain.Exec(context.Background(), `UPDATE users SET is_external = true WHERE id = $1`, ext); err != nil {
			t.Fatalf("make external: %v", err)
		}
		addMember(t, e.plain, e.pid, ext, e.bob)
		e.setFlag(ext, true)
		p, _ := e.boundaryLayout("", extName, nil)
		ctx := auth.WithIsExternal(auth.WithUserRole(t.Context(), "admin"), true)
		in := CreateIssueInput{Title: "external", Type: "task", Priority: 2, Source: SourceAgentDirected,
			AssignCreatorByDefault: true, ParentID: &p, DependenciesCaller: e.caller(ext, "admin")}
		dto, err := e.createAs(ctx, ext, e.pid, e.prefix, "counter", in)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		e.wantUnassigned(t, dto.ID)
	})

	t.Run("internal admin sees G on themselves", func(t *testing.T) {
		p, _ := e.boundaryLayout("", adminName, nil)
		dto, err := e.createAs(t.Context(), admin, e.pid, e.prefix, "counter", adminInput("internal", &p))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		e.wantOwner(t, dto.ID, adminName, admin)
	})

	t.Run("internal admin in a project they are not a member of", func(t *testing.T) {
		dto, err := e.createAs(t.Context(), admin, e.pid2, e.pref2, "counter", adminInput("non-member", nil))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		e.wantOwner(t, dto.ID, adminName, admin)
	})
}

// ---------------------------------------------------------------------------
// Rows 27–29: epics, rollback, one value everywhere
// ---------------------------------------------------------------------------

func (e *daEnv) epic(ctx context.Context, epic CreateIssueInput, children ...CreateIssueInput) (*IssueDetail, []*IssueDetail, error) {
	epic.Type = TypeEpic
	return e.store.CreateEpicWithChildren(ctx, e.pid, e.prefix, "counter", epic, children,
		SourceAgentDirected, e.alice, agentOf(e.alice))
}

func (e *daEnv) mustEpic(epic CreateIssueInput, children ...CreateIssueInput) (*IssueDetail, []*IssueDetail) {
	e.t.Helper()
	ep, ch, err := e.epic(e.t.Context(), epic, children...)
	if err != nil {
		e.t.Fatalf("CreateEpicWithChildren %q: %v", epic.Title, err)
	}
	return ep, ch
}

// TestDefaultAssignee_EpicComposition — row 27.
func TestDefaultAssignee_EpicComposition(t *testing.T) {
	e := newDAEnv(t)
	e.setFlag(e.alice, true)
	kids := func() []CreateIssueInput {
		return []CreateIssueInput{e.mcpInput("child 1"), e.mcpInput("child 2")}
	}

	for _, foreign := range []string{e.bName, "external-bot"} {
		in := e.mcpInput("epic on " + foreign)
		in.Assignee = ptrTo(foreign)
		_, children := e.mustEpic(in, kids()...)
		for _, c := range children {
			e.wantUnassigned(t, c.ID)
		}
	}

	bobTask := e.seedAssigned(e.bob, e.pid, e.prefix, e.bName, nil)
	underBob := e.mustEpicUnder(&bobTask, kids())
	e.wantUnassigned(t, underBob.epic.ID)
	for _, c := range underBob.children {
		e.wantUnassigned(t, c.ID)
	}

	offEpic := e.mcpInput("epic without P1")
	offEpic.AssignCreatorByDefault = false
	ep, children := e.mustEpic(offEpic, kids()...)
	e.wantUnassigned(t, ep.ID)
	for _, c := range children {
		e.wantOwner(t, c.ID, e.aName, e.alice)
	}

	ep, children = e.mustEpic(e.mcpInput("default epic"), kids()...)
	e.wantOwner(t, ep.ID, e.aName, e.alice)
	for _, c := range children {
		e.wantOwner(t, c.ID, e.aName, e.alice)
	}

	// A later child of the epic under Bob, and a nested epic there.
	late := e.mcpInput("late child of the epic under bob")
	late.ParentID = &underBob.epic.ID
	e.wantUnassigned(t, e.create(late).ID)

	nested := e.mustEpicUnder(&underBob.epic.ID, kids())
	e.wantUnassigned(t, nested.epic.ID)
	for _, c := range nested.children {
		e.wantUnassigned(t, c.ID)
	}
}

type epicResult struct {
	epic     *IssueDetail
	children []*IssueDetail
}

func (e *daEnv) mustEpicUnder(parent *string, children []CreateIssueInput) epicResult {
	e.t.Helper()
	in := e.mcpInput("epic under " + *parent)
	in.ParentID = parent
	ep, ch := e.mustEpic(in, children...)
	return epicResult{epic: ep, children: ch}
}

func countWhere(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// TestDefaultAssignee_ChildFailureRollsBackTheCall — row 28.
func TestDefaultAssignee_ChildFailureRollsBackTheCall(t *testing.T) {
	e := newDAEnv(t)
	e.setFlag(e.alice, true)
	foreignComponent := seedComponent(t, e.plain, e.pid2, e.carol, "elsewhere")

	issuesBefore := countWhere(t, e.plain, `SELECT count(*) FROM issues WHERE project_id = $1`, e.pid)
	eventsBefore := countWhere(t, e.plain, `SELECT count(*) FROM events WHERE project_id = $1`, e.pid)
	watchesBefore := countWhere(t, e.plain, `SELECT count(*) FROM issue_watchers WHERE user_id = $1`, e.alice)

	bad := e.mcpInput("child with another project's component")
	bad.ComponentIDs = []uuid.UUID{foreignComponent}
	bad.ComponentIDsExplicit = true
	_, _, err := e.epic(t.Context(), e.mcpInput("doomed epic"), e.mcpInput("good child"), bad)
	var childErr *ErrEpicChildFailed
	if !errors.As(err, &childErr) {
		t.Fatalf("CreateEpicWithChildren error = %v, want ErrEpicChildFailed", err)
	}

	if got := countWhere(t, e.plain, `SELECT count(*) FROM issues WHERE project_id = $1`, e.pid); got != issuesBefore {
		t.Errorf("issues in the project: %d before, %d after the failed call", issuesBefore, got)
	}
	if got := countWhere(t, e.plain, `SELECT count(*) FROM events WHERE project_id = $1`, e.pid); got != eventsBefore {
		t.Errorf("events in the project: %d before, %d after the failed call", eventsBefore, got)
	}
	if got := countWhere(t, e.plain, `SELECT count(*) FROM issue_watchers WHERE user_id = $1`, e.alice); got != watchesBefore {
		t.Errorf("the creator's subscriptions: %d before, %d after the failed call", watchesBefore, got)
	}
}

// wantOneValueEverywhere — row 29: column, issue.created, the response and
// the auto-accept event all carry the creator.
func (e *daEnv) wantOneValueEverywhere(t *testing.T, dto *IssueDetail) {
	t.Helper()
	e.wantOwner(t, dto.ID, e.aName, e.alice)
	if dto.Assignee == nil || *dto.Assignee != e.aName {
		t.Errorf("%s: IssueDetail.Assignee = %s, want %q", dto.ID, str(dto.Assignee), e.aName)
	}
	var evAssignee *string
	if err := e.plain.QueryRow(context.Background(), `
SELECT new_data->>'assignee' FROM events
 WHERE entity_type = 'issue' AND entity_id = $1 AND type = 'issue.created'`, dto.ID).Scan(&evAssignee); err != nil {
		t.Fatalf("%s: read issue.created: %v", dto.ID, err)
	}
	if evAssignee == nil || *evAssignee != e.aName {
		t.Errorf("%s: issue.created assignee = %s, want %q", dto.ID, str(evAssignee), e.aName)
	}
	var reason, acceptedBy *string
	if err := e.plain.QueryRow(context.Background(), `
SELECT reason, new_data->>'acceptedBy' FROM events
 WHERE entity_type = 'issue' AND entity_id = $1 AND type = 'issue.accepted'`, dto.ID).Scan(&reason, &acceptedBy); err != nil {
		t.Fatalf("%s: read issue.accepted: %v", dto.ID, err)
	}
	if reason == nil || *reason != "auto-accept" {
		t.Errorf("%s: issue.accepted reason = %s, want \"auto-accept\"", dto.ID, str(reason))
	}
	if acceptedBy == nil || *acceptedBy != e.alice.String() {
		t.Errorf("%s: issue.accepted acceptedBy = %s, want %s", dto.ID, str(acceptedBy), e.alice)
	}
}

// TestDefaultAssignee_OneValueEverywhere — row 29.
func TestDefaultAssignee_OneValueEverywhere(t *testing.T) {
	e := newDAEnv(t)
	e.setFlag(e.alice, true)
	e.wantOneValueEverywhere(t, e.create(e.mcpInput("single")))
	ep, children := e.mustEpic(e.mcpInput("epic"), e.mcpInput("child 1"), e.mcpInput("child 2"))
	e.wantOneValueEverywhere(t, ep)
	for _, c := range children {
		e.wantOneValueEverywhere(t, c)
	}
}

// ---------------------------------------------------------------------------
// Rows 30–31: what the committed attempt sees
// ---------------------------------------------------------------------------

// attemptHook runs fn once, on the first statement for which match returns
// true, synchronously on the creating connection — before that statement
// executes. It counts the transactions the call began.
type attemptHook struct {
	fired  atomic.Bool
	begins atomic.Int32
	match  func(sql string, afterBegin bool) bool
	fn     func(args []any)
	mu     sync.Mutex
	err    error

	lastWasBegin bool
}

func (h *attemptHook) onStatement(sql string, args []any) {
	isBegin := strings.HasPrefix(strings.ToLower(strings.TrimSpace(sql)), "begin")
	if isBegin {
		h.begins.Add(1)
	}
	afterBegin := h.lastWasBegin
	h.lastWasBegin = isBegin
	if isBegin || h.fired.Load() || !h.match(sql, afterBegin) {
		return
	}
	h.fired.Store(true)
	h.fn(args)
}

func (h *attemptHook) fail(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err == nil {
		h.err = err
	}
}

// insertIssueStatement matches the issue INSERT of createIssueInTx.
func insertIssueStatement(sql string, _ bool) bool {
	return strings.Contains(sql, "INSERT INTO issues (")
}

// firstAfterBegin matches the first statement of a transaction.
func firstAfterBegin(_ string, afterBegin bool) bool { return afterBegin }

// replayAnchor is a component the create links its issue to. Recreating
// it out of band (delete + insert, same id, one commit) after the attempt's
// snapshot makes that attempt's foreign-key check on issue_components fail
// with a serialization failure — AFTER the default-assignee step has run —
// so the store replays the attempt, and the replay sees the recreated row.
// Nothing about the assignee depends on the component.
func (e *daEnv) replayAnchor() uuid.UUID {
	e.t.Helper()
	return seedComponent(e.t, e.plain, e.pid, e.bob, "replay-"+uuid.NewString()[:8])
}

func (e *daEnv) recreate(h *attemptHook, component uuid.UUID) {
	ctx := context.Background()
	tx, err := e.plain.Begin(ctx)
	if err != nil {
		h.fail(err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM components WHERE id = $1`, component); err != nil {
		h.fail(err)
		return
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO components (id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at)
VALUES ($1, $2, $3, '', '[]'::jsonb, NULL, $4, now(), now())`,
		component, e.pid, "replay-"+component.String()[:8], e.bob); err != nil {
		h.fail(err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		h.fail(err)
	}
}

func withReplayAnchor(in CreateIssueInput, anchor uuid.UUID) CreateIssueInput {
	in.ComponentIDs = append(in.ComponentIDs, anchor)
	in.ComponentIDsExplicit = true
	return in
}

func (e *daEnv) withHook(h *attemptHook, fn func()) {
	e.tr.mu.Lock()
	e.tr.hook = h.onStatement
	e.tr.mu.Unlock()
	defer func() {
		e.tr.mu.Lock()
		e.tr.hook = nil
		e.tr.mu.Unlock()
	}()
	fn()
}

// TestDefaultAssignee_CommittedAttemptAppliesItsOwnSnapshot — row 30.
func TestDefaultAssignee_CommittedAttemptAppliesItsOwnSnapshot(t *testing.T) {
	e := newDAEnv(t)

	anchor := e.replayAnchor()
	create := func(t *testing.T) []*IssueDetail {
		dto, err := e.createAs(t.Context(), e.alice, e.pid, e.prefix, "counter", withReplayAnchor(e.mcpInput("single"), anchor))
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		return []*IssueDetail{dto}
	}
	createEpic := func(t *testing.T) []*IssueDetail {
		in := withReplayAnchor(e.mcpInput("epic"), anchor)
		in.Type = TypeEpic
		ep, ch, err := e.store.CreateEpicWithChildren(t.Context(), e.pid, e.prefix, "counter", in,
			[]CreateIssueInput{e.mcpInput("child 1"), e.mcpInput("child 2")},
			SourceAgentDirected, e.alice, agentOf(e.alice))
		if err != nil {
			t.Fatalf("CreateEpicWithChildren: %v", err)
		}
		return append([]*IssueDetail{ep}, ch...)
	}

	for _, closure := range []struct {
		name string
		run  func(t *testing.T) []*IssueDetail
	}{{"CreateIssue", create}, {"CreateEpicWithChildren", createEpic}} {
		t.Run(closure.name+": (a) flag flips between the aborted and the committed attempt", func(t *testing.T) {
			anchor = e.replayAnchor()
			e.setFlag(e.alice, false)
			h := &attemptHook{match: insertIssueStatement}
			h.fn = func([]any) {
				if _, err := e.plain.Exec(context.Background(),
					`UPDATE user_notification_prefs SET assign_agent_issues_to_me = true WHERE user_id = $1`, e.alice); err != nil {
					h.fail(err)
				}
				e.recreate(h, anchor)
			}
			var created []*IssueDetail
			e.withHook(h, func() { created = closure.run(t) })
			if h.err != nil {
				t.Fatalf("hook: %v", h.err)
			}
			if !h.fired.Load() || h.begins.Load() < 2 {
				t.Fatalf("no replay happened (hook fired %v, transactions %d) — the oracle did not run",
					h.fired.Load(), h.begins.Load())
			}
			for _, dto := range created {
				e.wantOneValueEverywhere(t, dto)
			}
		})

		t.Run(closure.name+": (b) flag committed after BEGIN, before the first statement", func(t *testing.T) {
			anchor = e.replayAnchor()
			e.setFlag(e.alice, false)
			h := &attemptHook{match: firstAfterBegin}
			h.fn = func([]any) {
				if _, err := e.plain.Exec(context.Background(),
					`UPDATE user_notification_prefs SET assign_agent_issues_to_me = true WHERE user_id = $1`, e.alice); err != nil {
					h.fail(err)
				}
			}
			var created []*IssueDetail
			e.withHook(h, func() { created = closure.run(t) })
			if h.err != nil {
				t.Fatalf("hook: %v", h.err)
			}
			if !h.fired.Load() {
				t.Fatalf("the hook never fired — the oracle did not run")
			}
			for _, dto := range created {
				e.wantOneValueEverywhere(t, dto)
			}
		})
	}
}

// TestDefaultAssignee_VisibilityIsPerAttempt — row 31.
func TestDefaultAssignee_VisibilityIsPerAttempt(t *testing.T) {
	e := newDAEnv(t)
	e.setFlag(e.alice, true)
	addMember(t, e.plain, e.pid2, e.alice, e.carol)

	anchor := e.replayAnchor()
	createUnder := func(t *testing.T, p string) *IssueDetail {
		in := e.mcpInput("under P")
		in.Dependencies = []DependencyEdgeInput{{ToID: p, Type: "parent-child"}}
		dto, err := e.createAs(t.Context(), e.alice, e.pid, e.prefix, "counter", withReplayAnchor(in, anchor))
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		return dto
	}

	// (a) Alice sees G's project, no replay: no assigned ancestor → owner.
	p, _ := e.boundaryLayout("", "", nil)
	e.wantOwner(t, createUnder(t, p).ID, e.aName, e.alice)

	// (b) The membership goes between the aborted and the committed attempt.
	p, _ = e.boundaryLayout("", "", nil)
	anchor = e.replayAnchor()
	h := &attemptHook{match: insertIssueStatement}
	h.fn = func([]any) {
		if _, err := e.plain.Exec(context.Background(),
			`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, e.pid2, e.alice); err != nil {
			h.fail(err)
		}
		e.recreate(h, anchor)
	}
	var dto *IssueDetail
	e.withHook(h, func() { dto = createUnder(t, p) })
	if h.err != nil {
		t.Fatalf("hook: %v", h.err)
	}
	if !h.fired.Load() || h.begins.Load() < 2 {
		t.Fatalf("no replay happened (hook fired %v, transactions %d) — the oracle did not run",
			h.fired.Load(), h.begins.Load())
	}
	e.wantUnassigned(t, dto.ID)
}

// ---------------------------------------------------------------------------
// Row 32: creating never touches the settings row
// ---------------------------------------------------------------------------

func (e *daEnv) prefsRow(user uuid.UUID) (string, bool) {
	e.t.Helper()
	var row string
	err := e.plain.QueryRow(context.Background(),
		`SELECT to_jsonb(p)::text FROM user_notification_prefs p WHERE user_id = $1`, user).Scan(&row)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	if err != nil {
		e.t.Fatalf("read prefs row: %v", err)
	}
	return row, true
}

// TestDefaultAssignee_CreateLeavesTheSettingsRowAlone — row 32.
func TestDefaultAssignee_CreateLeavesTheSettingsRowAlone(t *testing.T) {
	e := newDAEnv(t)
	foreignComponent := seedComponent(t, e.plain, e.pid2, e.carol, "elsewhere")
	failing := func() CreateIssueInput {
		in := e.mcpInput("fails")
		in.ComponentIDs = []uuid.UUID{foreignComponent}
		in.ComponentIDsExplicit = true
		return in
	}

	t.Run("no row", func(t *testing.T) {
		if _, err := e.plain.Exec(context.Background(), `DELETE FROM user_notification_prefs WHERE user_id = $1`, e.alice); err != nil {
			t.Fatalf("drop prefs row: %v", err)
		}
		e.wantOwner(t, e.create(e.mcpInput("succeeds")).ID, e.aName, e.alice)
		if _, ok := e.prefsRow(e.alice); ok {
			t.Errorf("a successful create left a prefs row behind")
		}
		if _, err := e.createAs(t.Context(), e.alice, e.pid, e.prefix, "counter", failing()); err == nil {
			t.Fatalf("the failing create succeeded")
		}
		if _, ok := e.prefsRow(e.alice); ok {
			t.Errorf("a failed create left a prefs row behind")
		}
	})

	t.Run("existing row", func(t *testing.T) {
		e.setFlag(e.alice, true)
		before, _ := e.prefsRow(e.alice)
		e.wantOwner(t, e.create(e.mcpInput("succeeds")).ID, e.aName, e.alice)
		if _, err := e.createAs(t.Context(), e.alice, e.pid, e.prefix, "counter", failing()); err == nil {
			t.Fatalf("the failing create succeeded")
		}
		if after, _ := e.prefsRow(e.alice); after != before {
			t.Errorf("the prefs row changed:\nbefore %s\nafter  %s", before, after)
		}
	})
}
