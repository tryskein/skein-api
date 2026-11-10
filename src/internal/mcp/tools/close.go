package tools

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/issueops"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
)

// RegisterClose wires the skein_close tool. Atomically transitions an
// issue to status='closed', clears the lock, and writes closed_reason.
//
// Predicate (in order — first failure wins):
//
//   - pinned issue without force=true → `pinned_no_force`
//   - locked_by != caller's session AND force=false → `locked_by_other`
//   - version != expectedVersion AND force=false → `version_conflict`
//
// Successful close emits two events when the issue was previously
// locked: `issue.closed` AND `issue.unclaimed` (mirrors skein_unclaim
// for symmetry — subscribers can filter on either event-type).
//
// Annotations:
//
//   - readOnlyHint: false
//   - destructiveHint: false (closing an issue is reversible — the
//     status can be re-opened; we don't drop comments / history)
//   - idempotentHint: false
//   - openWorldHint: false
func RegisterClose(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store) {
	tool := mcpgo.NewTool(
		"skein_close",
		mcpgo.WithDescription(withAckAnswerNote(
			"Close an issue: status → closed, locked_by cleared, "+
				"closed_at + closed_reason recorded, `issue.closed` (+ "+
				"`issue.unclaimed` when it was locked) emitted. `reason` is "+
				"required (non-empty after trim) — free-form context for a "+
				"human reader; progress counts use `closeOutcome`, never the reason. "+
				"Caller must hold the "+
				"lock OR pass `force=true` (force_unlock: project owner, space owner, instance admin); `expectedVersion` "+
				"is required; pinned issues require force=true. Optionally "+
				"attach the git commits that close the issue via "+
				"`commits[]`.\n\n"+
				"If a retried transaction aborts while the declared version still matches, serialization_retry means the close rolled back; retry unchanged.\n\n"+
				"closeOutcome is required when transitioning to closed; repeating a close without it preserves the previous outcome. "+
				"Target and outcome edits: skein_get_instructions, \"Close outcome\".\n\n"+
				"Re-close is NOT idempotent: every call overwrites "+
				"`closed_reason` / `closed_at` and bumps `version`. To add "+
				"commit hashes to an already-closed issue without losing "+
				"the original reason use `skein_commits_attach`.\n\n"+
				"Retiring a standing task is rejected with "+
				"`standing_has_open_children` (+ `childIds`) while any "+
				"child is not closed; force does not bypass it.\n\n"+
				"Close-time warnings ride on the response's TOP level in "+
				"both modes, under the same names: `warning` (fires when "+
				"the issue has open children, or when it is closed with "+
				"no assignee — both reasons joined when both apply) and "+
				"`openChildList` (the open children themselves). Do not "+
				"confuse `openChildList` with the nested "+
				"`epic.openChildren` counter — different things, "+
				"deliberately different names.",
		)),
		mcpgo.WithString("id",
			mcpgo.Required(),
			mcpgo.Description("Full hierarchical issue id (e.g. \"demo-42\")."),
		),
		mcpgo.WithNumber("expectedVersion",
			mcpgo.Required(),
			mcpgo.Description("The version the agent expects to see on the issue. The close only proceeds when the live version matches; otherwise returns version_conflict with the live version."),
			mcpgo.Min(1),
		),
		mcpgo.WithString("reason",
			mcpgo.Required(),
			mcpgo.Description("Free-form reason annotation (non-empty after trim). Lands in events.reason and issues.closed_reason."),
		),
		mcpgo.WithString("closeOutcome",
			mcpgo.Description(closeOutcomeParamDescription),
			mcpgo.Enum(issues.ValidCloseOutcomes()...),
		),
		mcpgo.WithString("closeTarget",
			mcpgo.Description(closeTargetParamDescription),
		),
		mcpgo.WithBoolean("force",
			mcpgo.Description("Bypass lock + version + pinned predicates. Needs the key right `manage` (else `insufficient_scope`) and force_unlock on the issue's project (project owner, space owner, instance admin); others get `force_unlock_forbidden`. Default: false."),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithBoolean("verbose",
			mcpgo.Description(ackVerboseDescription+cardUnavailableNote),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithArray("commits",
			mcpgo.Description(commitsArrayDescription),
			mcpgo.MaxItems(maxCommitsPerCloseCall),
			mcpgo.Items(commitInputItemSchema()),
		),
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)

	addTool(s, tool, handleClose(pool, issuesStore))
}

func handleClose(pool *pgxpool.Pool, issuesStore *issues.Store) server.ToolHandlerFunc {
	d := issueops.Deps{Pool: pool, Issues: issuesStore}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}
		res, f := issueops.Close(ctx, d, userID, issueops.Args(req.GetArguments()))
		if f != nil {
			return toolFailure(f), nil
		}
		id, dto, link, openChildren := res.IssueID, res.Issue, res.Link, res.OpenChildren
		verbose := req.GetBool("verbose", false)
		cardUnavailable := false
		if rb := res.ReadBack; rb != nil {
			// sk-607: the close committed but the issue could not be read
			// back — acknowledge it from the committed state.
			logReadBackFallback(ctx, "skein_close", userID.String(), rb)
			dto = ackSourceFromCommitted(rb.State)
			cardUnavailable = verbose
			verbose = false
		}

		var warnings []string
		if len(openChildren) > 0 {
			warnings = append(warnings,
				"epic closed with open children; consider closing or unlinking them first")
		}
		// sk-71: ownerless-on-close nudge. Closing with no assignee most
		// often means the agent did the work itself and skipped
		// skein_claim, so the issue dies owned by nobody (invisible in
		// inbox / board). Non-blocking — the close already succeeded; we
		// only remind. The conditional wording covers the legit ownerless
		// closes (someone else's stale issue, won't-fix, duplicate).
		//
		// sk-489 (US-9): the nudge assumes the close means «I did this
		// work», which is exactly what an outcome other than `done` denies.
		// On wont_do / duplicate / superseded the issue is SUPPOSED to end
		// ownerless, and the reminder was the false positive its own last
		// sentence apologised for. The apology is now a condition.
		if dto.Assignee == nil && dto.CloseOutcome != nil &&
			*dto.CloseOutcome == issues.CloseOutcomeDone {
			warnings = append(warnings,
				"closed with no assignee. If you did this work yourself, the "+
					"issue now has no owner (forgot skein_claim?). Set one with "+
					"skein_update(assignee=...) — reopen is NOT needed. Ignore if "+
					"this is a won't-fix / duplicate / someone else's stale issue.")
		}

		// sk-282 T021: the close now answers with the ACKNOWLEDGEMENT
		// (US-5), and the warning keys ride on `writeIssueAckWith`'s
		// top-level slot rather than on a response struct of their own.
		//
		// The slot is the whole point of that sibling helper. US-5 keeps
		// BOTH the names and the NESTING LEVEL of these two keys
		// unchanged and requires the two modes not to diverge in shape;
		// building this branch's response by hand — as it was built
		// before — is precisely how skein_close would have stayed off
		// the common serialisation point while every other write-tool
		// moved onto it (D-6, FP-2 / FP-9).
		//
		// W1 (kept from the struct this replaces): the key is
		// `openChildList`, NOT `openChildren`, to avoid a wire-key clash
		// with the nested `epic.openChildren` counter. In verbose mode
		// both live in the same object — an integer at
		// `epic.openChildren` and this BlockerRef list at the top level
		// — and agents grepping for "openChildren" must not conflate
		// them. Do not rename either.
		//
		// FP-7 — where US-3 and US-5 meet, stated rather than assumed.
		// Two facts, both verified against the queries, because the key
		// names actively mislead here:
		//
		//   - `openChildList` is EpicOpenChildrenFor's result, and that
		//     query filters `status NOT IN ('closed','standing')`. Its
		//     population is therefore the card's US-3
		//     `unclosedChildrenCount`, NOT the card's nested
		//     `epic.openChildren` — the latter counts `status = 'open'`
		//     alone and is strictly narrower (it excludes in_progress
		//     and deferred). A list longer than `epic.openChildren` is
		//     the two counters answering two different questions, and
		//     it is the arithmetic INV-10 asks to be checkable rather
		//     than merely explicable.
		//
		//   - the snapshot above is still gated on `issue.Type ==
		//     TypeEpic`, so a non-epic can carry a non-zero
		//     `unclosedChildrenCount` on its card and still close
		//     without a children warning. Extending the warning to
		//     every type is explicitly OUT of this run's scope; the
		//     gate is named here so the next reader recognises it as a
		//     deliberate limit and not an oversight to be "fixed".
		var extra map[string]any
		if len(warnings) > 0 {
			extra = map[string]any{"warning": strings.Join(warnings, " ")}
			if len(openChildren) > 0 {
				extra["openChildList"] = openChildren
			}
		}
		if cardUnavailable {
			if extra == nil {
				extra = map[string]any{}
			}
			extra[cardUnavailableKey] = true
		}
		return writeIssueCloseAck(ctx, "skein_close", userID.String(), id,
			dto, verbose, extra, link)
	}
}
