package issues

import (
	"reflect"
	"testing"
)

// sk-73 — the Activity feed now surfaces the event's own audit data
// (events.old_data / new_data / payload.changes) so the UI renders
// meaningful change detail. These DB-free unit tests pin the two decode
// helpers that feed ActivityEntry.OldValue / NewValue / Changes.

func TestDecodeJSONValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []byte
		want any
	}{
		{"nil bytes (SQL NULL)", nil, nil},
		{"empty bytes", []byte{}, nil},
		{"explicit JSON null", []byte("null"), nil},
		{"status string", []byte(`"in_progress"`), "in_progress"},
		{"number", []byte(`2`), float64(2)},
		{"malformed → nil", []byte(`{not json`), nil},
		{
			"reparent old_data object",
			[]byte(`{"parentIds":["sk-1","sk-2"]}`),
			map[string]any{"parentIds": []any{"sk-1", "sk-2"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decodeJSONValue(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("decodeJSONValue(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestDecodeActivityChanges(t *testing.T) {
	t.Parallel()

	t.Run("nil payload", func(t *testing.T) {
		t.Parallel()
		if got := decodeActivityChanges(nil); got != nil {
			t.Fatalf("expected nil, got %#v", got)
		}
	})

	t.Run("payload without changes key (e.g. issue.accepted)", func(t *testing.T) {
		t.Parallel()
		if got := decodeActivityChanges([]byte(`{"acceptedBy":"u-1"}`)); got != nil {
			t.Fatalf("expected nil for payload with no changes[], got %#v", got)
		}
	})

	t.Run("malformed payload → nil", func(t *testing.T) {
		t.Parallel()
		if got := decodeActivityChanges([]byte(`{bad`)); got != nil {
			t.Fatalf("expected nil for malformed payload, got %#v", got)
		}
	})

	t.Run("issue.updated changes lifted verbatim", func(t *testing.T) {
		t.Parallel()
		payload := []byte(`{"changes":[` +
			`{"field":"priority","oldValue":3,"newValue":2},` +
			`{"field":"assignee","oldValue":null,"newValue":"alice"},` +
			`{"field":"title","oldValue":"old","newValue":"new"}]}`)
		got := decodeActivityChanges(payload)
		want := []ActivityChange{
			{Field: "priority", OldValue: float64(3), NewValue: float64(2)},
			{Field: "assignee", OldValue: nil, NewValue: "alice"},
			{Field: "title", OldValue: "old", NewValue: "new"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("decodeActivityChanges = %#v, want %#v", got, want)
		}
	})
}
