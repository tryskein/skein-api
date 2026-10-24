package main

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/apikeys"
	"skein-api/internal/attachments"
	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
	"skein-api/internal/reminders"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
	"skein-api/internal/themes"
	"skein-api/internal/users"
)

// TestActionRouting_EveryActionReachesItsOwnHandler is the guard on the
// eight `action → handler` map literals of the sk-255.3 consolidation.
//
// Those literals are the artefact a mechanical, wave-by-wave fold breaks
// most easily and, until this test, the one artefact nothing read. The
// per-group tests bind SENTINEL handlers to the same table and prove that
// actionSet.dispatch routes — mechanics that stay true no matter how the
// production map is wired — and every DB-backed group test declares its own
// copy of the map instead of using the one Register* builds. Two transposed
// entries were verified to survive a full green run before this test
// existed: swapping mark_read with mark_read_issue, and inverting the
// direction flags of project_watch / project_unwatch, which turns "unwatch
// this project" into a subscribe.
//
// The property pinned here is identity, not behaviour: each action must
// reach the handler that IMPLEMENTS it, and the group tests own what that
// handler then does. Identity is established by a fingerprint — a substring
// of the response only that action's handler can produce — and the test
// asserts, per action, both that its own fingerprint is present and that no
// SIBLING's fingerprint is. The second half is what makes a transposition
// impossible to survive rather than merely unlikely to.
//
// Everything runs through mcpRegisterToolsFn, the production registration
// closure, so the map under test is the map that ships. Completeness is
// enforced in both directions against the enums read off tools/list, so a
// ninth consolidated tool — or a fortieth action on an existing one — fails
// here until it is covered instead of silently slipping past.
//
// Database-gated: routing identity for pairs like watch / unwatch or
// project_watch / project_unwatch is only observable in what the handler
// writes, so there is nothing to assert without a real store.
func TestActionRouting_EveryActionReachesItsOwnHandler(t *testing.T) {
	pool := routingPool(t)
	fx := seedRoutingFixture(t, pool)

	s := server.NewMCPServer("test", "0.1.0", server.WithToolCapabilities(false))
	mcpRegisterToolsFn(routingDeps(pool))(s)
	c := startInProcess(t, s)

	probes := routingProbes(fx)
	assertProbeCoverage(t, consolidatedTools(t), probes)

	for tool, group := range probes {
		t.Run(tool, func(t *testing.T) {
			// Sequential inside a group on purpose: later probes consume ids
			// captured by earlier ones (create → update → archive).
			for _, p := range group {
				body := callRoutingAction(t, c, fx, tool, p)
				if p.capture != nil {
					p.capture(fx, body)
				}
				if os.Getenv("SKEIN_DUMP_ROUTING") != "" {
					t.Logf("DUMP %s/%s -> %s", tool, p.action, body)
				}
				// Fingerprints resolve here rather than up front because a
				// few of them contain an id an earlier probe captured.
				want := p.want(fx)
				if want == "" {
					t.Fatalf("%s(action=%q): the probe's fingerprint resolved to the empty string — "+
						"it would match every response", tool, p.action)
				}
				if !strings.Contains(body, want) {
					t.Errorf("%s(action=%q) did not reach its own handler: response has no %q\n  response: %s",
						tool, p.action, want, body)
					continue
				}
				for _, sibling := range group {
					if sibling.action == p.action {
						continue
					}
					if siblingWant := sibling.want(fx); siblingWant != "" && strings.Contains(body, siblingWant) {
						t.Errorf("%s(action=%q) produced %s's fingerprint %q — the dispatch table is transposed\n  response: %s",
							tool, p.action, sibling.action, siblingWant, body)
					}
				}
			}
		})
	}
}

// actionProbe is one action's identity check: arguments that make its
// handler produce something no sibling handler in the same group can, and
// the fingerprint that proves it ran.
//
// A probe deliberately does NOT have to exercise the happy path. A
// rejection message naming the action is as good a fingerprint as a
// successful write, and cheaper — what is being pinned is which handler
// received the call.
type actionProbe struct {
	action string
	// args is a function so a probe can use ids captured from earlier
	// probes in the same group.
	args func(fx *routingFixture) map[string]any
	// want must appear in this action's response and in no sibling's. It is
	// a function for the same reason args is: one fingerprint is an id
	// minted by an earlier probe in the group.
	want func(fx *routingFixture) string
	// capture stores ids out of the response for later probes.
	capture func(fx *routingFixture, body string)
}

