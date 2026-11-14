package tools

// review_flag.go — sk-303.14, task T007, contract C1.
//
// The "review required" flag on the agent mutation path: parsing the
// skein_update argument and mapping store refusals to tool codes.
//
// There is no viewed mark here and there will not be: no agent tool for
// the mark is introduced. The mark is a human action after reading the
// result; an agent tool would set "viewed" without a viewing, and the
// unreviewed count would stop meaning the thing it was created for.

import (
	"context"
	"skein-api/internal/issueops"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// reviewUpdateEnvelopeArgs — skein_update arguments that change NOTHING
// on the issue: address, write condition and response shape.
//
// The list enumerates the "harmless" keys on purpose, not the "mutating"
// ones, and that direction is the only safe one. A list of MUTATING keys
// would stay silent on an omission: an argument added to skein_update
// tomorrow and forgotten here would travel with the flag, and the mixed
// request would apply halfway — exactly the outcome the refusal exists
// to forbid. A list of harmless keys errs the other way: a new argument
// is treated as mutating by default, and a request mixed with it is
// refused. A false refusal is visible immediately and is fixed with one
// line; a silent leak is never seen.
//
// `reason` is counted as MUTATING on purpose even though by itself it
// writes nothing: it is only meaningful on a status change, and a
// request "flip the flag and also explain a transition that is not
// happening" is mixed in intent.
var reviewUpdateEnvelopeArgs = []string{"id", "expectedVersion", "force", "verbose"}

func reviewFlagArg(args map[string]any) (*bool, *mcpgo.CallToolResult) {
	v0, f := issueops.ReviewFlagArg(args)
	return v0, toolFailure(f)
}

func rejectReviewFlagInBatch(args map[string]any) *mcpgo.CallToolResult {
	return toolFailure(issueops.RejectReviewFlagInBatch(args))
}

func mapReviewFlagError(ctx context.Context, err error, userID uuid.UUID, id string) *mcpgo.CallToolResult {
	return toolFailure(issueops.MapReviewFlagError(ctx, err, userID, id))
}

func createReviewRequiredArg(args map[string]any) (*bool, *mcpgo.CallToolResult) {
	v0, f := issueops.CreateReviewRequiredArg(args)
	return v0, toolFailure(f)
}
