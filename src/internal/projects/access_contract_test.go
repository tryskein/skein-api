package projects

// access_contract_test.go — sk-622.21, test matrix row 3 (US-9, the contract
// of the service check POST /api/v1/hub/project-access). The reference is the
// pair of literals fixed in the godoc of access_contract.go by T028 — the
// same literals skein-web's client (T031) checks itself against. They are
// copied here verbatim; the test proves they decode into the contract types
// and encode back without loss, and that absent / empty optional fields keep
// the meaning the godoc gives them.
//
// sk-674.18, matrix row 197 (ADR-3, T014): the literals follow contract
// version 2 — `visibleProjectIds` replaces `memberProjectIds`, is present for
// the internal admin too, and `internalAdmin` is the role only.

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

const accessRequestLiteral = `{"users":[
  {"userId":"0199a1b2-0000-7000-8000-000000000001",
   "projectIds":["0199a1b2-0000-7000-8000-0000000000a1","0199a1b2-0000-7000-8000-0000000000a2"]},
  {"userId":"0199a1b2-0000-7000-8000-000000000002","allVisible":true}
]}`

const accessResponseLiteral = `{"users":[
  {"userId":"0199a1b2-0000-7000-8000-000000000001","active":true,
   "internalAdmin":false,"external":false,
   "allowed":["0199a1b2-0000-7000-8000-0000000000a1"],
   "denied":["0199a1b2-0000-7000-8000-0000000000a2"]},
  {"userId":"0199a1b2-0000-7000-8000-000000000002","active":true,
   "internalAdmin":false,"external":false,
   "allowed":[],"denied":[],
   "visibleProjectIds":["0199a1b2-0000-7000-8000-0000000000a1","0199a1b2-0000-7000-8000-0000000000a3"]}
]}`

// sameJSON reports whether two JSON texts carry the same value (key order and
// whitespace aside).
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		t.Fatalf("unmarshal %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return reflect.DeepEqual(va, vb)
}

// TestProjectAccessContract_LiteralsRoundTrip — both godoc literals decode
// into the contract types with the meaning the godoc states, and encode back
// to the same JSON value.
func TestProjectAccessContract_LiteralsRoundTrip(t *testing.T) {
	a1 := uuid.MustParse("0199a1b2-0000-7000-8000-0000000000a1")
	a2 := uuid.MustParse("0199a1b2-0000-7000-8000-0000000000a2")
	a3 := uuid.MustParse("0199a1b2-0000-7000-8000-0000000000a3")

	var req ProjectAccessRequest
	if err := json.Unmarshal([]byte(accessRequestLiteral), &req); err != nil {
		t.Fatalf("request literal does not decode: %v", err)
	}
	if len(req.Users) != 2 {
		t.Fatalf("request users = %d, want 2", len(req.Users))
	}
	if got := req.Users[0].ProjectIDs; !reflect.DeepEqual(got, []uuid.UUID{a1, a2}) || req.Users[0].AllVisible {
		t.Errorf("request entry 0 = %+v, want projectIds [a1 a2] and allVisible false", req.Users[0])
	}
	if req.Users[1].ProjectIDs != nil || !req.Users[1].AllVisible {
		t.Errorf("request entry 1 = %+v, want no projectIds and allVisible true", req.Users[1])
	}
	back, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if !sameJSON(t, back, []byte(accessRequestLiteral)) {
		t.Errorf("request does not round-trip:\n got  %s\n want %s", back, accessRequestLiteral)
	}

	var resp ProjectAccessResponse
	if err := json.Unmarshal([]byte(accessResponseLiteral), &resp); err != nil {
		t.Fatalf("response literal does not decode: %v", err)
	}
	if len(resp.Users) != 2 {
		t.Fatalf("response users = %d, want 2", len(resp.Users))
	}
	first, second := resp.Users[0], resp.Users[1]
	if !reflect.DeepEqual(first.Allowed, []uuid.UUID{a1}) || !reflect.DeepEqual(first.Denied, []uuid.UUID{a2}) {
		t.Errorf("response entry 0 allowed/denied = %v / %v, want [a1] / [a2]", first.Allowed, first.Denied)
	}
	if first.VisibleProjectIDs != nil {
		t.Errorf("response entry 0 visibleProjectIds = %v, want absent", first.VisibleProjectIDs)
	}
	if !reflect.DeepEqual(second.VisibleProjectIDs, []uuid.UUID{a1, a3}) {
		t.Errorf("response entry 1 visibleProjectIds = %v, want [a1 a3]", second.VisibleProjectIDs)
	}
	back, err = json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if !sameJSON(t, back, []byte(accessResponseLiteral)) {
		t.Errorf("response does not round-trip:\n got  %s\n want %s", back, accessResponseLiteral)
	}
}

