package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/authz"
	"skein-api/internal/issues"
	skmcp "skein-api/internal/mcp"
	"skein-api/internal/projects"
)

// RegisterMove wires the skein_move tool. Honest signpost: never mutates
// state. Returns a pre-filled kit for the canonical copy+supersedes+close
// workflow so agents that reach for `skein_move` first (the universal
// intuition) get redirected to the right shape instead of inventing their
// own and forgetting the supersedes edge / reason / component match.
//
// Annotations:
//
//   - readOnlyHint: true   (loads source + target metadata only)
//   - destructiveHint: false
//   - idempotentHint: true
//   - openWorldHint: false
//
// _meta: anthropic/maxResultSizeChars=100000 mirrors skein_show, and sk-282
// T026 examined it and left it there.
//
// T025 halved the prose in this response — the source's texts are carried
// once, inside `nextSteps[0].args`, instead of once there and once on the
// card. That is a real cut, but it is a cut to a DUPLICATE, not to the
// bound: one copy of an issue's description still has no ceiling, so the
// heaviest legal answer here remains open-ended, exactly as it is for
// skein_show over the same object. A declaration fitted to a typical move
// would send the large ones out of the conversation as file references
// (result_size.go), which on a tool whose entire output is a
// ready-to-execute call kit is the worst possible place to land.
func RegisterMove(s *server.MCPServer, pool *pgxpool.Pool, issuesStore *issues.Store, projectsStore *projects.Store) {
	tool := mcpgo.NewTool(
		"skein_move",
		mcpgo.WithDescription(
			"There is no in-place project move in Skein. This tool is a "+
				"documented signpost: it does NOT mutate any state, it "+
				"returns a pre-filled kit for the canonical "+
				"copy+supersedes+close workflow. Inputs: `issueId` (source) "+
				"+ `targetProject` (handle). The output has one shape, and it "+
				"is this by default and by design: the source card "+
				"WITHOUT the fields the prepared call already carries "+
				"(title / description / type / priority and, when set, "+
				"assignee / acceptanceCriteria / design / notes / labels — "+
				"each of the source's texts appears in the response exactly "+
				"once, in `nextSteps[0].args`; read the whole card with "+
				"skein_show), the target project's components with naive "+
				"name-match hints against the source title/description, "+
				"and two ready-to-call `nextSteps` — `skein_create` in "+
				"target (with a `supersedes` dependency edge on source) "+
				"and `skein_close` on source. An unassigned source is "+
				"copied with `unassigned: true`, so the copy stays "+
				"unassigned whatever your «assign to me» setting says; an "+
				"assigned source carries its `assignee` as an explicit one. The agent picks a "+
				"componentId from `targetComponents` (or leaves the array "+
				"empty for a project-wide task), fills in the close "+
				"`reason` and `closeTarget` placeholders with the id "+
				"returned by skein_create, then makes the two calls "+
				"(the close answers `closeLink:\"existed\"` — step 1 "+
				"made the edge). The new issue "+
				"gets a fresh hierarchical id; the source closes and the "+
				"`supersedes` edge is the audit trail. Why no in-place "+
				"move: the hierarchical id is embedded in git commits, "+
				"chats, messages between agents, and other external refs — "+
				"renumbering would break them, and swapping `project_id` "+
				"while keeping the id creates visual incongruity in the "+
				"target's listing. Copy+supersedes is the honest mechanism "+
				"that preserves provenance without breaking external "+
				"pointers.",
		),
		mcpgo.WithString("issueId",
			mcpgo.Required(),
			mcpgo.Description("Full hierarchical id of the source issue (e.g. \"demo-42\")."),
		),
		mcpgo.WithString("targetProject",
			mcpgo.Required(),
			mcpgo.Description("Target project handle: alias, name, idPrefix, or UUID."),
		),
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)
	tool.Meta = mcpgo.NewMetaFromMap(map[string]any{
		"anthropic/maxResultSizeChars": 100000,
	})

	addTool(s, tool, handleMove(pool, issuesStore, projectsStore))
}

