package issueops

import (
	"context"
	"errors"
	"log/slog"
	"skein-api/internal/authz"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"strings"
	"time"

	"github.com/google/uuid"
)

// create.go — the create of an issue (MCP skein_create, the public API's
// createIssue): the arguments are checked, the duplicate gate runs unless
// force or duplicateOf, then the store writes the issue with its inline
// edges in one transaction.

// CreateResult is the outcome of Create.
type CreateResult struct {
	// ProjectID is the project the issue is created in.
	ProjectID uuid.UUID
	// Decision — the duplicate gate found Duplicates and nothing was
	// created; the caller repeats with force or duplicateOf.
	Decision bool
	// Duplicates are the candidates of the gate (empty when it was not run
	// or found none).
	Duplicates []dependencies.DuplicateCandidate
	// Degraded — the gate ran on the lexical path only.
	Degraded bool
	// Issue is the created issue as read back after the commit; nil when
	// the read-back failed (ReadBack).
	Issue *issues.IssueDetail
	// ReadBack is set when the issue committed but its card could not be
	// read back: its State is the committed state.
	ReadBack *issues.ErrReadBackFailed
}

// Create creates an issue for the caller userID from the arguments of
// skein_create. o says how the kind argument becomes the issue's source.
func Create(ctx context.Context, d Deps, userID uuid.UUID, a Args, o Origin) (CreateResult, *Failure) {
	start := time.Now()
	role := CallerRole(ctx)

	// ---- Resolve project ----
	projectArg, err := a.RequireString("project")
	if err != nil {
		return CreateResult{}, Fail(ErrCodeValidationError, "project is required", nil)
	}
	projectID, perr := ResolveProjectHandle(ctx, d.Pool, d.Projects, projectArg, userID)
	if perr != nil {
		return CreateResult{}, ProjectFailure(perr)
	}

	// ---- Required inputs ----
	rawTitle, err := a.RequireString("title")
	if err != nil {
		return CreateResult{}, Fail(ErrCodeValidationError, "title is required", nil)
	}
	title, vErr := issues.ValidateTitle(rawTitle)
	if vErr != nil {
		return CreateResult{}, ValidationFailure(vErr)
	}

	rawType, err := a.RequireString("type")
	if err != nil {
		return CreateResult{}, Fail(ErrCodeValidationError, "type is required", nil)
	}
	itype, vErr := issues.ValidateType(rawType)
	if vErr != nil {
		return CreateResult{}, ValidationFailure(vErr)
	}

	// Priority default 3 (per spec — vision-level "low default" rather
	// than the REST default of 2). Clamp + validate per the [0,4] rule.
	priority := a.GetInt("priority", 3)
	if vErr := issues.ValidatePriority(priority); vErr != nil {
		return CreateResult{}, ValidationFailure(vErr)
	}

	// ---- Optional inputs ----
	description := a.GetString("description", "")
	design := a.GetString("design", "")
	acceptance := a.GetString("acceptanceCriteria", "")
	notes := a.GetString("notes", "")

	// Guard against tool-call wrapper literals leaking into free-text
	// fields (sk-28). Title is checked inside ValidateTitle above.
	for _, pair := range [...]struct{ field, val string }{
		{"description", description},
		{"design", design},
		{"acceptanceCriteria", acceptance},
		{"notes", notes},
	} {
		if vErr := issues.ValidateProse(pair.field, pair.val); vErr != nil {
			return CreateResult{}, ValidationFailure(vErr)
		}
	}

	var parentIDArg *string
	if pid := strings.TrimSpace(a.GetString("parentId", "")); pid != "" {
		if !LooksLikeHierarchicalID(pid) {
			return CreateResult{}, Fail(ErrCodeValidationError,
				"parentId must be a full hierarchical id like \"demo-42\"",
				map[string]any{"parentId": pid})
		}
		parentIDArg = &pid
	}

	// Component IDs — caller passes UUID strings; we parse them up
	// front so a malformed entry returns validation_error rather than
	// failing inside the store.
	rawComponentIDs := a.GetStringSlice("componentIds", nil)
	componentIDs := make([]uuid.UUID, 0, len(rawComponentIDs))
	for _, raw := range rawComponentIDs {
		id, perr := uuid.Parse(strings.TrimSpace(raw))
		if perr != nil {
			return CreateResult{}, Fail(ErrCodeValidationError,
				"componentIds[i] is not a UUID",
				map[string]any{"componentId": raw})
		}
		componentIDs = append(componentIDs, id)
	}
	// componentIdsExplicit signals the parent-inheritance gate at
	// the store level: when the caller supplied the `componentIds`
	// key (even as an empty array) we honour their value as-is and
	// skip inheritance; when the key is absent we let the store
	// inherit from the parent (if one is set, same-project, non-
	// empty). The mcp-go arg map only contains keys the caller
	// actually sent, so a presence check is the right signal.
	_, componentIDsExplicit := a.GetArguments()["componentIds"]

	rawLabels := a.GetStringSlice("labels", nil)
	labels := make([]string, 0, len(rawLabels))
	for _, l := range rawLabels {
		canon, vErr := issues.ValidateLabel(l)
		if vErr != nil {
			return CreateResult{}, ValidationFailure(vErr)
		}
		labels = append(labels, canon)
	}

	// sk-605: a non-string assignee is refused here, before the
	// duplicate gate and any write.
	assigneeArg, assigneeErr := AssigneeFromArgs(a.GetArguments(), "", nil)
	if assigneeErr != nil {
		return CreateResult{}, assigneeErr
	}

	// sk-565 (P9, US-4): `unassigned` is parsed from the raw arguments
	// right after `assignee` — before inline edges, duplicateOf and the
	// duplicate gate — so its refusals leave nothing behind and win over
	// decisionRequired (INV-9).
	assignByDefault, modeErr := AssignCreatorByDefault(a.GetArguments(), assigneeArg, "", nil)
	if modeErr != nil {
		return CreateResult{}, modeErr
	}

	// sk-674.18 T066: creating an issue is ActionWrite of US-6; naming
	// an assignee is ActionAssign too. Before inline edges, duplicateOf
	// and the duplicate gate — a refused caller learns nothing more.
	createActions := []authz.Action{authz.ActionWrite}
	if assigneeArg != nil {
		createActions = append(createActions, authz.ActionAssign)
	}
	if f := ProjectActionGate(ctx, d.Pool, "skein_create", projectArg, projectID, userID, createActions...); f != nil {
		return CreateResult{}, f
	}

	// ---- Inline dependencies (typed-edge wiring atomic with create) ----
	// Parsed up-front so a malformed entry short-circuits BEFORE the
	// duplicate-check round-trip and the issue insert. The store
	// re-validates type/gate inside the tx; we do shape + cap
	// validation here so the error message can name the failing index
	// before any DB work has happened.
	inlineDeps, depParseErr := ParseInlineDeps(a.GetArguments())
	if depParseErr != nil {
		return CreateResult{}, depParseErr
	}

	var sourceContextArg *string
	if sc := strings.TrimSpace(a.GetString("sourceContext", "")); sc != "" {
		if vErr := issues.ValidateProse("sourceContext", sc); vErr != nil {
			return CreateResult{}, ValidationFailure(vErr)
		}
		sourceContextArg = &sc
	}

	// idMode override + custom id. The default mode comes from the
	// project; we let the caller override via idMode. id is only used
	// when the resolved mode is "custom".
	project, err := d.Projects.GetProject(ctx, projectID)
	if err != nil {
		slog.ErrorContext(ctx, "issue operation error",
			"op", "create",
			"reason", "load project failed",
			"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
		return CreateResult{}, Fail(ErrCodeInternalError, "failed to load project metadata", nil)
	}
	defaultIDMode := project.DefaultIDMode
	if mode := strings.TrimSpace(a.GetString("idMode", "")); mode != "" {
		switch mode {
		case "counter", "hash", "custom":
			defaultIDMode = mode
		default:
			return CreateResult{}, Fail(ErrCodeValidationError,
				"idMode must be one of: counter, hash, custom",
				map[string]any{"idMode": mode})
		}
	}
	customID := strings.TrimSpace(a.GetString("id", ""))

	force := a.GetBool("force", false)

	// sk-118 (F2i): create-as-standing flag. When true the store inserts
	// status='standing' (no started_at, no auto-accept, no lock) and
	// enforces the nested-standing ban against the parentId. Default false
	// → the legacy open create path, byte-identical.
	standing := a.GetBool("standing", false)

	// sk-303.14 / T009: explicit pass of the "review required" flag.
	// The agent path is the only one that has it (Q3/A3); other paths
	// stay silent and get the author's personal default.
	//
	// Read from the RAW arguments, not via GetBool with a default:
	// "did not pass" and "passed false" are different intents, and a
	// default would glue them together, silently cancelling the
	// personal default on every call that does not mention the flag.
	reviewRequiredArg, reviewErr := CreateReviewRequiredArg(a.GetArguments())
	if reviewErr != nil {
		return CreateResult{}, reviewErr
	}

	var duplicateOfID string
	if d := strings.TrimSpace(a.GetString("duplicateOf", "")); d != "" {
		if !LooksLikeHierarchicalID(d) {
			return CreateResult{}, Fail(ErrCodeValidationError,
				"duplicateOf must be a full hierarchical id like \"demo-42\"",
				map[string]any{"duplicateOf": d})
		}
		duplicateOfID = d
	}

	// ---- Pre-check duplicateOf existence + visibility ----
	// The `duplicates` edge is written as an inline dependency inside
	// CreateIssue's transaction (sk-607), so an edge that fails rolls
	// the whole create back. This pre-check still answers the common
	// cases before any transaction opens, with duplicateOf's own
	// codes. A visibility miss renders identically to not-found per
	// the project visibility pattern (never leak forbidden vs
	// not-found across project boundaries). After this check, an edge
	// failure inside the transaction is a genuine race (target deleted
	// or hidden between check and create); mapDuplicateOfEdgeError
	// renders it, and nothing was created.
	if duplicateOfID != "" {
		target, terr := d.Issues.GetIssue(ctx, duplicateOfID)
		if terr != nil {
			if errors.Is(terr, issues.ErrNotFound) {
				return CreateResult{}, Fail(ErrCodeValidationError,
					"duplicateOf target issue does not exist",
					map[string]any{"duplicateOf": duplicateOfID})
			}
			slog.ErrorContext(ctx, "issue operation error",
				"op", "create",
				"reason", "duplicateOf precheck load failed",
				"userId", userID.String(), "duplicateOf", duplicateOfID, "error", terr.Error())
			return CreateResult{}, Fail(ErrCodeInternalError,
				"failed to verify duplicateOf target", nil)
		}
		if _, vErr := projects.EnsureProjectVisible(ctx, d.Pool, target.ProjectID, userID); vErr != nil {
			if errors.Is(vErr, projects.ErrProjectNotVisible) {
				return CreateResult{}, Fail(ErrCodeValidationError,
					"duplicateOf target issue does not exist",
					map[string]any{"duplicateOf": duplicateOfID})
			}
			slog.ErrorContext(ctx, "issue operation error",
				"op", "create",
				"reason", "duplicateOf precheck visibility failed",
				"userId", userID.String(), "duplicateOf", duplicateOfID, "error", vErr.Error())
			return CreateResult{}, Fail(ErrCodeInternalError,
				"failed to verify duplicateOf target", nil)
		}
	}

	// ---- Duplicate gate ----
	// Skip the gate when the caller has already declared the issue
	// as a duplicate of a known id, or has explicitly forced through.
	var duplicates []dependencies.DuplicateCandidate
	// Degradation of the duplicate check has to reach BOTH exits of
	// this handler (the decision-required short-circuit below and the
	// successful create at the bottom), so it is held here rather than
	// inside the gate block.
	duplicateCheckDegraded := false
	if duplicateOfID == "" && !force {
		report, err := d.Deps.CheckDuplicates(ctx, projectID, title, description)
		if err != nil {
			slog.ErrorContext(ctx, "issue operation error",
				"op", "create",
				"reason", "duplicate check failed",
				"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
			return CreateResult{}, Fail(ErrCodeInternalError, "duplicate check failed", nil)
		}
		cands := report.Candidates
		if report.Degraded {
			// Carried on BOTH outcomes below — a degraded check that
			// found nothing is the more dangerous of the two, because
			// the create succeeds and nothing else in the response
			// hints that the comparison was weakened.
			//
			// The prose reason stays OUT of the response: SC-007 pins
			// this payload under 250 characters, and on a deployment
			// with no embedder the reason would ride along on every
			// single create, forever, for no decision the boolean
			// doesn't already drive. It goes to the log (and to the
			// REST endpoint, which has no such budget); the meaning of
			// the flag is documented in skein_get_instructions.
			duplicateCheckDegraded = true
			slog.InfoContext(ctx, "duplicate check degraded",
				"op", "create",
				"projectId", projectID.String(),
				"reason", report.DegradedReason)
		}
		if len(cands) > 0 {
			slog.InfoContext(ctx, "issue operation ok",
				"op", "create",
				"userId", userID.String(), "projectId", projectID.String(),
				"outcome", "decision_required",
				"duplicateCount", len(cands),
				"duplicateCheckDegraded", duplicateCheckDegraded,
				"latencyMs", time.Since(start).Milliseconds())
			return CreateResult{ProjectID: projectID, Decision: true, Duplicates: cands, Degraded: duplicateCheckDegraded}, nil
		}
		duplicates = cands // empty slice
	}
	if duplicates == nil {
		duplicates = []dependencies.DuplicateCandidate{}
	}

	// ---- Determine effective source from caller mode + kind ----
	// Phase 1 ui-redesign: skein_create classifies the new issue's
	// origin. Caller mode is detected via api-key presence in ctx
	// (set by the MCP api-key middleware). Agent callers (api-key)
	// MUST pass `kind` and the value maps to one of agent_directed
	// / agent_autonomous. Cookie-auth callers (future BFF
	// passthrough) ignore kind and force source=human (Q-2 = a).
	// Missing or unknown kind → validation_error (Q-5 = fail-closed).
	kindArg := strings.TrimSpace(a.GetString("kind", ""))
	var effectiveSource string
	if o == OriginKey {
		src, serr := issues.SourceForKeyWrite(kindArg)
		if serr != nil {
			return CreateResult{}, ValidationFailure(serr)
		}
		effectiveSource = src
	} else {
		callerMode, ok := CallerOriginMode(ctx)
		if !ok {
			// Fail-closed: neither api-key nor cookie auth populated
			// ctx. RequireUser above already validated UserID is set,
			// so this branch should be unreachable in production —
			// but if a future middleware refactor regresses, returning
			// internal_error makes the regression loud instead of
			// silently mis-tagging the issue's source.
			slog.ErrorContext(ctx, "callerOriginMode could not classify caller — neither api-key nor cookie signal present",
				"userId", userID)
			return CreateResult{}, Fail(ErrCodeInternalError,
				"ambiguous caller mode; cannot determine issue source",
				nil)
		}
		switch callerMode {
		case "human":
			effectiveSource = issues.SourceHuman
		case "agent":
			switch kindArg {
			case "directed":
				effectiveSource = issues.SourceAgentDirected
			case "autonomous":
				effectiveSource = issues.SourceAgentAutonomous
			case "":
				return CreateResult{}, Fail(ErrCodeValidationError,
					"kind is required for agent callers; pass \"directed\" "+
						"(user-requested) or \"autonomous\" (side-quest)", nil)
			default:
				return CreateResult{}, Fail(ErrCodeValidationError,
					"kind must be one of: directed, autonomous",
					map[string]any{"kind": kindArg})
			}
		default:
			// callerOriginMode contract: ok=true ⇒ mode ∈ {"agent", "human"}.
			// Defensive against future enum widening that forgets to
			// extend this switch.
			slog.ErrorContext(ctx, "callerOriginMode returned unexpected mode",
				"mode", callerMode, "userId", userID)
			return CreateResult{}, Fail(ErrCodeInternalError,
				"unknown caller mode; cannot determine issue source",
				map[string]any{"mode": callerMode})
		}

	}

	// ---- Create the issue ----
	actor := IssuesAgentActor(ctx, d.Pool, userID)
	input := issues.CreateIssueInput{
		ID:                   customID,
		ParentID:             parentIDArg,
		Title:                title,
		Description:          description,
		Type:                 itype,
		Priority:             priority,
		Source:               effectiveSource,
		SourceContext:        sourceContextArg,
		Assignee:             assigneeArg,
		Design:               design,
		AcceptanceCriteria:   acceptance,
		Notes:                notes,
		ComponentIDs:         componentIDs,
		ComponentIDsExplicit: componentIDsExplicit,
		Labels:               labels,
		Dependencies:         inlineDeps,
		Standing:             standing,
		ReviewRequired:       reviewRequiredArg,
		// sk-565 (P1): the creator's «assign to me» setting decides
		// unless the caller passed unassigned:true. The store applies
		// it (D2) — this layer never substitutes an assignee itself.
		AssignCreatorByDefault: assignByDefault,
	}
	// sk-607: the `duplicates` edge of duplicateOf is written in the
	// create's own transaction, as one more inline edge — not by a
	// second write after the commit, which a context ending in between
	// turned into internal_error for an issue that existed. The
	// pre-check above still runs first; a failure of this edge now
	// rolls the create back (dupEdgeIndex maps it to the duplicateOf
	// codes). An edge the caller already listed inline is not added
	// twice.
	dupEdgeIndex := -1
	if duplicateOfID != "" && !HasInlineEdge(inlineDeps, duplicateOfID, dependencies.DepDuplicates) {
		inlineDeps = append(inlineDeps, issues.DependencyEdgeInput{
			ToID: duplicateOfID,
			Type: dependencies.DepDuplicates,
		})
		dupEdgeIndex = len(inlineDeps) - 1
	}
	input.Dependencies = inlineDeps
	// DependenciesCaller is the resolved (uuid, role) the store uses
	// for visibility checks. Required when inlineDeps is non-empty
	// (each edge runs cross-project visibility) AND, since sk-35,
	// when ParentID is set (cross-project parents need the same
	// check). Set it once when either trigger fires.
	if len(inlineDeps) > 0 || (parentIDArg != nil && *parentIDArg != "") {
		input.DependenciesCaller = &issues.DependenciesCaller{
			UserID: userID,
			Role:   role,
		}
	}
	dto, err := d.Issues.CreateIssue(ctx, projectID, project.IDPrefix, defaultIDMode,
		input, userID, actor)
	if err != nil {
		if rb, committed := asReadBackFailed(err); committed {
			return CreateResult{ProjectID: projectID, Duplicates: duplicates, Degraded: duplicateCheckDegraded, ReadBack: rb}, nil
		}
		if f := MapDuplicateOfEdgeError(ctx, err, dupEdgeIndex, duplicateOfID, userID); f != nil {
			return CreateResult{}, f
		}
		return CreateResult{}, MapCreateError(ctx, err, userID, projectID)
	}
	return CreateResult{ProjectID: projectID, Duplicates: duplicates, Degraded: duplicateCheckDegraded, Issue: dto}, nil
}

// asReadBackFailed reports whether err is a write that committed but whose
// post-state could not be read back.
func asReadBackFailed(err error) (*issues.ErrReadBackFailed, bool) {
	var rb *issues.ErrReadBackFailed
	if errors.As(err, &rb) {
		return rb, true
	}
	return nil, false
}
