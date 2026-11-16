package redact_test

// sk-1007 US-2 — the registry of reference fields of event data, matrix
// rows No. 20–No. 25. Reference — spec R-1…R-3, INV-4, K5, written before
// the code: a hidden task id is the literal "<hidden>" (also inside the
// composite link id), a hidden end's project field is null with its key
// kept, lists keep their length, everything visible and every user-entered
// text is untouched, and a row without a hidden reference keeps its bytes.

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/redact"
)

var (
	projApp = uuid.MustParse("0a000000-0000-4000-8000-000000000001") // visible
	projOps = uuid.MustParse("0a000000-0000-4000-8000-000000000002") // visible, another project
	projSec = uuid.MustParse("0a000000-0000-4000-8000-000000000003") // hidden
)

// vis sees APP-12, APP-13, OPS-4 and the projects APP and OPS; SEC-7,
// SEC-9 and the project SEC are hidden.
var vis = redact.EventVisibility{
	Issues:   map[string]struct{}{"APP-12": {}, "APP-13": {}, "OPS-4": {}},
	Projects: map[uuid.UUID]struct{}{projApp: {}, projOps: {}},
}

// decoded reads a JSON column for a comparison by value (a rewritten object
// may order its keys differently).
func decoded(t *testing.T, b []byte) any {
	t.Helper()
	if b == nil {
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("not JSON: %s: %v", b, err)
	}
	return v
}