// fixed wraps a constant fingerprint, which is what most probes have.
func fixed(s string) func(*routingFixture) string {
	return func(*routingFixture) string { return s }
}

// assertProbeCoverage pins the probe table to the registered surface in
// both directions.
func assertProbeCoverage(t *testing.T, registered map[string][]string, probes map[string][]actionProbe) {
	t.Helper()
	for tool, enum := range registered {
		group, ok := probes[tool]
		if !ok {
			t.Fatalf("%s is a consolidated tool with no routing probes — its dispatch table is unguarded", tool)
		}
		covered := make([]string, 0, len(group))
		for _, p := range group {
			covered = append(covered, p.action)
		}
		slices.Sort(covered)
		wantActions := slices.Clone(enum)
		slices.Sort(wantActions)
		if !slices.Equal(covered, wantActions) {
			t.Fatalf("%s: probes cover %v, the schema enum is %v", tool, covered, wantActions)
		}
	}
	for tool := range probes {
		if _, ok := registered[tool]; !ok {
			t.Errorf("routing probes exist for %s, which is not a consolidated tool on the registered surface", tool)
		}
	}
}

// callRoutingAction invokes one tool action through the in-process client
// with the fixture's authenticated context and returns the raw response
// text (success payload or error envelope — both are fingerprint material).
func callRoutingAction(t *testing.T, c interface {
	CallTool(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
}, fx *routingFixture, tool string, p actionProbe,
) string {
	t.Helper()
	args := p.args(fx)
	args["action"] = p.action

	req := mcpgo.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	res, err := c.CallTool(fx.ctx, req)
	if err != nil {
		t.Fatalf("%s(action=%q): transport error %v", tool, p.action, err)
	}
	var sb strings.Builder
	for _, content := range res.Content {
		if text, ok := content.(mcpgo.TextContent); ok {
			sb.WriteString(text.Text)
		}
	}
	return sb.String()
}

// jsonField walks a response payload down a path of object keys and
// returns the string it lands on, for probes that chain (create → update →
// archive). Returns "" for any miss, which then surfaces as the next
// probe's own failure rather than as a panic.
func jsonField(body string, path ...string) string {
	var node any
	if err := json.Unmarshal([]byte(body), &node); err != nil {
		return ""
	}
	for _, key := range path {
		obj, ok := node.(map[string]any)
		if !ok {
			return ""
		}
		node = obj[key]
	}
	s, _ := node.(string)
	return s
}

// ---------------------------------------------------------------- fixture

// routingFixture is the seeded world the probes run against, plus the
// caller-authenticated context every call is made with.
type routingFixture struct {
	ctx       context.Context
	pool      *pgxpool.Pool
	userID    uuid.UUID
	memberID  uuid.UUID
	projectID uuid.UUID
	issueID   string
	nonce     string

	// archiveComponentID is seeded rather than created by a probe: the
	// archive probe must run against a component the create/update probes
	// never touched, or its response would echo their fingerprints and the
	// sibling-exclusion check could not tell the three apart.
	archiveComponentID uuid.UUID

	// spaceShort is a regular space of the admin that the member is not in
	// — the target of skein_user action=space_add (T024).
	spaceShort string
	// spaceUserID is the user space_add adds: not the member, whom the
	// update probe renames — space_add echoes the added member's name.
	spaceUserID uuid.UUID

	// captured mid-run by probes.
	componentID  string
	filterID     string
	attachmentID string
	reminderID   string
}

func routingPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the action-routing guard")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect TEST_DATABASE_URL: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// routingDeps builds the production dependency bundle over a real pool.
// Only the stores the eight consolidated tools use are populated; the rest
// stay nil, which is safe because nothing else is called.
func routingDeps(pool *pgxpool.Pool) mcpDeps {
	return mcpDeps{
		pool:                     pool,
		issuesStore:              issues.NewStore(pool),
		depsStore:                dependencies.NewStore(pool),
		projectsStore:            projects.NewStore(pool),
		usersStore:               users.NewStore(pool, 4, apikeys.New(pool), "local", "", nil),
		notifStore:               notifications.NewStore(pool),
		attachmentsStore:         attachments.NewStore(pool),
		s3Access:                 routingS3{},
		attachmentBucket:         "routing-test",
		attachmentMaxBytes:       1 << 20,
		attachmentURLTTL:         time.Minute,
		themesStore:              themes.NewStore(pool),
		themesPublisher:          themes.NewPublisher(nil),
		inspectCache:             themes.NewInspectCache(time.Minute),
		remindersStore:           reminders.NewStore(pool),
		monitorCredsStore:        monitorcreds.New(pool, time.Hour),
		spacesSvc:                spaces.NewService(pool, spaces.Options{Creation: spaces.CreationAll}),
		projectDefaultVisibility: "members",
	}
}

// routingS3 is a no-op object store. The attachment probes are about
// which handler the `action` reached, not about S3 — but the six handlers
// are indistinguishable when the subsystem reports itself unavailable, so
// they need SOMETHING wired in.
type routingS3 struct{}

func (routingS3) PresignedPostPolicy(_ context.Context, key, _ string, _ int64, _ time.Duration) (attachments.PostPolicy, error) {
	return attachments.PostPolicy{URL: "http://routing.invalid/" + key, Fields: map[string]string{"key": key}}, nil
}
func (routingS3) PresignedGetURL(_ context.Context, key, _ string, _ time.Duration) (string, error) {
	return "http://routing.invalid/get/" + key, nil
}
func (routingS3) RemoveObject(context.Context, string) error { return nil }
func (routingS3) StatObject(context.Context, string) error   { return nil }
func (routingS3) ListKeys(context.Context, string, func(string) error) error {
	return nil
}

func seedRoutingFixture(t *testing.T, pool *pgxpool.Pool) *routingFixture {
	t.Helper()
	ctx := context.Background()
	nonce := strings.ReplaceAll(uuid.Must(uuid.NewRandom()).String()[:8], "-", "")

	admin := seedRoutingUser(t, pool, "admin", nonce+"a")
	member := seedRoutingUser(t, pool, "user", nonce+"m")

	projectID := uuid.Must(uuid.NewRandom())
	prefix := "rt" + nonce[:6]
	seedSpaceID := testsupport.TestSpace(t, pool)
	if _, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at, created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		projectID, "routing_"+nonce, prefix, admin, seedSpaceID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $2)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, projectID, admin); err != nil {
		t.Fatalf("seed project member: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO project_counters (project_id, counter) VALUES ($1, 1)
ON CONFLICT (project_id) DO NOTHING`, projectID); err != nil {
		t.Fatalf("seed project counter: %v", err)
	}

	issueID := prefix + "-1"
	if _, err := pool.Exec(ctx, `
INSERT INTO issues (id, project_id, parent_id, version, title, description, type, priority,
                    status, pinned, source, design, acceptance_criteria, notes,
                    created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, 'routing fixture', '', 'task', 2, 'open', false, 'human',
        '', '', '', $3, now(), now(), '{}'::jsonb)`, issueID, projectID, admin); err != nil {
		t.Fatalf("seed issue: %v", err)
	}

	archiveComponent := uuid.Must(uuid.NewRandom())
	if _, err := pool.Exec(ctx, `
INSERT INTO components (id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at)
VALUES ($1, $2, $3, '', '[]'::jsonb, NULL, $4, now(), now())`,
		archiveComponent, projectID, "rt-arch-"+nonce, admin); err != nil {
		t.Fatalf("seed component: %v", err)
	}

	t.Cleanup(func() {
		bg := context.Background()
		// The create probe mints a user of its own; it is outside the
		// project cascade, so it is named here.
		_, _ = pool.Exec(bg, `DELETE FROM users WHERE username = $1`, routingNewUsername(nonce))
		_, _ = pool.Exec(bg, `DELETE FROM events WHERE project_id = $1`, projectID)
		_, _ = pool.Exec(bg, `DELETE FROM issues WHERE project_id = $1`, projectID)
		_, _ = pool.Exec(bg, `DELETE FROM projects WHERE id = $1`, projectID)
	})

	ctxAuth := auth.WithUserID(context.Background(), admin)
	ctxAuth = auth.WithUserRole(ctxAuth, "admin")
	ctxAuth = auth.WithSessionID(ctxAuth, "routing-"+nonce)
	ctxAuth = auth.WithAPIKeyID(ctxAuth, uuid.Must(uuid.NewRandom()))

	// INV-5 (T024): the member the project_members probes add is a
	// participant of the project's space.
	testsupport.AddSpaceMember(t, pool, seedSpaceID, member)
	spaceUser := seedRoutingUser(t, pool, "user", nonce+"s")
	spaceID := testsupport.SeedSpace(t, pool, admin)
	var spaceShort string
	if err := pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, spaceID).Scan(&spaceShort); err != nil {
		t.Fatalf("read the routing space: %v", err)
	}
	return &routingFixture{
		spaceShort:         spaceShort,
		spaceUserID:        spaceUser,
		ctx:                ctxAuth,
		pool:               pool,
		userID:             admin,
		memberID:           member,
		projectID:          projectID,
		issueID:            issueID,
		nonce:              nonce,
		archiveComponentID: archiveComponent,
	}
}

func seedRoutingUser(t *testing.T, pool *pgxpool.Pool, role, tag string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "rt" + tag
	if _, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, $5, 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)`,
		id, uname+"@routing.local", uname, uname, role); err != nil {
		t.Fatalf("seed %s user: %v", role, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// routingNewUsername is the account the skein_user create probe mints —
// named by a function so the probe and the cleanup cannot drift.
func routingNewUsername(nonce string) string { return "rtnew" + nonce[:6] }

// ----------------------------------------------------------------- probes

// routingProbes is the fingerprint table: one entry per action of every
// consolidated tool, in the order that group's probes must run.
//
// Two rules govern a fingerprint, and the test enforces both:
//
//   - it must appear in its own action's response;
//   - it must appear in NO sibling's response.
//
// The second is what turns the table from a smoke test into a
// transposition guard, and it is why several probes deliberately withhold
// an argument (create is called with no id, update with no name) or run
// against their own fixture row: a sibling handed the wrong action's
// arguments has to FAIL, not quietly do something that echoes the same
// entity back.
func routingProbes(fx *routingFixture) map[string][]actionProbe {
	project := fx.projectID.String()
	noArgs := func(*routingFixture) map[string]any { return map[string]any{} }

	return map[string][]actionProbe{
		// list runs first, while the only component is the seeded one the
		// archive probe owns — so the listing cannot yet echo create's or
		// update's fingerprint.
		"skein_component": {
			{
				action: "list",
				args: func(*routingFixture) map[string]any {
					return map[string]any{"project": project, "archivedFilter": "all"}
				},
				want: fixed(`"items":[`),
			},
			{
				action: "create",
				args: func(fx *routingFixture) map[string]any {
					// No id: handed to update or archive this must fail.
					return map[string]any{
						"project":     project,
						"name":        "rt-comp-" + fx.nonce,
						"description": "rt-created-" + fx.nonce,
					}
				},
				want:    fixed("rt-created-" + fx.nonce),
				capture: func(fx *routingFixture, body string) { fx.componentID = jsonField(body, "component", "id") },
			},
			{
				action: "update",
				args: func(fx *routingFixture) map[string]any {
					// No name: handed to create this must fail. The
					// description replaces create's, so create's fingerprint
					// stops existing anywhere from here on.
					return map[string]any{
						"project":     project,
						"id":          fx.componentID,
						"description": "rt-updated-" + fx.nonce,
					}
				},
				want: fixed("rt-updated-" + fx.nonce),
			},
			{
				action: "archive",
				args: func(fx *routingFixture) map[string]any {
					return map[string]any{
						"project":  project,
						"id":       fx.archiveComponentID.String(),
						"archived": true,
					}
				},
				want: fixed(`"archivedAt":"`),
			},
		},

		"skein_user": {
			{
				action: "list",
				args:   func(*routingFixture) map[string]any { return map[string]any{"limit": float64(1)} },
				want:   fixed(`"nextCursor"`),
			},
			{
				action: "create",
				args: func(fx *routingFixture) map[string]any {
					// No userId: handed to update this must fail.
					uname := routingNewUsername(fx.nonce)
					return map[string]any{
						"email":    uname + "@routing.local",
						"username": uname,
						"name":     uname,
						"password": "routing-pass-12345",
						"role":     "user",
					}
				},
				want: fixed(routingNewUsername(fx.nonce)),
			},
			{
				action: "update",
				args: func(fx *routingFixture) map[string]any {
					// No email/password: handed to create this must fail.
					return map[string]any{"userId": fx.memberID.String(), "name": "rt-renamed-" + fx.nonce}
				},
				want: fixed("rt-renamed-" + fx.nonce),
			},
			{
				action: "space_add",
				args: func(fx *routingFixture) map[string]any {
					// No email: handed to create this must fail; no name:
					// update would echo no space.
					return map[string]any{"userId": fx.spaceUserID.String(), "space": fx.spaceShort}
				},
				want: fixed(`"spaceId":"` + fx.spaceShort + `"`),
			},
		},

		"skein_project_members": {
			{
				action: "list",
				args:   func(*routingFixture) map[string]any { return map[string]any{"project": project} },
				want:   fixed(`"members":[`),
			},
			{
				action: "add",
				args: func(fx *routingFixture) map[string]any {
					return map[string]any{"project": project, "userId": fx.memberID.String(), "role": "member"}
				},
				want: fixed(`"member":{`),
			},
			{
				action: "remove",
				args: func(fx *routingFixture) map[string]any {
					// No role: handed to add this must fail.
					return map[string]any{"project": project, "userId": fx.memberID.String()}
				},
				want: fixed(`"removed":`),
			},
		},

		"skein_watchers": {
			{
				action: "watch",
				args:   func(fx *routingFixture) map[string]any { return map[string]any{"issueId": fx.issueID} },
				want:   fixed(`"issueId":"` + fx.issueID + `","watching":true`),
			},
			{
				action: "unwatch",
				args:   func(fx *routingFixture) map[string]any { return map[string]any{"issueId": fx.issueID} },
				want:   fixed(`"watching":false`),
			},
			{
				action: "project_watch",
				args: func(*routingFixture) map[string]any {
					return map[string]any{"project": project, "eventTypes": []any{"issue.created"}}
				},
				want: fixed(`"projectId":"` + project + `","watching":true`),
			},
			{
				// The direction pair is the transposition that matters most
				// here, and eventTypes is what separates them: project_watch
				// consumes it, project_unwatch rejects it by name.
				action: "project_unwatch",
				args: func(*routingFixture) map[string]any {
					return map[string]any{"project": project, "eventTypes": []any{"issue.created"}}
				},
				want: fixed("only read by project_watch"),
			},
			{
				action: "filter_create",
				args: func(fx *routingFixture) map[string]any {
					return map[string]any{"name": "rt-filter-" + fx.nonce, "filter": map[string]any{}}
				},
				want:    fixed(`"watcher":{`),
				capture: func(fx *routingFixture, body string) { fx.filterID = jsonField(body, "watcher", "id") },
			},
			{
				action: "filter_list",
				args:   noArgs,
				want:   fixed(`"watchers":[`),
			},
			{
				action: "filter_delete",
				args:   func(fx *routingFixture) map[string]any { return map[string]any{"filterId": fx.filterID} },
				// The ack is a bare {"id":"<the deleted filter>"} — nothing
				// else in this group answers with that exact object, and
				// the id has to come from the capture, so the fingerprint
				// resolves at assertion time.
				want: func(fx *routingFixture) string { return `{"id":"` + fx.filterID + `"}` },
			},
			{
				action: "list",
				args:   noArgs,
				want:   fixed(`"custom":[`),
			},
			{
				// An id that is not the caller's monitor: only the
				// monitor_stop handler answers with this code.
				action: "monitor_stop",
				args: func(*routingFixture) map[string]any {
					return map[string]any{"monitorId": "00000000-0000-4000-8000-000000000579"}
				},
				want: fixed(`monitor_not_found`),
			},
		},

		// Every notification action states its own requirement by name, so
		// the rejections alone separate all six — no fixture rows needed.
		"skein_notifications": {
			{
				action: "inbox",
				args:   func(*routingFixture) map[string]any { return map[string]any{"scope": "everything"} },
				want:   fixed("scope must be one of"),
			},
			{
				action: "mark_read",
				args:   noArgs,
				want:   fixed("notificationId is required for mark_read"),
			},
			{
				action: "mark_read_issue",
				args:   noArgs,
				want:   fixed("issueId is required for mark_read_issue"),
			},
			{
				// sk-622.21 T023 (US-7) — "read the card".
				action: "mark_read_card",
				args:   noArgs,
				want:   fixed("issueId is required for mark_read_card"),
			},
			{
				action: "mark_read_all",
				args:   func(*routingFixture) map[string]any { return map[string]any{"issueId": "demo-42"} },
				want:   fixed("cannot be narrowed"),
			},
			{
				action: "archive",
				args:   noArgs,
				want:   fixed("notificationId is required for archive"),
			},
		},

		"skein_remind": {
			{
				action: "set",
				args: func(fx *routingFixture) map[string]any {
					return map[string]any{
						"issueId":  fx.issueID,
						"remindAt": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
					}
				},
				want:    fixed(`"snoozeCount":0`),
				capture: func(fx *routingFixture, body string) { fx.reminderID = jsonField(body, "id") },
			},
			{
				action: "snooze",
				args: func(fx *routingFixture) map[string]any {
					return map[string]any{"reminderId": fx.reminderID, "by": "1h"}
				},
				want: fixed(`"snoozeCount":1`),
			},
			{
				action: "dismiss",
				args:   func(fx *routingFixture) map[string]any { return map[string]any{"reminderId": fx.reminderID} },
				want:   fixed(`"dismissed":true`),
			},
			{
				action: "list",
				args:   func(*routingFixture) map[string]any { return map[string]any{"state": "nonsense"} },
				want:   fixed("state must be one of"),
			},
		},

		"skein_theme": {
			{
				action: "instructions",
				args:   noArgs,
				want:   fixed("Anti-patterns"),
			},
			{
				action: "describe",
				args:   noArgs,
				want:   fixed(`"schema":[`),
			},
			{
				action: "fork",
				args: func(fx *routingFixture) map[string]any {
					return map[string]any{"source": "builtin-light", "name": "rt-theme-" + fx.nonce}
				},
				want: fixed(`"name":"rt-theme-` + fx.nonce),
			},
			{
				action: "preview_variables",
				args:   noArgs,
				want:   fixed("variables is required"),
			},
			{
				action: "set_variables",
				args:   noArgs,
				want:   fixed("themeId is required"),
			},
			{
				action: "inspect_clear",
				args:   noArgs,
				want:   fixed(`"cleared":`),
			},
		},

		// list runs first, on an issue with no attachments, so it cannot
		// echo the rows the later probes create. metadata deliberately runs
		// BEFORE confirm: "pending" vs "ready" is the only thing that tells
		// those two handlers apart — both return the same envelope.
		"skein_attachments": {
			{
				action: "list",
				args:   func(fx *routingFixture) map[string]any { return map[string]any{"issueId": fx.issueID} },
				want:   fixed(`"total":`),
			},
			{
				action: "upload_url",
				args: func(fx *routingFixture) map[string]any {
					return map[string]any{
						"issueId": fx.issueID, "filename": "rt.txt",
						"contentType": "text/plain", "sizeBytes": float64(11),
					}
				},
				want:    fixed(`"fields":{`),
				capture: func(fx *routingFixture, body string) { fx.attachmentID = jsonField(body, "attachmentId") },
			},
			{
				action: "metadata",
				args:   func(fx *routingFixture) map[string]any { return map[string]any{"attachmentId": fx.attachmentID} },
				want:   fixed(`"status":"pending"`),
			},
			{
				action: "confirm",
				args:   func(fx *routingFixture) map[string]any { return map[string]any{"attachmentId": fx.attachmentID} },
				want:   fixed(`"status":"ready"`),
			},
			{
				action: "get_url",
				args:   func(fx *routingFixture) map[string]any { return map[string]any{"attachmentId": fx.attachmentID} },
				want:   fixed("/get/"),
			},
			{
				action: "delete",
				args:   func(fx *routingFixture) map[string]any { return map[string]any{"attachmentId": fx.attachmentID} },
				want:   fixed(`"deleted":`),
			},
		},
	}
}
