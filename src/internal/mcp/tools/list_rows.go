package tools

// list_rows.go — the MCP projection of a list page (sk-282 T014).
//
// The envelope keys are unchanged — `items`, `total`, `nextCursor`,
// `hasMore` — because US-2 declares them Unchanged and an agent's paging
// loop reads them. What changes is what an item IS: the shared row
// projection (issuerow.go) with the named US-2 field set, the caller's
// explicit selection when it made one, and the US-3 child counters from
// the one counting fragment (childcount).
//
// Total and hasMore describe the QUERY, not the projection, so they are
// copied through untouched. That is a contract worth stating: a caller
// that narrows `fields` to save bytes must still be able to page, and a
// total that moved with the field selection would be a different number
// answering a different question.

import (
	"context"
	"fmt"

	"skein-api/internal/authz"
	"skein-api/internal/childcount"
	"skein-api/internal/issues"
)

// listResponse is the MCP wire shape of a list page.
type listResponse struct {
	Items      []map[string]any `json:"items"`
	Total      int              `json:"total"`
	NextCursor *string          `json:"nextCursor,omitempty"`
	HasMore    bool             `json:"hasMore"`
}

// parseRowFields reads the `fields` argument.
//
// It returns nil when the key is ABSENT (meaning «the default set») and a
// non-nil, possibly empty slice when it is present. The difference is the
// contract: US-2's edge-case table says an empty selection yields the
// identifier alone, which is a different answer from «no selection».
// req.GetStringSlice cannot express it — an absent key and an empty array
// both come back as an empty slice — so the raw argument map is read.
func parseRowFields(args map[string]any) ([]string, error) {
	raw, present := args["fields"]
	if !present || raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("fields must be an array of field names")
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		name, isString := item.(string)
		if !isString {
			return nil, fmt.Errorf("fields must contain field names as strings")
		}
		out = append(out, name)
	}
	return out, nil
}

// projectListPage turns a store list result into the MCP response.
//
// scope is the caller's horizon (authz), used for the counters so they
// agree with every other surface about what this caller may see.
func projectListPage(
	ctx context.Context,
	q childcount.Querier,
	res *issues.ListResult,
	fields []string,
	crossProject bool,
	scope authz.Scope,
) (*listResponse, error) {
	// The selection is validated HERE, before anything is projected, and
	// not as a by-product of projecting the first row.
	//
	// Validation used to live only inside ProjectIssueRow, which runs
	// inside the loop below — so on an EMPTY page the loop body never ran
	// and `fields: ["totallyBogusFieldName"]` came back isError=false with
	// items: []. That is the outcome US-2's edge-case table forbids by
	// name: a silently ignored field name is indistinguishable from «the
	// field exists and this page has no rows for it», and an empty page is
	// not an exotic edge — every filter that matches nothing lands on it.
	// Both list paths (project-scoped and cross-project) route through
	// this function, so both are covered by the one check.
	//
	// The universe comes from the carrier's ZERO value: the names a field
	// selection may use are a property of the row TYPE, not of whichever
	// rows the query happened to return.
	if err := validateIssueRowFields(fields, crossProject, issues.IssueSummary{}); err != nil {
		return nil, err
	}

	counts, err := childcount.For(ctx, q, rowIDs(res.Items), scope)
	if err != nil {
		return nil, err
	}

	items := make([]map[string]any, 0, len(res.Items))
	for i := range res.Items {
		row := res.Items[i]
		c := counts[row.ID]
		projected, perr := ProjectIssueRow(row, IssueRowOptions{
			Fields:       fields,
			CrossProject: crossProject,
			ChildCounts:  &IssueRowChildCounts{Children: c.Total, Unclosed: c.Unclosed},
		})
		if perr != nil {
			return nil, perr
		}
		items = append(items, projected)
	}

	return &listResponse{
		Items:      items,
		Total:      res.Total,
		NextCursor: res.NextCursor,
		HasMore:    res.HasMore,
	}, nil
}

func rowIDs(rows []issues.IssueSummary) []string {
	out := make([]string, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ID)
	}
	return out
}
