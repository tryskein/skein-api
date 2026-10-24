package tools

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"strconv"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

// jsonUnmarshal is a tiny shim so the integration test doesn't reach for
// encoding/json all over — keeps the read path readable.
func jsonUnmarshal(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}

// TestIntegration_WriteToolsViaInProcessClient registers all R2.2b/c
// write tools on a fresh in-process MCP server and exercises one
// happy-path call per tool. Mirrors the read-tool integration test in
// integration_read_test.go.
//
// Skips when TEST_DATABASE_URL is unset.
func TestIntegration_WriteToolsViaInProcessClient(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "wintg")
	// Seed a starter issue and a second target so skein_update /
	// skein_comment / skein_dep_add have rows to operate on.
	seedIssue(t, pool, pid, userID, prefix+"-1", "starter", "open", 2)
	seedIssue(t, pool, pid, userID, prefix+"-2", "target", "open", 2)

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	pStore := projects.NewStore(pool)
	uStore := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)

	s := server.NewMCPServer("skein-test", "0.1.0",
		server.WithToolCapabilities(false))
	RegisterCreate(s, pool, issuesStore, depsStore, pStore)
	RegisterUpdate(s, pool, issuesStore)
	RegisterComment(s, pool, issuesStore)
	RegisterDepAdd(s, pool, depsStore, issuesStore)
	RegisterProjectCreate(s, pool, pStore, "members")
	RegisterComponent(s, pool, pStore)
	RegisterClaim(s, pool, issuesStore)
	RegisterUnclaim(s, pool, issuesStore)
	RegisterClose(s, pool, issuesStore)
	// R3.1 — user + project member tools, both folded into one
	// action-dispatched tool each by sk-255.3.
	RegisterUser(s, uStore, pool, spaces.NewService(pool, spaces.Options{Creation: spaces.CreationAll}))
	RegisterProjectMembers(s, pool, pStore, uStore)

	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("new in-process client: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUserRole(ctx, "admin")
	ctx = auth.WithSessionID(ctx, "intg-write-"+userID.String()[:8])

	if err := c.Start(ctx); err != nil {
		t.Fatalf("start client: %v", err)
	}

	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "tests-write", Version: "1"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	// tools/list should advertise every registered tool.
	listRes, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := map[string]bool{
		"skein_create":          false,
		"skein_update":          false,
		"skein_comment":         false,
		"skein_dep_add":         false,
		"skein_project_create":  false,
		"skein_component":       false,
		"skein_claim":           false,
		"skein_unclaim":         false,
		"skein_close":           false,
		"skein_user":            false,
		"skein_project_members": false,
	}
	for _, tool := range listRes.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
	}
	for n, found := range want {
		if !found {
			t.Errorf("tools/list missing %q", n)
		}
	}

	// 1) skein_create — happy path, unique title.
	createReq := mcpgo.CallToolRequest{}
	createReq.Params.Name = "skein_create"
	createReq.Params.Arguments = map[string]any{
		"project": pid.String(),
		"title":   "integration unique " + strconv.Itoa(1),
		"type":    "task",
	}
	createRes, err := c.CallTool(ctx, createReq)
	if err != nil {
		t.Fatalf("CallTool(skein_create): %v", err)
	}
	if createRes == nil || createRes.IsError {
		dumpErr(t, "skein_create", createRes)
		return
	}

	// 2) skein_update — bump priority on the seeded issue.
	updateReq := mcpgo.CallToolRequest{}
	updateReq.Params.Name = "skein_update"
	updateReq.Params.Arguments = map[string]any{
		"id":              prefix + "-1",
		"expectedVersion": 1,
		"priority":        4,
	}
	updateRes, err := c.CallTool(ctx, updateReq)
	if err != nil {
		t.Fatalf("CallTool(skein_update): %v", err)
	}
	if updateRes == nil || updateRes.IsError {
		dumpErr(t, "skein_update", updateRes)
		return
	}

	// 3) skein_comment — add a comment to the seeded issue.
	commentReq := mcpgo.CallToolRequest{}
	commentReq.Params.Name = "skein_comment"
	commentReq.Params.Arguments = map[string]any{
		"issueId": prefix + "-1",
		"body":    "integration test comment",
	}
	commentRes, err := c.CallTool(ctx, commentReq)
	if err != nil {
		t.Fatalf("CallTool(skein_comment): %v", err)
	}
	if commentRes == nil || commentRes.IsError {
		dumpErr(t, "skein_comment", commentRes)
		return
	}

	// 4) skein_dep_add — link the two seeded issues with a related edge.
	depAddReq := mcpgo.CallToolRequest{}
	depAddReq.Params.Name = "skein_dep_add"
	depAddReq.Params.Arguments = map[string]any{
		"fromId": prefix + "-1",
		"toId":   prefix + "-2",
		"type":   "related",
	}
	depAddRes, err := c.CallTool(ctx, depAddReq)
	if err != nil {
		t.Fatalf("CallTool(skein_dep_add): %v", err)
	}
	if depAddRes == nil || depAddRes.IsError {
		dumpErr(t, "skein_dep_add", depAddRes)
		return
	}

	// 5) skein_project_create — fresh project, unique prefix per run.
	pcReq := mcpgo.CallToolRequest{}
	pcReq.Params.Name = "skein_project_create"
	pcReq.Params.Arguments = map[string]any{
		"name":     "Intg Project " + userID.String()[:8],
		"idPrefix": "intg" + userID.String()[:6],
	}
	pcRes, err := c.CallTool(ctx, pcReq)
	if err != nil {
		t.Fatalf("CallTool(skein_project_create): %v", err)
	}
	if pcRes == nil || pcRes.IsError {
		dumpErr(t, "skein_project_create", pcRes)
		return
	}

	// 6) skein_component(action="create") — new component on the original
	// project. sk-255.3 folded the four skein_component_* tools into one
	// tool with an `action` discriminator; this is the only test that goes
	// through the wire by tool NAME, so it is the only one that had to move.
	ccReq := mcpgo.CallToolRequest{}
	ccReq.Params.Name = "skein_component"
	ccReq.Params.Arguments = map[string]any{
		"action":  "create",
		"project": pid.String(),
		"name":    "intg-comp",
	}
	ccRes, err := c.CallTool(ctx, ccReq)
	if err != nil {
		t.Fatalf("CallTool(skein_component create): %v", err)
	}
	if ccRes == nil || ccRes.IsError {
		dumpErr(t, "skein_component create", ccRes)
		return
	}
	// Pull componentId out of the response so update + archive can target it.
	var componentID string
	if len(ccRes.Content) > 0 {
		if txt, ok := ccRes.Content[0].(mcpgo.TextContent); ok {
			var body map[string]any
			if uerr := jsonUnmarshal(txt.Text, &body); uerr == nil {
				if comp, ok := body["component"].(map[string]any); ok {
					componentID, _ = comp["id"].(string)
				}
			}
		}
	}
	if componentID == "" {
		t.Fatalf("skein_component create did not return a component id")
	}

	// 7) skein_component(action="update") — rename the component.
	cuReq := mcpgo.CallToolRequest{}
	cuReq.Params.Name = "skein_component"
	cuReq.Params.Arguments = map[string]any{
		"action":  "update",
		"project": pid.String(),
		"id":      componentID,
		"name":    "intg-comp-renamed",
	}
	cuRes, err := c.CallTool(ctx, cuReq)
	if err != nil {
		t.Fatalf("CallTool(skein_component update): %v", err)
	}
	if cuRes == nil || cuRes.IsError {
		dumpErr(t, "skein_component update", cuRes)
		return
	}

	// 8) skein_component(action="archive") — archive then verify post-state.
	caReq := mcpgo.CallToolRequest{}
	caReq.Params.Name = "skein_component"
	caReq.Params.Arguments = map[string]any{
		"action":  "archive",
		"project": pid.String(),
		"id":      componentID,
	}
	caRes, err := c.CallTool(ctx, caReq)
	if err != nil {
		t.Fatalf("CallTool(skein_component archive): %v", err)
	}
	if caRes == nil || caRes.IsError {
		dumpErr(t, "skein_component archive", caRes)
		return
	}

	// 9) skein_claim — claim the second seeded issue.
	claimReq := mcpgo.CallToolRequest{}
	claimReq.Params.Name = "skein_claim"
	claimReq.Params.Arguments = map[string]any{
		"id": prefix + "-2",
	}
	claimRes, err := c.CallTool(ctx, claimReq)
	if err != nil {
		t.Fatalf("CallTool(skein_claim): %v", err)
	}
	if claimRes == nil || claimRes.IsError {
		dumpErr(t, "skein_claim", claimRes)
		return
	}

	// Pull the new version off the claim response so the close call's
	// expectedVersion matches the post-claim live version.
	var claimedVersion int
	if len(claimRes.Content) > 0 {
		if txt, ok := claimRes.Content[0].(mcpgo.TextContent); ok {
			var body map[string]any
			if uerr := jsonUnmarshal(txt.Text, &body); uerr == nil {
				if v, ok := body["version"].(float64); ok {
					claimedVersion = int(v)
				}
			}
		}
	}
	if claimedVersion < 2 {
		t.Fatalf("skein_claim did not return a sensible version: got %d", claimedVersion)
	}

	// 10) skein_close — close the just-claimed issue.
	closeReq := mcpgo.CallToolRequest{}
	closeReq.Params.Name = "skein_close"
	closeReq.Params.Arguments = map[string]any{
		"id":              prefix + "-2",
		"expectedVersion": claimedVersion,
		"closeOutcome":    "done",
		"reason":          "integration test done",
	}
	closeRes, err := c.CallTool(ctx, closeReq)
	if err != nil {
		t.Fatalf("CallTool(skein_close): %v", err)
	}
	if closeRes == nil || closeRes.IsError {
		dumpErr(t, "skein_close", closeRes)
		return
	}

	// 11) skein_unclaim — idempotent on already-unlocked starter issue
	// (skein_close cleared the lock; this asserts the no-op path doesn't
	// regress). sk-19 makes nextStatus + reason required even when the
	// call ends up as a no-op — the contract is enforced at the boundary,
	// the store layer is what decides "actually nothing to do".
	unclaimReq := mcpgo.CallToolRequest{}
	unclaimReq.Params.Name = "skein_unclaim"
	unclaimReq.Params.Arguments = map[string]any{
		"id":         prefix + "-2",
		"nextStatus": "open",
		"reason":     "integration test no-op path",
	}
	unclaimRes, err := c.CallTool(ctx, unclaimReq)
	if err != nil {
		t.Fatalf("CallTool(skein_unclaim): %v", err)
	}
	if unclaimRes == nil || unclaimRes.IsError {
		dumpErr(t, "skein_unclaim", unclaimRes)
		return
	}

	// 12) skein_user(action="create") — admin creates a fresh user.
	// sk-255.3 folded the three skein_user_* tools into one tool with an
	// `action` discriminator; this file calls tools by NAME through the
	// wire, which is why it moves and the handler tests do not.
	createUserReq := mcpgo.CallToolRequest{}
	createUserReq.Params.Name = "skein_user"
	createUserReq.Params.Arguments = map[string]any{
		"action":   "create",
		"email":    "intguser-" + userID.String()[:8] + "@x.local",
		"name":     "Intg Created User",
		"password": "passw0rd!",
		"role":     "user",
	}
	createUserRes, err := c.CallTool(ctx, createUserReq)
	if err != nil {
		t.Fatalf("CallTool(skein_user create): %v", err)
	}
	if createUserRes == nil || createUserRes.IsError {
		dumpErr(t, "skein_user create", createUserRes)
		return
	}
	var createdUserID string
	if len(createUserRes.Content) > 0 {
		if txt, ok := createUserRes.Content[0].(mcpgo.TextContent); ok {
			var body map[string]any
			if uerr := jsonUnmarshal(txt.Text, &body); uerr == nil {
				if u, ok := body["user"].(map[string]any); ok {
					createdUserID, _ = u["id"].(string)
				}
			}
		}
	}
	if createdUserID == "" {
		t.Fatalf("skein_user create did not return a user id")
	}
	// 13) skein_user(action="list") — admin sees the freshly-created user.
	userListReq := mcpgo.CallToolRequest{}
	userListReq.Params.Name = "skein_user"
	userListReq.Params.Arguments = map[string]any{"action": "list"}
	userListRes, err := c.CallTool(ctx, userListReq)
	if err != nil {
		t.Fatalf("CallTool(skein_user list): %v", err)
	}
	if userListRes == nil || userListRes.IsError {
		dumpErr(t, "skein_user list", userListRes)
		return
	}

	// 14) skein_user(action="update") — rename via patch.
	userUpdateReq := mcpgo.CallToolRequest{}
	userUpdateReq.Params.Name = "skein_user"
	userUpdateReq.Params.Arguments = map[string]any{
		"action": "update",
		"userId": createdUserID,
		"name":   "Renamed Intg User",
	}
	userUpdateRes, err := c.CallTool(ctx, userUpdateReq)
	if err != nil {
		t.Fatalf("CallTool(skein_user update): %v", err)
	}
	if userUpdateRes == nil || userUpdateRes.IsError {
		dumpErr(t, "skein_user update", userUpdateRes)
		return
	}

	// 15) skein_project_members(action="list") — admin lists the members.
	pmListReq := mcpgo.CallToolRequest{}
	pmListReq.Params.Name = "skein_project_members"
	pmListReq.Params.Arguments = map[string]any{
		"action":  "list",
		"project": pid.String(),
	}
	pmListRes, err := c.CallTool(ctx, pmListReq)
	if err != nil {
		t.Fatalf("CallTool(skein_project_members list): %v", err)
	}
	if pmListRes == nil || pmListRes.IsError {
		dumpErr(t, "skein_project_members list", pmListRes)
		return
	}

	// INV-5 (T024): a project member is a participant of its space.
	testsupport.JoinProjectSpace(t, pool, pid, uuid.MustParse(createdUserID))

	// 16) skein_project_members(action="add") — add the new user.
	pmAddReq := mcpgo.CallToolRequest{}
	pmAddReq.Params.Name = "skein_project_members"
	pmAddReq.Params.Arguments = map[string]any{
		"action":  "add",
		"project": pid.String(),
		"userId":  createdUserID,
		"role":    "member",
	}
	pmAddRes, err := c.CallTool(ctx, pmAddReq)
	if err != nil {
		t.Fatalf("CallTool(skein_project_members add): %v", err)
	}
	if pmAddRes == nil || pmAddRes.IsError {
		dumpErr(t, "skein_project_members add", pmAddRes)
		return
	}

	// 17) skein_project_members(action="remove") — remove the new user.
	pmRmReq := mcpgo.CallToolRequest{}
	pmRmReq.Params.Name = "skein_project_members"
	pmRmReq.Params.Arguments = map[string]any{
		"action":  "remove",
		"project": pid.String(),
		"userId":  createdUserID,
	}
	pmRmRes, err := c.CallTool(ctx, pmRmReq)
	if err != nil {
		t.Fatalf("CallTool(skein_project_members remove): %v", err)
	}
	if pmRmRes == nil || pmRmRes.IsError {
		dumpErr(t, "skein_project_members remove", pmRmRes)
		return
	}
}

