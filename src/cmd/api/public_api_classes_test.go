package main

// public_api_classes_test.go — the class rows of the public API over the
// operations of the product route table (sk-878.3.1, rows 100–104, 106,
// 129): the key right of each operation, fields= on reads, Prefer on
// writes, If-Match and Idempotency-Key, the origin of a write by key, the
// key's scope on every list, the owner's role on every write to a project,
// a key migrated by the backfill of US-13, and the strict parsing of the
// query and the body.
//
// The reference is publicOps: a literal of the operations, written from the
// table «API surface» of the spec and its rules for Prefer, If-Match and
// Idempotency-Key — not from routes.go. It is compared with the flags of
// routes.go both ways (a wrong flag must not become a wrong reference), and
// every class is asked of every operation in it. The literal grows with the
// tasks that add operations (T022 creates it, T023–T028 add their slices).
// Object storage is the no-op routingS3: the classes are about the
// operations, not about S3.
//
// The server is the product one: publicAPIServer over config.Load, the real
// request chain in front of the product route table.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/attachments"
	"skein-api/internal/auth"
	"skein-api/internal/config"
	"skein-api/internal/dependencies"
	"skein-api/internal/issueops"
	"skein-api/internal/issues"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
	"skein-api/internal/publicapi"
	"skein-api/internal/reminders"
	"skein-api/internal/search"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

// publicOp is one operation of the spec: its route, the right of its key,
// and the request rules of the surface.
type publicOp struct {
	id     string
	method string
	path   string // under /api/v1/, as routes.go writes it
	right  string // none | read | write | manage | admin
	// read: a read (fields= applies). prefer: Prefer: return=minimal
	// applies. ifMatch: required | optional | "" (not checked).
	// idempotent: accepts Idempotency-Key. body: takes a JSON body.
	read, prefer bool
	ifMatch      string
	idempotent   bool
	body         bool
	// projectWrite: a write on an object of a project, refused to an owner
	// without the role (row 104).
	projectWrite bool
	// field is the field a read keeps with fields= (row 101).
	field string
	// noEvent: a write that records no event (the delegation outbox is the
	// owner's own view).
	noEvent bool
}

// publicOps is the literal of the spec: the operations of T022, then the
// slices the later tasks add.
var publicOps = []publicOp{
	{id: "getMe", method: http.MethodGet, path: "me", right: "none", read: true, field: "isAdmin"},
	{id: "listProjects", method: http.MethodGet, path: "projects", right: "read", read: true, field: "name"},
	{id: "resolveProject", method: http.MethodGet, path: "projects/resolve", right: "read", read: true, field: "match"},
	{id: "createProject", method: http.MethodPost, path: "projects", right: "manage", body: true},
	{id: "getProject", method: http.MethodGet, path: "projects/{project}", right: "read", read: true, field: "name"},
	{id: "updateProject", method: http.MethodPatch, path: "projects/{project}", right: "manage", body: true, projectWrite: true},
	{id: "listProjectMembers", method: http.MethodGet, path: "projects/{project}/members", right: "read", read: true, field: "role"},
	{id: "putProjectMember", method: http.MethodPut, path: "projects/{project}/members/{user}", right: "manage", body: true, projectWrite: true},
	{id: "deleteProjectMember", method: http.MethodDelete, path: "projects/{project}/members/{user}", right: "manage", body: true, projectWrite: true},
	{id: "listComponents", method: http.MethodGet, path: "projects/{project}/components", right: "read", read: true, field: "name"},
	{id: "createComponent", method: http.MethodPost, path: "projects/{project}/components", right: "manage", body: true, projectWrite: true},
	{id: "updateComponent", method: http.MethodPatch, path: "projects/{project}/components/{component}", right: "manage", body: true, projectWrite: true},
	{id: "archiveComponent", method: http.MethodPost, path: "projects/{project}/components/{component}/archive", right: "manage", body: true, projectWrite: true},

	// T023 (row 123).
	{id: "listIssues", method: http.MethodGet, path: "issues", right: "read", read: true, field: "title"},
	{id: "listReadyIssues", method: http.MethodGet, path: "issues/ready", right: "read", read: true, field: "title"},
	{id: "getIssue", method: http.MethodGet, path: "issues/{issue}", right: "read", read: true, field: "title"},
	{id: "listIssueEvents", method: http.MethodGet, path: "issues/{issue}/events", right: "read", read: true, field: "type"},
	{id: "listDependencies", method: http.MethodGet, path: "issues/{issue}/dependencies", right: "read", read: true, field: "type"},
	{id: "listComments", method: http.MethodGet, path: "issues/{issue}/comments", right: "read", read: true, field: "body"},
	{id: "getIssueMovePlan", method: http.MethodGet, path: "issues/{issue}/move-plan", right: "read", read: true, field: "targetProject"},
	{id: "search", method: http.MethodGet, path: "search", right: "read", read: true, field: "title"},

	// T024 (row 124).
	{id: "createIssue", method: http.MethodPost, path: "projects/{project}/issues", right: "write", prefer: true, idempotent: true, body: true},
	{id: "createEpic", method: http.MethodPost, path: "projects/{project}/epics", right: "write", prefer: true, idempotent: true, body: true},
	{id: "updateIssue", method: http.MethodPatch, path: "issues/{issue}", right: "write", prefer: true, ifMatch: "required", body: true},
	{id: "claimIssue", method: http.MethodPost, path: "issues/{issue}/claim", right: "write", prefer: true, body: true},
	{id: "claimIssues", method: http.MethodPost, path: "issues/claim", right: "write", body: true},
	{id: "unclaimIssue", method: http.MethodPost, path: "issues/{issue}/unclaim", right: "write", prefer: true, body: true},
	{id: "releaseIssueLock", method: http.MethodPost, path: "issues/{issue}/release-lock", right: "write", prefer: true, body: true},
	{id: "closeIssue", method: http.MethodPost, path: "issues/{issue}/close", right: "write", prefer: true, ifMatch: "required", idempotent: true, body: true},
	{id: "acceptIssue", method: http.MethodPost, path: "issues/{issue}/accept", right: "write", prefer: true, ifMatch: "optional", body: true},
	{id: "reparentIssue", method: http.MethodPost, path: "issues/{issue}/reparent", right: "write", prefer: true, ifMatch: "required", body: true},
	{id: "dropDelegated", method: http.MethodPost, path: "issues/{issue}/drop-delegated", right: "write", body: true, noEvent: true},
	{id: "resumeDelegated", method: http.MethodPost, path: "issues/{issue}/resume-delegated", right: "write", body: true, noEvent: true},
	{id: "attachCommits", method: http.MethodPost, path: "issues/{issue}/commits", right: "write", body: true},
	{id: "detachCommit", method: http.MethodDelete, path: "issues/{issue}/commits/{hash}", right: "write", body: true},

	// T025 (row 125).
	{id: "bulkUpdateIssues", method: http.MethodPost, path: "issues/bulk-update", right: "write", body: true},
	{id: "bulkCloseIssues", method: http.MethodPost, path: "issues/bulk-close", right: "write", body: true},
	{id: "bulkAddDependencies", method: http.MethodPost, path: "issues/dependencies/bulk-add", right: "write", body: true},

	// T026 (row 126).
	{id: "createComment", method: http.MethodPost, path: "issues/{issue}/comments", right: "write", prefer: true, idempotent: true, body: true},
	{id: "getComment", method: http.MethodGet, path: "comments/{comment}", right: "read", read: true, field: "body"},
	{id: "updateComment", method: http.MethodPatch, path: "comments/{comment}", right: "write", prefer: true, ifMatch: "required", body: true},
	{id: "deleteComment", method: http.MethodDelete, path: "comments/{comment}", right: "write", body: true},
	{id: "addDependency", method: http.MethodPost, path: "issues/{issue}/dependencies", right: "write", body: true},
	{id: "removeDependency", method: http.MethodDelete, path: "issues/{issue}/dependencies/{toIssue}/{type}", right: "write", body: true},
	{id: "listAttachments", method: http.MethodGet, path: "issues/{issue}/attachments", right: "read", read: true, field: "filename"},
	// An upload records no event until it is confirmed.
	{id: "createAttachmentUpload", method: http.MethodPost, path: "issues/{issue}/attachments", right: "write", body: true, noEvent: true},
	{id: "confirmAttachment", method: http.MethodPost, path: "attachments/{attachment}/confirm", right: "write", body: true},
	{id: "getAttachment", method: http.MethodGet, path: "attachments/{attachment}", right: "read", read: true, field: "filename"},
	{id: "getAttachmentDownloadUrl", method: http.MethodGet, path: "attachments/{attachment}/download-url", right: "read", read: true, field: "url"},
	{id: "deleteAttachment", method: http.MethodDelete, path: "attachments/{attachment}", right: "write", body: true},

	// T027 (row 127). The notification and subscription writes record no
	// event; the reminder writes do.
	{id: "listNotifications", method: http.MethodGet, path: "notifications", right: "read", read: true, field: "type"},
	{id: "markNotificationRead", method: http.MethodPost, path: "notifications/{notification}/read", right: "write", body: true, noEvent: true},
	{id: "markAllNotificationsRead", method: http.MethodPost, path: "notifications/read-all", right: "write", body: true, noEvent: true},
	{id: "archiveNotification", method: http.MethodPost, path: "notifications/{notification}/archive", right: "write", body: true, noEvent: true},
	{id: "markIssueNotificationsRead", method: http.MethodPost, path: "issues/{issue}/notifications/read", right: "write", body: true, noEvent: true},
	{id: "markWatchingCardRead", method: http.MethodPost, path: "me/watching/issues/{issue}/read", right: "write", body: true, noEvent: true},
	{id: "listWatching", method: http.MethodGet, path: "me/watching", right: "read", read: true, field: "issues"},
	{id: "watchIssue", method: http.MethodPut, path: "issues/{issue}/watch", right: "write", body: true, noEvent: true},
	{id: "unwatchIssue", method: http.MethodDelete, path: "issues/{issue}/watch", right: "write", body: true, noEvent: true},
	{id: "watchProject", method: http.MethodPut, path: "projects/{project}/watch", right: "write", body: true, noEvent: true},
	{id: "unwatchProject", method: http.MethodDelete, path: "projects/{project}/watch", right: "write", body: true, noEvent: true},
	{id: "listWatchFilters", method: http.MethodGet, path: "me/watch-filters", right: "read", read: true, field: "name"},
	{id: "createWatchFilter", method: http.MethodPost, path: "me/watch-filters", right: "write", body: true, noEvent: true},
	{id: "deleteWatchFilter", method: http.MethodDelete, path: "me/watch-filters/{filter}", right: "write", body: true, noEvent: true},
	{id: "listReminders", method: http.MethodGet, path: "reminders", right: "read", read: true, field: "issueId"},
	{id: "createReminder", method: http.MethodPost, path: "issues/{issue}/reminders", right: "write", body: true},
	{id: "snoozeReminder", method: http.MethodPost, path: "reminders/{reminder}/snooze", right: "write", body: true},
	{id: "dismissReminder", method: http.MethodDelete, path: "reminders/{reminder}", right: "write", body: true},

	// T028 (row 128): the key right admin; the classes ask them with a key
	// of the instance admin (keyFor).
	{id: "listUsers", method: http.MethodGet, path: "users", right: "admin", read: true, field: "email"},
	{id: "createUser", method: http.MethodPost, path: "users", right: "admin", body: true, noEvent: true},
	{id: "updateUser", method: http.MethodPatch, path: "users/{user}", right: "admin", body: true, noEvent: true},
	{id: "addSpaceMember", method: http.MethodPost, path: "spaces/{space}/members", right: "admin", body: true, noEvent: true},
}

