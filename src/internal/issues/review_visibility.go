package issues

// review_visibility.go — sk-303.14, task T006, contract C1.
//
// # What lives here and why it lives here
//
// The review workflow is the AUTHOR's private bookkeeping (INV-2). Its three
// fields — the "review required" flag, the mark timestamp, and the mark's
// author — are returned ONLY to the issue author, and not as empty or zero
// values, but as ABSENT from the object. The difference is not cosmetic:
// `reviewRequired:false` on an assignee tells them the mechanic exists and
// did not fire on this issue — exactly what INV-2 must hide. A missing key
// tells them nothing.
//
// The gate lives in ONE place — applyReviewVisibility — and is called from
// hydrateIssueDetail, the only function that finishes assembling a card.
// This is not a "convenient" place, it is the only sufficient one: a card
// leaves this package not only from a read. Every IssueDetail return in this
// package — create, patch, claim, unclaim, close, accept, attach commits,
// labels, pin, epic batch, batch-claim — converges on Store.GetIssue or
// Store.getIssuesByIDs, and both go through hydrateIssueDetail (checkable:
// grep 'GetIssue(ctx\|getIssuesByIDs(' across the package). A gate in the
// read handler would leave the other returns open, and a REST patch returns
// the card as-is.
//
// # Why this is not the project-visibility mechanism
//
// internal/visibility limits the PROJECT HORIZON in lists — which rows to
// show at all. The question here is different: the row is shown to everyone
// who can see the issue, but the COMPOSITION OF ITS FIELDS depends on
// whether the reader is the author. There is no usable predicate there, and
// adding one would be extending someone else's contract.
//
// # Fail-closed by construction
//
// The exported fields are NEVER filled anywhere except this function; row
// scanning puts values into unexported raw fields that encoding/json does
// not see at all. So any future path that forgets to call the gate returns a
// card WITHOUT the fields — it errs toward privacy, not toward a leak. The
// inverse scheme ("always fill, then strip for a stranger") would err toward
// a leak, and the cost of that error is irreversible loss of trust in the
// mechanic (the plan's risk register, row INV-2).
//
// # Identity
//
// Taken from the request context — auth.UserIDFrom — which both transports
// stamp: the REST session middleware and the MCP API-key middleware (see
// mcp.RequireUser, handler.RequireUser). A context with no user (background
// workers, migration utilities, store tests) is NOT the author, and the
// fields are not issued.

import (
	"context"

	"github.com/google/uuid"

	"skein-api/internal/auth"
)

// applyReviewVisibility is the only place the three review-mechanic fields
// land on a card.
//
// There is exactly one predicate: the caller is the issue author. Neither
// the admin role, nor being the assignee, nor project membership widens it
// (INV-4: a role widens visibility, not authorship).
//
// Idempotent and safe to call twice: for a non-author it NILLS the exported
// fields rather than merely "not filling them". That matters on paths where
// the same card is hydrated twice.
func (d *IssueDetail) applyReviewVisibility(ctx context.Context) {
	caller, ok := auth.UserIDFrom(ctx)
	if !ok || caller == uuid.Nil || caller != d.CreatedBy {
		d.ReviewRequired = nil
		d.ReviewedAt = nil
		d.ReviewedBy = nil
		return
	}

	// The flag is copied into a local, not taken as the address of the
	// struct field: that address would outlive the card itself and alias
	// two values that must stay independent.
	required := d.rawReviewRequired
	d.ReviewRequired = &required
	d.ReviewedAt = d.rawReviewedAt
	d.ReviewedBy = d.rawReviewedBy
}
