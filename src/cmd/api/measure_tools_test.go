package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	mcptools "skein-api/internal/mcp/tools"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/monitorregistry"
	"skein-api/internal/notifications"
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

// The fixed input of the skein_get_monitor_script row of
// TestMeasureToolResponses.
const (
	measureMonitorPublicURL    = "https://skein.example.test"
	measureMonitorRunnerSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	measureMonitorLoaderSHA256 = "1111111111111111111111111111111111111111111111111111111111111111"
	measureMonitorIdleTTL      = 6 * time.Hour
	// The MCP session of the monitor calls: a literal, so the response and
	// the working command do not depend on a generated id (SC-009 fixed
	// inputs).
	measureMonitorSession = "mcp-session-0123456789abcdef0123456789abcdef"
)

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
	// skein_get_monitor_script on a fixed input: the public URL, the runner
	// digest and the renewal window are constants of this measurement, not
	// of a deployment, so the row changes only when the tool's response does.
	mcptools.RegisterGetMonitorScript(s, pool, notifications.NewStore(pool),
		monitorcreds.New(pool, measureMonitorIdleTTL), measureMonitorRegistry(t), measureMonitorPublicURL,
		measureMonitorRunnerSHA256, measureMonitorLoaderSHA256, measureMonitorIdleTTL)

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
			"closeOutcome":    "done",
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

	// SC-009 — skein_get_monitor_script without `client`: the baseline the
	// growth of the response without `client` (≤ 200 bytes) and the response
	// with `client` (≤ this + 4 096 bytes) are compared against. The mint
	// needs the caller's API key; only this call carries one, so the rows
	// above keep the context they were baselined with.
	monitorKey := seedMeasureAPIKey(t, pool, fx.callerID)
	monitorCtx := auth.WithKeyGrant(auth.WithAPIKeyID(auth.WithSessionID(ctx, measureMonitorSession), monitorKey),
		apikeys.Grant{Level: apikeys.LevelManage, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}})
	noClient := m.callCtx(monitorCtx, "skein_get_monitor_script.no_client",
		"skein_get_monitor_script", map[string]any{"id": fx.referenceID})
	// The same call with `client` on POSIX, for a name the registry does
	// not know (the general record): the response with `client` is compared
	// against the one without it (SC-009: ≤ + 4 096 bytes).
	m.callCtx(monitorCtx, "skein_get_monitor_script.client_general_posix",
		"skein_get_monitor_script", map[string]any{"id": fx.referenceID, "client": "measure-unknown-agent"})
	// SC-009, STOP & VALIDATE 2: the representative record of the contract
	// (stream, exit-on-event and a host in the registry) on POSIX.
	fixture := m.callCtx(monitorCtx, "skein_get_monitor_script.client_fixture3_posix",
		"skein_get_monitor_script", map[string]any{"id": fx.referenceID, "client": measureMonitorFixtureClient})
	measureMonitorStart(m, noClient, fixture)
	measureMonitorRealRegistry(m, pool, monitorCtx, fx.referenceID)

	fmt.Printf("MEASURE_RESP_TOTAL %d %d\n", m.totalBytes, m.rows)

	// The harness asserts nothing about the numbers — the SC gates live
	// in the tasks that change the wire. It does assert the fixture is
	// the shape the numbers are supposed to describe, because a baseline
	// taken on the wrong object is worse than no baseline.
	if waveDefault == "" {
		t.Fatal("wave node default read returned an empty payload")
	}
}

