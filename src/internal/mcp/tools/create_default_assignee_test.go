package tools

// create_default_assignee_test.go — sk-565, T031, test matrix rows 35–45
// (US-1, US-2, US-4, US-5, D2, D5, INV-2, INV-9, SC-001, SC-003, Edge cases).
// References — the sk-565 spec: without `assignee` and `unassigned` with the
// setting on, the assignee is the key owner, the issue is accepted (US-1, SC-001);
// `unassigned: true` — no assignee, not accepted; together with a non-empty
// `assignee` — validation_error, nothing created; `false` and `null` —
// as an absent flag; any other type — validation_error (US-4); the epic's
// flags do not spread to children (US-5); an empty and a whitespace-only
// `assignee` is "not given" (Edge); `unassigned: true` + standing — standing
// without an assignee, not an error (Edge).
//
// The tools are called through their handlers, so the parse is part of
// what is proved (the type matrix included — a separate unit test of
// assignee_mode.go would only repeat this branch).

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type mcpDA struct {
	t         *testing.T
	pool      *pgxpool.Pool
	owner     uuid.UUID
	ownerName string
	bob       uuid.UUID
	bobName   string
	pid       uuid.UUID
	prefix    string
	ctx       context.Context
	create    server.ToolHandlerFunc
	epic      server.ToolHandlerFunc
	seq       int
}

func newMCPDA(t *testing.T) *mcpDA {
	t.Helper()
	pool := newTestPool(t)
	m := &mcpDA{t: t, pool: pool}
	m.owner, m.ownerName = seedUser(t, pool, "user")
	m.bob, m.bobName = seedUser(t, pool, "user")
	m.pid, m.prefix = seedProject(t, pool, m.owner, "mcpda")
	addProjectMember(t, pool, m.pid, m.bob)
	m.ctx = withCtx(t, m.owner, "user")
	m.create = handleCreate(pool, issues.NewStore(pool), dependencies.NewStore(pool), projects.NewStore(pool))
	m.epic = handleEpicCreate(pool, issues.NewStore(pool), projects.NewStore(pool))
	return m
}

// title returns a fresh title, so the duplicate gate never fires by accident.
func (m *mcpDA) title(what string) string {
	m.seq++
	return what + " #" + strconv.Itoa(m.seq) + " " + uuid.NewString()
}

func (m *mcpDA) setFlag(user uuid.UUID, on bool) {
	m.t.Helper()
	if _, err := m.pool.Exec(context.Background(), `
INSERT INTO user_notification_prefs (user_id, assign_agent_issues_to_me, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (user_id) DO UPDATE SET assign_agent_issues_to_me = EXCLUDED.assign_agent_issues_to_me`,
		user, on); err != nil {
		m.t.Fatalf("set flag: %v", err)
	}
}

func (m *mcpDA) call(ctx context.Context, h server.ToolHandlerFunc, args map[string]any) (map[string]any, bool) {
	m.t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := h(ctx, req)
	if err != nil {
		m.t.Fatalf("handler error: %v", err)
	}
	return extractToolJSON(m.t, res), res.IsError
}

