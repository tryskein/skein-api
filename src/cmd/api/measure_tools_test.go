package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	mcptools "skein-api/internal/mcp/tools"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

// measureEnv gates TestMeasureToolsList. It is a measurement harness, not
// an assertion, so it must not run as part of `go test ./...` — but it
// lives in the test tree anyway because it has to call
// mcpRegisterToolsFn, which is unexported and in package main. A throwaway
// script cannot reach it, and that is exactly how the first two waves of
// sk-255.3 ended up quoting numbers produced by two different ad-hoc
// scripts that disagreed by 60 chars on the same commit.
const measureEnv = "SKEIN_MEASURE_TOOLS"

// TestMeasureToolsList prints the size of the tools/list payload the
// production registration produces, per tool and in total.
//
// Metric: sum of len(json.Marshal(tool)) over every tool in the
// tools/list response, read back through an in-process MCP client. That
// is the descriptor the client actually receives — name, description,
// inputSchema, annotations and _meta — so it moves when a schema moves
// and does not move when only Go comments do.
//
// Output is line-oriented and prefixed so scripts/measure-tools-list.sh
// can parse it without depending on the go test log format:
//
//	MEASURE_TOOL <bytes> <name>
//	MEASURE_TOTAL <bytes> <count>
//
// Run it through the script, not by hand, so successive waves report
// comparable numbers.
//
// # This total is a FIXED PER-SESSION COST, and sk-282 raised it
//
// Everything sk-282 saved is paid per CALL; tools/list is paid ONCE per
// session, by every agent, and then re-sent with every later request of
// that session — same as the guide (instructions/full.md, printed as
// MEASURE_GUIDE by TestGetInstructions_GuideWithinBudget). The run made
// descriptions longer in order to make them true, so the two numbers moved
// the wrong way while the per-call numbers fell, and that is worth stating
// with figures rather than leaving to be discovered:
//
//	                       tools/list   full.md   sum
//	before the run         99 848       57 849    157 697
//	before the rewrite    106 465      57 943    164 408
//	after the rewrite     113 266      63 335    176 601
//	after the review fixes 114 214      64 061    178 275
//
// So +20 578 bytes of fixed per-session cost across the run: +12 193 in
// the rewrite itself — the change that rewrote the descriptions and the guide —
// and a further +1 674 in the round that fixed that rewrite's own review
// findings, where several of those descriptions had to say MORE because
// what they said was wrong. Measured by this harness and by `wc -c` on the
// guide, at the commits named.
//
// The trade is still heavily positive and it is arithmetic, not an
// argument: one read of a wave node fell 23 496 → 2 901 (SC-001), and a
// session does that tens of times. The point of writing the number here is
// that it stops being invisible — a later run that adds another 20 KB of
// prose has to see what it is adding to.
func TestMeasureToolsList(t *testing.T) {
	if os.Getenv(measureEnv) == "" {
		t.Skipf("set %s=1 to run the tools/list size measurement (scripts/measure-tools-list.sh)", measureEnv)
	}

	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	mcpRegisterToolsFn(mcpDeps{})(s)

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
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "measure", Version: "1"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	listRes, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(listRes.Tools) == 0 {
		t.Fatal("tools/list is empty — registration wiring broke")
	}

	type row struct {
		name  string
		bytes int
	}
	rows := make([]row, 0, len(listRes.Tools))
	total := 0
	for i := range listRes.Tools {
		blob, err := json.Marshal(listRes.Tools[i])
		if err != nil {
			t.Fatalf("marshal %s: %v", listRes.Tools[i].Name, err)
		}
		rows = append(rows, row{name: listRes.Tools[i].Name, bytes: len(blob)})
		total += len(blob)
	}
	// Sorted by name, not by size: a stable order makes two runs
	// diffable line by line.
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	for _, r := range rows {
		fmt.Printf("MEASURE_TOOL %d %s\n", r.bytes, r.name)
	}
	fmt.Printf("MEASURE_TOTAL %d %d\n", total, len(rows))
}

