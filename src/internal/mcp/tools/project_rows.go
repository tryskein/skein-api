package tools

// project_rows.go — the MCP projection of a project-list page (sk-282
// T016, spec US-8).
//
// A project's description is prose with no ceiling. On the wire it is
// carried by every row of every project list an agent asks for, and it is
// not what a list is for: a list answers "which projects are there", the
// description answers "what is this one about", and the second question
// has its own read.
//
// So the description is cut at ProjectDescriptionPreviewMaxRunes through
// the shared truncation rule (T004), in the MCP projection only — the REST
// project list feeds the web UI and INV-2 freezes its values, so the cut
// must not be made in the store.
//
// The marker follows INV-1's second row exactly, and US-8's edge case
// spells out the consequence: a project with an EMPTY description must not
// come back marked as truncated. Empty and short are different inputs from
// long, and only long carries the flag.

import (
	"encoding/json"
	"fmt"

	"skein-api/internal/projects"
)

// projectRowDescriptionTruncatedKey is the INV-1 "truncated" marker on a
// project row. It is a sibling key rather than a wrapper object so the row
// stays flat and a caller that ignores it reads the description exactly as
// before.
const projectRowDescriptionTruncatedKey = "descriptionTruncated"

// projectListResponse is the MCP wire shape of a project-list page. The
// envelope keys are the store result's — US-8 changes what a row carries,
// not how paging works.
type projectListResponse struct {
	Items      []map[string]any `json:"items"`
	Total      int              `json:"total"`
	NextCursor *string          `json:"nextCursor,omitempty"`
	HasMore    bool             `json:"hasMore"`
}

// projectListRow projects one row: the real type marshalled to a map, with
// the description cut and the marker added only when the cut happened.
//
// A JSON map over the real type rather than a parallel struct, for the
// reason D-1 gives everywhere else: a parallel struct silently loses every
// field later added to projects.ProjectListItem, and that loss is
// indistinguishable from a deliberate omission.
func projectListRow(item projects.ProjectListItem) (map[string]any, error) {
	b, err := json.Marshal(item)
	if err != nil {
		return nil, fmt.Errorf("marshal project row: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("unmarshal project row: %w", err)
	}

	preview, truncated := TruncateRunes(item.Description, ProjectDescriptionPreviewMaxRunes)
	m["description"] = preview
	if truncated {
		m[projectRowDescriptionTruncatedKey] = true
	}
	return m, nil
}

// projectListPageRows projects a whole page.
func projectListPageRows(res *projects.ListResult) (*projectListResponse, error) {
	items := make([]map[string]any, 0, len(res.Items))
	for i := range res.Items {
		row, err := projectListRow(res.Items[i])
		if err != nil {
			return nil, err
		}
		items = append(items, row)
	}
	return &projectListResponse{
		Items:      items,
		Total:      res.Total,
		NextCursor: res.NextCursor,
		HasMore:    res.HasMore,
	}, nil
}