// TestPublicAPIClasses_LiteralIsRoutesGo — rows 100 and 101, the literal
// against the flags of routes.go, both ways: every operation of the literal
// is a route with its method, path, right, Prefer, If-Match and
// Idempotency-Key, and every route of the table is in the literal. Red when
// a flag of routes.go differs from the spec.
func TestPublicAPIClasses_LiteralIsRoutesGo(t *testing.T) {
	routes := map[string]publicapi.Route{}
	for _, rt := range publicapi.ProductRoutes(publicapi.Deps{}) {
		if rt.OperationID != publicapi.OperationGetOpenAPI {
			routes[rt.OperationID] = rt
		}
	}
	ifMatch := map[string]publicapi.IfMatch{"": publicapi.IfMatchNone, "required": publicapi.IfMatchRequired, "optional": publicapi.IfMatchOptional}
	for _, op := range publicOps {
		rt, ok := routes[op.id]
		if !ok {
			t.Errorf("%s: not in routes.go", op.id)
			continue
		}
		delete(routes, op.id)
		if rt.Method != op.method || rt.Path != publicapi.PathPrefix+op.path {
			t.Errorf("%s: routes.go %s %s, spec %s %s", op.id, rt.Method, rt.Path, op.method, publicapi.PathPrefix+op.path)
		}
		if got := rt.Key.Right.String(); got != op.right {
			t.Errorf("%s: right %s, spec %s", op.id, got, op.right)
		}
		if rt.Prefer != op.prefer {
			t.Errorf("%s: Prefer %v, spec %v", op.id, rt.Prefer, op.prefer)
		}
		if rt.IfMatch != ifMatch[op.ifMatch] {
			t.Errorf("%s: If-Match %v, spec %q", op.id, rt.IfMatch, op.ifMatch)
		}
		if rt.Idempotent != op.idempotent {
			t.Errorf("%s: Idempotency-Key %v, spec %v", op.id, rt.Idempotent, op.idempotent)
		}
	}
	for id := range routes {
		t.Errorf("%s: in routes.go but not in the spec's literal", id)
	}
	// createProject is the one operation that also needs a wide scope.
	for _, rt := range publicapi.ProductRoutes(publicapi.Deps{}) {
		if rt.Key.WideScope != (rt.OperationID == "createProject") {
			t.Errorf("%s: wide scope %v", rt.OperationID, rt.Key.WideScope)
		}
	}
}

// classWorld is the seed of the class tests: an owner with two projects in
// his space, a member of the first, an instance admin, the product server.
type classWorld struct {
	t          *testing.T
	pool       *pgxpool.Pool
	h          http.Handler
	owner      uuid.UUID
	member     uuid.UUID
	admin      uuid.UUID
	space      uuid.UUID
	spaceShort string
	p, q       uuid.UUID
	pPrefix    string
	qPrefix    string
	qName      string
	sfx        string
	n          int
	// issue and ready are issues of p: issue waits on ready and has an
	// event, a comment and the edge; qIssue is an issue of q. All three
	// titles carry word.
	issue, ready, qIssue string
	word                 string
	// repoURL is the repository of p, for the commit writes.
	repoURL string
	// attachment is a ready attachment of issue.
	attachment string
}