// createArgs is a skein_create call from the owner's agent; extra overrides.
func (m *mcpDA) createArgs(extra map[string]any) map[string]any {
	args := map[string]any{"project": m.pid.String(), "title": m.title("create"), "type": "task", "kind": "directed"}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// mustCreate runs skein_create and returns the created id and ack.
func (m *mcpDA) mustCreate(extra map[string]any) (string, map[string]any) {
	m.t.Helper()
	body, isErr := m.call(m.ctx, m.create, m.createArgs(extra))
	if isErr || body["created"] != true {
		m.t.Fatalf("skein_create %v: %v", extra, body)
	}
	issue, _ := body["issue"].(map[string]any)
	id, _ := issue["id"].(string)
	return id, issue
}

func (m *mcpDA) epicArgs(extra map[string]any, children ...map[string]any) map[string]any {
	kids := make([]any, 0, len(children))
	for _, c := range children {
		if _, ok := c["title"]; !ok {
			c["title"] = m.title("child")
		}
		kids = append(kids, c)
	}
	args := map[string]any{"project": m.pid.String(), "title": m.title("epic"), "kind": "directed", "childTemplates": kids}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// mustEpic runs skein_epic_create and returns the epic id and the child ids.
func (m *mcpDA) mustEpic(extra map[string]any, children ...map[string]any) (string, []string) {
	m.t.Helper()
	body, isErr := m.call(m.ctx, m.epic, m.epicArgs(extra, children...))
	if isErr {
		m.t.Fatalf("skein_epic_create %v: %v", extra, body)
	}
	ep, _ := body["epic"].(map[string]any)
	epicID, _ := ep["id"].(string)
	var ids []string
	rows, _ := body["children"].([]any)
	for _, r := range rows {
		row, _ := r.(map[string]any)
		id, _ := row["id"].(string)
		ids = append(ids, id)
	}
	if epicID == "" || len(ids) != len(children) {
		m.t.Fatalf("skein_epic_create returned epic %q and %d children, want %d: %v", epicID, len(ids), len(children), body)
	}
	return epicID, ids
}

func (m *mcpDA) stored(id string) (assignee *string, acceptedAt *time.Time, status string) {
	m.t.Helper()
	if err := m.pool.QueryRow(context.Background(),
		`SELECT assignee, accepted_at, status FROM issues WHERE id = $1`, id).Scan(&assignee, &acceptedAt, &status); err != nil {
		m.t.Fatalf("read %s: %v", id, err)
	}
	return assignee, acceptedAt, status
}

func (m *mcpDA) acceptedBy(id string) *uuid.UUID {
	m.t.Helper()
	var by *uuid.UUID
	if err := m.pool.QueryRow(context.Background(),
		`SELECT accepted_by FROM issues WHERE id = $1`, id).Scan(&by); err != nil {
		m.t.Fatalf("read accepted_by of %s: %v", id, err)
	}
	return by
}

func (m *mcpDA) wantOwner(t *testing.T, id string) {
	t.Helper()
	assignee, acceptedAt, _ := m.stored(id)
	if assignee == nil || *assignee != m.ownerName {
		t.Errorf("%s: assignee = %v, want the key owner %q", id, deref(assignee), m.ownerName)
	}
	if acceptedAt == nil {
		t.Errorf("%s: not accepted, want accepted by the key owner", id)
	}
	if by := m.acceptedBy(id); by == nil || *by != m.owner {
		t.Errorf("%s: accepted_by = %v, want the key owner %s", id, by, m.owner)
	}
}

func (m *mcpDA) wantAssignee(t *testing.T, id, want string) {
	t.Helper()
	if assignee, _, _ := m.stored(id); assignee == nil || *assignee != want {
		t.Errorf("%s: assignee = %v, want %q", id, deref(assignee), want)
	}
}

func (m *mcpDA) wantUnassigned(t *testing.T, id string) {
	t.Helper()
	assignee, acceptedAt, _ := m.stored(id)
	if assignee != nil && *assignee != "" {
		t.Errorf("%s: assignee = %q, want none", id, *assignee)
	}
	if acceptedAt != nil {
		t.Errorf("%s: accepted at %v, want not accepted", id, acceptedAt)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// bobTask seeds an issue assigned to Bob.
func (m *mcpDA) bobTask() string {
	m.t.Helper()
	m.seq++
	id := m.prefix + "-bob" + strconv.Itoa(m.seq)
	seedIssue(m.t, m.pool, m.pid, m.bob, id, "bob's task", "open", 2)
	if _, err := m.pool.Exec(context.Background(), `UPDATE issues SET assignee = $2 WHERE id = $1`, id, m.bobName); err != nil {
		m.t.Fatalf("assign bob: %v", err)
	}
	return id
}

// footprint counts what a refused call must not leave behind.
type footprint struct{ issues, events, watches int }

func (m *mcpDA) footprint() footprint {
	m.t.Helper()
	var f footprint
	ctx := context.Background()
	for _, q := range []struct {
		dst *int
		sql string
		arg any
	}{
		{&f.issues, `SELECT count(*) FROM issues WHERE project_id = $1`, m.pid},
		{&f.events, `SELECT count(*) FROM events WHERE project_id = $1`, m.pid},
		{&f.watches, `SELECT count(*) FROM issue_watchers WHERE user_id = $1`, m.owner},
	} {
		if err := m.pool.QueryRow(ctx, q.sql, q.arg).Scan(q.dst); err != nil {
			m.t.Fatalf("footprint: %v", err)
		}
	}
	return f
}

// wantRefused: validation_error with the given detail code, nothing written.
func (m *mcpDA) wantRefused(t *testing.T, what string, h server.ToolHandlerFunc, args map[string]any, code string, childIndex *int) {
	t.Helper()
	before := m.footprint()
	body, isErr := m.call(m.ctx, h, args)
	if !isErr || body["error"] != ErrCodeValidationError {
		t.Errorf("%s: response %v, want validation_error", what, body)
		return
	}
	details, _ := body["details"].(map[string]any)
	if details == nil {
		details = body
	}
	if details["code"] != code {
		t.Errorf("%s: refusal code = %v, want %q (%v)", what, details["code"], code, body)
	}
	if childIndex != nil {
		if got, _ := details["childIndex"].(float64); details["childIndex"] == nil || int(got) != *childIndex {
			t.Errorf("%s: childIndex = %v, want %d", what, details["childIndex"], *childIndex)
		}
	}
	if after := m.footprint(); after != before {
		t.Errorf("%s: refused call left a trace: before %+v, after %+v", what, before, after)
	}
}

// ---------------------------------------------------------------------------
// Rows 35–37, 41–44
// ---------------------------------------------------------------------------

// TestCreateDefaultAssignee_OwnerOnAllThreePaths — row 35.
func TestCreateDefaultAssignee_OwnerOnAllThreePaths(t *testing.T) {
	m := newMCPDA(t)

	top, ack := m.mustCreate(nil)
	if ack["assignee"] != m.ownerName {
		t.Errorf("skein_create ack assignee = %v, want %q", ack["assignee"], m.ownerName)
	}
	m.wantOwner(t, top)

	ownersTask, _ := m.mustCreate(nil)
	sub, ack := m.mustCreate(map[string]any{"parentId": ownersTask})
	if ack["assignee"] != m.ownerName {
		t.Errorf("subtask ack assignee = %v, want %q", ack["assignee"], m.ownerName)
	}
	m.wantOwner(t, sub)

	body, isErr := m.call(m.ctx, m.epic, m.epicArgs(nil, map[string]any{}, map[string]any{}))
	if isErr {
		t.Fatalf("skein_epic_create: %v", body)
	}
	rows := []any{body["epic"]}
	rows = append(rows, body["children"].([]any)...)
	for _, r := range rows {
		row, _ := r.(map[string]any)
		if row["assignee"] != m.ownerName {
			t.Errorf("epic-create ack row %v: assignee = %v, want %q", row["id"], row["assignee"], m.ownerName)
		}
		id, _ := row["id"].(string)
		m.wantOwner(t, id)
	}

	prime := handlePrime(m.pool, issues.NewStore(m.pool), dependencies.NewStore(m.pool), projects.NewStore(m.pool))
	pbody, isErr := m.call(m.ctx, prime, map[string]any{"project": m.pid.String(), "limit": 25})
	if isErr {
		t.Fatalf("skein_prime: %v", pbody)
	}
	found := false
	ready, _ := pbody["readyForMe"].([]any)
	for _, r := range ready {
		if row, _ := r.(map[string]any); row["id"] == top {
			found = true
		}
	}
	if !found {
		t.Errorf("skein_prime readyForMe of the owner does not contain %s: %v", top, pbody["readyForMe"])
	}
}

// TestCreateDefaultAssignee_SettingOffOnMCP — row 36.
func TestCreateDefaultAssignee_SettingOffOnMCP(t *testing.T) {
	m := newMCPDA(t)
	m.setFlag(m.owner, false)
	id, _ := m.mustCreate(nil)
	m.wantUnassigned(t, id)
	epic, kids := m.mustEpic(nil, map[string]any{}, map[string]any{})
	m.wantUnassigned(t, epic)
	for _, k := range kids {
		m.wantUnassigned(t, k)
	}
}

// TestCreateDefaultAssignee_UnassignedFlagOnEverySurface — row 37.
func TestCreateDefaultAssignee_UnassignedFlagOnEverySurface(t *testing.T) {
	m := newMCPDA(t)

	cases := []struct {
		name  string
		extra map[string]any
		want  string // "" — unassigned; "owner"; otherwise the handle
	}{
		{"unassigned true", map[string]any{"unassigned": true}, ""},
		{"unassigned false", map[string]any{"unassigned": false}, "owner"},
		{"unassigned null", map[string]any{"unassigned": nil}, "owner"},
		{"explicit bob", map[string]any{"assignee": m.bobName}, m.bobName},
	}
	check := func(t *testing.T, id, want string) {
		switch want {
		case "":
			m.wantUnassigned(t, id)
		case "owner":
			m.wantOwner(t, id)
		default:
			m.wantAssignee(t, id, want)
		}
	}
	for _, tc := range cases {
		t.Run("skein_create: "+tc.name, func(t *testing.T) {
			id, _ := m.mustCreate(tc.extra)
			check(t, id, tc.want)
		})
		t.Run("epic: "+tc.name, func(t *testing.T) {
			epic, _ := m.mustEpic(tc.extra, map[string]any{})
			check(t, epic, tc.want)
		})
		t.Run("childTemplates[i]: "+tc.name, func(t *testing.T) {
			child := map[string]any{}
			for k, v := range tc.extra {
				child[k] = v
			}
			_, kids := m.mustEpic(nil, map[string]any{}, child)
			check(t, kids[1], tc.want)
		})
	}
}

// TestCreateDefaultAssignee_EpicFlagsDoNotPropagate — row 41.
func TestCreateDefaultAssignee_EpicFlagsDoNotPropagate(t *testing.T) {
	m := newMCPDA(t)

	epic, kids := m.mustEpic(map[string]any{"unassigned": true}, map[string]any{}, map[string]any{})
	m.wantUnassigned(t, epic)
	for _, k := range kids {
		m.wantOwner(t, k)
	}

	epic, kids = m.mustEpic(nil, map[string]any{}, map[string]any{"unassigned": true})
	m.wantOwner(t, epic)
	m.wantOwner(t, kids[0])
	m.wantUnassigned(t, kids[1])
}

// TestCreateDefaultAssignee_BlankAssigneeIsNotSpecified — row 42.
func TestCreateDefaultAssignee_BlankAssigneeIsNotSpecified(t *testing.T) {
	m := newMCPDA(t)
	for _, blank := range []string{"", "   "} {
		t.Run("skein_create "+strconv.Quote(blank), func(t *testing.T) {
			id, _ := m.mustCreate(map[string]any{"assignee": blank})
			m.wantOwner(t, id)
			id, _ = m.mustCreate(map[string]any{"assignee": blank, "parentId": m.bobTask()})
			m.wantUnassigned(t, id)
			id, _ = m.mustCreate(map[string]any{"assignee": blank, "unassigned": true})
			m.wantUnassigned(t, id)
		})
		t.Run("epic "+strconv.Quote(blank), func(t *testing.T) {
			epic, _ := m.mustEpic(map[string]any{"assignee": blank}, map[string]any{})
			m.wantOwner(t, epic)
			epic, kids := m.mustEpic(map[string]any{"assignee": blank, "parentId": m.bobTask()}, map[string]any{})
			m.wantUnassigned(t, epic)
			m.wantUnassigned(t, kids[0])
			epic, _ = m.mustEpic(map[string]any{"assignee": blank, "unassigned": true}, map[string]any{})
			m.wantUnassigned(t, epic)
		})
		t.Run("childTemplates[i] "+strconv.Quote(blank), func(t *testing.T) {
			_, kids := m.mustEpic(nil, map[string]any{"assignee": blank})
			m.wantOwner(t, kids[0])
			_, kids = m.mustEpic(map[string]any{"assignee": m.bobName}, map[string]any{"assignee": blank})
			m.wantUnassigned(t, kids[0])
			_, kids = m.mustEpic(nil, map[string]any{"assignee": blank, "unassigned": true})
			m.wantUnassigned(t, kids[0])
		})
	}
}

// TestCreateDefaultAssignee_UnassignedStanding — row 43.
func TestCreateDefaultAssignee_UnassignedStanding(t *testing.T) {
	m := newMCPDA(t)
	id, _ := m.mustCreate(map[string]any{"unassigned": true, "standing": true})
	m.wantUnassigned(t, id)
	if _, _, status := m.stored(id); status != "standing" {
		t.Errorf("%s: status = %q, want standing", id, status)
	}
}

// TestCreateDefaultAssignee_OtherMemberCannotClaim — row 44.
func TestCreateDefaultAssignee_OtherMemberCannotClaim(t *testing.T) {
	m := newMCPDA(t)
	id, _ := m.mustCreate(nil)
	m.wantOwner(t, id)

	claim := handleClaim(m.pool, issues.NewStore(m.pool))
	body, isErr := m.call(withCtx(t, m.bob, "user"), claim, map[string]any{"id": id})
	if !isErr || body["error"] != ErrCodeAlreadyClaimed {
		t.Errorf("claim by another member: %v, want %s", body, ErrCodeAlreadyClaimed)
	}

	body, isErr = m.call(m.ctx, claim, map[string]any{"id": id})
	if isErr {
		t.Fatalf("claim by the owner: %v", body)
	}
	var lockedBy *string
	if err := m.pool.QueryRow(context.Background(), `SELECT locked_by FROM issues WHERE id = $1`, id).Scan(&lockedBy); err != nil {
		t.Fatalf("read lock: %v", err)
	}
	if _, _, status := m.stored(id); status != "in_progress" || lockedBy == nil {
		t.Errorf("after the owner's claim: status = %q, locked_by = %v, want in_progress and a lock", status, lockedBy)
	}
}

// ---------------------------------------------------------------------------
// Rows 38–40: refusals
// ---------------------------------------------------------------------------

// badUnassigned are the refusals of US-4: the conflict with an assignee,
// and every non-boolean JSON type.
func (m *mcpDA) badUnassigned() []struct {
	name  string
	extra map[string]any
	code  string
} {
	return []struct {
		name  string
		extra map[string]any
		code  string
	}{
		{"true with a non-empty assignee", map[string]any{"unassigned": true, "assignee": m.bobName}, unassignedWithAssigneeCode},
		{"string", map[string]any{"unassigned": "true"}, unassignedNotBooleanCode},
		{"number", map[string]any{"unassigned": 1.0}, unassignedNotBooleanCode},
		{"object", map[string]any{"unassigned": map[string]any{"v": true}}, unassignedNotBooleanCode},
		{"array", map[string]any{"unassigned": []any{true}}, unassignedNotBooleanCode},
	}
}

// TestCreateDefaultAssignee_RefusalsCreateNothing — row 38.
func TestCreateDefaultAssignee_RefusalsCreateNothing(t *testing.T) {
	m := newMCPDA(t)
	for _, tc := range m.badUnassigned() {
		m.wantRefused(t, "skein_create "+tc.name, m.create, m.createArgs(tc.extra), tc.code, nil)
	}
}

// TestCreateDefaultAssignee_RefusalBeforeDuplicateGate — row 39.
func TestCreateDefaultAssignee_RefusalBeforeDuplicateGate(t *testing.T) {
	m := newMCPDA(t)
	title := "the one and only title of a duplicate probe"
	m.mustCreate(map[string]any{"title": title})
	// Precondition: the same title alone does trip the duplicate gate —
	// otherwise the refusal below proves nothing about the order.
	if body, isErr := m.call(m.ctx, m.create, m.createArgs(map[string]any{"title": title})); isErr || body["decisionRequired"] != true {
		t.Fatalf("precondition: the repeated title did not reach decisionRequired: %v", body)
	}
	m.wantRefused(t, "duplicate title with assignee+unassigned", m.create,
		m.createArgs(map[string]any{"title": title, "unassigned": true, "assignee": m.bobName}),
		unassignedWithAssigneeCode, nil)
}

// TestCreateDefaultAssignee_EpicAndChildRefusals — row 40.
func TestCreateDefaultAssignee_EpicAndChildRefusals(t *testing.T) {
	m := newMCPDA(t)
	for _, tc := range m.badUnassigned() {
		m.wantRefused(t, "epic "+tc.name, m.epic, m.epicArgs(tc.extra, map[string]any{}), tc.code, nil)

		child := map[string]any{}
		for k, v := range tc.extra {
			child[k] = v
		}
		idx := 1
		m.wantRefused(t, "childTemplates[1] "+tc.name, m.epic,
			m.epicArgs(nil, map[string]any{}, child), tc.code, &idx)
	}
}

// ---------------------------------------------------------------------------
// Row 45: the schema contract
// ---------------------------------------------------------------------------

func listedTools(t *testing.T) map[string]map[string]any {
	t.Helper()
	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	RegisterCreate(s, nil, nil, nil, nil)
	RegisterEpicCreate(s, nil, nil, nil)
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("new in-process client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "test", Version: "1"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	res, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	out := map[string]map[string]any{}
	for _, tool := range res.Tools {
		raw, err := json.Marshal(tool)
		if err != nil {
			t.Fatalf("marshal %s: %v", tool.Name, err)
		}
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("unmarshal %s: %v", tool.Name, err)
		}
		out[tool.Name] = obj
	}
	return out
}

func dig(obj map[string]any, path ...string) map[string]any {
	cur := obj
	for _, p := range path {
		next, _ := cur[p].(map[string]any)
		if next == nil {
			return nil
		}
		cur = next
	}
	return cur
}

// TestCreateDefaultAssignee_SchemaDeclaresUnassigned — row 45.
func TestCreateDefaultAssignee_SchemaDeclaresUnassigned(t *testing.T) {
	tools := listedTools(t)
	for _, where := range []struct {
		name string
		path []string
	}{
		{"skein_create", []string{"inputSchema", "properties", "unassigned"}},
		{"skein_epic_create", []string{"inputSchema", "properties", "unassigned"}},
		{"skein_epic_create", []string{"inputSchema", "properties", "childTemplates", "items", "properties", "unassigned"}},
	} {
		tool, ok := tools[where.name]
		if !ok {
			t.Fatalf("%s is not listed", where.name)
		}
		prop := dig(tool, where.path...)
		if prop == nil || prop["type"] != "boolean" {
			t.Errorf("%s %v: %v, want {type: boolean}", where.name, where.path, prop)
		}
	}
}

// ---------------------------------------------------------------------------
// sk-605: a non-string `assignee`
// ---------------------------------------------------------------------------

// assigneeNotStringWire is the wire code of the sk-605 refusal, written out
// as the contract value rather than borrowed from the implementation.
const assigneeNotStringWire = "assignee_not_string"

// badAssigneeTypes are the non-string JSON types of `assignee` (sk-605):
// each was silently read as «not passed» and the issue went to the key
// owner instead of being refused.
var badAssigneeTypes = []struct {
	name  string
	value any
}{
	{"number", 42.0},
	{"array", []any{"bob"}},
	{"object", map[string]any{"username": "bob"}},
	{"boolean", true},
}

// TestCreateDefaultAssignee_NonStringAssigneeRefused — sk-605 regression:
// on skein_create, on the epic and on childTemplates[i] a non-string
// assignee is a validation_error with the machine code (childIndex for a
// child), and nothing is created.
func TestCreateDefaultAssignee_NonStringAssigneeRefused(t *testing.T) {
	m := newMCPDA(t)
	for _, tc := range badAssigneeTypes {
		m.wantRefused(t, "skein_create "+tc.name, m.create,
			m.createArgs(map[string]any{"assignee": tc.value}), assigneeNotStringWire, nil)
		m.wantRefused(t, "epic "+tc.name, m.epic,
			m.epicArgs(map[string]any{"assignee": tc.value}, map[string]any{}), assigneeNotStringWire, nil)
		idx := 1
		m.wantRefused(t, "childTemplates[1] "+tc.name, m.epic,
			m.epicArgs(nil, map[string]any{}, map[string]any{"assignee": tc.value}), assigneeNotStringWire, &idx)
	}
}

// TestCreateDefaultAssignee_NonStringAssigneeBeforeDuplicateGate — sk-605:
// the refusal wins over decisionRequired, as the `unassigned` refusals do.
func TestCreateDefaultAssignee_NonStringAssigneeBeforeDuplicateGate(t *testing.T) {
	m := newMCPDA(t)
	title := "duplicate probe for a non-string assignee"
	m.mustCreate(map[string]any{"title": title})
	if body, isErr := m.call(m.ctx, m.create, m.createArgs(map[string]any{"title": title})); isErr || body["decisionRequired"] != true {
		t.Fatalf("precondition: the repeated title did not reach decisionRequired: %v", body)
	}
	m.wantRefused(t, "duplicate title with a numeric assignee", m.create,
		m.createArgs(map[string]any{"title": title, "assignee": 42.0}), assigneeNotStringWire, nil)
}

// TestCreateDefaultAssignee_NullAssigneeIsNotSpecified — sk-605: null stays
// «not passed» on all three surfaces (D5) — the setting decides.
func TestCreateDefaultAssignee_NullAssigneeIsNotSpecified(t *testing.T) {
	m := newMCPDA(t)
	id, _ := m.mustCreate(map[string]any{"assignee": nil})
	m.wantOwner(t, id)
	epic, kids := m.mustEpic(map[string]any{"assignee": nil}, map[string]any{"assignee": nil})
	m.wantOwner(t, epic)
	m.wantOwner(t, kids[0])
	id, _ = m.mustCreate(map[string]any{"assignee": nil, "unassigned": true})
	m.wantUnassigned(t, id)
}