// ---------------------------------------------------------------------
// sk-282 T002 — baseline "before": sizes of the actual serialized tool
// responses on the measured objects (test-plan row 14).
//
// TestMeasureToolsList above weighs what the client is TOLD about the
// tools (tools/list descriptors). This second harness weighs what the
// client is SENT when it calls them, which is what SC-001 / SC-002 /
// SC-006 / SC-007 / SC-008 / SC-009 / SC-015 are stated in. Both live in
// this file on purpose: same env-gated, line-oriented, script-parsable
// discipline, and one place to look when a wave has to state what it
// cost.
//
// Metric: len of the tool result's text content — the JSON a real client
// receives (internal/mcp.ToolJSON puts the whole payload in one text
// block). Reported twice per row, in bytes and in runes, because the SC
// thresholds are written in CHARACTERS while the task asks for bytes; on
// ASCII fixtures the two agree, and a future non-ASCII fixture makes the
// difference visible instead of silently shifting the baseline.
//
// Sub-object rows (a single list row, a single dependency edge) are
// measured by re-marshaling the parsed element, NOT by slicing the
// envelope — the SC thresholds are stated per row / per edge.
// ---------------------------------------------------------------------

// measureResponsesEnv gates TestMeasureToolResponses. Same reasoning as
// measureEnv: this is a measurement, it asserts no number, and it needs a
// database, so it must not ride along with `go test ./...`.
const measureResponsesEnv = "SKEIN_MEASURE_RESPONSES"

// Measured-object shape constants. They are the fixture's contract with
// the spec's measured objects; changing one changes every baseline number
// this harness prints, so they are named rather than inlined.
const (
	// measureWaveNodeChildren is the wave node's fan-out (spec's measured
	// node: 57 children, 45 closed + 12 open).
	measureWaveNodeChildren = 57
	measureWaveNodeClosed   = 45
	// measureThreadComments is the thread length SC-008 is stated on.
	measureThreadComments = 48
	// measureReferenceTitleLen is the reference issue's title length —
	// SC-002 and SC-009 exclude the title from their budgets and shift by
	// its actual length, and 60 is the length they shift from.
	measureReferenceTitleLen = 60
	// measureEpicChildren is the epic size SC-015 is stated on.
	measureEpicChildren = 20
)

