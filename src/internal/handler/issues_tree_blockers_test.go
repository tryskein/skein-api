package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
)

// sk-421.2.8 — the blockers field of a tree node at the HTTP boundary (US-4). The file is shared
// by test plan rows 8, 9, 12, 13, 18, 19, 20, 22: each adds its own
// tests and fixtures without breaking others'.

// Row 8 — the includeBlockers parsing is attached to BOTH tree routes.
// The parsing function can exist and be covered by a table (row 7) but
// not be called in one of the handlers — then that route would answer 200 to an
// invalid value. That the value is attached in args is proven by row 12.
func TestTreeBlockersHTTP_InvalidValueRefusedOnBothRoutes(t *testing.T) {
	env := newListEnv(t)

	t.Run("project route", func(t *testing.T) {
		res, body := callProjectTree(t, env, env.owner, "user", "includeBlockers=yes")
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("GET /projects/{pid}/issues/tree?includeBlockers=yes = %d, want 400 (body: %v)",
				res.StatusCode, body)
		}
		if body["error"] != "invalid_include_blockers" {
			t.Errorf("code in body = %v, want invalid_include_blockers (body: %v)", body["error"], body)
		}
	})

	t.Run("cross-project route", func(t *testing.T) {
		res, body := callAllTree(t, env.pool, env.owner, "user", false, "includeBlockers=yes")
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("GET /issues/tree?includeBlockers=yes = %d, want 400 (body: %v)",
				res.StatusCode, body)
		}
		if body["error"] != "invalid_include_blockers" {
			t.Errorf("code in body = %v, want invalid_include_blockers (body: %v)", body["error"], body)
		}
	})
}

// treeRoute names one of the two tree endpoints.
type treeRoute string

const (
	treeRouteProject treeRoute = "project"
	treeRouteAll     treeRoute = "cross-project"
)

// callTreeRaw calls a tree handler as the given reader — user, stored role
// and the external flag, all taken from the request context exactly as the
// transports stash them — and returns the status and the response body AS
// WRITTEN. Row 9 compares bodies byte for byte, so the decoded maps of
// callProjectTree / callAllTree are not enough; rows 12, 13, 18–20 and 22
// need readers other than the env's owner, including an external admin that
// exists only in the context (CHECK 000033). The project route uses
// env.projectID.
func callTreeRaw(t *testing.T, env listTestEnv, route treeRoute, userID uuid.UUID, role string, isExternal bool, query string) (int, []byte) {
	t.Helper()
	url := "/api/v1/issues/tree"
	if route == treeRouteProject {
		url = "/api/v1/projects/" + env.projectID.String() + "/issues/tree"
	}
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	ctx := req.Context()
	if route == treeRouteProject {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("projectID", env.projectID.String())
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	}
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUserRole(ctx, role)
	ctx = auth.WithIsExternal(ctx, isExternal)
	// As the production router does (authz.RequestCache).
	ctx = authz.WithRequestCache(ctx)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	if route == treeRouteProject {
		env.handler.Tree(rec, req)
	} else {
		NewIssuesAllHandler(env.pool, issues.NewStore(env.pool), nil).Tree(rec, req)
	}
	return rec.Code, append([]byte(nil), rec.Body.Bytes()...)
}

// treeWireRow is the part of a tree row row 9 reads: the flag, and whether
// the blockers KEY is there at all (a nil RawMessage means absent).
type treeWireRow struct {
	Item struct {
		ID        string `json:"id"`
		IsBlocked bool   `json:"isBlocked"`
	} `json:"item"`
	Blockers *json.RawMessage `json:"blockers"`
}