// measureMonitorStart reports the parts of the start of a monitor that
// SC-009 and SC-012 bound, on the same fixed input as the responses
// (STOP & VALIDATE 2, the early slice of R-10):
//   - starter: the starter source the bootstrap without `client` carries;
//   - prepare_ready: the ready line the `prepare` of the fixture record
//     prints (run for real; it creates and then loses its token directory);
//   - working_command: the `command` of the line of a real preflight of the
//     runner, run with the variables of the fixture record's `preflight`
//     command in the token directory prepare made;
//   - first_start_monitor_started, restart_monitor_started: the
//     monitor_started lines of the working start by that command (the
//     behavioural fixture): the first start by the result, then a restart
//     by the same command after the detached runner was stopped (C1);
//   - delivered: the stop line of an exit-on-event cycle that printed one
//     event (a preflight of the same token with an exit-on-event record).
func measureMonitorStart(m *respMeasurer, noClient, fixture string) {
	t := m.t
	t.Helper()
	var plain, named struct {
		Bootstrap     string `json:"bootstrap"`
		Prepare       string `json:"prepare"`
		Preflight     string `json:"preflight"`
		TokenFilePath string `json:"tokenFilePath"`
	}
	if err := json.Unmarshal([]byte(noClient), &plain); err != nil {
		t.Fatalf("decode the response without client: %v", err)
	}
	if err := json.Unmarshal([]byte(fixture), &named); err != nil {
		t.Fatalf("decode the response of the fixture record: %v", err)
	}

	const run = " python3 -c "
	i := strings.LastIndex(plain.Bootstrap, run)
	if i < 0 {
		t.Fatalf("the bootstrap without client runs no starter text:\n%s", plain.Bootstrap)
	}
	quoted := plain.Bootstrap[i+len(run):]
	if len(quoted) < 2 || quoted[0] != '\'' || quoted[len(quoted)-1] != '\'' {
		t.Fatalf("the starter of the bootstrap is not one single-quoted word:\n%s", quoted)
	}
	m.part("skein_get_monitor_script.starter", strings.ReplaceAll(quoted[1:len(quoted)-1], `'\''`, "'"))

	if named.Prepare == "" || named.Preflight == "" || named.TokenFilePath == "" {
		t.Fatalf("the response of the fixture record lacks prepare, preflight or tokenFilePath")
	}
	out, err := exec.Command("sh", "-c", named.Prepare).Output()
	if err != nil {
		// prepare refused: the directory, if any, is not one it created.
		t.Fatalf("run prepare: %v; stdout %q", err, out)
	}
	// prepare created the token directory: it is this measurement's to remove.
	tokenDir := path.Dir(named.TokenFilePath)
	t.Cleanup(func() { _ = os.RemoveAll(tokenDir) })
	if err := os.WriteFile(named.TokenFilePath, []byte("skm_"+strings.Repeat("m", 43)), 0o600); err != nil {
		t.Fatalf("write the token: %v", err)
	}
	m.part("skein_get_monitor_script.prepare_ready", strings.TrimRight(string(out), "\n"))

	cmd := exec.Command("python3", "-c", measureWorkingCommandPy)
	cmd.Dir = "../../internal/handler/assets/runner_tests"
	cmd.Env = append(os.Environ(), "MEASURE_PREFLIGHT="+named.Preflight, "MEASURE_STATE_HOME="+t.TempDir())
	cmd.Stderr = os.Stderr
	out, err = cmd.Output()
	if err != nil {
		t.Fatalf("run the preflight and the working start: %v; stdout %q", err, out)
	}
	var lines struct {
		Command   string `json:"command"`
		First     string `json:"first"`
		Restart   string `json:"restart"`
		Delivered string `json:"delivered"`
	}
	if err := json.Unmarshal(out, &lines); err != nil {
		t.Fatalf("decode the lines of the start: %v; stdout %q", err, out)
	}
	m.part("skein_get_monitor_script.working_command", lines.Command)
	m.part("skein_get_monitor_script.first_start_monitor_started", lines.First)
	m.part("skein_get_monitor_script.restart_monitor_started", lines.Restart)
	m.part("skein_get_monitor_script.delivered", lines.Delivered)
}

