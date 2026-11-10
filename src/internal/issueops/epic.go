package issueops

import (
	"context"
	"log/slog"
	"skein-api/internal/authz"
	"skein-api/internal/issues"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// epic.go — the create of an epic with its children in one transaction
// (MCP skein_epic_create, the public API's createEpic). There is no
// duplicate gate.

// EpicResult is the outcome of EpicCreate.
type EpicResult struct {
	// ProjectID is the project of the epic.
	ProjectID uuid.UUID
	// Epic and Children as read back after the commit; nil when the
	// read-back failed (ReadBack).
	Epic     *issues.IssueDetail
	Children []*issues.IssueDetail
	// ReadBack is set when the tree committed but could not be read back:
	// State is the epic's, Children the children's committed states.
	ReadBack *issues.ErrReadBackFailed
}

// EpicCreate creates an epic and its children for the caller userID from
// the arguments of skein_epic_create.
func EpicCreate(ctx context.Context, d Deps, userID uuid.UUID, a Args, o Origin) (EpicResult, *Failure) {
	role := CallerRole(ctx)

	// ---- Resolve project ----
	projectArg, err := a.RequireString("project")
	if err != nil {
		return EpicResult{}, Fail(ErrCodeValidationError, "project is required", nil)
	}
	projectID, perr := ResolveProjectHandle(ctx, d.Pool, d.Projects, projectArg, userID)
	if perr != nil {
		return EpicResult{}, ProjectFailure(perr)
	}

	// ---- Epic title + validation ----
	rawTitle, err := a.RequireString("title")
	if err != nil {
		return EpicResult{}, Fail(ErrCodeValidationError, "title is required", nil)
	}
	title, vErr := issues.ValidateTitle(rawTitle)
	if vErr != nil {
		return EpicResult{}, ValidationFailure(vErr)
	}

	// Epic priority — same default (3) as skein_create.
	priority := a.GetInt("priority", 3)
	if vErr := issues.ValidatePriority(priority); vErr != nil {
		return EpicResult{}, ValidationFailure(vErr)
	}

	description := a.GetString("description", "")
	design := a.GetString("design", "")
	acceptance := a.GetString("acceptanceCriteria", "")
	notes := a.GetString("notes", "")

	// Guard the epic's own free-text fields (sk-28). Children are
	// validated in epicChildTemplatesFromArgs below.
	for _, pair := range [...]struct{ field, val string }{
		{"description", description},
		{"design", design},
		{"acceptanceCriteria", acceptance},
		{"notes", notes},
	} {
		if vErr := issues.ValidateProse(pair.field, pair.val); vErr != nil {
			return EpicResult{}, ValidationFailure(vErr)
		}
	}

	// sk-605: a non-string assignee is refused before any write.
	assigneeArg, assigneeErr := AssigneeFromArgs(a.GetArguments(), "", nil)
	if assigneeErr != nil {
		return EpicResult{}, assigneeErr
	}
	// sk-565 (P9, US-4/US-5): the epic's own `unassigned`. It does not
	// propagate — each child has its own (parseChildTemplates).
	epicAssignByDefault, modeErr := AssignCreatorByDefault(a.GetArguments(), assigneeArg, "", nil)
	if modeErr != nil {
		return EpicResult{}, modeErr
	}
	var sourceContextArg *string
	if sc := strings.TrimSpace(a.GetString("sourceContext", "")); sc != "" {
		if vErr := issues.ValidateProse("sourceContext", sc); vErr != nil {
			return EpicResult{}, ValidationFailure(vErr)
		}
		sourceContextArg = &sc
	}

	// Top-level componentIds — caller-supplied UUIDs that attach to
	// the epic at create time. Child templates inherit the epic's
	// component set by default via the existing parent-component
	// inheritance path (issues.Store step 1b); per-child overrides
	// live in childTemplates[i].componentIds (parsed in
	// parseChildTemplates below). `Explicit=true` when the key was
	// present in the wire payload with a non-null value (a present
	// `[]` counts as "explicit no components, suppress inheritance";
	// a present `null` is treated as omitted because morally the
	// caller said nothing).
	args := a.GetArguments()
	epicComponentIDs, parseErr := ParseComponentIDs(args, "componentIds", nil)
	if parseErr != nil {
		return EpicResult{}, parseErr
	}
	epicComponentIDsExplicit := false
	if v, exists := args["componentIds"]; exists && v != nil {
		epicComponentIDsExplicit = true
	}

	// parentId — optional, and its absence is the legacy behaviour:
	// the epic lands at the project root. Shape is validated here so
	// a malformed handle fails as validation_error rather than as a
	// parent lookup miss inside the store.
	var parentIDArg *string
	if pid := strings.TrimSpace(a.GetString("parentId", "")); pid != "" {
		if !LooksLikeHierarchicalID(pid) {
			return EpicResult{}, Fail(ErrCodeValidationError,
				"parentId must be a full hierarchical id like \"demo-42\"",
				map[string]any{"parentId": pid})
		}
		parentIDArg = &pid
	}

	// Epic labels, canonicalised through the same validator
	// skein_create uses; per-child labels are parsed in
	// parseChildTemplates.
	//
	// Read from the raw arguments rather than through
	// a.GetStringSlice: that helper keeps the elements that assert
	// to string and DROPS the rest, so `labels: ["triage", 7]` would
	// create the epic with one label and no complaint. A label the
	// caller asked for and did not get must not vanish quietly, and
	// the per-child path right below rejects the same input — one
	// tool answering two ways about one field is worse than the
	// divergence from skein_create, which has the same silent drop.
	var epicLabels []string
	if rawLabels, exists := args["labels"]; exists && rawLabels != nil {
		rawSlice, okSlice := rawLabels.([]any)
		if !okSlice {
			return EpicResult{}, Fail(ErrCodeValidationError,
				"labels must be an array of strings", nil)
		}
		epicLabels = make([]string, 0, len(rawSlice))
		for j, raw := range rawSlice {
			str, okStr := raw.(string)
			if !okStr {
				return EpicResult{}, Fail(ErrCodeValidationError,
					"labels["+strconv.Itoa(j)+"] must be a string",
					map[string]any{"labelIndex": j})
			}
			canon, vErr := issues.ValidateLabel(str)
			if vErr != nil {
				return EpicResult{}, ValidationFailure(vErr)
			}
			epicLabels = append(epicLabels, canon)
		}
	}

	// reviewRequired — read from the RAW arguments, not via GetBool
	// with a default: "did not pass" and "passed false" are different
	// intents, and a default would silently cancel the author's
	// personal default on every call that does not mention the flag.
	reviewRequiredArg, reviewErr := CreateReviewRequiredArg(args)
	if reviewErr != nil {
		return EpicResult{}, reviewErr
	}

	// ---- Determine effective source from caller mode + kind ----
	// Mirrors skein_create — agents must supply `kind`; cookie-auth
	// (Web UI) callers force source=human and may omit kind.
	kindArg := strings.TrimSpace(a.GetString("kind", ""))
	var effectiveSource string
	if o == OriginKey {
		src, serr := issues.SourceForKeyWrite(kindArg)
		if serr != nil {
			return EpicResult{}, ValidationFailure(serr)
		}
		effectiveSource = src
	} else {
		callerMode, ok := CallerOriginMode(ctx)
		if !ok {
			slog.ErrorContext(ctx, "callerOriginMode could not classify caller",
				"op", "epic_create", "userId", userID.String())
			return EpicResult{}, Fail(ErrCodeInternalError,
				"ambiguous caller mode; cannot determine issue source", nil)
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
				return EpicResult{}, Fail(ErrCodeValidationError,
					"kind is required for agent callers; pass \"directed\" "+
						"(user-requested) or \"autonomous\" (side-quest)", nil)
			default:
				return EpicResult{}, Fail(ErrCodeValidationError,
					"kind must be one of: directed, autonomous",
					map[string]any{"kind": kindArg})
			}
		default:
			slog.ErrorContext(ctx, "callerOriginMode returned unexpected mode",
				"op", "epic_create", "mode", callerMode, "userId", userID.String())
			return EpicResult{}, Fail(ErrCodeInternalError,
				"unknown caller mode; cannot determine issue source",
				map[string]any{"mode": callerMode})
		}

		// ---- Parse childTemplates ----
	}

	childTemplates, parseResp := ParseChildTemplates(a.GetArguments())
	if parseResp != nil {
		return EpicResult{}, parseResp
	}

	// sk-674.18 T066: creating the epic and its children is ActionWrite
	// of US-6; naming an assignee on the epic or on any child is
	// ActionAssign too. Every input is parsed by now; nothing is written
	// before this gate.
	epicActions := []authz.Action{authz.ActionWrite}
	namesAssignee := assigneeArg != nil
	for _, ct := range childTemplates {
		namesAssignee = namesAssignee || ct.Assignee != nil
	}
	if namesAssignee {
		epicActions = append(epicActions, authz.ActionAssign)
	}
	if f := ProjectActionGate(ctx, d.Pool, "skein_epic_create", projectArg, projectID, userID, epicActions...); f != nil {
		return EpicResult{}, f
	}

	// ---- Build epic input ----
	epicInput := issues.CreateIssueInput{
		ParentID:             parentIDArg,
		Title:                title,
		Description:          description,
		Type:                 issues.TypeEpic,
		Priority:             priority,
		Source:               effectiveSource,
		SourceContext:        sourceContextArg,
		Assignee:             assigneeArg,
		Design:               design,
		AcceptanceCriteria:   acceptance,
		Notes:                notes,
		ComponentIDs:         epicComponentIDs,
		ComponentIDsExplicit: epicComponentIDsExplicit,
		Labels:               epicLabels,
		ReviewRequired:       reviewRequiredArg,
		// sk-565 (P1): see skein_create — the store applies the setting.
		AssignCreatorByDefault: epicAssignByDefault,
	}
	// DependenciesCaller is the resolved (uuid, role) the store uses
	// for the parent's cross-project visibility check — the same
	// trigger skein_create has since sk-35. Children need none: their
	// parent is the epic just inserted in this very transaction, in
	// this very project.
	if parentIDArg != nil && *parentIDArg != "" {
		epicInput.DependenciesCaller = &issues.DependenciesCaller{
			UserID: userID,
			Role:   role,
		}
	}

	project, err := d.Projects.GetProject(ctx, projectID)
	if err != nil {
		slog.ErrorContext(ctx, "issue operation error",
			"op", "epic_create",
			"reason", "load project failed",
			"userId", userID.String(), "projectId", projectID.String(), "error", err.Error())
		return EpicResult{}, Fail(ErrCodeInternalError, "failed to load project metadata", nil)
	}
	actor := IssuesAgentActor(ctx, d.Pool, userID)

	// ---- Atomic create ----
	epicDto, childDtos, err := d.Issues.CreateEpicWithChildren(ctx,
		projectID, project.IDPrefix, project.DefaultIDMode,
		epicInput, childTemplates, effectiveSource, userID, actor)
	if err != nil {
		if rb, committed := asReadBackFailed(err); committed {
			return EpicResult{ProjectID: projectID, ReadBack: rb}, nil
		}
		return EpicResult{}, MapEpicCreateError(ctx, err, userID, projectID)
	}
	return EpicResult{ProjectID: projectID, Epic: epicDto, Children: childDtos}, nil
}