// newClassWorld seeds the world and builds the product public server from
// config.Load.
func newClassWorld(t *testing.T) *classWorld {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	t.Setenv("DATABASE_URL", os.Getenv("TEST_DATABASE_URL"))
	t.Setenv("SKEIN_INTERNAL_SECRET", wiringInternalSecret)
	t.Setenv("SKEIN_BCRYPT_COST", "4")
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("SKEIN_AGENT_PUBLIC_URL", "http://skein-classes.test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	srv, err := publicAPIServer(cfg, pool, classDeps(pool), slog.New(&recordingHandler{}))
	if err != nil || srv == nil {
		t.Fatalf("publicAPIServer: %v %v", srv, err)
	}
	w := &classWorld{t: t, pool: pool, h: srv.Handler, sfx: strings.ReplaceAll(uuid.NewString()[:8], "-", "")}
	w.owner, _ = testsupport.SeedUser(t, pool, "")
	w.member, _ = testsupport.SeedUser(t, pool, "")
	w.admin, _ = testsupport.SeedUser(t, pool, "admin")
	space := testsupport.SeedSpace(t, pool, w.owner)
	w.space = space
	// A project member of a non-personal space is a participant of it.
	testsupport.AddSpaceMember(t, pool, space, w.member)
	if err := pool.QueryRow(context.Background(), `SELECT short_id FROM spaces WHERE id = $1`, space).Scan(&w.spaceShort); err != nil {
		t.Fatalf("read space: %v", err)
	}
	w.p, w.pPrefix = testsupport.SeedProjectInSpace(t, pool, w.owner, space, "clsp")
	w.q, w.qPrefix = testsupport.SeedProjectInSpace(t, pool, w.owner, space, "clsq")
	if err := pool.QueryRow(context.Background(), `SELECT name FROM projects WHERE id = $1`, w.q).Scan(&w.qName); err != nil {
		t.Fatalf("read name: %v", err)
	}
	w.exec(`INSERT INTO project_members (project_id, user_id, role, added_at, added_by) VALUES ($1, $2, 'member', now(), $3)`,
		w.p, w.member, w.owner)
	for _, p := range []uuid.UUID{w.p, w.q} {
		w.component(p)
	}
	w.word = "clsw" + w.sfx
	w.repoURL = "https://git.example.test/" + w.pPrefix + ".git"
	w.exec(`UPDATE projects SET git_repos = jsonb_build_array(jsonb_build_object('id', $2::text, 'name', 'main', 'url', $3::text)) WHERE id = $1`,
		w.p, uuid.NewString(), w.repoURL)
	w.issue = testsupport.SeedIssue(t, pool, w.p, w.owner, w.pPrefix+"-1", "first "+w.word, "open", 1)
	w.ready = testsupport.SeedIssue(t, pool, w.p, w.owner, w.pPrefix+"-2", "second "+w.word, "open", 2)
	w.qIssue = testsupport.SeedIssue(t, pool, w.q, w.owner, w.qPrefix+"-1", "third "+w.word, "open", 2)
	w.exec(`INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'blocks', '{}'::jsonb, 1, $3, now())`, w.issue, w.ready, w.owner)
	w.exec(`INSERT INTO comments (id, issue_id, author, author_user_id, author_type, author_kind, body, created_at, updated_at)
VALUES ($1, $2, 'owner', $3, 'human', 'human', 'a note', now(), now())`, uuid.New(), w.issue, w.owner)
	w.exec(`INSERT INTO events (type, actor, actor_user_id, actor_type, entity_type, entity_id, project_id)
VALUES ('issue.created', 'owner', $1, 'human', 'issue', $2, $3)`, w.owner, w.issue, w.p)
	w.attachment = w.seedAttachment(w.issue, "ready")
	// The lists of the owner's own state are not empty.
	w.seedNotification(w.issue)
	w.seedFilter()
	testsupport.SeedReminder(t, pool, w.issue, w.owner)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM comments WHERE issue_id = ANY($1)`, []string{w.issue, w.ready, w.qIssue})
		_, _ = pool.Exec(ctx, `DELETE FROM dependencies WHERE from_id = ANY($1) OR to_id = ANY($1)`, []string{w.issue, w.ready, w.qIssue})
		_, _ = pool.Exec(ctx, `DELETE FROM events WHERE entity_id = ANY($1)`, []string{w.issue, w.ready, w.qIssue})
	})
	t.Cleanup(func() {
		// The users createUser made, with their personal spaces.
		ctx := context.Background()
		like := "%" + w.sfx + "%@classes.test"
		_, _ = pool.Exec(ctx, `DELETE FROM space_members WHERE space_id IN (SELECT s.id FROM spaces s JOIN users u ON u.id = s.owner_id WHERE u.email LIKE $1)`, like)
		_, _ = pool.Exec(ctx, `DELETE FROM spaces WHERE owner_id IN (SELECT id FROM users WHERE email LIKE $1)`, like)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE email LIKE $1`, like)
	})
	t.Cleanup(func() {
		ctx := context.Background()
		like := "%" + w.sfx + "%"
		_, _ = pool.Exec(ctx, `DELETE FROM events WHERE project_id IN (SELECT id FROM projects WHERE name LIKE $1)`, like)
		_, _ = pool.Exec(ctx, `DELETE FROM projects WHERE name LIKE $1`, like)
	})
	return w
}

// classDeps are the stores of the product, as main builds them.
func classDeps(pool *pgxpool.Pool) publicapi.Deps {
	searchSvc, err := search.NewService(search.NewStore(pool), nil, 0)
	if err != nil {
		panic("search.NewService: " + err.Error())
	}
	return publicapi.Deps{
		Projects:      projects.NewStore(pool),
		Users:         users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil),
		Issues:        issues.NewStore(pool),
		Dependencies:  dependencies.NewStore(pool),
		Search:        searchSvc,
		Notifications: notifications.NewStore(pool),
		Reminders:     reminders.NewStore(pool),
		Attachments: issueops.AttachmentDeps{Store: attachments.NewStore(pool), S3: routingS3{}, Bucket: "classes",
			MaxSize: 10 << 20, URLTTL: 10 * time.Minute},
	}
}

// seedAttachment inserts an attachment of issue uploaded by the owner, in
// status pending or ready, and returns its id.
func (w *classWorld) seedAttachment(issue, status string) string {
	w.t.Helper()
	id := uuid.New()
	w.exec(`INSERT INTO attachments (id, issue_id, filename, content_type, size_bytes, s3_bucket, s3_key, status, uploaded_by)
VALUES ($1, $2, 'note.txt', 'text/plain', 12, 'classes', $3, $4, $5)`, id, issue, "classes/"+id.String(), status, w.owner)
	return id.String()
}

