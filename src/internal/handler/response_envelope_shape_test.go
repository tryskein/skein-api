// response_envelope_shape_test.go locks two REST response envelopes that
// audit sk-194 converted from map[string]any to typed structs
// (findings opt-A03-015 DeleteComment, opt-A04-014 LookupID/LookupIDBatch).
//
// The endpoints themselves are covered by comments_test.go /
// search_lookup_id*_test.go, but those are DB-gated and skip without
// TEST_DATABASE_URL — i.e. the wire shape is unprotected in the plain unit
// job, which is exactly where a serialization regression would be cheapest
// to catch. These tests marshal the structs directly instead.

package handler

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/search"
)

func TestDeleteCommentResponse_MatchesLegacyMapBytes(t *testing.T) {
	id := uuid.MustParse("33333333-3333-4333-8333-333333333333")

	got, err := json.Marshal(deleteCommentResponse{
		OK: true, ID: id, IssueID: "sk-42", Deleted: true,
	})
	if err != nil {
		t.Fatalf("marshalling typed response: %v", err)
	}
	want, err := json.Marshal(map[string]any{
		"ok": true, "id": id, "issueId": "sk-42", "deleted": true,
	})
	if err != nil {
		t.Fatalf("marshalling legacy map: %v", err)
	}

	// encoding/json sorts map keys, so the legacy bytes are ordered
	// deleted,id,issueId,ok while the struct emits declaration order.
	// Compare semantically — key order was never contractual here.
	assertSameJSONObject(t, got, want)
}

func TestLookupIDResponse_MatchesLegacyMapBytes(t *testing.T) {
	tests := []struct {
		name  string
		items []search.Item
	}{
		{
			// A nil slice must stay `null`, not become `[]` — clients that
			// distinguish the two would see a behaviour change.
			name: "nil items", items: nil,
		},
		{name: "empty non-nil items", items: []search.Item{}},
		{
			name:  "one item",
			items: []search.Item{{Kind: "issue", ID: "sk-42", Title: "Some issue"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(lookupIDResponse{Items: tc.items})
			if err != nil {
				t.Fatalf("marshalling typed response: %v", err)
			}
			want, err := json.Marshal(map[string]any{"items": tc.items})
			if err != nil {
				t.Fatalf("marshalling legacy map: %v", err)
			}
			// Single-key envelope — byte identity is achievable and worth
			// asserting, since `items` feeds the typeahead directly.
			if string(got) != string(want) {
				t.Fatalf("envelope bytes differ\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

func assertSameJSONObject(t *testing.T, got, want []byte) {
	t.Helper()
	var gotAny, wantAny map[string]any
	if err := json.Unmarshal(got, &gotAny); err != nil {
		t.Fatalf("unmarshalling got %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &wantAny); err != nil {
		t.Fatalf("unmarshalling want %s: %v", want, err)
	}
	if len(gotAny) != len(wantAny) {
		t.Fatalf("key count differs\n got: %v\nwant: %v", gotAny, wantAny)
	}
	for k, w := range wantAny {
		g, ok := gotAny[k]
		if !ok {
			t.Fatalf("key %q missing\n got: %s", k, got)
		}
		if g != w {
			t.Fatalf("key %q = %v, want %v", k, g, w)
		}
	}
}
