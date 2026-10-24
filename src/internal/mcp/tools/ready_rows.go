package tools

// ready_rows.go — the "ready to take" row projection (sk-282 T017, spec
// US-9).
//
// # The lie being removed
//
// dependencies.IssueSummary carries `labels` and `componentIds`, and the
// ready query never loads either — the aggregate joins would make it N+1,
// which is why the type's own comment says they are "always []". After
// marshalling, that `[]` is indistinguishable from "this issue has no
// labels", and an agent filtering work by label reads a confident, wrong
// answer on every row.
//
// # Why the keys go, rather than the values
//
// Zeroing them changes nothing: an empty array IS the lie. The fix is that
// the keys are ABSENT — "not provided" expressed as INV-1's third
// form, a uniform documented contract on a surface that NEVER supplies
// these fields in this mode. The tool's description names the route to
// them (INV-4), because a narrowing without a named way out is not
// allowed to exist.
//
// # What is NOT touched
//
// Everything else on the row. US-9 declares the rest Unchanged, so this is
// a removal of two keys and not a re-projection onto the US-2 field set —
// that belongs to prime's copy of this carrier (T018), where US-12 asks
// for the US-2 set with these same two names as its single exception.

import (
	"encoding/json"
	"fmt"

	"skein-api/internal/dependencies"
)

// readyRowNotProvided are the keys the ready carrier does not load. Named
// once so this file and prime's projection (T018) cannot disagree about
// which fields the same carrier is silent on.
var readyRowNotProvided = []string{
	IssueRowFieldLabels,
	"componentIds",
}

// projectReadyRow removes the not-provided keys from one ready row.
func projectReadyRow(item dependencies.IssueSummary) (map[string]any, error) {
	b, err := json.Marshal(item)
	if err != nil {
		return nil, fmt.Errorf("marshal ready row: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("unmarshal ready row: %w", err)
	}
	for _, key := range readyRowNotProvided {
		delete(m, key)
	}
	return m, nil
}

// projectReadyRows projects a whole page, preserving order.
func projectReadyRows(items []dependencies.IssueSummary) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(items))
	for i := range items {
		row, err := projectReadyRow(items[i])
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}