// TestMeasureToolResponses prints the size of every measured tool
// response. Output is line-oriented and prefixed so
// scripts/measure-tool-responses.sh can parse it:
//
//	MEASURE_RESP <bytes> <runes> <label>
//	MEASURE_PART <bytes> <runes> <label>
//	MEASURE_RESP_TOTAL <bytes> <rows>
//
// MEASURE_RESP is a whole tool response; MEASURE_PART is a sub-object of
// one (a list row, one dependency edge) that an SC threshold is stated on.
func TestMeasureToolResponses(t *testing.T) {
	if os.Getenv(measureResponsesEnv) == "" {
		t.Skipf("set %s=1 to run the tool-response size measurement (scripts/measure-tool-responses.sh)", measureResponsesEnv)
	}
	pool := testsupport.NewTestPool(t)
	fx := seedMeasureFixture(t, pool)

	s := server.NewMCPServer("measure", "0.1.0", server.WithToolCapabilities(false))
	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	projectsStore := projects.NewStore(pool)
	mcptools.RegisterShow(s, pool, issuesStore, depsStore)
	mcptools.RegisterList(s, pool, issuesStore, projectsStore, nil)
	mcptools.RegisterClose(s, pool, issuesStore)
	mcptools.RegisterCreate(s, pool, issuesStore, depsStore, projectsStore)
	mcptools.RegisterEpicCreate(s, pool, issuesStore, projectsStore)

	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("new in-process client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// The caller is a NON-admin member of the visible project only —
	// that is what makes the cross-project edge redacted, and a redacted
	// edge is one of the objects SC-009 is stated on.
	ctx := auth.WithUserID(context.Background(), fx.callerID)
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithSessionID(ctx, fx.sessionID)
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "measure", Version: "1"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	m := &respMeasurer{t: t, ctx: ctx, c: c}

	// SC-001 — the wave node read with no parameters at all.
	waveDefault := m.call("skein_show.wave_node.default",
		"skein_show", map[string]any{"id": fx.waveNodeID})

	// SC-008 — the whole thread of measureThreadComments comments.
	m.call("skein_show.wave_node.comments_all",
		"skein_show", map[string]any{"id": fx.waveNodeID, "commentsLimit": 0})

	// SC-002 — one list row on the reference issue. The envelope is
	// reported too so the row's share of it stays visible.
	//
	// The filter is `text`, not `q`: `q` is the CROSS-PROJECT full-text
	// parameter and the project-scoped path ignores it silently, so the
	// row named "reference_issue" used to measure the project's whole
	// default page. Since `q` is the wrong argument here rather than the
	// wrong name, it is the CALL that was corrected — the label describes
	// what the row is for, and a baseline whose name and object disagree
	// enters T030's before/after comparison as a number about something
	// else. `text` filters on `i.title ILIKE`, which the padded reference
	// title matches uniquely inside the fixture's throwaway project.
	listResp := m.call("skein_list.reference_issue",
		"skein_list", map[string]any{"project": fx.projectID.String(), "text": fx.referenceTitle})
	m.part("skein_list.reference_issue.row", pickListRow(t, listResp, fx.referenceID))

	// SC-002 cross-project shift: the same row read WITHOUT `project`
	// carries the project identifier, and SC-002/SC-014 shift the budget
	// by exactly its length. Measured so the shift is a number, not an
	// estimate.
	crossResp := m.call("skein_list.cross_project",
		"skein_list", map[string]any{"crossProject": true})
	m.part("skein_list.cross_project.row", pickListRow(t, crossResp, fx.referenceID))

	// SC-009 — the dependency edges of the reference issue: one to a
	// visible issue (carries the other side's title) and one redacted
	// cross-project edge (carries no title at all).
	// Since T009 the default read carries NO edges, so the objects SC-009
	// is stated on have to be asked for by type. Both rows are kept: the
	// default one is what SC-001 is measured on, the typed one is where
	// the edges live now.
	m.call("skein_show.reference_issue.default",
		"skein_show", map[string]any{"id": fx.referenceID})
	refShow := m.call("skein_show.reference_issue.typed_edges",
		"skein_show", map[string]any{
			"id":        fx.referenceID,
			"depsTypes": []any{"parent-child", "related"},
		})
	visibleEdge, redactedEdge := pickEdges(t, refShow)
	m.part("skein_show.reference_issue.edge_visible", visibleEdge)
	m.part("skein_show.reference_issue.edge_redacted", redactedEdge)

	// SC-006 — close with no warnings: the issue has an assignee (no
	// ownerless warning) and no children (no open-children warning).
	m.call("skein_close.no_warnings",
		"skein_close", map[string]any{
			"id":              fx.closeTargetID,
			"expectedVersion": fx.closeTargetVersion,
			"reason":          "baseline measurement",
		})

	// SC-007 — create with the duplicate gate NOT firing.
	m.call("skein_create.no_duplicates",
		"skein_create", map[string]any{
			"project": fx.projectID.String(),
			"title":   "zzz baseline create " + fx.suffix,
			"type":    "task",
			"kind":    "autonomous",
		})

	// SC-015 — epic with measureEpicChildren children.
	children := make([]any, 0, measureEpicChildren)
	for i := 1; i <= measureEpicChildren; i++ {
		children = append(children, map[string]any{"title": fmt.Sprintf("baseline epic child %02d", i)})
	}
	m.call("skein_epic_create.20_children",
		"skein_epic_create", map[string]any{
			"project":        fx.projectID.String(),
			"title":          "zzz baseline epic " + fx.suffix,
			"kind":           "autonomous",
			"childTemplates": children,
		})

	fmt.Printf("MEASURE_RESP_TOTAL %d %d\n", m.totalBytes, m.rows)

	// The harness asserts nothing about the numbers — the SC gates live
	// in the tasks that change the wire. It does assert the fixture is
	// the shape the numbers are supposed to describe, because a baseline
	// taken on the wrong object is worse than no baseline.
	if waveDefault == "" {
		t.Fatal("wave node default read returned an empty payload")
	}
}

// respMeasurer calls a tool and reports the size of its response.
type respMeasurer struct {
	t          *testing.T
	ctx        context.Context
	c          *client.Client
	totalBytes int
	rows       int
}

// call invokes a tool, fails on a transport or business error (a measured
// error payload would be a number describing the wrong thing), prints the
// MEASURE_RESP row and returns the raw payload.
func (m *respMeasurer) call(label, tool string, args map[string]any) string {
	m.t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	res, err := m.c.CallTool(m.ctx, req)
	if err != nil {
		m.t.Fatalf("%s: CallTool: %v", label, err)
	}
	if len(res.Content) == 0 {
		m.t.Fatalf("%s: empty content", label)
	}
	txt, ok := res.Content[0].(mcpgo.TextContent)
	if !ok {
		m.t.Fatalf("%s: content[0] is %T, want TextContent", label, res.Content[0])
	}
	if res.IsError {
		m.t.Fatalf("%s: tool returned an error payload: %s", label, txt.Text)
	}
	m.emit("MEASURE_RESP", label, txt.Text)
	return txt.Text
}

// part reports a sub-object of a response (one list row, one edge).
func (m *respMeasurer) part(label string, payload string) {
	m.t.Helper()
	m.emit("MEASURE_PART", label, payload)
}

func (m *respMeasurer) emit(prefix, label, payload string) {
	m.t.Helper()
	fmt.Printf("%s %d %d %s\n", prefix, len(payload), utf8.RuneCountInString(payload), label)
	m.totalBytes += len(payload)
	m.rows++
}

// pickListRow re-marshals the one items[] entry whose id matches, so the
// number is "the row", not "the row plus its share of the envelope".
func pickListRow(t *testing.T, listPayload, wantID string) string {
	t.Helper()
	var parsed struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(listPayload), &parsed); err != nil {
		t.Fatalf("parse skein_list payload: %v", err)
	}
	for _, raw := range parsed.Items {
		var probe struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatalf("parse skein_list row: %v", err)
		}
		if probe.ID == wantID {
			return string(raw)
		}
	}
	t.Fatalf("skein_list result has no row for %s (rows: %d) — the fixture or the filter drifted", wantID, len(parsed.Items))
	return ""
}

