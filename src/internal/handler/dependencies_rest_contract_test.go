package handler

// dependencies_rest_contract_test.go — sk-282 T010, test-plan row 31.
//
// "INV-2: REST dependencies endpoint — key set, JSON types and
// representative values of the edge, including the fields the MCP
// projection strips (metadata, version, created stamp, nested project
// id): those are exactly the T010 leak vector."
//
// # Why this test exists at all, and why it is written from the leak side
//
// T010 removes four things from the MCP edge — the always-empty metadata
// map, the edge version, the created-at stamp and the nested project id.
// Every one of them is a field the REST dependencies endpoint returns and
// the web UI may read. The cheap way to implement T010 is to slim the
// SHARED struct, which would change the REST response and break INV-2 —
// "no REST response changes its field set, their types and values,
// INCLUDING additive key insertion".
//
// So the assertions here are deliberately concentrated on exactly those
// four fields, and they check TYPE as well as presence: a `version` that
// became a string, or a `metadata` that became null instead of `{}`, is
// the same broken contract as a missing key.
//
// The endpoint had no test before this one, which is precisely why the
// leak vector was open.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/testsupport"
)

// callDepsList invokes the REST dependencies List handler for one issue.
func callDepsList(
	t *testing.T,
	pool *pgxpool.Pool,
	caller uuid.UUID,
	role string,
	projectID uuid.UUID,
	issueID string,
) map[string]any {
	t.Helper()
	h := NewDependenciesHandler(pool, dependencies.NewStore(pool), issues.NewStore(pool))

	url := "/api/v1/projects/" + projectID.String() + "/issues/" + issueID + "/dependencies"
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", projectID.String())
	rctx.URLParams.Add("issueID", issueID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUserID(ctx, caller)
	ctx = auth.WithUserRole(ctx, role)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.List(rec, req)
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })

	if res.StatusCode != http.StatusOK {
		var problem map[string]any
		_ = json.NewDecoder(res.Body).Decode(&problem)
		t.Fatalf("GET dependencies = %d, want 200: %v", res.StatusCode, problem)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

// TestDependenciesREST_EdgeShapeUnchanged pins the REST edge.
func TestDependenciesREST_EdgeShapeUnchanged(t *testing.T) {
	pool := testsupport.NewTestPool(t)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "deprest")
	node := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1", "node", "open", 2)
	child := testsupport.SeedIssue(t, pool, pid, owner, prefix+"-1.1", "child of node", "closed", 1)
	testsupport.SeedDep(t, pool, child, node, dependencies.DepParentChild, owner)

	body := callDepsList(t, pool, owner, "user", pid, node)

	for _, dir := range []string{"outgoing", "incoming"} {
		if _, ok := body[dir].([]any); !ok {
			t.Fatalf("REST body has no %q array: %#v", dir, body[dir])
		}
	}
	incoming := body["incoming"].([]any)
	if len(incoming) != 1 {
		t.Fatalf("incoming = %d edges, want 1", len(incoming))
	}
	edge := incoming[0].(map[string]any)

	// The four fields T010 removes from the MCP edge. Each is asserted
	// present AND with its JSON type — a version that became a string is
	// the same broken contract as a missing one.
	t.Run("the fields the MCP projection removes are still here", func(t *testing.T) {
		meta, ok := edge["metadata"]
		if !ok {
			t.Error("metadata is gone from the REST edge — the MCP projection drops it, the " +
				"REST response may not")
		} else if _, isObj := meta.(map[string]any); !isObj {
			t.Errorf("metadata = %#v (%T), want a JSON object — it is `{}` on every edge and "+
				"turning it into null is a value change", meta, meta)
		}
		if v, ok := edge["version"]; !ok {
			t.Error("version is gone from the REST edge")
		} else if _, isNum := v.(float64); !isNum {
			t.Errorf("version = %#v (%T), want a JSON number", v, v)
		}
		if v, ok := edge["createdAt"]; !ok {
			t.Error("createdAt is gone from the REST edge")
		} else if s, isStr := v.(string); !isStr || s == "" {
			t.Errorf("createdAt = %#v (%T), want a non-empty RFC3339 string", v, v)
		}
	})

	t.Run("the nested card is still nested and still carries projectId", func(t *testing.T) {
		nested, ok := edge["issue"].(map[string]any)
		if !ok {
			t.Fatalf("issue card is gone or is not an object: %#v — the MCP projection "+
				"flattens it, the REST response may not", edge["issue"])
		}
		if got, ok := nested["projectId"].(string); !ok || got != pid.String() {
			t.Errorf("issue.projectId = %#v, want %q — this is the one card field the MCP "+
				"projection drops outright", nested["projectId"], pid.String())
		}
		// …with its own keys and types.
		wantCard := map[string]string{
			"id": "string", "projectId": "string", "title": "string",
			"status": "string", "type": "string",
		}
		for key, kind := range wantCard {
			v, ok := nested[key]
			if !ok {
				t.Errorf("issue card lost %q", key)
				continue
			}
			if kind == "string" {
				if _, isStr := v.(string); !isStr {
					t.Errorf("issue.%s = %#v (%T), want a string", key, v, v)
				}
			}
		}
		if _, isNum := nested["priority"].(float64); !isNum {
			t.Errorf("issue.priority = %#v (%T), want a JSON number", nested["priority"], nested["priority"])
		}
	})

	t.Run("representative values are what the store wrote", func(t *testing.T) {
		if edge["fromId"] != child {
			t.Errorf("fromId = %v, want %q", edge["fromId"], child)
		}
		if edge["toId"] != node {
			t.Errorf("toId = %v, want %q", edge["toId"], node)
		}
		if edge["type"] != dependencies.DepParentChild {
			t.Errorf("type = %v, want %q", edge["type"], dependencies.DepParentChild)
		}
		nested := edge["issue"].(map[string]any)
		if nested["title"] != "child of node" {
			t.Errorf("issue.title = %v, want %q", nested["title"], "child of node")
		}
		if nested["status"] != "closed" {
			t.Errorf("issue.status = %v, want %q", nested["status"], "closed")
		}
		if nested["priority"] != float64(1) {
			t.Errorf("issue.priority = %v, want 1", nested["priority"])
		}
	})

	t.Run("the key set is exactly this, in both directions", func(t *testing.T) {
		// INV-2 forbids ADDITIVE changes too: a depsCounts or a flattened
		// title leaking onto the REST edge is as much a contract break as
		// a removal. But an allowlist alone is only the upper half of
		// "field set" — it cannot see a key that VANISHED, which is
		// the other way a consumer breaks. So both directions are
		// checked: nothing outside the enumeration, and nothing
		// mandatory missing from it.
		for key, value := range edge {
			wantKind, known := restDepEdgeKeys[key]
			if !known {
				wantKind, known = restDepEdgeOptionalKeys[key]
			}
			if !known {
				t.Errorf("REST edge carries a new key %q — INV-2 covers additive changes: "+
					"\"no REST response changes its field set … including additive "+
					"key insertion\"", key)
				continue
			}
			// INV-2 is "field set, THEIR TYPES and values". The
			// enumeration has always declared the types; until now nothing
			// read them back, so `metadata` could turn from an object into
			// a string and this file would still be green.
			assertJSONKind(t, "REST edge", key, value, wantKind)
		}
		for key := range restDepEdgeKeys {
			if _, ok := edge[key]; !ok {
				t.Errorf("REST edge lost %q — INV-2 is \"does not change the field set\", not \"does not "+
					"grow\": a consumer reading this key now gets undefined", key)
			}
		}
		// The visible edge carries the card; `redacted` is omitempty and
		// legitimately absent here (its own test covers the other side).
		if _, ok := edge["issue"]; !ok {
			t.Error("REST edge lost \"issue\" — on a visible edge the nested card is not optional")
		}

		for key := range body {
			if !restDepListKeys[key] {
				t.Errorf("REST dependencies body carries a new top-level key %q", key)
			}
		}
		for key := range restDepListKeys {
			if _, ok := body[key]; !ok {
				t.Errorf("REST dependencies body lost the %q array", key)
			}
		}
	})
}