// seedNotification inserts an unread notification of the owner about issue
// and returns its id.
func (w *classWorld) seedNotification(issue string) string {
	w.t.Helper()
	var event, id int64
	if err := w.pool.QueryRow(context.Background(), `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_user_id, actor_type, payload)
VALUES (now(), 'issue.updated', 'issue', $1, $2, 'seed', $3, 'human', '{}'::jsonb) RETURNING id`, issue, w.p, w.owner).Scan(&event); err != nil {
		w.t.Fatalf("seed event: %v", err)
	}
	if err := w.pool.QueryRow(context.Background(), `
INSERT INTO notifications (user_id, event_id, type, project_id, created_at, feed_channel)
VALUES ($1, $2, 'issue.updated', $3, now(), true) RETURNING id`, w.owner, event, w.p).Scan(&id); err != nil {
		w.t.Fatalf("seed notification: %v", err)
	}
	return strconv.FormatInt(id, 10)
}

// seedFilter inserts a subscription filter of the owner over p and returns
// its id.
func (w *classWorld) seedFilter() string {
	w.t.Helper()
	id := uuid.New()
	w.exec(`INSERT INTO custom_watchers (id, user_id, name, filter) VALUES ($1, $2, $3, jsonb_build_object('projects', jsonb_build_array($4::text)))`,
		id, w.owner, w.fresh("filter"), w.p.String())
	return id.String()
}

// seedComment inserts a comment of the owner on issue and returns its id.
func (w *classWorld) seedComment(issue string) string {
	w.t.Helper()
	id := uuid.New()
	w.exec(`INSERT INTO comments (id, issue_id, author, author_user_id, author_type, author_kind, body, created_at, updated_at)
VALUES ($1, $2, 'owner', $3, 'human', 'human', 'a seeded note', now(), now())`, id, issue, w.owner)
	return id.String()
}

func (w *classWorld) exec(sql string, args ...any) {
	w.t.Helper()
	if _, err := w.pool.Exec(context.Background(), sql, args...); err != nil {
		w.t.Fatalf("exec %q: %v", sql, err)
	}
}

// fresh is a name unique to the test.
func (w *classWorld) fresh(what string) string {
	w.n++
	return "cls" + what + w.sfx + "x" + strings.Repeat("a", w.n%5) + string(rune('a'+w.n%26)) + uuid.NewString()[:4]
}

// component inserts a component of project and returns its id.
func (w *classWorld) component(project uuid.UUID) uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	w.exec(`INSERT INTO components (id, project_id, name, description, paths, created_by, created_at, updated_at)
VALUES ($1, $2, $3, '', '[]'::jsonb, $4, now(), now())`, id, project, w.fresh("comp"), w.owner)
	return id
}

// freshIssue seeds an open issue of project p with a fresh id.
func (w *classWorld) freshIssue(p uuid.UUID) string {
	w.t.Helper()
	w.n++
	prefix := w.pPrefix
	if p == w.q {
		prefix = w.qPrefix
	}
	return testsupport.SeedIssue(w.t, w.pool, p, w.owner, prefix+"-"+strconv.Itoa(100+w.n), "fresh "+w.word, "open", 2)
}

// issueVersion is the current version of issue id.
func (w *classWorld) issueVersion(id string) int {
	w.t.Helper()
	var v int
	if err := w.pool.QueryRow(context.Background(), `SELECT version FROM issues WHERE id = $1`, id).Scan(&v); err != nil {
		w.t.Fatalf("read version: %v", err)
	}
	return v
}

// freshHash is a commit hash unique to the test.
func (w *classWorld) freshHash() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
}

// key mints a key of user with grant.
func (w *classWorld) key(user uuid.UUID, g apikeys.Grant) string {
	w.t.Helper()
	plain, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		w.t.Fatalf("GenerateAPIKey: %v", err)
	}
	c, err := apikeys.New(w.pool).Create(context.Background(), apikeys.NewKey{
		UserID: user, Label: "classes", Prefix: prefix, Hash: hash, Grant: g,
	}, testsupport.KeyTTLPolicy)
	if err != nil {
		w.t.Fatalf("create key: %v", err)
	}
	w.t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(), `DELETE FROM agents WHERE api_key_id = $1`, c.ID)
		_, _ = w.pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, c.ID)
	})
	return plain
}

// keyFor is the key a class asks op with: an operation of the right admin
// needs an instance admin as the key's owner, so it goes with a manage key
// of w.admin that also has admin; any other operation with ownerKey.
func (w *classWorld) keyFor(op publicOp, ownerKey string) string {
	w.t.Helper()
	if op.right != "admin" {
		return ownerKey
	}
	return w.key(w.admin, apikeys.Grant{Level: apikeys.LevelManage, Admin: true, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}})
}

func levelKey(l apikeys.Level) apikeys.Grant {
	return apikeys.Grant{Level: l, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}}
}

// opCall is one request of an operation.
type opCall struct {
	method, path string
	body         map[string]any
	// headers are sent with the request (If-Match).
	headers map[string]string
}