// measureWorkingCommandPy runs the runner's preflight (the body the server
// serves, loaded by the runner tests' harness) in the token directory that
// prepare made, with the SKEIN_* variables of the preflight command in
// MEASURE_PREFLIGHT; that command is the measured one. Then it starts the
// monitor twice the way an agent does: the `command` of the line of a
// preflight of the same token against a local server (its address and the
// SHA-256 of the loader and the runner it serves are the only values that
// differ), run by sh, through the starter prepare wrote, the served loader
// and the served runner: the first start by the result, and a restart after
// the detached runner was stopped by a signal (it is found by its token
// file). Then it runs a preflight of the same token against a registry
// whose record of the fixture client has only an exit-on-event method, and
// one working cycle of its command, run by sh the same way, against a page
// with one event. It prints {"command", "first", "restart", "delivered"}:
// the command, the two monitor_started lines and the last line of the
// cycle as printed. The state home is a directory of the measurement. A
// detached runner of the token left by a failure is killed on exit.
const measureWorkingCommandPy = `import atexit, base64, hashlib, json, os, shlex, signal, subprocess, sys, time
sys.path.insert(0, os.getcwd())
import harness
R = harness.load_runner()
home = os.environ["MEASURE_STATE_HOME"]
env = {"HOME": home, "XDG_STATE_HOME": home, "PATH": os.environ.get("PATH", "")}
for word in shlex.split(os.environ["MEASURE_PREFLIGHT"]):
    name, sep, value = word.partition("=")
    if sep and name.startswith("SKEIN_"):
        env[name] = value
token_file = env["SKEIN_TOKEN_FILE"]
registry = R.load_registry()
settings = R.read_settings(env, registry, mode="client")
command = R.preflight(token_file, env, registry, settings)["command"]

def runners():
    """The pids of processes that run a monitor of this token file."""
    mark = ("SKEIN_TOKEN_FILE=" + token_file).encode()
    pids = []
    for name in os.listdir("/proc"):
        if not name.isdigit() or int(name) == os.getpid():
            continue
        try:
            with open("/proc/%s/environ" % name, "rb") as fh:
                if mark in fh.read().split(b"\0"):
                    pids.append(int(name))
        except OSError:
            pass
    return pids

def kill_left():
    for pid in runners():
        try:
            os.killpg(pid, signal.SIGKILL)
        except OSError:
            pass
atexit.register(kill_left)

runner = harness.render_runner().encode("utf-8")
loader = harness.served_loader().encode("utf-8")
page = {"notifications": [], "cursor": {"ts": "2026-01-01T00:00:00Z", "id": 0}}
srv = harness.LoaderServer(script=lambda h: h.send_body(200, runner),
                           poll=lambda h: h.send_body(200, json.dumps(page).encode()))
atexit.register(srv.stop)
local = dict(env, SKEIN_PUBLIC_URL=srv.url(), SKEIN_LOADER_SHA256=hashlib.sha256(loader).hexdigest(),
             SKEIN_RUNNER_SHA256=hashlib.sha256(runner).hexdigest())
shell_env = {"HOME": home, "XDG_STATE_HOME": home, "PATH": env["PATH"]}

def run(cmd):
    """The command as the agent runs it: its printed lines and exit code."""
    done = subprocess.run(["sh", "-c", cmd], env=shell_env, stdout=subprocess.PIPE, stderr=sys.stderr,
                          timeout=120)
    return done.returncode, [l for l in done.stdout.decode("utf-8").splitlines() if l]

def leaders():
    """The detached runners of the token: the leaders of their sessions (a
    runner's request process shares its session)."""
    out = []
    for pid in runners():
        try:
            if os.getsid(pid) == pid:
                out.append(pid)
        except OSError:
            pass
    return out

def start(cmd):
    code, lines = run(cmd)
    if code != 0 or len(lines) != 1 or json.loads(lines[0]).get("event") != "monitor_started":
        sys.exit("the working start did not print one monitor_started: code %r, %r" % (code, lines))
    pids = leaders()
    if len(pids) != 1:
        sys.exit("the working start left %d detached runners of the token, not one" % len(pids))
    return lines[0], pids[0]

def stop(pid):
    os.kill(pid, signal.SIGTERM)
    deadline = time.monotonic() + 30
    while runners():
        if time.monotonic() > deadline:
            sys.exit("the detached runner did not stop")
        time.sleep(0.05)

run_command = R.preflight(token_file, local, registry, R.read_settings(local, registry, mode="client"))["command"]
first, pid = start(run_command)
stop(pid)
restart, pid = start(run_command)
stop(pid)
if "journal" not in json.loads(first) or "journal" in json.loads(restart):
    sys.exit("the first start is not the full line or the restart is not the short one: %s / %s" % (first, restart))

# One exit-on-event cycle: the same token, a record with exit-on-event only.
with open(harness.REGISTRY_JSON) as fh:
    doc = json.load(fh)
doc["agents"] = [{"id": env["SKEIN_CLIENT"], "about": "measurement", "source": "measurement",
                  "process": {"executables": ["measure-agent"]},
                  "methods": [{"kind": "exit-on-event", "conditions": {},
                               "instruction": "Run command as a background task; run it again when it ends."}]}]
exit_registry = R.load_registry(base64.b64encode(json.dumps(doc).encode()).decode())
exit_command = R.preflight(token_file, local, exit_registry,
                           R.read_settings(local, exit_registry, mode="client"))["command"]
page = {"notifications": [{"id": 7, "type": "comment_added", "entity": {"type": "issue", "id": env["SKEIN_ISSUE"]},
                           "actor": {"username": "measure"}, "commentId": "c-7", "bodyPreview": "measured event"}],
        "cursor": {"ts": "2026-01-01T00:00:01Z", "id": 7}}
code, cycle = run(exit_command)
if code != 0 or not cycle or json.loads(cycle[-1]).get("reason") != "delivered":
    sys.exit("the exit-on-event cycle did not end with delivered: code %r, %r" % (code, cycle))
sys.stdout.write(json.dumps({"command": command, "first": first, "restart": restart, "delivered": cycle[-1]}))
`