// moveTargetProject is the slim target-project echo so the agent sees
// what the fuzzy `targetProject` handle resolved to (Resolve might have
// auto-matched "gamma-web" against a typo input).
type moveTargetProject struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IDPrefix string `json:"idPrefix"`
}

// moveComponent is one row of `targetComponents` — the full picker the
// agent chooses from. MatchHint is a one-line human-readable nudge when
// the component name appears verbatim in the source title or description
// (case-insensitive substring); nil otherwise. Intentionally cheap — no
// fuzzy / embedding match — because the agent always has full freedom to
// override and a wrong-but-plausible hint would be worse than no hint.
type moveComponent struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	MatchHint   *string `json:"matchHint"`
}

// moveNewIDPlaceholder stands for the id skein_create returns in step 1 of
// the kit; the agent substitutes it in the close step's `reason` and
// `closeTarget` (sk-540.2).
const moveNewIDPlaceholder = "<NEW-ID-FROM-skein_create-RESPONSE>"

// moveNextStep is one prepared tool call. `args` is the exact body the
// agent should pass to the named tool; `comment` carries any human
// instruction the agent must act on (pick a componentId, fill in a
// reason placeholder, etc).
type moveNextStep struct {
	Tool    string         `json:"tool"`
	Comment string         `json:"comment,omitempty"`
	Args    map[string]any `json:"args"`
}

// moveResponse is the full signpost shape. `supported: false` makes the
// "there is no real move" stance explicit at the top level — agents
// branching on `ok` / status fields see the negative path without having
// to parse the prose.
type moveResponse struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
	// Source is the source issue card MINUS every field the prepared
	// `nextSteps[0].args` already carry (sk-282 T025). It is a projected
	// map rather than *issues.IssueDetail precisely because that subtraction
	// is computed, not declared — see moveCreateArgs.
	Source           map[string]any    `json:"source"`
	TargetProject    moveTargetProject `json:"targetProject"`
	TargetComponents []moveComponent   `json:"targetComponents"`
	NextSteps        []moveNextStep    `json:"nextSteps"`
}

// moveCreateArgs assembles the prepared `skein_create` body and, at the
// same time, records which fields of the source card it consumed
// (sk-282 T025, US-5: "each of its texts appears in the response exactly
// once, and the prepared argument set remains directly callable").
//
// # Why the two are built together
//
// The duplication US-5 names here is real and total: today the response
// carries the source's title, description, design, acceptance criteria,
// notes and labels twice — once on the card, once inside the arguments
// that make this tool useful. The arguments cannot be the copy that goes:
// "the prepared argument set on a move signpost — that IS the point of
// the tool", and an argument set that has to be completed from elsewhere is
// not directly callable. So the CARD gives them up.
//
// Which fields the card gives up is then a list — and a hand-maintained
// list of that kind is precisely what has rotted three times in this run.
// Here it cannot: a field enters the arguments ONLY through carry(), and
// carry() is the same call that marks it for removal from the card. The
// two can no longer disagree, because they are one statement.
//
// setDerived() is the deliberate other half: values that are NOT the
// source's — the target project handle, the provenance note, the empty
// component set the agent is meant to fill, the supersedes edge. They
// look like card fields (`sourceContext`, `componentIds` are real card
// keys) but carry different values, so removing the card's copy would
// destroy information rather than de-duplicate it.
type moveCreateArgs struct {
	args     map[string]any
	consumed []string
}

func newMoveCreateArgs() *moveCreateArgs {
	return &moveCreateArgs{args: map[string]any{}}
}

// carry copies a field of the source card into the prepared arguments
// under the SAME wire name, and records that the card no longer needs to
// carry it. `value` may be decorated (the description gets a provenance
// footer) — what matters is that the source's text is present here, so
// the card's copy becomes the duplicate.
func (b *moveCreateArgs) carry(key string, value any) {
	b.args[key] = value
	b.consumed = append(b.consumed, key)
}