func sameJSON(t *testing.T, what string, got []byte, want string) {
	t.Helper()
	if want == "" {
		if got != nil {
			t.Errorf("%s = %s, want no value", what, got)
		}
		return
	}
	if g, w := decoded(t, got), decoded(t, []byte(want)); !reflect.DeepEqual(g, w) {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

// TestEventRefs_Apply — No. 20, No. 21, No. 22: the registry fields of each
// type with visible and hidden references at once.
func TestEventRefs_Apply(t *testing.T) {
	app, sec := projApp.String(), projSec.String()
	for _, tc := range []struct {
		name                           string
		in                             redact.EventRow
		entity, oldData, newData, payl string // expected; "" = no value
	}{
		{
			name: "No. 20 dependency.created, toId hidden",
			in: redact.EventRow{Type: "dependency.created", EntityID: "APP-12::SEC-7::blocks",
				NewData: []byte(`{"fromId":"APP-12","toId":"SEC-7","type":"blocks","metadata":{},"fromProjectId":"` + app + `","toProjectId":"` + sec + `"}`)},
			entity:  "APP-12::<hidden>::blocks",
			newData: `{"fromId":"APP-12","toId":"<hidden>","type":"blocks","metadata":{},"fromProjectId":"` + app + `","toProjectId":null}`,
		},
		{
			name: "No. 20 dependency.deleted, toId hidden",
			in: redact.EventRow{Type: "dependency.deleted", EntityID: "APP-12::SEC-7::blocks",
				OldData: []byte(`{"fromId":"APP-12","toId":"SEC-7","type":"blocks","fromProjectId":"` + app + `","toProjectId":"` + sec + `"}`)},
			entity:  "APP-12::<hidden>::blocks",
			oldData: `{"fromId":"APP-12","toId":"<hidden>","type":"blocks","fromProjectId":"` + app + `","toProjectId":null}`,
		},
		{
			name: "No. 20 dependency.created, fromId hidden (the end moved away)",
			in: redact.EventRow{Type: "dependency.created", EntityID: "SEC-9::APP-13::related",
				NewData: []byte(`{"fromId":"SEC-9","toId":"APP-13","type":"related","fromProjectId":"` + app + `","toProjectId":"` + app + `"}`)},
			entity:  "<hidden>::APP-13::related",
			newData: `{"fromId":"<hidden>","toId":"APP-13","type":"related","fromProjectId":null,"toProjectId":"` + app + `"}`,
		},
		{
			name:    "No. 21 issue.created, hidden parent",
			in:      redact.EventRow{Type: "issue.created", EntityID: "APP-14", NewData: []byte(`{"id":"APP-14","parentId":"SEC-7","title":"child"}`)},
			entity:  "APP-14",
			newData: `{"id":"APP-14","parentId":"<hidden>","title":"child"}`,
		},
		{
			name: "No. 22 issue.reparented, every column",
			in: redact.EventRow{Type: "issue.reparented", EntityID: "APP-14",
				OldData: []byte(`{"parentIds":["APP-12","SEC-7","SEC-9"]}`),
				NewData: []byte(`{"parentId":"SEC-9"}`),
				Payload: []byte(`{"oldParentIds":["APP-12","SEC-7","SEC-9"],"newParentId":"SEC-9"}`)},
			entity:  "APP-14",
			oldData: `{"parentIds":["APP-12","<hidden>","<hidden>"]}`,
			newData: `{"parentId":"<hidden>"}`,
			payl:    `{"oldParentIds":["APP-12","<hidden>","<hidden>"],"newParentId":"<hidden>"}`,
		},
		{
			name: "No. 20 dependency.created, hidden toId with no toProjectId",
			in: redact.EventRow{Type: "dependency.created", EntityID: "APP-12::SEC-7::blocks",
				NewData: []byte(`{"fromId":"APP-12","toId":"SEC-7","type":"blocks","fromProjectId":"` + app + `"}`)},
			entity:  "APP-12::<hidden>::blocks",
			newData: `{"fromId":"APP-12","toId":"<hidden>","type":"blocks","fromProjectId":"` + app + `"}`,
		},
		{
			name: "No. 20 dependency.created, hidden toId with a null toProjectId",
			in: redact.EventRow{Type: "dependency.created", EntityID: "APP-12::SEC-7::blocks",
				NewData: []byte(`{"fromId":"APP-12","toId":"SEC-7","type":"blocks","fromProjectId":"` + app + `","toProjectId":null}`)},
			entity:  "APP-12::<hidden>::blocks",
			newData: `{"fromId":"APP-12","toId":"<hidden>","type":"blocks","fromProjectId":"` + app + `","toProjectId":null}`,
		},
		{
			name: "No. 20 dependency.created, toId a number: not an id, hidden",
			in: redact.EventRow{Type: "dependency.created", EntityID: "APP-12::SEC-7::blocks",
				NewData: []byte(`{"fromId":"APP-12","toId":7,"type":"blocks","fromProjectId":"` + app + `","toProjectId":"` + app + `"}`)},
			entity:  "APP-12::<hidden>::blocks",
			newData: `{"fromId":"APP-12","toId":"<hidden>","type":"blocks","fromProjectId":"` + app + `","toProjectId":null}`,
		},
		{
			name: "No. 20 dependency.created, fromId an object: not an id, hidden",
			in: redact.EventRow{Type: "dependency.created", EntityID: "SEC-7::APP-13::blocks",
				NewData: []byte(`{"fromId":{"id":"SEC-7"},"toId":"APP-13","type":"blocks","fromProjectId":"` + app + `","toProjectId":"` + app + `"}`)},
			entity:  "<hidden>::APP-13::blocks",
			newData: `{"fromId":"<hidden>","toId":"APP-13","type":"blocks","fromProjectId":null,"toProjectId":"` + app + `"}`,
		},
		{
			name:    "No. 21 issue.created, parentId a number: not an id, hidden",
			in:      redact.EventRow{Type: "issue.created", EntityID: "APP-14", NewData: []byte(`{"id":"APP-14","parentId":7,"title":"child"}`)},
			entity:  "APP-14",
			newData: `{"id":"APP-14","parentId":"<hidden>","title":"child"}`,
		},
		{
			name: "No. 22 issue.reparented, parent lists stored as a single id",
			in: redact.EventRow{Type: "issue.reparented", EntityID: "APP-14",
				OldData: []byte(`{"parentIds":"SEC-7"}`),
				NewData: []byte(`{"parentId":"APP-12"}`),
				Payload: []byte(`{"oldParentIds":"SEC-7","newParentId":"APP-12"}`)},
			entity:  "APP-14",
			oldData: `{"parentIds":"<hidden>"}`,
			newData: `{"parentId":"APP-12"}`,
			payl:    `{"oldParentIds":"<hidden>","newParentId":"APP-12"}`,
		},
		{
			name: "No. 22 issue.reparented, a visible new parent stays",
			in: redact.EventRow{Type: "issue.reparented", EntityID: "APP-14",
				OldData: []byte(`{"parentIds":["SEC-7"]}`),
				NewData: []byte(`{"parentId":"APP-12"}`),
				Payload: []byte(`{"oldParentIds":["SEC-7"],"newParentId":"APP-12"}`)},
			entity:  "APP-14",
			oldData: `{"parentIds":["<hidden>"]}`,
			newData: `{"parentId":"APP-12"}`,
			payl:    `{"oldParentIds":["<hidden>"],"newParentId":"APP-12"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := tc.in
			if !redact.ApplyEventRefs(&row, vis) {
				t.Errorf("ApplyEventRefs reported no change")
			}
			if row.EntityID != tc.entity {
				t.Errorf("entity_id = %q, want %q", row.EntityID, tc.entity)
			}
			sameJSON(t, "old_data", row.OldData, tc.oldData)
			sameJSON(t, "new_data", row.NewData, tc.newData)
			sameJSON(t, "payload", row.Payload, tc.payl)
		})
	}
}

// TestEventRefs_UntouchedRowsKeepTheirBytes — No. 23, R-3, INV-2: a row
// whose references are all visible (a cross-project link included, and one
// whose recorded project is null), user
// text naming a hidden id, and a type outside the registry come back with
// the same bytes — key order and spacing included.
func TestEventRefs_UntouchedRowsKeepTheirBytes(t *testing.T) {
	app, ops := projApp.String(), projOps.String()
	for _, row := range []redact.EventRow{
		{Type: "dependency.created", EntityID: "APP-12::OPS-4::blocks",
			NewData: []byte(`{ "toProjectId": "` + ops + `",  "fromId":"APP-12", "toId":"OPS-4","type":"blocks", "fromProjectId":"` + app + `" }`)},
		{Type: "dependency.created", EntityID: "APP-12::OPS-4::blocks",
			NewData: []byte(`{ "toProjectId": null,  "fromId":"APP-12", "toId":"OPS-4","type":"blocks", "fromProjectId":"` + app + `" }`)},
		{Type: "issue.created", EntityID: "APP-15",
			NewData: []byte(`{"title": "follow-up of SEC-7",  "parentId": "APP-12", "description":"see SEC-7"}`)},
		{Type: "issue.updated", EntityID: "APP-12",
			Payload: []byte(`{"changes":[{"field":"title","oldValue":"x","newValue":"blocked by SEC-7"}]}`)},
		{Type: "comment.created", EntityID: "c-1",
			NewData: []byte(`{"body":"SEC-7 is the cause","parentId":"SEC-7"}`)},
		{Type: "issue.reparented", EntityID: "APP-13",
			OldData: []byte(`{"parentIds": ["APP-12"]}`), NewData: []byte(`{"parentId":  null}`),
			Payload: []byte(`{"newParentId":"", "oldParentIds":["APP-12"]}`)},
	} {
		t.Run(row.Type, func(t *testing.T) {
			got := row
			got.OldData, got.NewData, got.Payload = clone(row.OldData), clone(row.NewData), clone(row.Payload)
			if redact.ApplyEventRefs(&got, vis) {
				t.Errorf("ApplyEventRefs reported a change")
			}
			if got.EntityID != row.EntityID || string(got.OldData) != string(row.OldData) ||
				string(got.NewData) != string(row.NewData) || string(got.Payload) != string(row.Payload) {
				t.Errorf("row changed:\n got %+v\nwant %+v", stringsOf(got), stringsOf(row))
			}
		})
	}
}

func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

func stringsOf(r redact.EventRow) [4]string {
	return [4]string{r.EntityID, string(r.OldData), string(r.NewData), string(r.Payload)}
}

// TestEventRefs_OneLiteralForEveryHiddenTask — No. 24, INV-4, K5: the marker
// is exactly "<hidden>" whichever task is hidden — two rows that differ only
// in which hidden id they name give the same bytes — and a project field is
// null both when its end is hidden in a visible project and when the end is
// visible but the recorded project is one the reader does not see.
func TestEventRefs_OneLiteralForEveryHiddenTask(t *testing.T) {
	app, sec := projApp.String(), projSec.String()
	apply := func(r redact.EventRow) redact.EventRow {
		redact.ApplyEventRefs(&r, vis)
		return r
	}
	mk := func(hidden string) redact.EventRow {
		return redact.EventRow{Type: "dependency.created", EntityID: "APP-12::" + hidden + "::blocks",
			NewData: []byte(`{"fromId":"APP-12","toId":"` + hidden + `","fromProjectId":"` + app + `","toProjectId":"` + app + `"}`)}
	}
	a, b := apply(mk("SEC-7")), apply(mk("SEC-9"))
	if stringsOf(a) != stringsOf(b) {
		t.Errorf("rows naming different hidden tasks differ:\n%v\n%v", stringsOf(a), stringsOf(b))
	}
	if a.EntityID != "APP-12::<hidden>::blocks" {
		t.Errorf("entity_id = %q, want the bare marker", a.EntityID)
	}
	// The end is hidden although its recorded project is visible.
	sameJSON(t, "hidden end in a visible project", a.NewData,
		`{"fromId":"APP-12","toId":"<hidden>","fromProjectId":"`+app+`","toProjectId":null}`)

	// The end is visible, the recorded project (where it was) is not.
	moved := apply(redact.EventRow{Type: "dependency.created", EntityID: "APP-12::APP-13::blocks",
		NewData: []byte(`{"fromId":"APP-12","toId":"APP-13","fromProjectId":"` + app + `","toProjectId":"` + sec + `"}`)})
	if moved.EntityID != "APP-12::APP-13::blocks" {
		t.Errorf("entity_id = %q, want both visible ends", moved.EntityID)
	}
	sameJSON(t, "visible end, hidden recorded project", moved.NewData,
		`{"fromId":"APP-12","toId":"APP-13","fromProjectId":"`+app+`","toProjectId":null}`)
}

// TestEventRefs_CollectsTheRegistryFieldsOnly — No. 25, SC-006, K4: one
// collection over a page of mixed types yields every task id (both parts of
// a composite id) and every project id of the registry fields, and nothing
// from any other field.
func TestEventRefs_CollectsTheRegistryFieldsOnly(t *testing.T) {
	app, sec := projApp.String(), projSec.String()
	refs := redact.NewEventRefs()
	for _, r := range []redact.EventRow{
		{Type: "dependency.created", EntityID: "APP-12::SEC-7::blocks",
			NewData: []byte(`{"fromId":"APP-12","toId":"SEC-7","fromProjectId":"` + app + `","toProjectId":"` + sec + `","metadata":{"note":"NOTE-1"}}`)},
		{Type: "dependency.deleted", EntityID: "DEL-1::DEL-2::related",
			OldData: []byte(`{"fromId":"DEL-1","toId":"DEL-2"}`)},
		{Type: "issue.created", EntityID: "APP-14",
			NewData: []byte(`{"id":"APP-14","parentId":"PAR-1","title":"TITLE-1","projectId":"` + projOps.String() + `"}`)},
		{Type: "issue.reparented", EntityID: "APP-15",
			OldData: []byte(`{"parentIds":["OLD-1","OLD-2"]}`), NewData: []byte(`{"parentId":"NEW-1"}`),
			Payload: []byte(`{"oldParentIds":["OLD-3"],"newParentId":"NEW-2"}`)},
		{Type: "issue.updated", EntityID: "APP-16",
			Payload: []byte(`{"changes":[{"field":"title","newValue":"TEXT-1"}]}`)},
		{Type: "comment.created", EntityID: "c-1", NewData: []byte(`{"parentId":"COM-1"}`)},
	} {
		redact.CollectEventRefs(r, refs)
	}
	issues := refs.IssueIDs()
	slices.Sort(issues)
	want := []string{"APP-12", "DEL-1", "DEL-2", "NEW-1", "NEW-2", "OLD-1", "OLD-2", "OLD-3", "PAR-1", "SEC-7"}
	if !reflect.DeepEqual(issues, want) {
		t.Errorf("task ids = %v, want %v", issues, want)
	}
	projects := refs.ProjectIDs()
	slices.SortFunc(projects, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	if wantP := []uuid.UUID{projApp, projSec}; !reflect.DeepEqual(projects, wantP) {
		t.Errorf("project ids = %v, want %v", projects, wantP)
	}
}

// TestEventRefs_MalformedLinkIDIsHidden — review fix (circle 1): a composite
// link id of a dependency event that is not exactly "from::to::type" (fewer
// or more parts) names no known id, so the whole value becomes "<hidden>"
// and nothing of it is collected (fail-closed, the registry's rule for a
// field of an unexpected shape).
func TestEventRefs_MalformedLinkIDIsHidden(t *testing.T) {
	for _, entity := range []string{"APP-12::SEC-7", "APP-12::OPS-4::blocks::SEC-7", "SEC-7"} {
		t.Run(entity, func(t *testing.T) {
			row := redact.EventRow{Type: "dependency.created", EntityID: entity}
			refs := redact.NewEventRefs()
			redact.CollectEventRefs(row, refs)
			if ids := refs.IssueIDs(); len(ids) != 0 {
				t.Errorf("collected %v from a malformed link id, want nothing", ids)
			}
			if !redact.ApplyEventRefs(&row, vis) {
				t.Errorf("ApplyEventRefs reported no change")
			}
			if row.EntityID != "<hidden>" {
				t.Errorf("entity_id = %q, want <hidden>", row.EntityID)
			}
		})
	}
}