// dumpErr surfaces the tool error body so a failure in the integration
// test is actionable. Mirrors the helper in integration_read_test.go.
func dumpErr(t *testing.T, tool string, res *mcpgo.CallToolResult) {
	t.Helper()
	if res == nil {
		t.Errorf("CallTool(%s): nil result", tool)
		return
	}
	if len(res.Content) > 0 {
		if txt, ok := res.Content[0].(mcpgo.TextContent); ok {
			t.Errorf("CallTool(%s) IsError=true: %s", tool, strings.TrimSpace(txt.Text))
			return
		}
	}
	t.Errorf("CallTool(%s) IsError=true with no body", tool)
}

// TestIntegration_OptimisticCycleRunsOnAcknowledgementsAlone is matrix
// row 92 (sk-282, T020–T024).
//
// "INV-7: the optimistic-lock cycle is built from the version in the
// acknowledgement without an extra read (end-to-end scenario claim →
// update → close)."
//
// The "without an extra read" half is the one that is easy to
// assert weakly — a test that simply never calls skein_show proves
// nothing, because nothing stopped it from calling it. So the MCP server
// this test drives has NO reading tool registered at all: skein_show,
// skein_list and skein_search do not exist on it. If any step of the
// cycle needed a re-read, there would be no way to perform one and the
// test could not be made to pass.
//
// The chain is create → claim → update → close, and every
// `expectedVersion` is taken from the PREVIOUS acknowledgement's
// `version` field. Nothing is derived from the caller's own arguments
// and nothing is counted forward: reading the number back off each
// response is precisely the invariant.
func TestIntegration_OptimisticCycleRunsOnAcknowledgementsAlone(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "inv7")

	issuesStore := issues.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	pStore := projects.NewStore(pool)

	s := server.NewMCPServer("skein-test-inv7", "0.1.0", server.WithToolCapabilities(false))
	RegisterCreate(s, pool, issuesStore, depsStore, pStore)
	RegisterClaim(s, pool, issuesStore)
	RegisterUpdate(s, pool, issuesStore)
	RegisterClose(s, pool, issuesStore)
	// Deliberately NOT registered: skein_show / skein_list / skein_search.

	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("new in-process client: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUserRole(ctx, "admin")
	ctx = auth.WithSessionID(ctx, "intg-inv7-"+userID.String()[:8])
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start client: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "tests-inv7", Version: "1"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	listRes, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range listRes.Tools {
		switch tool.Name {
		case "skein_show", "skein_list", "skein_search":
			t.Fatalf("a reading tool (%s) is registered — this test must be unable to "+
				"re-read, or it does not prove the cycle needs no extra read", tool.Name)
		}
	}

	call := func(t *testing.T, name string, args map[string]any) map[string]any {
		t.Helper()
		req := mcpgo.CallToolRequest{}
		req.Params.Name = name
		req.Params.Arguments = args
		res, err := c.CallTool(ctx, req)
		if err != nil {
			t.Fatalf("CallTool(%s): %v", name, err)
		}
		if res == nil || res.IsError {
			dumpErr(t, name, res)
			t.FailNow()
		}
		txt, ok := res.Content[0].(mcpgo.TextContent)
		if !ok {
			t.Fatalf("CallTool(%s): content[0] is %T", name, res.Content[0])
		}
		var body map[string]any
		if err := jsonUnmarshal(txt.Text, &body); err != nil {
			t.Fatalf("CallTool(%s): decode %q: %v", name, txt.Text, err)
		}
		return body
	}
	versionOf := func(t *testing.T, step string, body map[string]any) int {
		t.Helper()
		v, ok := body["version"].(float64)
		if !ok {
			t.Fatalf("%s acknowledgement has no usable version: %#v — INV-7 says the cycle "+
				"continues from this number", step, body)
		}
		return int(v)
	}

	// 1) create — the acknowledgement is nested in the create envelope.
	created := call(t, "skein_create", map[string]any{
		"project":     pid.String(),
		"title":       "optimistic cycle probe",
		"type":        "task",
		"kind":        "autonomous",
		"description": "body the caller supplied and must not get back",
	})
	issueAck, ok := created["issue"].(map[string]any)
	if !ok {
		t.Fatalf("create envelope has no issue: %#v", created)
	}
	issueID, _ := issueAck["id"].(string)
	if issueID == "" {
		t.Fatalf("create acknowledgement has no id: %#v", issueAck)
	}
	vCreate := versionOf(t, "create", issueAck)

	// 2) claim — no expectedVersion of its own, but its acknowledgement
	// must hand back the version the next step needs.
	claimed := call(t, "skein_claim", map[string]any{"id": issueID})
	vClaim := versionOf(t, "claim", claimed)
	if vClaim <= vCreate {
		t.Errorf("claim version %d did not advance past create's %d", vClaim, vCreate)
	}
	if claimed["status"] != "in_progress" {
		t.Errorf("claim status = %v, want in_progress", claimed["status"])
	}

	// 3) update — expectedVersion comes from the CLAIM's acknowledgement.
	updated := call(t, "skein_update", map[string]any{
		"id":              issueID,
		"expectedVersion": vClaim,
		"priority":        0,
	})
	vUpdate := versionOf(t, "update", updated)
	if vUpdate <= vClaim {
		t.Errorf("update version %d did not advance past claim's %d", vUpdate, vClaim)
	}

	// 4) close — expectedVersion comes from the UPDATE's acknowledgement.
	closed := call(t, "skein_close", map[string]any{
		"id":              issueID,
		"expectedVersion": vUpdate,
		"closeOutcome":    "done",
		"reason":          "cycle complete",
	})
	if closed["status"] != "closed" {
		t.Errorf("close status = %v, want closed", closed["status"])
	}
	if v := versionOf(t, "close", closed); v <= vUpdate {
		t.Errorf("close version %d did not advance past update's %d", v, vUpdate)
	}

	// The row agrees with what the chain of acknowledgements claimed.
	var status string
	var version int
	if err := pool.QueryRow(context.Background(),
		`SELECT status, version FROM issues WHERE id = $1`, issueID).Scan(&status, &version); err != nil {
		t.Fatalf("read back the issue: %v", err)
	}
	if status != "closed" {
		t.Errorf("stored status = %q, want closed", status)
	}
	if version != versionOf(t, "close", closed) {
		t.Errorf("stored version = %d, acknowledgement said %d",
			version, versionOf(t, "close", closed))
	}
}