func decodeTreeRows(t *testing.T, where string, body []byte) []treeWireRow {
	t.Helper()
	var parsed struct {
		Items []treeWireRow `json:"items"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("%s: body does not parse: %v (%s)", where, err, body)
	}
	return parsed.Items
}

// Row 9 — without the flag the response is as before (US-4 acceptance 1, SC-009, D-2).
// A fixture with blocked nodes: the root and its child are blocked
// by a third issue. On both routes the bodies with the parameter absent,
// `includeBlockers=` and `includeBlockers=false` are byte-for-byte equal and carry no
// blockers key on any node; the isBlocked mark of each node without the flag
// equals the mark with the flag — the predicate has not changed.
func TestTreeBlockersHTTP_WithoutFlagResponseIsUnchanged(t *testing.T) {
	env := newListEnv(t)
	root, child, blocker := env.prefix+"-1", env.prefix+"-2", env.prefix+"-3"
	for _, id := range []string{root, child, blocker} {
		seedListIssue(t, env.pool, env.projectID, env.owner, id, "human", "", nil)
	}
	treeSeedDep(t, env.pool, child, root, "parent-child", env.owner)
	treeSeedDep(t, env.pool, root, blocker, "blocks", env.owner)
	treeSeedDep(t, env.pool, child, blocker, "blocks", env.owner)

	for _, route := range []treeRoute{treeRouteProject, treeRouteAll} {
		t.Run(string(route), func(t *testing.T) {
			status, absent := callTreeRaw(t, env, route, env.owner, "user", false, "")
			if status != http.StatusOK {
				t.Fatalf("without the parameter = %d, want 200 (%s)", status, absent)
			}

			for _, q := range []string{"includeBlockers=", "includeBlockers=false"} {
				st, body := callTreeRaw(t, env, route, env.owner, "user", false, q)
				if st != http.StatusOK {
					t.Fatalf("%s = %d, want 200 (%s)", q, st, body)
				}
				if !bytes.Equal(body, absent) {
					t.Errorf("body with %s differs from the body without the parameter:\n  %s\n  %s", q, body, absent)
				}
			}

			plain := decodeTreeRows(t, "without the parameter", absent)
			blockedRows := 0
			for _, r := range plain {
				if r.Blockers != nil {
					t.Errorf("node %s without the flag carries the blockers key — the field is off by default (D-2)", r.Item.ID)
				}
				if r.Item.IsBlocked {
					blockedRows++
				}
			}
			if blockedRows == 0 {
				t.Fatalf("precondition: the fixture has no blocked nodes — comparing the mark proves nothing "+
					"(rows: %+v)", plain)
			}

			st, flagged := callTreeRaw(t, env, route, env.owner, "user", false, "includeBlockers=true")
			if st != http.StatusOK {
				t.Fatalf("includeBlockers=true = %d, want 200 (%s)", st, flagged)
			}
			withFlag := decodeTreeRows(t, "with the flag", flagged)
			if len(withFlag) != len(plain) {
				t.Fatalf("with the flag %d rows, without — %d", len(withFlag), len(plain))
			}
			for i := range plain {
				if withFlag[i].Item.ID != plain[i].Item.ID {
					t.Fatalf("row %d: with the flag %s, without — %s", i, withFlag[i].Item.ID, plain[i].Item.ID)
				}
				if withFlag[i].Item.IsBlocked != plain[i].Item.IsBlocked {
					t.Errorf("node %s: isBlocked without the flag %v, with the flag %v — the predicate changed",
						plain[i].Item.ID, plain[i].Item.IsBlocked, withFlag[i].Item.IsBlocked)
				}
			}
			if bytes.Equal(flagged, absent) {
				t.Errorf("body with includeBlockers=true matched the body without the parameter — the flag did not reach " +
					"the storage, and the equality above distinguishes nothing")
			}
		})
	}
}

// addTreeMember adds user to project as a plain member.
func addTreeMember(t *testing.T, pool *pgxpool.Pool, projectID, user, addedBy uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, projectID, user, addedBy); err != nil {
		t.Fatalf("add member %s to %s: %v", user, projectID, err)
	}
}

// decodeTreeItems returns the rows of a tree body as raw JSON objects —
// KEY presence is what the forms of US-4 are about.
func decodeTreeItems(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var parsed struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body does not parse: %v (%s)", err, body)
	}
	return parsed.Items
}

// treeRowByItemID returns the FIRST row whose item.id is id.
func treeRowByItemID(t *testing.T, items []map[string]any, id string) map[string]any {
	t.Helper()
	for _, row := range items {
		if item, _ := row["item"].(map[string]any); item != nil && item["id"] == id {
			return row
		}
	}
	t.Fatalf("no row with item.id=%s in the response: %v", id, items)
	return nil
}

func joinedKeys(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// Row 12 — the horizon of the blocker entries is the READER's horizon, in both
// variants of the endpoint (US-4, US-2, INV-1, SC-003 internal). An issue in P
// is blocked by issues from P, Q and R; the reader is an internal member of P and R,
// not a member of Q. Project R is needed precisely for the project variant: there
// args.VisibleProjects=[P], and classification by it would hide R — without R in the
// fixture this error is not visible. Asymmetry-edge: a blocker from R is itself a node
// in the project tree of P — a stub node, the entry about it is full.
func TestTreeBlockersHTTP_EntriesUseReaderHorizonOnBothRoutes(t *testing.T) {
	pool := newListTestPool(t)
	pOwner, _ := seedListUser(t, pool, "user")
	qOwner, _ := seedListUser(t, pool, "user")
	rOwner, _ := seedListUser(t, pool, "user")
	pID, pPrefix := seedListProject(t, pool, pOwner, "tbp")
	qID, qPrefix := seedListProject(t, pool, qOwner, "tbq")
	rID, rPrefix := seedListProject(t, pool, rOwner, "tbr")

	readerPR := pOwner // member of P (owner) and R, not a member of Q
	addTreeMember(t, pool, rID, readerPR, rOwner)
	memberAll, _ := seedListUser(t, pool, "user")
	addTreeMember(t, pool, pID, memberAll, pOwner)
	addTreeMember(t, pool, qID, memberAll, qOwner)
	addTreeMember(t, pool, rID, memberAll, rOwner)
	admin, _ := seedListUser(t, pool, "admin")

	subject := pPrefix + "-1"
	pBlocker, qBlocker, rBlocker := pPrefix+"-2", qPrefix+"-1", rPrefix+"-1"
	seedListIssue(t, pool, pID, pOwner, subject, "human", "", nil)
	seedListIssue(t, pool, pID, pOwner, pBlocker, "human", "", nil)
	seedListIssue(t, pool, qID, qOwner, qBlocker, "human", "", nil)
	seedListIssue(t, pool, rID, rOwner, rBlocker, "human", "", nil)
	for _, b := range []string{pBlocker, qBlocker, rBlocker} {
		treeSeedDep(t, pool, subject, b, dependencies.DepBlocks, pOwner)
	}
	// Asymmetry-edge: a blocker from R is also a child of the subject, a tree node.
	treeSeedDep(t, pool, rBlocker, subject, dependencies.DepParentChild, pOwner)

	env := listTestEnv{pool: pool, handler: NewIssuesHandler(pool,
		projects.NewStore(pool), dependencies.NewStore(pool)),
		owner: pOwner, projectID: pID, prefix: pPrefix}
	projectOf := map[string]uuid.UUID{pBlocker: pID, qBlocker: qID, rBlocker: rID}

	readers := []struct {
		name  string
		user  uuid.UUID
		role  string
		seesQ bool
	}{
		{"internal member of P and R, not a member of Q", readerPR, "user", false},
		{"member of P, Q and R", memberAll, "user", true},
		{"internal administrator", admin, "admin", true},
	}
	query := "rootId=" + subject + "&includeBlockers=true"

	for _, rd := range readers {
		for _, route := range []treeRoute{treeRouteAll, treeRouteProject} {
			t.Run(rd.name+"/"+string(route), func(t *testing.T) {
				status, body := callTreeRaw(t, env, route, rd.user, rd.role, false, query)
				if status != http.StatusOK {
					t.Fatalf("code %d, body %s", status, body)
				}
				items := decodeTreeItems(t, body)
				root := treeRowByItemID(t, items, subject)
				if item, _ := root["item"].(map[string]any); item["isBlocked"] != true {
					t.Errorf("subject: isBlocked = %v, want true", item["isBlocked"])
				}
				entries, ok := root["blockers"].([]any)
				if !ok {
					t.Fatalf("the subject has no blockers array: %v", root)
				}

				// sk-423: a blocker outside the reader's horizon is not an entry but a count.
				hiddenQ := !rd.seesQ
				var want []string
				for _, id := range []string{pBlocker, qBlocker, rBlocker} {
					if id == qBlocker && hiddenQ {
						continue
					}
					want = append(want, id)
				}
				sort.Strings(want)
				links, linksPresent := root["unavailableBlockingLinks"]
				if hiddenQ && links != float64(1) {
					t.Errorf("unavailableBlockingLinks = %v; want 1 — an edge into Q outside the reader's horizon", links)
				}
				if !hiddenQ && linksPresent {
					t.Errorf("unavailableBlockingLinks = %v for a reader who sees Q; there must be no key", links)
				}
				if len(entries) != len(want) {
					t.Fatalf("entries %d, want %d: %v", len(entries), len(want), entries)
				}
				for i, raw := range entries {
					e, _ := raw.(map[string]any)
					id := want[i]
					if keys := joinedKeys(e); keys != "id,projectId,status,title" {
						t.Errorf("entry %d (%s): keys %s; want id,projectId,status,title", i, id, keys)
					}
					if e["id"] != id || e["title"] != id || e["status"] != "open" || e["projectId"] != projectOf[id].String() {
						t.Errorf("entry %d = %v; want the full entry %s with projectId %s — the horizon of entries is "+
							"the reader's horizon, not the request project", i, e, id, projectOf[id])
					}
				}

				// Asymmetry-edge: in the project tree of P the node from R is a stub for
				// a non-administrator, while the entry about it (checked above) is full.
				// Since sk-674.18 T009 the stub's id is synthetic for every
				// reader: it is found by its form, and R's real id must be
				// nowhere among the nodes.
				if route == treeRouteProject && rd.role != "admin" {
					var stub map[string]any
					for _, row := range items {
						item, _ := row["item"].(map[string]any)
						if item["id"] == rBlocker {
							t.Errorf("node %s in the project tree of P carries the real id: %v", rBlocker, row)
						}
						if item["title"] == "<hidden>" {
							stub = row
						}
					}
					if stub == nil || stub["isReference"] != true {
						t.Errorf("no stub for node %s in the project tree of P (project variant rule), "+
							"with a full entry about it: %v", rBlocker, items)
					}
				}
			})
		}
	}
}

// Row 13 — an external employee and an external "administrator": a blocker from Q is not
// an entry but a count (sk-423), and the real id of the Q issue goes out in no field that carries
// identifiers (SC-003, SC-002 server, US-4 acceptance 4, US-2 acceptance 4,
// INV-1). An external administrator cannot be created in the DB (CHECK 000033), so
// the admin role and the external mark of both readers exist only in the request
// context, as the transports put them.
//
// Fixture: subject S in P; issue X in Q is both a blocker of S and a child of S,
// i.e. a stub node; issue Y in P is a child of X, through which parentRef and
// parentIdCurrent of the row under the stub go. The hierarchy is written the same way the
// production path writes it: a parent-child edge in dependencies (sk-35: the column
// issues.parent_id is frozen, and item.parentIdCurrent is derived from the edge).
//
// Reference row: Y has a second parent V (S → W → V → Y, depth 3), so the
// canonical occurrence of Y is under X at depth 2, and under V Y comes out as a reference
// row with firstSeenAt = the parent of the canonical occurrence, i.e. X. The visible
// blocker B gives blockers[].id: for an external reader the hidden entry carries no id.
//
// Checking five fields is pointless if a field was not encountered: the test requires
// EACH of item.id, parentRef.id, item.parentIdCurrent, firstSeenAt,
// blockers[].id to appear in the response at least once (review skein-api/4).
func TestTreeBlockersHTTP_ExternalReadersGetNoHiddenIDs(t *testing.T) {
	pool := newListTestPool(t)
	pOwner, _ := seedListUser(t, pool, "user")
	qOwner, _ := seedListUser(t, pool, "user")
	pID, pPrefix := seedListProject(t, pool, pOwner, "tbp")
	qID, qPrefix := seedListProject(t, pool, qOwner, "tbq")

	external, _ := seedListUser(t, pool, "user")
	addTreeMember(t, pool, pID, external, pOwner)
	externalAdmin, _ := seedListUser(t, pool, "user")
	addTreeMember(t, pool, pID, externalAdmin, pOwner)

	subject, childOfHidden := pPrefix+"-1", pPrefix+"-2"
	middleW, middleV, visibleBlocker := pPrefix+"-3", pPrefix+"-4", pPrefix+"-5"
	hiddenX := qPrefix + "-1"
	seedListIssue(t, pool, pID, pOwner, subject, "human", "", nil)
	seedListIssue(t, pool, qID, qOwner, hiddenX, "human", "", nil)
	seedListIssue(t, pool, pID, pOwner, childOfHidden, "human", "", nil)
	seedListIssue(t, pool, pID, pOwner, middleW, "human", "", nil)
	seedListIssue(t, pool, pID, pOwner, middleV, "human", "", nil)
	seedListIssue(t, pool, pID, pOwner, visibleBlocker, "human", "", nil)
	treeSeedDep(t, pool, subject, hiddenX, dependencies.DepBlocks, pOwner)
	treeSeedDep(t, pool, subject, visibleBlocker, dependencies.DepBlocks, pOwner)
	treeSeedDep(t, pool, hiddenX, subject, dependencies.DepParentChild, pOwner)
	// The first edge Y → X: parentIdCurrent of Y is the earliest parent-child edge.
	treeSeedDep(t, pool, childOfHidden, hiddenX, dependencies.DepParentChild, pOwner)
	treeSeedDep(t, pool, middleW, subject, dependencies.DepParentChild, pOwner)
	treeSeedDep(t, pool, middleV, middleW, dependencies.DepParentChild, pOwner)
	// The second parent of Y is deeper than the first — under V, Y comes out as a reference row.
	if _, err := pool.Exec(t.Context(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'parent-child', '{}'::jsonb, 1, $3, now() + interval '1 second')`,
		childOfHidden, middleV, pOwner); err != nil {
		t.Fatalf("seed second parent %s -> %s: %v", childOfHidden, middleV, err)
	}

	env := listTestEnv{pool: pool, handler: NewIssuesHandler(pool,
		projects.NewStore(pool), dependencies.NewStore(pool)),
		owner: pOwner, projectID: pID, prefix: pPrefix}

	readers := []struct {
		name string
		user uuid.UUID
		role string
	}{
		{"external member of P", external, "user"},
		{"external with the admin role (context only), member of P", externalAdmin, "admin"},
	}
	stubIDOf := func(t *testing.T, items []map[string]any) string {
		t.Helper()
		for _, row := range items {
			if item, _ := row["item"].(map[string]any); item != nil && item["title"] == "<hidden>" {
				id, _ := item["id"].(string)
				return id
			}
		}
		t.Fatalf("no stub row in the response: %v", items)
		return ""
	}

	for _, rd := range readers {
		for _, route := range []treeRoute{treeRouteAll, treeRouteProject} {
			t.Run(rd.name+"/"+string(route), func(t *testing.T) {
				status, body := callTreeRaw(t, env, route, rd.user, rd.role, true,
					"rootId="+subject+"&includeBlockers=true")
				if status != http.StatusOK {
					t.Fatalf("code %d, body %s", status, body)
				}
				items := decodeTreeItems(t, body)

				root := treeRowByItemID(t, items, subject)
				// sk-423: X is outside the horizon — not an entry but the unavailableBlockingLinks count.
				entries, ok := root["blockers"].([]any)
				if !ok || len(entries) != 1 {
					t.Fatalf("expected one blocker entry for the subject (B visible; X — count only), got %v", root["blockers"])
				}
				if e, _ := entries[0].(map[string]any); e["id"] != visibleBlocker || joinedKeys(e) != "id,projectId,status,title" {
					t.Errorf("entry = %v; want the full entry of the visible blocker %s", e, visibleBlocker)
				}
				if root["unavailableBlockingLinks"] != float64(1) {
					t.Errorf("unavailableBlockingLinks = %v; want 1 — edge S → X", root["unavailableBlockingLinks"])
				}

				// All response fields that carry identifiers. Titles of visible
				// issues are outside the check, as in the spec.
				const (
					fItemID          = "item.id"
					fParentRefID     = "parentRef.id"
					fParentIDCurrent = "item.parentIdCurrent"
					fFirstSeenAt     = "firstSeenAt"
					fBlockerID       = "blockers[].id"
				)
				var fields []string
				seen := map[string]int{}
				add := func(name, value string) {
					fields = append(fields, name+"="+value)
					seen[name]++
				}
				for _, row := range items {
					item, _ := row["item"].(map[string]any)
					if id, ok := item["id"].(string); ok {
						add(fItemID, id)
					}
					if p, ok := item["parentIdCurrent"].(string); ok {
						add(fParentIDCurrent, p)
					}
					if pr, ok := row["parentRef"].(map[string]any); ok {
						if id, ok := pr["id"].(string); ok {
							add(fParentRefID, id)
						}
					}
					if f, ok := row["firstSeenAt"].(string); ok {
						add(fFirstSeenAt, f)
					}
					if bl, ok := row["blockers"].([]any); ok {
						for _, raw := range bl {
							if e, _ := raw.(map[string]any); e != nil {
								if id, ok := e["id"].(string); ok {
									add(fBlockerID, id)
								}
							}
						}
					}
				}
				for _, name := range []string{fItemID, fParentRefID, fParentIDCurrent, fFirstSeenAt, fBlockerID} {
					if seen[name] == 0 {
						t.Errorf("field %s never appeared in the response — the leak check on it would pass "+
							"always; the fixture must fill it", name)
					}
				}
				for _, f := range fields {
					if strings.Contains(f, qPrefix) {
						t.Errorf("leak: %s carries the prefix of project Q %q (issue id %s)", f, qPrefix, hiddenX)
					}
				}

				stubID := stubIDOf(t, items)
				if !strings.HasPrefix(stubID, "hidden-") {
					t.Errorf("stub id = %q; want a synthetic hidden-*", stubID)
				}
				// The canonical occurrence of Y is under the stub: parentRef and
				// parentIdCurrent lead to the synthetic id.
				var canonicalY, referenceY map[string]any
				for _, row := range items {
					if item, _ := row["item"].(map[string]any); item != nil && item["id"] == childOfHidden {
						if row["isReference"] == true {
							referenceY = row
						} else {
							canonicalY = row
						}
					}
				}
				if canonicalY == nil || referenceY == nil {
					t.Fatalf("expected canonical and reference occurrences of %s (canon=%v, ref=%v)",
						childOfHidden, canonicalY != nil, referenceY != nil)
				}
				if pr, _ := canonicalY["parentRef"].(map[string]any); pr == nil || pr["id"] != stubID {
					t.Errorf("parentRef of the stub's child = %v; want synthetic %q", canonicalY["parentRef"], stubID)
				}
				if item, _ := canonicalY["item"].(map[string]any); item["parentIdCurrent"] != stubID {
					t.Errorf("parentIdCurrent of the stub's child = %v; want synthetic %q", item["parentIdCurrent"], stubID)
				}
				if referenceY["firstSeenAt"] != stubID {
					t.Errorf("firstSeenAt of reference row %s = %v; want synthetic %q (the parent of the canonical "+
						"occurrence is the stub)", childOfHidden, referenceY["firstSeenAt"], stubID)
				}

				// The synthetic id of the stub does not change with the flag.
				st, plainBody := callTreeRaw(t, env, route, rd.user, rd.role, true, "rootId="+subject)
				if st != http.StatusOK {
					t.Fatalf("without the flag: code %d, body %s", st, plainBody)
				}
				if plainStub := stubIDOf(t, decodeTreeItems(t, plainBody)); plainStub != stubID {
					t.Errorf("stub id without the flag %q, with the flag %q — the flag must not change it", plainStub, stubID)
				}
			})
		}
	}
}