// measureMonitorRealRegistry measures skein_get_monitor_script with `client`
// on POSIX against the registry the server embeds: a row per record, the
// general record (an unknown id: its warning lists every id of the real
// registry), and the largest of them as the part
// skein_get_monitor_script.client_posix_max (SC-009).
func measureMonitorRealRegistry(m *respMeasurer, pool *pgxpool.Pool, ctx context.Context, issueID string) {
	t := m.t
	t.Helper()
	reg := monitorregistry.MustEmbedded()
	onReal := &respMeasurer{t: t, ctx: ctx, c: measureMonitorClient(t, ctx, pool, "measure-real", reg)}
	largest := onReal.call("skein_get_monitor_script.client_general_real_posix",
		"skein_get_monitor_script", map[string]any{"id": issueID, "client": "measure-unknown-agent"})
	// The windows - posix growth of each record's response, by the shell its
	// Windows commands are written for (SC-009).
	growth := map[string][2]int{}
	measureWindows := func(label, client, shell, posix string) {
		resp := onReal.call("skein_get_monitor_script.client_"+label+"_windows", "skein_get_monitor_script",
			map[string]any{"id": issueID, "client": client, "platform": "windows", "tokenDir": measureMonitorTokenDir})
		d := [2]int{len(resp) - len(posix), utf8.RuneCountInString(resp) - utf8.RuneCountInString(posix)}
		if old, ok := growth[shell]; !ok || d[0] > old[0] {
			growth[shell] = d
		}
	}
	// The general record runs Windows PowerShell.
	measureWindows("general_real", "measure-unknown-agent", monitorregistry.WindowsShellPowerShell, largest)
	for _, id := range reg.AgentIDs() {
		resp := onReal.call("skein_get_monitor_script.client_"+id+"_posix",
			"skein_get_monitor_script", map[string]any{"id": issueID, "client": id})
		shell := reg.Resolve(id).WindowsShell()
		if shell == "" {
			shell = monitorregistry.WindowsShellPowerShell
		}
		measureWindows(id, id, shell, resp)
		if len(resp) > len(largest) {
			largest = resp
		}
	}
	m.totalBytes += onReal.totalBytes
	m.rows += onReal.rows
	// No record of the registry names pwsh: its commands are measured on a
	// copy of the registry with one more record that does, so the pwsh row
	// is a response built by the pwsh rules, not a relabelled one.
	onPwsh := &respMeasurer{t: t, ctx: ctx, c: measureMonitorClient(t, ctx, pool, "measure-pwsh", measurePwshRegistry(t))}
	posixPwsh := onPwsh.call("skein_get_monitor_script.client_"+measurePwshRecord+"_posix",
		"skein_get_monitor_script", map[string]any{"id": issueID, "client": measurePwshRecord})
	respPwsh := onPwsh.call("skein_get_monitor_script.client_"+measurePwshRecord+"_windows", "skein_get_monitor_script",
		map[string]any{"id": issueID, "client": measurePwshRecord, "platform": "windows", "tokenDir": measureMonitorTokenDir})
	growth[monitorregistry.WindowsShellPwsh] = [2]int{len(respPwsh) - len(posixPwsh),
		utf8.RuneCountInString(respPwsh) - utf8.RuneCountInString(posixPwsh)}
	m.totalBytes += onPwsh.totalBytes
	m.rows += onPwsh.rows
	m.part("skein_get_monitor_script.client_posix_max", largest)
	for _, sh := range []struct{ shell, label string }{
		{monitorregistry.WindowsShellGitBash, "gitbash"},
		{monitorregistry.WindowsShellPwsh, "pwsh"},
		{monitorregistry.WindowsShellPowerShell, "powershell"},
	} {
		d, ok := growth[sh.shell]
		if !ok {
			t.Fatalf("no record of the real registry or general record runs %s", sh.shell)
		}
		m.figure("skein_get_monitor_script.windows_"+sh.label+"_delta_max", d[0], d[1])
	}
}

