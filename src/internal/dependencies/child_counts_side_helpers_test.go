package dependencies

import (
	"encoding/json"
	"strings"
	"testing"
)

func marshalReadyRows(t *testing.T, rows []IssueSummary) string {
	t.Helper()
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal ready rows: %v", err)
	}
	return string(b)
}

func containsJSONKey(encoded, key string) bool {
	return strings.Contains(encoded, `"`+key+`"`)
}