// Row 18 — where the field exists, without races (SC-004, INV-2 static part,
// US-4 acceptances). One epic fixture E in a response with the flag; traversal along
// parent-child and blocks, so that the response has reference rows.
//
//   - C1 closed, C2 permanent — both with an unclosed blocker B → the field exists;
//   - C3 — its only blocker D is closed before the request → neither the mark nor the field;
//   - C4 — blocks and waits-for to the same issue B → two entries;
//   - C5 — the blockers are the epic root E itself and the neighbouring child C4 → ordinary entries.
//     The blocks edge is written C5 → C4 (from — the blocked issue, to —
//     the blocker); traversal goes along outgoing blocks edges, so C4 comes out under
//     C5 once more — as a reference row (the canonical occurrence of C4 is under E).
//
// For EVERY visible non-reference node isBlocked ⇔ blockers exists and is
// non-empty; a reference row carries no field, and its isBlocked equals the canonical one's.
func TestTreeBlockersHTTP_FieldPresentExactlyOnBlockedVisibleRows(t *testing.T) {
	env := newListEnv(t)
	id := func(n int) string { return env.prefix + "-" + strconv.Itoa(n) }
	epic, c1, c2, c3, c4, c5, b, d := id(1), id(2), id(3), id(4), id(5), id(6), id(10), id(11)
	for _, x := range []string{epic, c1, c2, c3, c4, c5, b, d} {
		seedListIssue(t, env.pool, env.projectID, env.owner, x, "human", "", nil)
	}
	for _, child := range []string{c1, c2, c3, c4, c5, b} {
		treeSeedDep(t, env.pool, child, epic, dependencies.DepParentChild, env.owner)
	}
	setMeIssueStatus(t, env.pool, c1, "closed")
	setMeIssueStatus(t, env.pool, c2, "standing")
	setMeIssueStatus(t, env.pool, d, "closed")

	treeSeedDep(t, env.pool, c1, b, dependencies.DepBlocks, env.owner)
	treeSeedDep(t, env.pool, c2, b, dependencies.DepBlocks, env.owner)
	treeSeedDep(t, env.pool, c3, d, dependencies.DepBlocks, env.owner)
	treeSeedDep(t, env.pool, c4, b, dependencies.DepBlocks, env.owner)
	if _, err := env.pool.Exec(t.Context(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'waits-for', '{"gate":"all-children"}'::jsonb, 1, $3, now())`, c4, b, env.owner); err != nil {
		t.Fatalf("seed waits-for %s -> %s: %v", c4, b, err)
	}
	treeSeedDep(t, env.pool, c5, epic, dependencies.DepBlocks, env.owner)
	treeSeedDep(t, env.pool, c5, c4, dependencies.DepBlocks, env.owner)

	query := "rootId=" + epic + "&types=parent-child&types=blocks&includeBlockers=true"
	for _, route := range []treeRoute{treeRouteAll, treeRouteProject} {
		t.Run(string(route), func(t *testing.T) {
			status, body := callTreeRaw(t, env, route, env.owner, "user", false, query)
			if status != http.StatusOK {
				t.Fatalf("code %d, body %s", status, body)
			}
			items := decodeTreeItems(t, body)

			canonical := map[string]map[string]any{}
			var references []map[string]any
			for _, row := range items {
				item, _ := row["item"].(map[string]any)
				rid, _ := item["id"].(string)
				if row["isReference"] == true {
					references = append(references, row)
					continue
				}
				canonical[rid] = row
			}
			entryIDs := func(rid string) []string {
				row, ok := canonical[rid]
				if !ok {
					t.Fatalf("canonical row %s is not in the response", rid)
				}
				bl, _ := row["blockers"].([]any)
				out := make([]string, 0, len(bl))
				for _, raw := range bl {
					e, _ := raw.(map[string]any)
					if keys := joinedKeys(e); keys != "id,projectId,status,title" {
						t.Errorf("%s: entry %v with keys %s — all fixture blockers are visible, entries are full", rid, e, keys)
					}
					if e["projectId"] != env.projectID.String() {
						t.Errorf("%s: projectId of entry %v; want the blocker project %s", rid, e["projectId"], env.projectID)
					}
					s, _ := e["id"].(string)
					out = append(out, s)
				}
				return out
			}
			isBlocked := func(row map[string]any) bool {
				item, _ := row["item"].(map[string]any)
				return item["isBlocked"] == true
			}

			// Every visible non-reference node: isBlocked ⇔ blockers exists and is non-empty.
			for rid, row := range canonical {
				bl, present := row["blockers"]
				arr, _ := bl.([]any)
				if present && len(arr) == 0 {
					t.Errorf("%s: blockers key exists but the list is empty", rid)
				}
				if isBlocked(row) != (present && len(arr) > 0) {
					t.Errorf("%s: isBlocked=%v, blockers=%v — the mark and the field diverged (INV-2)", rid, isBlocked(row), bl)
				}
			}

			for _, rid := range []string{c1, c2} {
				if got := entryIDs(rid); len(got) != 1 || got[0] != b {
					t.Errorf("%s (closed/permanent node with an unclosed blocker): blockers=%v, want [%s]", rid, got, b)
				}
			}
			if row := canonical[c3]; isBlocked(row) || row["blockers"] != nil {
				t.Errorf("%s: the only blocker is closed before the request — neither mark nor field; got %v", c3, row)
			}
			if got := entryIDs(c4); len(got) != 2 || got[0] != b || got[1] != b {
				t.Errorf("%s: blocks and waits-for to %s — two entries; got %v", c4, b, got)
			}
			got5 := entryIDs(c5)
			sort.Strings(got5)
			want5 := []string{c4, epic}
			sort.Strings(want5)
			if strings.Join(got5, ",") != strings.Join(want5, ",") {
				t.Errorf("%s: the blockers are the epic root and the neighbouring child — ordinary entries %v; got %v", c5, want5, got5)
			}

			blockedRef := false
			for _, ref := range references {
				item, _ := ref["item"].(map[string]any)
				rid, _ := item["id"].(string)
				if _, present := ref["blockers"]; present {
					t.Errorf("reference row %s carries blockers — the field only on the canonical occurrence", rid)
				}
				if canon, ok := canonical[rid]; ok {
					if isBlocked(ref) != isBlocked(canon) {
						t.Errorf("reference row %s: isBlocked=%v, canonical %v", rid, isBlocked(ref), isBlocked(canon))
					}
					if isBlocked(canon) {
						blockedRef = true
					}
				}
			}
			if !blockedRef {
				t.Fatalf("precondition: no reference row of a blocked issue in the response — the check of reference "+
					"rows proves nothing (reference rows: %d)", len(references))
			}
		})
	}
}

// treeHookTracer — a pgx.QueryTracer that ACTS between the statements of the
// handler: at the start and at the end of a statement it calls a hook given by the test with
// the connection and the query text (rows 19 and 20). The limit of the sqlCapture
// sample (review_unreviewed_index_plan_test.go): it only collects texts. The query
// text is carried from TraceQueryStart to TraceQueryEnd by the context — pgx
// passes to TraceQueryEnd the context returned by TraceQueryStart.
type treeHookTracer struct {
	mu      sync.Mutex
	onStart func(conn *pgx.Conn, sql string)
	onEnd   func(conn *pgx.Conn, sql string)
}

type treeHookSQLKey struct{}

func (tr *treeHookTracer) set(onStart, onEnd func(conn *pgx.Conn, sql string)) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.onStart, tr.onEnd = onStart, onEnd
}

func (tr *treeHookTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tr.mu.Lock()
	hook := tr.onStart
	tr.mu.Unlock()
	if hook != nil {
		hook(conn, data.SQL)
	}
	return context.WithValue(ctx, treeHookSQLKey{}, data.SQL)
}

func (tr *treeHookTracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, _ pgx.TraceQueryEndData) {
	tr.mu.Lock()
	hook := tr.onEnd
	tr.mu.Unlock()
	if hook == nil {
		return
	}
	sql, _ := ctx.Value(treeHookSQLKey{}).(string)
	hook(conn, sql)
}

// newTreeHookPool — a pool with the hook tracer for the handler; fixture data
// is seeded and changed by a SECOND pool.
func newTreeHookPool(t *testing.T, tracer *treeHookTracer) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.Tracer = tracer
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Row 19 — the mark and the node list from ONE graph state, with a race
// (INV-2, SC-004, FOR-PLAN M-3/N5). At the end of the row-assembly statement
// (summariesForIDs, recognised by "AS is_blocked") the second pool closes
// the only blocker of node C; mirrored — a blocker is added in the same place.
// Then for C isBlocked ⇔ blockers is non-empty. An implementation with a transaction snapshot
// is green too — that is allowed by INV-2. No time.Sleep: the action stands
// exactly between the two reads.
func TestTreeBlockersHTTP_FlagAndListFromOneStateUnderRace(t *testing.T) {
	seedPool := newListTestPool(t)
	owner, _ := seedListUser(t, seedPool, "user")
	pid, prefix := seedListProject(t, seedPool, owner, "tbr")

	tracer := &treeHookTracer{}
	pool := newTreeHookPool(t, tracer)
	env := listTestEnv{pool: pool, handler: NewIssuesHandler(pool,
		projects.NewStore(pool), dependencies.NewStore(pool)),
		owner: owner, projectID: pid, prefix: prefix}

	runRace := func(t *testing.T, node string, mutate func()) {
		t.Helper()
		var once sync.Once
		fired := false
		tracer.set(nil, func(_ *pgx.Conn, sql string) {
			if strings.Contains(sql, "AS is_blocked") {
				once.Do(func() {
					mutate()
					fired = true
				})
			}
		})
		status, body := callTreeRaw(t, env, treeRouteProject, owner, "user", false,
			"rootId="+node+"&includeBlockers=true")
		tracer.set(nil, nil)
		if !fired {
			t.Fatalf("hook did not fire: the row-assembly statement with \"AS is_blocked\" was not met — there was no race")
		}
		if status != http.StatusOK {
			t.Fatalf("code %d, body %s", status, body)
		}
		row := treeRowByItemID(t, decodeTreeItems(t, body), node)
		item, _ := row["item"].(map[string]any)
		bl, present := row["blockers"]
		arr, _ := bl.([]any)
		if (item["isBlocked"] == true) != (present && len(arr) > 0) {
			t.Errorf("node %s: isBlocked=%v, blockers=%v — the mark and the list come from different graph states (INV-2)",
				node, item["isBlocked"], bl)
		}
	}

	t.Run("blocker closes between reads", func(t *testing.T) {
		c, b := prefix+"-1", prefix+"-2"
		seedListIssue(t, seedPool, pid, owner, c, "human", "", nil)
		seedListIssue(t, seedPool, pid, owner, b, "human", "", nil)
		treeSeedDep(t, seedPool, c, b, dependencies.DepBlocks, owner)
		runRace(t, c, func() { setMeIssueStatus(t, seedPool, b, "closed") })
	})

	t.Run("blocker is added between reads", func(t *testing.T) {
		c, b := prefix+"-3", prefix+"-4"
		seedListIssue(t, seedPool, pid, owner, c, "human", "", nil)
		seedListIssue(t, seedPool, pid, owner, b, "human", "", nil)
		runRace(t, c, func() { treeSeedDep(t, seedPool, c, b, dependencies.DepBlocks, owner) })
	})
}

// Row 20 — a failure of blockers assembly is a failure of the whole response (edge "assembly
// failure", INV-2 "no partial response", Observability). At the start of the
// batch query statement — recognised by the shared label constant
// issues.TreeBlockersBatchLabel, the same one T006 reads — the second pool
// calls pg_terminate_backend(<connection PID>, <timeout>). The form with a
// timeout waits for the backend to actually terminate, so the statement fails
// deterministically. The response is not 200, there are no nodes in the body.
func TestTreeBlockersHTTP_BatchFailureFailsWholeResponse(t *testing.T) {
	seedPool := newListTestPool(t)
	owner, _ := seedListUser(t, seedPool, "user")
	pid, prefix := seedListProject(t, seedPool, owner, "tbf")
	node, blocker := prefix+"-1", prefix+"-2"
	seedListIssue(t, seedPool, pid, owner, node, "human", "", nil)
	seedListIssue(t, seedPool, pid, owner, blocker, "human", "", nil)
	treeSeedDep(t, seedPool, node, blocker, dependencies.DepBlocks, owner)

	tracer := &treeHookTracer{}
	pool := newTreeHookPool(t, tracer)
	env := listTestEnv{pool: pool, handler: NewIssuesHandler(pool,
		projects.NewStore(pool), dependencies.NewStore(pool)),
		owner: owner, projectID: pid, prefix: prefix}

	for _, route := range []treeRoute{treeRouteProject, treeRouteAll} {
		t.Run(string(route), func(t *testing.T) {
			var once sync.Once
			fired, terminated := false, false
			var termErr error
			tracer.set(func(conn *pgx.Conn, sql string) {
				if !strings.Contains(sql, issues.TreeBlockersBatchLabel) {
					return
				}
				once.Do(func() {
					fired = true
					termErr = seedPool.QueryRow(context.Background(),
						`SELECT pg_terminate_backend($1, 5000)`, int(conn.PgConn().PID())).Scan(&terminated)
				})
			}, nil)
			status, body := callTreeRaw(t, env, route, owner, "user", false,
				"rootId="+node+"&includeBlockers=true")
			tracer.set(nil, nil)

			if !fired {
				t.Fatalf("batch query statement with label %q was not met — the failure was not triggered",
					issues.TreeBlockersBatchLabel)
			}
			if termErr != nil || !terminated {
				t.Fatalf("pg_terminate_backend did not terminate the backend: terminated=%v err=%v", terminated, termErr)
			}
			if status == http.StatusOK {
				t.Errorf("response 200 on a blockers assembly failure — a partial response without blockers (INV-2): %s", body)
			}
			var parsed map[string]any
			if err := json.Unmarshal(body, &parsed); err == nil {
				if items, present := parsed["items"]; present {
					t.Errorf("failure body carries items = %v — there must be no nodes on failure", items)
				}
			}
		})
	}
}

// Row 22 — SC-011 / INV-7: one horizon classification for nodes and blocker
// entries, five readers. Cross-project variant. Issue X in Q is a direct
// child of epic E in P and a blocker of child C in P. For each reader: node X is
// a stub ⇔ there is no entry about X, and C has unavailableBlockingLinks = 1 (sk-423:
// a blocker outside the horizon is not an entry but a count). The stub level (real id
// or synthetic hidden-*) is no longer reflected on the entry. Nodes come from
// loadVisibleProjects, entries from ProjectVisibleExpr: two paths, and
// a divergence of either on one reader reddens that reader.
// The external administrator — by context only (CHECK 000033, as in row 13).
func TestTreeBlockersHTTP_OneHorizonForNodesAndEntries(t *testing.T) {
	pool := newListTestPool(t)
	pOwner, _ := seedListUser(t, pool, "user")
	qOwner, _ := seedListUser(t, pool, "user")
	pID, pPrefix := seedListProject(t, pool, pOwner, "tbp")
	qID, qPrefix := seedListProject(t, pool, qOwner, "tbq")

	memberBoth, _ := seedListUser(t, pool, "user")
	addTreeMember(t, pool, pID, memberBoth, pOwner)
	addTreeMember(t, pool, qID, memberBoth, qOwner)
	nonMemberQ := pOwner // internal member of P, not a member of Q
	admin, _ := seedListUser(t, pool, "admin")
	external, _ := seedListUser(t, pool, "user")
	addTreeMember(t, pool, pID, external, pOwner)
	externalAdmin, _ := seedListUser(t, pool, "user")
	addTreeMember(t, pool, pID, externalAdmin, pOwner)

	epic, child, x := pPrefix+"-1", pPrefix+"-2", qPrefix+"-1"
	seedListIssue(t, pool, pID, pOwner, epic, "human", "", nil)
	seedListIssue(t, pool, pID, pOwner, child, "human", "", nil)
	seedListIssue(t, pool, qID, qOwner, x, "human", "", nil)
	treeSeedDep(t, pool, child, epic, dependencies.DepParentChild, pOwner)
	treeSeedDep(t, pool, x, epic, dependencies.DepParentChild, pOwner)
	treeSeedDep(t, pool, child, x, dependencies.DepBlocks, pOwner)

	env := listTestEnv{pool: pool, handler: NewIssuesHandler(pool,
		projects.NewStore(pool), dependencies.NewStore(pool)),
		owner: pOwner, projectID: pID, prefix: pPrefix}

	type outcome string
	const (
		full      outcome = "node full, entry full"
		softStub  outcome = "stub with the real id, no entry, count 1"
		hardStub  outcome = "synthetic stub, no entry, count 1"
		unmatched outcome = "divergence"
	)
	readers := []struct {
		name       string
		user       uuid.UUID
		role       string
		isExternal bool
		want       outcome
	}{
		{"member of P and Q", memberBoth, "user", false, full},
		// sk-674.18 T009: the internal reader's stub is synthetic too — a
		// stub with the real id (softStub) is now a failure outcome.
		{"internal non-member of Q", nonMemberQ, "user", false, hardStub},
		{"internal administrator", admin, "admin", false, full},
		{"external member of P", external, "user", true, hardStub},
		{"external with the admin role (context only), member of P", externalAdmin, "admin", true, hardStub},
	}

	for _, rd := range readers {
		t.Run(rd.name, func(t *testing.T) {
			status, body := callTreeRaw(t, env, treeRouteAll, rd.user, rd.role, rd.isExternal,
				"rootId="+epic+"&includeBlockers=true")
			if status != http.StatusOK {
				t.Fatalf("code %d, body %s", status, body)
			}
			items := decodeTreeItems(t, body)

			// Node X is a child of E at depth 1, not C. Found by position: for an
			// external reader its id is synthetic.
			var xNode, cNode map[string]any
			for _, row := range items {
				if row["depth"] != float64(1) {
					continue
				}
				item, _ := row["item"].(map[string]any)
				if item["id"] == child {
					cNode = row
				} else {
					xNode = row
				}
			}
			if xNode == nil || cNode == nil {
				t.Fatalf("the response has not both children of the epic (C=%v, X=%v): %v", cNode != nil, xNode != nil, items)
			}
			xItem, _ := xNode["item"].(map[string]any)
			xID, _ := xItem["id"].(string)
			isStub := xItem["title"] == "<hidden>"

			entries, _ := cNode["blockers"].([]any)
			links := cNode["unavailableBlockingLinks"]
			isHidden := len(entries) == 0 && links == float64(1)
			isEntry := len(entries) == 1 && links == nil
			if !isHidden && !isEntry {
				t.Fatalf("for C expected either one entry about X, or none and count 1; got blockers=%v, "+
					"unavailableBlockingLinks=%v", cNode["blockers"], links)
			}
			var e map[string]any
			if isEntry {
				e, _ = entries[0].(map[string]any)
			}

			if isStub != isHidden {
				t.Errorf("node X stub=%v, X at C only as a count=%v — nodes and entries are classified by different "+
					"horizons (INV-7)", isStub, isHidden)
			}
			got := unmatched
			switch {
			case !isStub && isEntry && xID == x && joinedKeys(e) == "id,projectId,status,title" && e["id"] == x:
				got = full
			case isStub && isHidden && xID == x:
				got = softStub
			case isStub && isHidden && strings.HasPrefix(xID, "hidden-"):
				got = hardStub
			}
			if got != rd.want {
				t.Errorf("outcome %q, want %q (node X: id=%q stub=%v; entry %v)", got, rd.want, xID, isStub, e)
			}
		})
	}
}
