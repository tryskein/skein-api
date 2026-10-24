// hybrid_payload_shape_test.go locks the parent-event payload envelope.
//
// Audit sk-194 (finding opt-A20-007) replaced
// json.Marshal(map[string]any{"changes": changes}) with a typed struct.
// The payload is persisted to events.payload and read by NATS subscribers,
// so the bytes are an external contract. DB-free by design — this must run
// in the plain unit job, unlike the rest of hybrid_test.go.

package eventbus

import (
	"encoding/json"
	"testing"
)

func TestHybridParentPayload_MatchesLegacyMapBytes(t *testing.T) {
	tests := []struct {
		name    string
		changes []FieldChange
	}{
		{
			name:    "nil changes",
			changes: nil,
		},
		{
			name:    "empty non-nil changes",
			changes: []FieldChange{},
		},
		{
			name: "single change",
			changes: []FieldChange{
				{Field: "title", OldValue: "old", NewValue: "new"},
			},
		},
		{
			name: "null old and new values",
			changes: []FieldChange{
				{Field: "assignee", OldValue: nil, NewValue: nil},
			},
		},
		{
			name: "several changes keep registry order",
			changes: []FieldChange{
				{Field: "title", OldValue: "a", NewValue: "b"},
				{Field: "priority", OldValue: 3, NewValue: 1},
				{Field: "labels", OldValue: []string{"x"}, NewValue: []string{"x", "y"}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(hybridParentPayload{Changes: tc.changes})
			if err != nil {
				t.Fatalf("marshalling typed payload: %v", err)
			}
			want, err := json.Marshal(map[string]any{"changes": tc.changes})
			if err != nil {
				t.Fatalf("marshalling legacy map payload: %v", err)
			}
			if string(got) != string(want) {
				t.Fatalf("payload bytes differ\n got: %s\nwant: %s", got, want)
			}
		})
	}
}
