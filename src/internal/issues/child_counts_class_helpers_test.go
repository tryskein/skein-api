package issues_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// marshalJSON renders a value the way a REST handler would.
func marshalJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// containsKey reports whether a JSON document contains the given key.
// Substring matching is enough here and deliberately blunt: the assertion
// is «this name appears nowhere in the payload», which is stronger than
// «the top level has no such field» and catches a counter smuggled into a
// nested object.
func containsKey(encoded, key string) bool {
	return strings.Contains(encoded, `"`+key+`"`)
}