// pickEdges returns (visible edge, redacted edge) from a skein_show
// payload. Both must be present: the redacted one is the object SC-009
// states its 110-char sub-budget on, and finding none would silently
// measure only half the criterion.
func pickEdges(t *testing.T, showPayload string) (string, string) {
	t.Helper()
	var parsed struct {
		Deps struct {
			Outgoing []json.RawMessage `json:"outgoing"`
			Incoming []json.RawMessage `json:"incoming"`
		} `json:"deps"`
	}
	if err := json.Unmarshal([]byte(showPayload), &parsed); err != nil {
		t.Fatalf("parse skein_show payload: %v", err)
	}
	var visible, redacted string
	for _, raw := range append(append([]json.RawMessage{}, parsed.Deps.Outgoing...), parsed.Deps.Incoming...) {
		var probe struct {
			Redacted bool `json:"redacted"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatalf("parse dependency edge: %v", err)
		}
		if probe.Redacted && redacted == "" {
			redacted = string(raw)
		}
		if !probe.Redacted && visible == "" {
			visible = string(raw)
		}
	}
	if visible == "" {
		t.Fatal("skein_show returned no visible dependency edge — SC-009's main object is missing from the fixture")
	}
	if redacted == "" {
		t.Fatal("skein_show returned no redacted dependency edge — SC-009's invisible-side object is missing from the fixture")
	}
	return visible, redacted
}

// measureCommentBodyLen is the per-comment body length of the measured
// thread. The SC-008 number is a function of it, so it is named: T030
// must re-measure on the same fixture or its "after" is not comparable
// with this "before".
//
// The value is not arbitrary — and the way it is derived was WRONG until
// T009 exposed it. Serialisation adds ~323 characters of CommentDTO around
// each body. The original figure was read off a whole-thread response that
// still carried all 58 EDGES, so ~16 900 characters of graph were
// attributed to the comments and the per-comment overhead came out at
// ~674; a body sized against that inflated number produced a thread of
// ~49 000 while the arithmetic claimed ~66 000. Removing the edges from
// the response (T009) made the two disagree out loud.
//
// Sized against the real overhead: 45 bodies at this length plus the three
// short recent ones put the whole-thread response at ~64 700, which is the
// 64 672 the spec measured on the real 48-comment thread. That matters
// because SC-008's threshold (< 10 000) is an ABSOLUTE number derived
// against that object — a thinner fixture would let the criterion pass
// without proving anything.
const measureCommentBodyLen = 1070

// measureRecentCommentBodyLen is the body length of the THREE most recent
// comments — the ones a default read returns in full.
//
// It is separate from measureCommentBodyLen because the spec pins TWO
// numbers on the same node and they constrain different parts of the
// thread. SC-008 is stated on the whole thread (64 672 characters over 48
// comments), which fixes the AVERAGE body. SC-001 is stated on the default
// read and the spec spells out its composition: "on the measured node the
// edges were 20 742 characters of 23 496, so after removing them 2 754
// remain (card, envelope, metadata, three most recent comments)" — which
// fixes the size of the LAST THREE.
//
// A thread with uniform bodies cannot satisfy both: sized for the average
// it makes the default read a thousand characters fatter than the object
// SC-001's threshold was derived against, and SC-001 then fails for a
// property of the fixture rather than of the code. Real threads are not
// uniform either. So the tail is short and the body of the thread is not,
// and the two measured totals land on the two numbers the spec recorded.
const measureRecentCommentBodyLen = 340

// measureFixture is the set of measured objects the spec names: the wave
// node, the reference issue, a dependency edge (visible and redacted), and
// throwaway targets for close / create / epic_create.
type measureFixture struct {
	callerID   uuid.UUID
	callerName string
	sessionID  string
	projectID  uuid.UUID
	suffix     string

	waveNodeID     string
	referenceID    string
	referenceTitle string
	closeTargetID  string
	// closeTargetVersion is read back rather than assumed: SetAssignee
	// bumps the row's version, and skein_close needs the live one.
	closeTargetVersion int
}

// seedMeasureFixture builds the measured objects. Everything is seeded
// through internal/testsupport so teardown is the project cleanup those
// helpers already register.
//
// The caller is a NON-admin who owns the visible project and has no
// membership at all in the second one; that is what turns the reference
// issue's cross-project edge into a redacted edge.
func seedMeasureFixture(t *testing.T, pool *pgxpool.Pool) *measureFixture {
	t.Helper()

	callerID, callerName := testsupport.SeedUser(t, pool, "user")
	projectID, prefix := testsupport.SeedProject(t, pool, callerID, "meas")
	hiddenOwner, _ := testsupport.SeedUser(t, pool, "user")
	hiddenID, hiddenPrefix := testsupport.SeedProject(t, pool, hiddenOwner, "mhid")
	componentID := testsupport.SeedComponent(t, pool, projectID, callerID, "measure", "")

	fx := &measureFixture{
		callerID:   callerID,
		callerName: callerName,
		sessionID:  "measure-session-" + projectID.String(),
		projectID:  projectID,
		suffix:     prefix,
	}

	// ---- wave node: 57 children (45 closed / 12 open), one standing
	// parent above it, and a thread of 48 comments.
	waveParent := testsupport.SeedIssue(t, pool, projectID, callerID, prefix+"-1",
		"wave parent", "standing", 2)
	fx.waveNodeID = testsupport.SeedIssue(t, pool, projectID, callerID, prefix+"-2",
		"wave node under measurement", "open", 1)
	testsupport.SeedDep(t, pool, fx.waveNodeID, waveParent, "parent-child", callerID)
	for i := 1; i <= measureWaveNodeChildren; i++ {
		status := "open"
		if i <= measureWaveNodeClosed {
			status = "closed"
		}
		childID := testsupport.SeedIssue(t, pool, projectID, callerID,
			fmt.Sprintf("%s-2.%d", prefix, i),
			fmt.Sprintf("wave child %02d", i), status, 2)
		testsupport.SeedDep(t, pool, childID, fx.waveNodeID, "parent-child", callerID)
	}
	body := strings.Repeat("a", measureCommentBodyLen)
	recentBody := strings.Repeat("a", measureRecentCommentBodyLen)
	for i := 0; i < measureThreadComments; i++ {
		// The last three are the ones a default read returns in full —
		// see measureRecentCommentBodyLen.
		if i >= measureThreadComments-3 {
			testsupport.SeedComment(t, pool, fx.waveNodeID, callerID, recentBody)
			continue
		}
		testsupport.SeedComment(t, pool, fx.waveNodeID, callerID, body)
	}

	// ---- reference issue (US-2): 60-char title, one assignee, two
	// labels, one component, a parent, no lock.
	refParent := testsupport.SeedIssue(t, pool, projectID, callerID, prefix+"-3",
		"reference parent", "open", 2)
	fx.referenceTitle = padTo("reference issue used for every row and edge budget", measureReferenceTitleLen)
	fx.referenceID = testsupport.SeedIssue(t, pool, projectID, callerID, prefix+"-3.1",
		fx.referenceTitle, "open", 2)
	testsupport.SeedDep(t, pool, fx.referenceID, refParent, "parent-child", callerID)
	testsupport.SetAssignee(t, pool, fx.referenceID, callerName)
	seedIssueLabels(t, pool, fx.referenceID, "backend", "p1")
	seedIssueComponent(t, pool, fx.referenceID, componentID)

	// The redacted edge: the other side lives in a project the caller
	// cannot see, so skein_show returns the edge without a title.
	hiddenIssue := testsupport.SeedIssue(t, pool, hiddenID, hiddenOwner, hiddenPrefix+"-1",
		"invisible counterpart", "open", 2)
	testsupport.SeedDep(t, pool, fx.referenceID, hiddenIssue, "related", callerID)

	// ---- close target: locked by THIS session (skein_close requires the
	// lock) and assigned (an unassigned issue would draw the ownerless
	// warning, and SC-006 is stated on a close WITHOUT warnings).
	fx.closeTargetID = testsupport.SeedLockedIssue(t, pool, projectID, callerID,
		prefix+"-4", fx.sessionID, 0)
	testsupport.SetAssignee(t, pool, fx.closeTargetID, callerName)
	if err := pool.QueryRow(context.Background(),
		`SELECT version FROM issues WHERE id = $1`, fx.closeTargetID).
		Scan(&fx.closeTargetVersion); err != nil {
		t.Fatalf("read close-target version: %v", err)
	}

	return fx
}

// padTo right-pads s with 'x' to exactly n runes (or truncates it), so the
// reference title is exactly the length SC-002 / SC-009 shift their
// budgets from.
func padTo(s string, n int) string {
	r := []rune(s)
	if len(r) >= n {
		return string(r[:n])
	}
	return s + strings.Repeat("x", n-len(r))
}

func seedIssueLabels(t *testing.T, pool *pgxpool.Pool, issueID string, labels ...string) {
	t.Helper()
	for _, l := range labels {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO issue_labels (issue_id, label) VALUES ($1, $2)
			 ON CONFLICT DO NOTHING`, issueID, l); err != nil {
			t.Fatalf("seed label %q on %s: %v", l, issueID, err)
		}
	}
}

func seedIssueComponent(t *testing.T, pool *pgxpool.Pool, issueID string, componentID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, issueID, componentID); err != nil {
		t.Fatalf("seed component on %s: %v", issueID, err)
	}
}