// call is a request of op that succeeds for the owner of project p: the
// state it needs (a member to remove, a component to change) is seeded
// first.
func (w *classWorld) call(op publicOp, p uuid.UUID, key string) opCall {
	w.t.Helper()
	base := "/api/v1/"
	proj := base + "projects/" + p.String()
	switch op.id {
	case "getMe":
		return opCall{http.MethodGet, base + "me", nil, nil}
	case "listProjects":
		return opCall{http.MethodGet, base + "projects", nil, nil}
	case "resolveProject":
		return opCall{http.MethodGet, base + "projects/resolve?q=" + url.QueryEscape(w.pPrefix), nil, nil}
	case "createProject":
		return opCall{http.MethodPost, base + "projects", map[string]any{"name": w.fresh("proj"), "space": w.spaceShort}, nil}
	case "getProject":
		return opCall{http.MethodGet, proj, nil, nil}
	case "updateProject":
		return opCall{http.MethodPatch, proj, map[string]any{"description": w.fresh("desc")}, nil}
	case "listProjectMembers":
		return opCall{http.MethodGet, proj + "/members", nil, nil}
	case "putProjectMember":
		u, _ := testsupport.SeedUser(w.t, w.pool, "")
		testsupport.AddSpaceMember(w.t, w.pool, w.space, u)
		return opCall{http.MethodPut, proj + "/members/" + u.String(), map[string]any{"role": "member"}, nil}
	case "deleteProjectMember":
		u, _ := testsupport.SeedUser(w.t, w.pool, "")
		w.exec(`INSERT INTO project_members (project_id, user_id, role, added_at, added_by) VALUES ($1, $2, 'member', now(), $3)`,
			p, u, w.owner)
		return opCall{http.MethodDelete, proj + "/members/" + u.String(), nil, nil}
	case "listComponents":
		return opCall{http.MethodGet, proj + "/components", nil, nil}
	case "createComponent":
		return opCall{http.MethodPost, proj + "/components", map[string]any{"name": w.fresh("comp")}, nil}
	case "updateComponent":
		return opCall{http.MethodPatch, proj + "/components/" + w.component(p).String(), map[string]any{"name": w.fresh("comp")}, nil}
	case "archiveComponent":
		return opCall{http.MethodPost, proj + "/components/" + w.component(p).String() + "/archive", nil, nil}
	case "listIssues":
		return opCall{http.MethodGet, base + "issues?project=" + w.pPrefix, nil, nil}
	case "listReadyIssues":
		return opCall{http.MethodGet, base + "issues/ready?project=" + w.pPrefix, nil, nil}
	case "getIssue":
		return opCall{http.MethodGet, base + "issues/" + w.issue, nil, nil}
	case "listIssueEvents":
		return opCall{http.MethodGet, base + "issues/" + w.issue + "/events", nil, nil}
	case "listDependencies":
		return opCall{http.MethodGet, base + "issues/" + w.issue + "/dependencies", nil, nil}
	case "listComments":
		return opCall{http.MethodGet, base + "issues/" + w.issue + "/comments", nil, nil}
	case "getIssueMovePlan":
		return opCall{http.MethodGet, base + "issues/" + w.issue + "/move-plan?targetProject=" + w.qPrefix, nil, nil}
	case "search":
		return opCall{http.MethodGet, base + "search?kinds=issue&query=" + w.word, nil, nil}
	case "listNotifications":
		return opCall{http.MethodGet, base + "notifications", nil, nil}
	case "markNotificationRead":
		return opCall{http.MethodPost, base + "notifications/" + w.seedNotification(w.issue) + "/read", nil, nil}
	case "markAllNotificationsRead":
		return opCall{http.MethodPost, base + "notifications/read-all", nil, nil}
	case "archiveNotification":
		return opCall{http.MethodPost, base + "notifications/" + w.seedNotification(w.issue) + "/archive", nil, nil}
	case "listWatching":
		return opCall{http.MethodGet, base + "me/watching", nil, nil}
	case "watchProject":
		return opCall{http.MethodPut, proj + "/watch", nil, nil}
	case "unwatchProject":
		return opCall{http.MethodDelete, proj + "/watch", nil, nil}
	case "listWatchFilters":
		return opCall{http.MethodGet, base + "me/watch-filters", nil, nil}
	case "createWatchFilter":
		return opCall{http.MethodPost, base + "me/watch-filters", map[string]any{"name": w.fresh("filter"),
			"filter": map[string]any{"projects": []string{p.String()}}}, nil}
	case "deleteWatchFilter":
		return opCall{http.MethodDelete, base + "me/watch-filters/" + w.seedFilter(), nil, nil}
	case "listReminders":
		return opCall{http.MethodGet, base + "reminders", nil, nil}
	case "listUsers":
		return opCall{http.MethodGet, base + "users", nil, nil}
	case "createUser":
		return opCall{http.MethodPost, base + "users", map[string]any{"email": w.fresh("u") + "@classes.test", "name": "classes",
			"password": "initial-pass", "role": "user"}, nil}
	case "updateUser":
		u, _ := testsupport.SeedUser(w.t, w.pool, "")
		return opCall{http.MethodPatch, base + "users/" + u.String(), map[string]any{"name": w.fresh("name")}, nil}
	case "addSpaceMember":
		u, _ := testsupport.SeedUser(w.t, w.pool, "")
		return opCall{http.MethodPost, base + "spaces/" + w.spaceShort + "/members", map[string]any{"userId": u.String()}, nil}
	case "createIssue":
		return opCall{http.MethodPost, proj + "/issues", map[string]any{"title": w.fresh("iss"), "type": "task", "force": true}, nil}
	case "createEpic":
		return opCall{http.MethodPost, proj + "/epics", map[string]any{"title": w.fresh("epic"), "childTemplates": []map[string]any{{"title": "child"}}}, nil}
	}
	id := w.freshIssue(p)
	issue := base + "issues/" + id
	match := map[string]string{"If-Match": `"` + strconv.Itoa(w.issueVersion(id)) + `"`}
	switch op.id {
	case "updateIssue":
		return opCall{http.MethodPatch, issue, map[string]any{"title": w.fresh("title")}, match}
	case "claimIssue":
		return opCall{http.MethodPost, issue + "/claim", nil, nil}
	case "claimIssues":
		return opCall{http.MethodPost, base + "issues/claim", map[string]any{"ids": []string{id}}, nil}
	case "unclaimIssue":
		w.do(key, opCall{http.MethodPost, issue + "/claim", nil, nil}, "", nil)
		return opCall{http.MethodPost, issue + "/unclaim", map[string]any{"nextStatus": "open", "reason": "handover"}, nil}
	case "releaseIssueLock":
		w.do(key, opCall{http.MethodPost, issue + "/claim", nil, nil}, "", nil)
		return opCall{http.MethodPost, issue + "/release-lock", nil, nil}
	case "closeIssue":
		return opCall{http.MethodPost, issue + "/close", map[string]any{"reason": "done", "closeOutcome": "done"}, match}
	case "acceptIssue":
		w.exec(`UPDATE issues SET assignee = (SELECT username FROM users WHERE id = $2) WHERE id = $1`, id, w.owner)
		return opCall{http.MethodPost, issue + "/accept", nil, nil}
	case "reparentIssue":
		return opCall{http.MethodPost, issue + "/reparent", map[string]any{"newParentId": w.freshIssue(p)}, match}
	case "dropDelegated", "resumeDelegated":
		w.exec(`UPDATE issues SET assignee = (SELECT username FROM users WHERE id = $2) WHERE id = $1`, id, w.member)
		action := "/drop-delegated"
		if op.id == "resumeDelegated" {
			action = "/resume-delegated"
		}
		return opCall{http.MethodPost, issue + action, nil, nil}
	case "attachCommits":
		return opCall{http.MethodPost, issue + "/commits", map[string]any{"commits": []map[string]any{{"hash": w.freshHash(), "repoUrl": w.repoURL}}}, nil}
	case "bulkUpdateIssues":
		return opCall{http.MethodPost, base + "issues/bulk-update", map[string]any{"items": []map[string]any{
			{"id": id, "expectedVersion": w.issueVersion(id), "title": w.fresh("bulk")}}}, nil}
	case "bulkCloseIssues":
		return opCall{http.MethodPost, base + "issues/bulk-close", map[string]any{"items": []map[string]any{
			{"id": id, "expectedVersion": w.issueVersion(id), "reason": "done", "closeOutcome": "done"}}}, nil}
	case "bulkAddDependencies":
		return opCall{http.MethodPost, base + "issues/dependencies/bulk-add", map[string]any{"items": []map[string]any{
			{"fromId": id, "toId": w.freshIssue(p), "type": "related"}}}, nil}
	case "createComment":
		return opCall{http.MethodPost, issue + "/comments", map[string]any{"body": w.fresh("comment")}, nil}
	case "getComment":
		return opCall{http.MethodGet, base + "comments/" + w.seedComment(id), nil, nil}
	case "updateComment":
		return opCall{http.MethodPatch, base + "comments/" + w.seedComment(id), map[string]any{"body": w.fresh("edit")},
			map[string]string{"If-Match": `"1"`}}
	case "deleteComment":
		return opCall{http.MethodDelete, base + "comments/" + w.seedComment(id), nil, nil}
	case "addDependency":
		return opCall{http.MethodPost, issue + "/dependencies", map[string]any{"toId": w.freshIssue(p), "type": "related"}, nil}
	case "removeDependency":
		to := w.freshIssue(p)
		w.exec(`INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'related', '{}'::jsonb, 1, $3, now())`, id, to, w.owner)
		return opCall{http.MethodDelete, issue + "/dependencies/" + to + "/related", nil, nil}
	case "listAttachments":
		return opCall{http.MethodGet, base + "issues/" + w.issue + "/attachments", nil, nil}
	case "createAttachmentUpload":
		return opCall{http.MethodPost, issue + "/attachments", map[string]any{"filename": "note.txt", "contentType": "text/plain", "sizeBytes": 12}, nil}
	case "confirmAttachment":
		return opCall{http.MethodPost, base + "attachments/" + w.seedAttachment(id, "pending") + "/confirm", nil, nil}
	case "getAttachment":
		return opCall{http.MethodGet, base + "attachments/" + w.attachment, nil, nil}
	case "getAttachmentDownloadUrl":
		return opCall{http.MethodGet, base + "attachments/" + w.attachment + "/download-url", nil, nil}
	case "deleteAttachment":
		return opCall{http.MethodDelete, base + "attachments/" + w.seedAttachment(id, "ready"), nil, nil}
	case "markIssueNotificationsRead":
		return opCall{http.MethodPost, issue + "/notifications/read", nil, nil}
	case "markWatchingCardRead":
		return opCall{http.MethodPost, base + "me/watching/issues/" + id + "/read", nil, nil}
	case "watchIssue":
		return opCall{http.MethodPut, issue + "/watch", nil, nil}
	case "unwatchIssue":
		return opCall{http.MethodDelete, issue + "/watch", nil, nil}
	case "createReminder":
		return opCall{http.MethodPost, issue + "/reminders", map[string]any{"remindAt": "2099-01-01T00:00:00Z"}, nil}
	case "snoozeReminder":
		return opCall{http.MethodPost, base + "reminders/" + testsupport.SeedReminder(w.t, w.pool, id, w.owner).String() + "/snooze",
			map[string]any{"by": "1h"}, nil}
	case "dismissReminder":
		return opCall{http.MethodDelete, base + "reminders/" + testsupport.SeedReminder(w.t, w.pool, id, w.owner).String(), nil, nil}
	case "detachCommit":
		hash := w.freshHash()
		w.do(key, opCall{http.MethodPost, issue + "/commits", map[string]any{"commits": []map[string]any{{"hash": hash, "repoUrl": w.repoURL}}}, nil}, "", nil)
		return opCall{http.MethodDelete, issue + "/commits/" + hash, nil, nil}
	}
	w.t.Fatalf("no request for %s", op.id)
	return opCall{}
}