// restDepEdgeKeys / restDepEdgeOptionalKeys / restDepIssueRefKeys /
// restDepListKeys enumerate the REST dependency shapes. Hand-written for
// the same reason the issue-card enumeration is: a list derived from the
// struct it guards agrees with every change to that struct. The
// cross-check below keeps them honest without deriving them.
var restDepEdgeKeys = map[string]string{
	"fromId": "string", "toId": "string", "type": "string",
	"metadata": "object", "version": "number", "createdAt": "string",
}

var restDepEdgeOptionalKeys = map[string]string{
	"issue": "object", "redacted": "bool",
}

var restDepIssueRefKeys = map[string]string{
	"id": "string", "projectId": "string", "title": "string",
	"status": "string", "priority": "number", "type": "string",
}

var restDepListKeys = map[string]bool{"outgoing": true, "incoming": true}

// TestDependenciesREST_EnumerationStillDescribesTheDTOs is the ratchet
// under the enumerations.
//
// A response-level allowlist can only see the keys a fixture happens to
// produce: an OPTIONAL field deleted from the DTO simply stops appearing,
// which is exactly what "absent because omitempty" looks like, and the
// suite stays green while a consumer breaks. Comparing the hand-written
// enumeration against the struct's json tags is the only place that
// removal is visible, and it fails on a change to EITHER side — so the
// author has to state in the same commit that a REST field moved.
func TestDependenciesREST_EnumerationStillDescribesTheDTOs(t *testing.T) {
	cases := []struct {
		what      string
		typ       reflect.Type
		mandatory map[string]string
		optional  map[string]string
	}{
		{
			what:      "dependencies.DependencyEdge",
			typ:       reflect.TypeOf(dependencies.DependencyEdge{}),
			mandatory: restDepEdgeKeys,
			optional:  restDepEdgeOptionalKeys,
		},
		{
			what:      "dependencies.IssueRef",
			typ:       reflect.TypeOf(dependencies.IssueRef{}),
			mandatory: restDepIssueRefKeys,
			optional:  map[string]string{},
		},
		{
			what:      "dependencies.DependencyList",
			typ:       reflect.TypeOf(dependencies.DependencyList{}),
			mandatory: map[string]string{"outgoing": "array", "incoming": "array"},
			optional:  map[string]string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			mandatory, optional := jsonWireTags(tc.typ)
			if missing, extra := diffKeySets(mandatory, tc.mandatory); len(missing) > 0 || len(extra) > 0 {
				t.Errorf("%s: the always-serialised fields no longer match the enumeration — "+
					"struct has %v the enumeration does not; enumeration has %v the struct does "+
					"not. Adding one CHANGES the REST response; removing one BREAKS its "+
					"consumers. Either way say so here, in the same commit.",
					tc.what, missing, extra)
			}
			if missing, extra := diffKeySets(optional, tc.optional); len(missing) > 0 || len(extra) > 0 {
				t.Errorf("%s: the omitempty fields no longer match the enumeration — struct has "+
					"%v the enumeration does not; enumeration has %v the struct does not. An "+
					"optional field that vanishes is invisible to the response-level allowlist: "+
					"the fixture never populates it, so this is the only place it is caught.",
					tc.what, missing, extra)
			}
		})
	}
}