// TestProjectAccessContract_OptionalFieldPresence — the godoc's presence
// rules on the wire: allowed / denied are always arrays; visibleProjectIds is
// absent when not asked and `[]` when asked and empty (an inactive user);
// projectIds / allVisible are absent when unset.
func TestProjectAccessContract_OptionalFieldPresence(t *testing.T) {
	user := uuid.MustParse("0199a1b2-0000-7000-8000-000000000003")
	cases := []struct {
		name string
		v    any
		want string
	}{
		{
			name: "query with nothing optional",
			v:    ProjectAccessQuery{UserID: user},
			want: `{"userId":"0199a1b2-0000-7000-8000-000000000003"}`,
		},
		{
			name: "result not asked for all visible",
			v:    ProjectAccessResult{UserID: user, Allowed: []uuid.UUID{}, Denied: []uuid.UUID{}},
			want: `{"userId":"0199a1b2-0000-7000-8000-000000000003","active":false,"internalAdmin":false,` +
				`"external":false,"allowed":[],"denied":[]}`,
		},
		{
			name: "inactive user asked for all visible",
			v: ProjectAccessResult{UserID: user, Allowed: []uuid.UUID{}, Denied: []uuid.UUID{},
				VisibleProjectIDs: []uuid.UUID{}},
			want: `{"userId":"0199a1b2-0000-7000-8000-000000000003","active":false,"internalAdmin":false,` +
				`"external":false,"allowed":[],"denied":[],"visibleProjectIds":[]}`,
		},
	}
	cases = append(cases, struct {
		name string
		v    any
		want string
	}{
		// Version 2: the internal admin asking for all visible gets the SET,
		// next to the role — never the role alone as a wildcard.
		name: "internal admin asked for all visible",
		v: ProjectAccessResult{UserID: user, Active: true, InternalAdmin: true,
			Allowed: []uuid.UUID{}, Denied: []uuid.UUID{},
			VisibleProjectIDs: []uuid.UUID{uuid.MustParse("0199a1b2-0000-7000-8000-0000000000a1")}},
		want: `{"userId":"0199a1b2-0000-7000-8000-000000000003","active":true,"internalAdmin":true,` +
			`"external":false,"allowed":[],"denied":[],` +
			`"visibleProjectIds":["0199a1b2-0000-7000-8000-0000000000a1"]}`,
	})
	for _, c := range cases {
		got, err := json.Marshal(c.v)
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.name, err)
		}
		if !sameJSON(t, got, []byte(c.want)) {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
	}
}

// TestProjectAccessContract_VersionOneKeyIsGone — row 197: the version 1 key
// `memberProjectIds` is no longer part of the contract. A version 1 body
// decodes with no visible set (the key is not read), and no result encodes
// the old key.
func TestProjectAccessContract_VersionOneKeyIsGone(t *testing.T) {
	const v1 = `{"users":[{"userId":"0199a1b2-0000-7000-8000-000000000002","active":true,` +
		`"internalAdmin":false,"external":false,"allowed":[],"denied":[],` +
		`"memberProjectIds":["0199a1b2-0000-7000-8000-0000000000a1"]}]}`
	var resp ProjectAccessResponse
	if err := json.Unmarshal([]byte(v1), &resp); err != nil {
		t.Fatalf("decoding a version 1 body: %v", err)
	}
	if got := resp.Users[0].VisibleProjectIDs; got != nil {
		t.Errorf("memberProjectIds was read into visibleProjectIds (%v) — version 2 has no such key", got)
	}
	out, err := json.Marshal(ProjectAccessResult{Allowed: []uuid.UUID{}, Denied: []uuid.UUID{},
		VisibleProjectIDs: []uuid.UUID{}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]any
	if err := json.Unmarshal(out, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := keys["memberProjectIds"]; ok {
		t.Errorf("a result still encodes memberProjectIds: %s", out)
	}
}