// answer is one response of the server.
type answer struct {
	status int
	header http.Header
	body   []byte
}

func (a answer) object(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(a.body, &m); err != nil {
		t.Fatalf("answer %d is not a JSON object: %v (%s)", a.status, err, a.body)
	}
	return m
}

func (a answer) code() string {
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(a.body, &p)
	return p.Code
}

// do sends c with key and headers; query is added to the path's query.
func (w *classWorld) do(key string, c opCall, query string, headers map[string]string) answer {
	w.t.Helper()
	path := c.path
	if query != "" {
		if strings.Contains(path, "?") {
			path += "&" + query
		} else {
			path += "?" + query
		}
	}
	var rd io.Reader = http.NoBody
	if c.body != nil {
		raw, err := json.Marshal(c.body)
		if err != nil {
			w.t.Fatalf("marshal: %v", err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(c.method, path, rd)
	if c.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(auth.HeaderAPIKey, key)
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, req)
	return answer{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

// withBody is c with the members of extra added to its body.
func withBody(c opCall, extra map[string]any) opCall {
	body := map[string]any{}
	for k, v := range c.body {
		body[k] = v
	}
	for k, v := range extra {
		body[k] = v
	}
	c.body = body
	return c
}

// lastEvent is the id of the latest event: the baseline of «nothing written».
func (w *classWorld) lastEvent() int64 {
	w.t.Helper()
	var id int64
	if err := w.pool.QueryRow(context.Background(), `SELECT coalesce(max(id), 0) FROM events`).Scan(&id); err != nil {
		w.t.Fatalf("read the last event: %v", err)
	}
	return id
}

// actorsSince are the actor types of the events written after since.
func (w *classWorld) actorsSince(since int64) []string {
	w.t.Helper()
	rows, err := w.pool.Query(context.Background(), `SELECT actor_type FROM events WHERE id > $1 ORDER BY id`, since)
	if err != nil {
		w.t.Fatalf("read events: %v", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			w.t.Fatalf("scan: %v", err)
		}
		out = append(out, a)
	}
	return out
}

// wantNothingWritten asserts that no event was written after since.
func (w *classWorld) wantNothingWritten(t *testing.T, what string, since int64) {
	t.Helper()
	if got := w.actorsSince(since); len(got) != 0 {
		t.Errorf("%s: %d events written, want none", what, len(got))
	}
}

func success(status int) bool { return status >= 200 && status < 300 }

// below is the grant one level below right: a key without a level (admin
// only, of an admin owner) for read, read for write, write for manage.
func (w *classWorld) below(right string) (owner uuid.UUID, g apikeys.Grant) {
	switch right {
	case "read":
		return w.admin, apikeys.Grant{Admin: true, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}}
	case "write":
		return w.owner, levelKey(apikeys.LevelRead)
	case "manage":
		return w.owner, levelKey(apikeys.LevelWrite)
	case "admin":
		return w.admin, levelKey(apikeys.LevelManage)
	}
	w.t.Fatalf("no level below %s", right)
	return uuid.Nil, apikeys.Grant{}
}

// TestPublicAPIClasses_KeyRight — row 100 (SC-002). For each operation, a
// key one level below its right gets 403 insufficient_scope naming the
// right and the operation, and nothing is written. Red when the right of an
// operation in routes.go is below the spec's.
func TestPublicAPIClasses_KeyRight(t *testing.T) {
	w := newClassWorld(t)
	for _, op := range publicOps {
		if op.right == "none" {
			continue
		}
		t.Run(op.id, func(t *testing.T) {
			owner, g := w.below(op.right)
			key := w.key(owner, g)
			c := w.call(op, w.p, key)
			since := w.lastEvent()
			a := w.do(key, c, "", nil)
			if a.status != http.StatusForbidden || a.code() != "insufficient_scope" {
				t.Fatalf("%d %s, want 403 insufficient_scope", a.status, a.body)
			}
			if p := a.object(t); p["requiredRight"] != op.right || p["operationId"] != op.id {
				t.Errorf("refusal names %v / %v, want %s / %s", p["requiredRight"], p["operationId"], op.right, op.id)
			}
			w.wantNothingWritten(t, op.id, since)
		})
	}
}

// TestPublicAPIClasses_FieldsAndPrefer — row 101 (US-8, SC-008, OST-3-7).
// Each read keeps with fields= the named field plus id — on each item of a
// list, whose nextCursor and hasMore stay — and refuses an unknown field
// with 400; each write refuses fields= with 400 and writes nothing; each
// write where Prefer does not apply answers Prefer: return=minimal as
// without it, with no Preference-Applied. Red when a flag of routes.go
// differs from the literal.
func TestPublicAPIClasses_FieldsAndPrefer(t *testing.T) {
	w := newClassWorld(t)
	ownerKey := w.key(w.owner, levelKey(apikeys.LevelManage))
	for _, op := range publicOps {
		t.Run(op.id, func(t *testing.T) {
			key := w.keyFor(op, ownerKey)
			if op.read {
				c := w.call(op, w.p, key)
				full := w.do(key, c, "", nil).object(t)
				a := w.do(key, c, "fields="+op.field, nil)
				if a.status != http.StatusOK {
					t.Fatalf("fields=%s: %d %s", op.field, a.status, a.body)
				}
				got := a.object(t)
				if items, isList := full["items"]; isList {
					for _, k := range []string{"items", "nextCursor", "hasMore"} {
						if _, ok := got[k]; !ok {
							t.Errorf("fields= on a list dropped %s: %s", k, a.body)
						}
					}
					list, _ := got["items"].([]any)
					if fullList, _ := items.([]any); len(fullList) == 0 || len(list) != len(fullList) {
						t.Fatalf("fields= on a list: %d items of %d (the list must not be empty)", len(list), len(fullList))
					}
					for _, it := range list {
						m, _ := it.(map[string]any)
						if _, ok := m[op.field]; !ok {
							t.Errorf("item without %s: %v", op.field, m)
						}
						for k := range m {
							if k != op.field && k != "id" {
								t.Errorf("item kept %s besides %s and id", k, op.field)
							}
						}
					}
				} else {
					want := map[string]bool{op.field: true}
					if _, ok := full["id"]; ok {
						want["id"] = true
					}
					if len(got) != len(want) {
						t.Errorf("fields=%s kept %v, want %v", op.field, keysOf(got), want)
					}
					for k := range want {
						if _, ok := got[k]; !ok {
							t.Errorf("fields=%s dropped %s", op.field, k)
						}
					}
				}
				if a := w.do(key, c, "fields=nosuchfield", nil); a.status != http.StatusBadRequest || a.code() != "validation_error" {
					t.Errorf("an unknown field: %d %s, want 400 validation_error", a.status, a.body)
				}
				return
			}
			c := w.call(op, w.p, key)
			since := w.lastEvent()
			if a := w.do(key, c, "fields=id", nil); a.status != http.StatusBadRequest || a.code() != "validation_error" {
				t.Errorf("fields= on a write: %d %s, want 400 validation_error", a.status, a.body)
			}
			w.wantNothingWritten(t, op.id+" with fields=", since)
			a := w.do(key, c, "", map[string]string{"Prefer": "return=minimal"})
			if op.prefer {
				if !success(a.status) {
					t.Fatalf("with Prefer: %d %s", a.status, a.body)
				}
				if h := a.header.Get("Preference-Applied"); h != "return=minimal" {
					t.Errorf("Preference-Applied %q, want return=minimal", h)
				}
				if got := keysOf(a.object(t)); !slices.Equal(got, []string{"id", "version"}) {
					t.Errorf("Prefer: return=minimal answered %v, want id and version", got)
				}
				return
			}
			if !success(a.status) {
				t.Fatalf("with Prefer: %d %s", a.status, a.body)
			}
			if h := a.header.Get("Preference-Applied"); h != "" {
				t.Errorf("Preference-Applied %q on an operation Prefer does not apply to", h)
			}
			if len(a.body) > 0 {
				m := a.object(t)
				extra := false
				for k := range m {
					if k != "id" && k != "version" {
						extra = true
					}
				}
				if !extra {
					t.Errorf("Prefer shortened the answer to %s", a.body)
				}
			}
		})
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestPublicAPIClasses_OriginOfAWrite — row 102 (SC-014, INV-4, INV-5,
// OST-2-8). Each write by key — without kind, with kind directed and with
// kind autonomous — writes its events as an agent, never as a human; a kind
// outside the two is refused with 400. An alias added by key carries the
// agent's added_by_kind (updateProject, createProject). Red when a write by
// key is recorded as a human one.
func TestPublicAPIClasses_OriginOfAWrite(t *testing.T) {
	w := newClassWorld(t)
	for _, op := range publicOps {
		if op.read {
			continue
		}
		t.Run(op.id, func(t *testing.T) {
			// A key per operation: the run makes more requests than one key's
			// rate limit lets through in a minute.
			key := w.keyFor(op, w.key(w.owner, levelKey(apikeys.LevelManage)))
			for _, kind := range []string{"", "directed", "autonomous"} {
				c := w.call(op, w.p, key)
				if kind != "" {
					c = withBody(c, map[string]any{"kind": kind})
				}
				since := w.lastEvent()
				a := w.do(key, c, "", nil)
				if !success(a.status) {
					t.Fatalf("kind %q: %d %s", kind, a.status, a.body)
				}
				actors := w.actorsSince(since)
				if len(actors) == 0 && !op.noEvent {
					t.Fatalf("kind %q: no event written", kind)
				}
				if op.id == "createIssue" || op.id == "createEpic" {
					want := map[string]string{"": "api", "directed": "agent_directed", "autonomous": "agent_autonomous"}[kind]
					var source string
					if err := w.pool.QueryRow(context.Background(), `SELECT source FROM issues WHERE id = $1`, a.object(t)["id"]).Scan(&source); err != nil {
						t.Fatalf("read source: %v", err)
					}
					if source != want {
						t.Errorf("kind %q: source %q, want %s", kind, source, want)
					}
				}
				for _, at := range actors {
					if at != "agent" {
						t.Errorf("kind %q: an event of actor type %q, want agent", kind, at)
					}
				}
			}
			c := withBody(w.call(op, w.p, key), map[string]any{"kind": "human"})
			since := w.lastEvent()
			if a := w.do(key, c, "", nil); a.status != http.StatusBadRequest || a.code() != "validation_error" {
				t.Errorf("kind human: %d %s, want 400 validation_error", a.status, a.body)
			}
			w.wantNothingWritten(t, op.id+" with kind human", since)
		})
	}
	t.Run("aliases added by key", func(t *testing.T) {
		key := w.key(w.owner, levelKey(apikeys.LevelManage))
		updated := w.fresh("al")
		if a := w.do(key, opCall{http.MethodPatch, "/api/v1/projects/" + w.p.String(), map[string]any{"aliasesAdd": []string{updated}}, nil}, "", nil); !success(a.status) {
			t.Fatalf("updateProject: %d %s", a.status, a.body)
		}
		created := w.fresh("al")
		if a := w.do(key, opCall{http.MethodPost, "/api/v1/projects", map[string]any{"name": w.fresh("proj"), "space": w.spaceShort, "aliases": []string{created}}, nil}, "", nil); !success(a.status) {
			t.Fatalf("createProject: %d %s", a.status, a.body)
		}
		for _, alias := range []string{updated, created} {
			var kind string
			if err := w.pool.QueryRow(context.Background(), `SELECT added_by_kind FROM project_aliases WHERE alias = $1`, alias).Scan(&kind); err != nil {
				t.Fatalf("read alias %s: %v", alias, err)
			}
			if kind != "agent" {
				t.Errorf("alias %s added by key has added_by_kind %q, want agent", alias, kind)
			}
		}
	})
}

// TestPublicAPIClasses_KeyScope — row 103 (SC-003, INV-2). With a key of
// scope «project P», every list, search and GET /me of the owner shows
// nothing of Q, his other project, and Q answers as a missing project; with
// a key of scope «all» the same owner sees Q in the same answers (base
// line). Red when an answer of the P key carries Q.
func TestPublicAPIClasses_KeyScope(t *testing.T) {
	w := newClassWorld(t)
	scoped := w.key(w.owner, apikeys.Grant{Level: apikeys.LevelRead, Scope: apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{w.p}}})
	all := w.key(w.owner, levelKey(apikeys.LevelRead))
	q := w.q.String()
	get := func(key, path string) answer { return w.do(key, opCall{http.MethodGet, path, nil, nil}, "", nil) }

	for _, c := range []struct {
		name, path string
	}{
		{"getMe", "/api/v1/me"},
		{"listProjects", "/api/v1/projects?limit=100"},
		{"listProjects by search", "/api/v1/projects?q=" + url.QueryEscape(w.qName)},
		{"resolveProject", "/api/v1/projects/resolve?q=" + url.QueryEscape(w.qPrefix)},
		{"listIssues across projects", "/api/v1/issues?limit=100&q=" + w.word},
		{"search", "/api/v1/search?kinds=issue&limit=50&query=" + w.word},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, a := get(scoped, c.path), get(all, c.path)
			if s.status != http.StatusOK || a.status != http.StatusOK {
				t.Fatalf("answers %d %s / %d %s", s.status, s.body, a.status, a.body)
			}
			for _, of := range []string{q, w.qIssue} {
				if strings.Contains(string(s.body), `"`+of+`"`) {
					t.Errorf("the P key's answer carries %s of Q: %s", of, s.body)
				}
			}
			if !strings.Contains(string(a.body), `"`+q+`"`) && !strings.Contains(string(a.body), `"`+w.qIssue+`"`) {
				t.Errorf("the base line does not show Q: %s", a.body)
			}
		})
	}
	for _, c := range []struct{ path, code string }{
		{"/api/v1/projects/" + q, "project_not_found"},
		{"/api/v1/projects/" + q + "/members", "project_not_found"},
		{"/api/v1/projects/" + q + "/components", "project_not_found"},
		{"/api/v1/projects/" + w.qPrefix, "project_not_found"},
		{"/api/v1/issues/" + w.qIssue, "issue_not_found"},
		{"/api/v1/issues/" + w.qIssue + "/comments", "issue_not_found"},
		{"/api/v1/issues/" + w.qIssue + "/events", "issue_not_found"},
		{"/api/v1/issues/" + w.qIssue + "/dependencies", "issue_not_found"},
	} {
		path := c.path
		t.Run(path, func(t *testing.T) {
			if s := get(scoped, path); s.status != http.StatusNotFound || s.code() != c.code {
				t.Errorf("the P key: %d %s, want 404 %s", s.status, s.body, c.code)
			}
			if a := get(all, path); a.status != http.StatusOK {
				t.Errorf("the base line: %d %s", a.status, a.body)
			}
		})
	}
}

// TestPublicAPIClasses_OwnerRole — row 104 (SC-015, INV-1). The owner of a
// manage key who is a plain member of P gets 403 forbidden — the code of
// the role, not insufficient_scope — on every write to an object of P, and
// nothing is written; the same key passes once its owner holds the role.
// Red when the key's right widens the owner's role.
func TestPublicAPIClasses_OwnerRole(t *testing.T) {
	w := newClassWorld(t)
	key := w.key(w.member, levelKey(apikeys.LevelManage))
	for _, op := range publicOps {
		if !op.projectWrite {
			continue
		}
		t.Run(op.id, func(t *testing.T) {
			c := w.call(op, w.p, key)
			since := w.lastEvent()
			if a := w.do(key, c, "", nil); a.status != http.StatusForbidden || a.code() != "forbidden" {
				t.Fatalf("a member: %d %s, want 403 forbidden", a.status, a.body)
			}
			w.wantNothingWritten(t, op.id, since)
			w.exec(`UPDATE project_members SET role = 'owner' WHERE project_id = $1 AND user_id = $2`, w.p, w.member)
			defer w.exec(`UPDATE project_members SET role = 'member' WHERE project_id = $1 AND user_id = $2`, w.p, w.member)
			if a := w.do(key, w.call(op, w.p, key), "", nil); !success(a.status) {
				t.Errorf("the owner: %d %s", a.status, a.body)
			}
		})
	}
}

// TestPublicAPIClasses_BackfilledKey — row 106 (SC-013, US-13). A key in
// the state the backfill of migration 000086 leaves an old key in — level
// manage, the admin flag only of an admin owner, scope all, no expiry —
// makes the representative operations of the public API: reads and writes.
// Red when the migrated key is refused.
func TestPublicAPIClasses_BackfilledKey(t *testing.T) {
	w := newClassWorld(t)
	plain, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	var keyID uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `
INSERT INTO api_keys (id, user_id, key_hash, prefix, label, level, is_admin, scope_mode, expires_at)
VALUES ($1, $2, $3, $4, 'backfilled', 'manage', false, 'all', NULL) RETURNING id`, uuid.New(), w.owner, hash, prefix).Scan(&keyID); err != nil {
		t.Fatalf("seed a backfilled key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(), `DELETE FROM agents WHERE api_key_id = $1`, keyID)
		_, _ = w.pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, keyID)
	})
	// The backfill gives admin only to the key of an admin owner: the
	// operations of the right admin go with such a key.
	adminPlain, adminPrefix, adminHash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	var adminKeyID uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `
INSERT INTO api_keys (id, user_id, key_hash, prefix, label, level, is_admin, scope_mode, expires_at)
VALUES ($1, $2, $3, $4, 'backfilled admin', 'manage', true, 'all', NULL) RETURNING id`, uuid.New(), w.admin, adminHash, adminPrefix).Scan(&adminKeyID); err != nil {
		t.Fatalf("seed a backfilled admin key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(), `DELETE FROM agents WHERE api_key_id = $1`, adminKeyID)
		_, _ = w.pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, adminKeyID)
	})
	for _, op := range publicOps {
		t.Run(op.id, func(t *testing.T) {
			plain := plain
			if op.right == "admin" {
				plain = adminPlain
			}
			if a := w.do(plain, w.call(op, w.p, plain), "", nil); !success(a.status) {
				t.Errorf("%d %s", a.status, a.body)
			}
		})
	}
}

// TestPublicAPIClasses_StrictParsing — row 129 («General rules of the
// surface», OST-3-7). Every operation refuses an unknown query parameter
// with 400, and every operation with a body an unknown body field with 400;
// the operation is not run. Red when an unknown parameter or field is
// silently ignored.
func TestPublicAPIClasses_StrictParsing(t *testing.T) {
	w := newClassWorld(t)
	ownerKey := w.key(w.owner, levelKey(apikeys.LevelManage))
	for _, op := range publicOps {
		t.Run(op.id, func(t *testing.T) {
			key := w.keyFor(op, ownerKey)
			c := w.call(op, w.p, key)
			since := w.lastEvent()
			if a := w.do(key, c, "nosuchparam=1", nil); a.status != http.StatusBadRequest || a.code() != "validation_error" {
				t.Errorf("an unknown parameter: %d %s, want 400 validation_error", a.status, a.body)
			}
			if op.body {
				if a := w.do(key, withBody(c, map[string]any{"nosuchfield": 1}), "", nil); a.status != http.StatusBadRequest || a.code() != "validation_error" {
					t.Errorf("an unknown body field: %d %s, want 400 validation_error", a.status, a.body)
				}
			}
			w.wantNothingWritten(t, op.id, since)
		})
	}
}