// TestDependenciesREST_RedactedEdgeShapeUnchanged pins the other side of
// the endpoint: a cross-project edge the caller cannot see still renders
// with the same envelope, the redaction flag, and no card.
func TestDependenciesREST_RedactedEdgeShapeUnchanged(t *testing.T) {
	pool := testsupport.NewTestPool(t)

	owner, _ := testsupport.SeedUser(t, pool, "user")
	viewer, _ := testsupport.SeedUser(t, pool, "user")
	visProj, visPrefix := testsupport.SeedProject(t, pool, owner, "deprestv")
	hidProj, hidPrefix := testsupport.SeedProject(t, pool, owner, "depresth")
	testsupport.AddProjectMember(t, pool, visProj, viewer, "member")

	node := testsupport.SeedIssue(t, pool, visProj, owner, visPrefix+"-1", "node", "open", 2)
	hidden := testsupport.SeedIssue(t, pool, hidProj, owner, hidPrefix+"-1", "secret", "open", 2)
	testsupport.SeedDep(t, pool, node, hidden, dependencies.DepRelated, owner)

	body := callDepsList(t, pool, viewer, "user", visProj, node)
	outgoing := body["outgoing"].([]any)
	if len(outgoing) != 1 {
		t.Fatalf("outgoing = %d edges, want 1", len(outgoing))
	}
	edge := outgoing[0].(map[string]any)

	if edge["redacted"] != true {
		t.Errorf("redacted = %v, want true", edge["redacted"])
	}
	if v, ok := edge["issue"]; ok && v != nil {
		t.Errorf("issue card present on a redacted REST edge: %#v", v)
	}
	// The envelope is unchanged even here — the fields the MCP projection
	// drops are still on the REST edge.
	for _, key := range []string{"fromId", "toId", "type", "metadata", "version", "createdAt"} {
		if _, ok := edge[key]; !ok {
			t.Errorf("redacted REST edge lost %q", key)
		}
	}
	// The hidden end is a synthetic token, never the real id (sk-674.18
	// T009).
	if edge["fromId"] != node || edge["toId"] != "hidden-1" {
		t.Errorf("redacted edge ids = %v/%v, want %q/%q (real hidden id %s must not appear)",
			edge["fromId"], edge["toId"], node, "hidden-1", hidden)
	}
}
