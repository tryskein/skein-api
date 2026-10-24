package issues

// default_assignee.go — sk-565 (US-1, US-6, D9, P2, P4, P5, P6): the
// «assign agent-created issues to me» default and its «someone else's
// parent» rule.
//
// THE RULE (spec, "Composition norm" in US-6). The default does not fire for
// an issue «under someone else»: walking up the chain of parents as it
// exists at create time, the nearest ancestor that HAS an assignee decides.
// That ancestor is the creator → the default works; anyone else, an entry
// that resolves to nobody, or an ancestor the creator cannot see → the issue
// stays unassigned. No assigned ancestor at all → the default works.
//
// THE VISIBILITY BOUNDARY (P5, "Security and visibility"). The walk reads,
// for each node, FIRST only its project, by the node's own id. A node in the
// creation project is visible without a check; any other project goes
// through projects.EnsureProjectVisible — the one predicate, with the
// external-admin ceiling. An invisible node ends the walk as «someone
// else's»: its assignee and its parent are never read. Only a visible node
// has its assignee and parent read, again by its own id — no statement joins
// across the boundary. Visibility is cached per create attempt
// (createAttempt) and never across attempts, so a membership revoked between
// an aborted attempt and the committed one is honoured.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/notifications"
	"skein-api/internal/projects"
)

// ancestorVerdict is what the chain of parents says about the default.
type ancestorVerdict uint8

const (
	// verdictNoAssignedAncestor — no ancestor has an assignee (or there is
	// no parent at all): the default works (US-6 item 5).
	verdictNoAssignedAncestor ancestorVerdict = iota
	// verdictOwner — the nearest assigned ancestor is the creator: the
	// default works (US-6 item 5).
	verdictOwner
	// verdictForeign — the nearest assigned ancestor is someone else, an
	// entry that resolves to no user, or an ancestor the creator cannot
	// see: the default does not fire (US-6 item 4).
	verdictForeign
)

// allowsDefault reports whether the default may assign the creator.
func (v ancestorVerdict) allowsDefault() bool { return v != verdictForeign }

// classifyAssignee tells the creator from everyone else by identity, not by
// string (P4, US-6 item 3): the creator written as username, e-mail or uuid
// is the creator; an entry resolving to nobody is someone else.
func classifyAssignee(ctx context.Context, tx pgx.Tx, assignee string, creatorID uuid.UUID) (ancestorVerdict, error) {
	id, err := notifications.ResolveAssigneeIdentity(ctx, tx, assignee)
	if err != nil {
		return verdictForeign, err
	}
	if id != uuid.Nil && id == creatorID {
		return verdictOwner, nil
	}
	return verdictForeign, nil
}

// declaredParentID is the parent the input declares (P2): ParentID, or
// without it the target of the first inline parent-child edge. A second
// parent is refused later in the same transaction (ErrParentAlreadySet), so
// the declared parent is the final one whenever the create commits.
func declaredParentID(input *CreateIssueInput) string {
	if input.ParentID != nil && *input.ParentID != "" {
		return *input.ParentID
	}
	for _, d := range input.Dependencies {
		if d.Type == "parent-child" && d.ToID != "" {
			return d.ToID
		}
	}
	return ""
}

// projectVisible answers, once per attempt and project, whether the creator
// sees the project. The creation project is visible by construction: the
// caller already had to see it to create there.
func (a *createAttempt) projectVisible(
	ctx context.Context, tx pgx.Tx, creationProject, project uuid.UUID, caller *DependenciesCaller,
) (bool, error) {
	if project == creationProject {
		return true, nil
	}
	if v, ok := a.visible[project]; ok {
		return v, nil
	}
	if caller == nil {
		return false, fmt.Errorf("DependenciesCaller is required to walk a cross-project parent chain")
	}
	_, err := projects.EnsureProjectVisible(ctx, tx, project, caller.UserID)
	switch {
	case err == nil:
		a.markVisible(project, true)
		return true, nil
	case errors.Is(err, projects.ErrProjectNotVisible):
		a.markVisible(project, false)
		return false, nil
	default:
		return false, fmt.Errorf("checking ancestor project visibility: %w", err)
	}
}

func (a *createAttempt) markVisible(project uuid.UUID, visible bool) {
	if a.visible == nil {
		a.visible = make(map[uuid.UUID]bool)
	}
	a.visible[project] = visible
}

// chainVerdict walks up from startID (the declared parent) and returns the
// verdict of the nearest assigned ancestor (P5).
func chainVerdict(
	ctx context.Context,
	tx pgx.Tx,
	att *createAttempt,
	creationProject uuid.UUID,
	startID string,
	caller *DependenciesCaller,
	creatorID uuid.UUID,
) (ancestorVerdict, error) {
	seen := make(map[string]struct{})
	for cur := startID; cur != ""; {
		// (1) Only the node's project — the minimum the predicate needs.
		var project uuid.UUID
		err := tx.QueryRow(ctx, `SELECT project_id FROM issues WHERE id = $1`, cur).Scan(&project)
		if errors.Is(err, pgx.ErrNoRows) {
			// A declared parent that does not exist fails the create on its
			// own path (parent lookup / inline edge); here it ends the chain.
			return verdictNoAssignedAncestor, nil
		}
		if err != nil {
			return verdictForeign, fmt.Errorf("reading project of ancestor %s: %w", cur, err)
		}
		// (2)–(3) The boundary: an invisible node is «someone else's».
		visible, err := att.projectVisible(ctx, tx, creationProject, project, caller)
		if err != nil {
			return verdictForeign, err
		}
		if !visible {
			return verdictForeign, nil
		}
		// (5) A visible node met twice is a broken hierarchy.
		if _, dup := seen[cur]; dup {
			return verdictForeign, fmt.Errorf("parent chain of %s loops at %s", startID, cur)
		}
		seen[cur] = struct{}{}
		// (4) A visible node: its assignee and its parent, by its own id.
		var assignee, parent *string
		if err := tx.QueryRow(ctx, `
SELECT i.assignee, `+parentIDCurrentExpr+`
  FROM issues i
 WHERE i.id = $1`, cur).Scan(&assignee, &parent); err != nil {
			return verdictForeign, fmt.Errorf("reading ancestor %s: %w", cur, err)
		}
		if assignee != nil && *assignee != "" {
			return classifyAssignee(ctx, tx, *assignee, creatorID)
		}
		if parent == nil {
			return verdictNoAssignedAncestor, nil
		}
		cur = *parent
	}
	return verdictNoAssignedAncestor, nil
}