// measurePwshRecord is the record the pwsh rows are measured on: a copy of
// the general-purpose Claude record of the real registry that names pwsh.
const measurePwshRecord = "measure-pwsh"

// measurePwshRegistry is the real registry with measurePwshRecord added.
func measurePwshRegistry(t *testing.T) *monitorregistry.Registry {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(monitorregistry.Embedded(), &doc); err != nil {
		t.Fatalf("embedded registry: %v", err)
	}
	agents, _ := doc["agents"].([]any)
	var claude map[string]any
	for _, a := range agents {
		if rec, ok := a.(map[string]any); ok && rec["id"] == "claude" {
			claude = rec
		}
	}
	if claude == nil {
		t.Fatal("the real registry has no claude record to copy")
	}
	rec := map[string]any{}
	for k, v := range claude {
		rec[k] = v
	}
	rec["id"] = measurePwshRecord
	rec["windowsShell"] = monitorregistry.WindowsShellPwsh
	rec["process"] = map[string]any{"executables": []any{measurePwshRecord}}
	doc["agents"] = append(agents, rec)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	reg, err := monitorregistry.Load(raw)
	if err != nil {
		t.Fatalf("the registry with a pwsh record: %v", err)
	}
	return reg
}

// measureMonitorClient is an in-process MCP client of a server that has
// skein_get_monitor_script over reg.
func measureMonitorClient(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string,
	reg *monitorregistry.Registry) *client.Client {
	t.Helper()
	s := server.NewMCPServer(name, "0.1.0", server.WithToolCapabilities(false))
	mcptools.RegisterGetMonitorScript(s, pool, notifications.NewStore(pool),
		monitorcreds.New(pool, measureMonitorIdleTTL), reg, measureMonitorPublicURL,
		measureMonitorRunnerSHA256, measureMonitorLoaderSHA256, measureMonitorIdleTTL)
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("new in-process client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "measure", Version: "1"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return c
}

// measureMonitorTokenDir is the tokenDir of the Windows rows: one fixed
// literal for every record, so the growth over POSIX compares like with
// like (SC-009, "Units and points of measurement").
const measureMonitorTokenDir = `C:\Users\measure\AppData\Local`

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
	return m.callCtx(m.ctx, label, tool, args)
}