// setDerived adds an argument whose value does NOT come from the source
// card, so nothing is removed from the card.
func (b *moveCreateArgs) setDerived(key string, value any) {
	b.args[key] = value
}

// sourceCard projects the issue card with every carried field removed.
//
// Removal is unconditional for the fields that were carried — including
// when the underlying value is empty — so the contract is uniform: what
// the prepared call carries, the card does not. The way to the full card
// is skein_show on the same id, which is what INV-4 asks for and what the
// tool description names.
func (b *moveCreateArgs) sourceCard(d *issues.IssueDetail) (map[string]any, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("marshal source issue: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("unmarshal source issue: %w", err)
	}
	for _, key := range b.consumed {
		delete(m, key)
	}
	return m, nil
}

func handleMove(pool *pgxpool.Pool, issuesStore *issues.Store, projectsStore *projects.Store) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		userID, errResp := skmcp.RequireUser(ctx)
		if errResp != nil {
			return errResp, nil
		}

		rawIssueID, err := req.RequireString("issueId")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "issueId is required", nil), nil
		}
		issueID := strings.TrimSpace(rawIssueID)
		if !looksLikeHierarchicalID(issueID) {
			return skmcp.ToolError(ErrCodeValidationError,
				"issueId must be a full hierarchical id like \"demo-42\" — bare numbers are not accepted",
				map[string]any{"issueId": rawIssueID}), nil
		}

		targetHandle, err := req.RequireString("targetProject")
		if err != nil {
			return skmcp.ToolError(ErrCodeValidationError, "targetProject is required", nil), nil
		}

		// Load source issue first. Not-found short-circuits the rest —
		// no point resolving the target project if we can't even see
		// the source.
		source, err := issuesStore.GetIssue(ctx, issueID)
		if err != nil {
			if errors.Is(err, issues.ErrNotFound) {
				return skmcp.ToolError(ErrCodeIssueNotFound,
					"source issue does not exist or is not visible",
					map[string]any{"issueId": issueID}), nil
			}
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_move",
				"reason", "get source issue failed",
				"userId", userID.String(), "issueId", issueID, "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load source issue", nil), nil
		}

		// Source-side visibility (mask invisible projects as not_found,
		// same rule as skein_show — never leak existence of issues in
		// projects the caller can't see).
		// sk-674.18 T066: a move rewrites the source (closed as moved) —
		// ActionWrite of US-6 there; the target is checked below.
		if res := issueActionGate(ctx, pool, "skein_move", "issueId", issueID, source.ProjectID, userID, authz.ActionWrite); res != nil {
			return res, nil
		}

		// Resolve target project handle (alias / name / idPrefix / UUID).
		// Visibility is enforced inside ResolveProjectHandle so an
		// invisible target surfaces as project_not_found.
		targetID, perr := ResolveProjectHandle(ctx, pool, projectsStore, targetHandle, userID)
		if perr != nil {
			return projectErrorResult(perr)
		}
		// INV-24 (sk-674.18 T033): a copy stays inside the space of the
		// issue; a project of another space answers as a nonexistent one —
		// before the action gate, so it never says «forbidden» either.
		same, serr := projects.SameSpace(ctx, pool, source.ProjectID, targetID)
		if serr != nil {
			slog.ErrorContext(ctx, "mcp tool error", "tool", "skein_move",
				"reason", "comparing spaces failed", "userId", userID.String(),
				"issueId", issueID, "error", serr.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to resolve project handle", nil), nil
		}
		if !same {
			return projectErrorResult(&ProjectNotFoundError{Query: targetHandle})
		}
		// The move creates the issue in the target — ActionWrite there too.
		if res := projectActionGate(ctx, pool, "skein_move", targetHandle, targetID, userID, authz.ActionWrite); res != nil {
			return res, nil
		}

		// Same-project guard — nothing to move. validation_error rather
		// than a no-op success so the agent doesn't quietly do nothing
		// when the user actually meant a different project but mistyped.
		if targetID == source.ProjectID {
			return skmcp.ToolError(ErrCodeValidationError,
				"issue is already in the target project — nothing to move",
				map[string]any{
					"issueId":          issueID,
					"currentProjectId": source.ProjectID.String(),
					"targetProjectId":  targetID.String(),
				}), nil
		}

		// Fetch target project for IDPrefix + Name so nextSteps can
		// reference the canonical handle (`project: "gamma-web"`)
		// rather than the raw input the user might have fuzzed.
		targetProj, err := projectsStore.GetProject(ctx, targetID)
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_move",
				"reason", "get target project failed",
				"userId", userID.String(), "targetId", targetID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to load target project", nil), nil
		}

		// List target's active components — the picker the agent
		// chooses from. Archived ones excluded; the agent should not
		// attach a moved task to an archived component.
		components, err := projectsStore.ListComponents(ctx, targetID, "false", userID, "")
		if err != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_move",
				"reason", "list target components failed",
				"userId", userID.String(), "targetId", targetID.String(), "error", err.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to list target components", nil), nil
		}

		// Cheap substring match for hints. Case-insensitive; title
		// match takes precedence over description match. Empty hints
		// stay nil (omitempty would hide the field entirely; nil keeps
		// the JSON shape stable so the agent can branch on null vs string).
		moveComps := make([]moveComponent, 0, len(components))
		titleLower := strings.ToLower(source.Title)
		descLower := strings.ToLower(source.Description)
		for _, c := range components {
			nameLower := strings.ToLower(c.Name)
			var hint *string
			switch {
			case nameLower != "" && strings.Contains(titleLower, nameLower):
				h := fmt.Sprintf("name %q appears in source title", c.Name)
				hint = &h
			case nameLower != "" && strings.Contains(descLower, nameLower):
				h := fmt.Sprintf("name %q appears in source description", c.Name)
				hint = &h
			}
			moveComps = append(moveComps, moveComponent{
				ID:          c.ID.String(),
				Name:        c.Name,
				Description: c.Description,
				MatchHint:   hint,
			})
		}

		// Pre-filled skein_create args. We append a footer to the
		// description that documents the provenance — the supersedes
		// edge is machine-readable, the footer is human-readable for
		// people opening the new issue cold.
		movedFooter := fmt.Sprintf(
			"\n\n---\n\n_Moved from `%s` (via `skein_move` signpost): see the `supersedes` edge on the source issue._",
			source.ID,
		)
		builder := newMoveCreateArgs()
		builder.carry("title", source.Title)
		builder.carry("description", source.Description+movedFooter)
		builder.carry("type", source.Type)
		builder.carry("priority", source.Priority)
		builder.setDerived("project", targetProj.IDPrefix)
		builder.setDerived("kind", "directed")
		builder.setDerived("sourceContext", fmt.Sprintf(
			"manual move from %s → %s (via skein_move signpost)",
			source.ID, targetProj.IDPrefix,
		))
		builder.setDerived("dependencies", []map[string]any{
			{"toId": source.ID, "type": "supersedes"},
		})
		builder.setDerived("componentIds", []string{})

		// The optional fields are carried ONLY when the source has them,
		// and the card gives up exactly what was carried — so an issue
		// with no notes keeps its empty `notes` key on the card, and one
		// with notes has them in the prepared call instead. Either way,
		// once.
		if source.Assignee != nil && *source.Assignee != "" {
			builder.carry("assignee", *source.Assignee)
		} else {
			// sk-565 (US-7, P10): an unassigned source stays unassigned on
			// the copy whatever the caller's «assign to me» setting says.
			// Derived, not carried: the card has no such field to give up.
			builder.setDerived("unassigned", true)
		}
		if source.AcceptanceCriteria != "" {
			builder.carry("acceptanceCriteria", source.AcceptanceCriteria)
		}
		if source.Design != "" {
			builder.carry("design", source.Design)
		}
		if source.Notes != "" {
			builder.carry("notes", source.Notes)
		}
		if len(source.Labels) > 0 {
			builder.carry("labels", source.Labels)
		}
		createArgs := builder.args

		sourceCard, cerr := builder.sourceCard(source)
		if cerr != nil {
			slog.ErrorContext(ctx, "mcp tool error",
				"tool", "skein_move",
				"reason", "source card projection failed",
				"userId", userID.String(), "issueId", issueID, "error", cerr.Error())
			return skmcp.ToolError(ErrCodeInternalError, "failed to encode result", nil), nil
		}

		// Pre-filled skein_close args. expectedVersion is taken at
		// signpost time — if someone bumps it before the agent
		// closes, the close errors with version_conflict and the
		// agent re-fetches.
		closeArgs := map[string]any{
			"id":              source.ID,
			"expectedVersion": source.Version,
			"reason": fmt.Sprintf(
				"moved to %s in project %s — <one-sentence why>",
				moveNewIDPlaceholder, targetProj.IDPrefix,
			),
			// sk-489 (US-9) — a move is the textbook «superseded»: the copy
			// in the target project replaces this row, which is not work
			// that was done and must not count as done in any progress
			// number. Left at the default the close would record `done`,
			// and the move would inflate every rollup the source sits in.
			"closeOutcome": issues.CloseOutcomeSuperseded,
			// sk-540.2: duplicate/superseded require `closeTarget` on every
			// close path, so the kit names it — the new issue from step 1,
			// spelled as the same placeholder `reason` uses, since that id
			// does not exist at signpost time. Step 1 already created the
			// `supersedes` edge (new → source) inline in skein_create's
			// dependencies[], so the close finds it and answers
			// `closeLink:"existed"`: that is the expected outcome of a move,
			// not a warning — the target confirms which issue replaced this
			// one, and no second edge is made.
			"closeTarget": moveNewIDPlaceholder,
		}

		nextSteps := []moveNextStep{
			{
				Tool: "skein_create",
				Comment: "Pick a componentId from `targetComponents` above (or leave the " +
					"array empty if the task is project-wide), then call this. The " +
					"response will give you the new issue id — copy it into the next " +
					"step's `reason` and `closeTarget` placeholders.",
				Args: createArgs,
			},
			{
				Tool: "skein_close",
				Comment: "Replace the `" + moveNewIDPlaceholder + "` placeholder in " +
					"`reason` AND `closeTarget` with the id returned by skein_create above, " +
					"then add a one-sentence why. The answer's `closeLink:\"existed\"` is " +
					"expected: step 1 already made the supersedes edge. `expectedVersion` is the source's version at " +
					"signpost time — if a concurrent edit bumps it before you close, " +
					"re-fetch via skein_show and retry.",
				Args: closeArgs,
			},
		}

		resp := moveResponse{
			Supported: false,
			Reason: "Skein has no in-place project change for issues. The canonical " +
				"workflow is copy-to-target + `supersedes` edge + close source with " +
				"reason. This tool returns a pre-filled kit for that workflow — " +
				"execute the two `nextSteps` in order to complete the move. Why no " +
				"in-place move: the hierarchical id is embedded in git commits, chats, " +
				"and other external refs — renumbering breaks them, and swapping " +
				"`project_id` while keeping the id creates visual incongruity in the " +
				"target's listing. Copy+supersedes is the honest mechanism that " +
				"preserves provenance without breaking external pointers.",
			Source: sourceCard,
			TargetProject: moveTargetProject{
				ID:       targetProj.ID.String(),
				Name:     targetProj.Name,
				IDPrefix: targetProj.IDPrefix,
			},
			TargetComponents: moveComps,
			NextSteps:        nextSteps,
		}

		slog.InfoContext(ctx, "mcp tool ok",
			"tool", "skein_move",
			"userId", userID.String(),
			"sourceIssue", source.ID,
			"sourceProject", source.ProjectID.String(),
			"targetProject", targetProj.IDPrefix,
			"componentCount", len(moveComps),
			"latencyMs", time.Since(start).Milliseconds())
		return skmcp.ToolJSON(resp)
	}
}
