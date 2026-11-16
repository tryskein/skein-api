package notifications

import (
	"context"
	"fmt"

	"skein-api/internal/authz"
	"skein-api/internal/redact"
)

// RedactEventRefs rewrites the reference fields of a page of event rows by
// what the reader of scope sees now (sk-1007 US-2, R-1…R-4, D2, K4): a
// hidden task id becomes redact.HiddenMarker, the project of a hidden end
// or a hidden project becomes JSON null, everything else keeps its bytes.
// The registry of reference fields is redact/eventrefs.go#CollectEventRefs.
//
// It is the one step every surface of R-4 runs between scanning its raw
// rows and building its wire rows — the activity feeds (issues), the inbox
// (REST UI, public API, MCP), the Watching timeline and the agent monitor
// poll — so the summary and the composite link id are built from the
// already edited columns and one rule holds everywhere.
//
// Visibility is read once for the whole page, not per row (SC-006): one
// authz.VisibleIssueIDs for every task id the rows name and one
// authz.VisibleAmong for every project id. A page that names no reference
// reads nothing; its rows are still passed through the registry, which
// hides a reference field it cannot read as an id (fail-closed). rows are
// edited in place. A failed read returns the error and leaves rows as they
// were: the caller fails the request rather than answer unredacted.
func RedactEventRefs(ctx context.Context, q authz.RowsQuerier, scope authz.Scope, rows []redact.EventRow) error {
	if len(rows) == 0 {
		return nil
	}
	refs := redact.NewEventRefs()
	for i := range rows {
		redact.CollectEventRefs(rows[i], refs)
	}
	issues, err := authz.VisibleIssueIDs(ctx, q, scope, refs.IssueIDs())
	if err != nil {
		return fmt.Errorf("reading the visibility of event references: %w", err)
	}
	projects, err := authz.VisibleAmong(ctx, q, scope, refs.ProjectIDs())
	if err != nil {
		return fmt.Errorf("reading the visibility of event projects: %w", err)
	}
	v := redact.EventVisibility{Issues: issues, Projects: projects}
	for i := range rows {
		redact.ApplyEventRefs(&rows[i], v)
	}
	return nil
}