// callCtx is call with the request context given by the caller.
func (m *respMeasurer) callCtx(ctx context.Context, label, tool string, args map[string]any) string {
	m.t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	res, err := m.c.CallTool(ctx, req)
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

// figure reports a figure that is not one payload (a difference of two
// responses) as a MEASURE_PART row of bytes and runes.
func (m *respMeasurer) figure(label string, bytes, runes int) {
	m.t.Helper()
	fmt.Printf("%s %d %d %s\n", "MEASURE_PART", bytes, runes, label)
	m.rows++
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
	// The lock belongs to a session AND a user (sk-424): without the owner
	// the close answers locked_by_other even for the same session string.
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET locked_by_user_id = $2 WHERE id = $1`, fx.closeTargetID, callerID); err != nil {
		t.Fatalf("set close-target lock owner: %v", err)
	}
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

// seedMeasureAPIKey inserts an API key row for the caller: the monitor mint
// references the calling key (FK to api_keys), so the key must be real.
func seedMeasureAPIKey(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	prefix := "ska_" + strings.ReplaceAll(id.String()[:12], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO api_keys (id, user_id, key_hash, prefix, label, created_at,
                      level, is_admin, scope_mode)
VALUES ($1, $2, 'fake-hash-for-measure', $3, 'measure', now(), 'manage', false, 'all')`, id, userID, prefix)
	if err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	// Registered after the fixture → runs first in LIFO cleanup, so the
	// CASCADE drops the minted monitor credentials before the user goes.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, id)
	})
	return id
}

// measureMonitorFixtureClient is the agent record of measureMonitorRegistry.
const measureMonitorFixtureClient = "measure-fixture"

// measureMonitorRegistry is the embedded registry with its records replaced
// by a representative one for the size of a response with `client`: an
// agent with a stream and an exit-on-event method (instructions as long as
// the catalogue's streaming tools need) and two warnings, and a host
// channel the agent can be woken by. Its values describe no real agent.
func measureMonitorRegistry(t *testing.T) *monitorregistry.Registry {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(monitorregistry.Embedded(), &doc); err != nil {
		t.Fatalf("decode embedded registry: %v", err)
	}
	instruction := func(tool string) string {
		return "Run `command` with your " + tool + " tool as a background task with the longest " +
			"timeout it allows and keep the task id; each stdout line is one event. When the task " +
			"ends, run the same `command` again; do not run a second copy while one is running."
	}
	doc["agents"] = []any{map[string]any{
		"id":      measureMonitorFixtureClient,
		"about":   "Measurement fixture: an agent with a streaming tool and an exit-on-event fallback.",
		"source":  "measurement fixture of cmd/api/measure_tools_test.go",
		"process": map[string]any{"executables": []any{"measure-agent"}},
		"methods": []any{
			map[string]any{"kind": "stream", "instruction": instruction("Monitor"),
				"conditions": map[string]any{"env": []any{map[string]any{"name": "MEASURE_AGENT_MONITOR"}}},
				"limits": map[string]any{"line": map[string]any{"max": 400, "unit": "chars"},
					"batch": map[string]any{"max": 50, "unit": "lines"}}},
			map[string]any{"kind": "exit-on-event", "instruction": instruction("Bash"),
				"conditions": map[string]any{}},
		},
		"warnings": []any{
			"The streaming tool stops after its timeout; run the same command again when it ends.",
			"Lines printed while no tool reads them are kept in the journal.",
		},
	}}
	doc["channels"] = []any{map[string]any{
		"id": "measure-host", "about": "Measurement fixture host.", "source": "measurement fixture",
		"step": 5, "order": 1, "kind": "addressed", "blind": false,
		"detect":         map[string]any{"env": []any{map[string]any{"name": "MEASURE_HOST_ID"}}},
		"executor":       map[string]any{"type": "argv", "argv": []any{"measure-host", "send", "{env:MEASURE_HOST_ID}"}, "feed": "stdin", "timeoutSec": 30},
		"idle":           map[string]any{"type": "builtin"},
		"outcomes":       []any{map[string]any{"match": map[string]any{"exitCode": 0}, "outcome": "queued"}},
		"probeUndefined": true,
		"addressee":      map[string]any{"process": "none"},
		"binaries":       map[string]any{"linux": []any{"measure-host"}},
	}}
	// The engine map names agent records; the embedded ones were replaced
	// above, so the fixture carries a map of its own.
	doc["zavodEngines"] = map[string]any{}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode fixture registry: %v", err)
	}
	reg, err := monitorregistry.Load(raw)
	if err != nil {
		t.Fatalf("fixture registry: %v", err)
	}
	return reg
}
