package issues

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/notifications"
)

// creatorAutoWatches decides whether the author of a NEW issue is
// subscribed to it automatically with reason `creator` (sk-551.10, item 4).
//
// The author is the human who owns the api key, whoever actually typed
// the create — a person in the web UI or one of their agents over MCP.
// Subscribing that person to every issue their agents file is what made
// the Watching list a heap of thousands of closed subtasks: the owner
// never asked to follow those, an agent decided to split its own work.
//
// The owner's rule (2026-10-03):
//
//   - source=human — the person filed it themselves → subscribed;
//   - source=agent_directed AND top level (no parent) → subscribed: that
//     is the errand the person gave the agent, the one thing in the tree
//     they want to hear about;
//   - source=agent_directed WITH a parent → not subscribed: it is a step
//     inside an errand, and the errand's own subscription is where the
//     person watches it from;
//   - source=agent_autonomous, at any level → not subscribed: nobody
//     asked for it, the agent spawned it on its own.
//
// An unknown source is treated as human: validation rejects anything
// outside the closed set before a create reaches the store, so this
// branch exists only to keep the default on the side that loses no
// notifications.
//
// The assignee rule is separate and unchanged — an issue assigned to a
// person subscribes them as `assignee` whoever created it.
func creatorAutoWatches(source string, hasParent bool) bool {
	switch source {
	case SourceAgentAutonomous:
		return false
	case SourceAgentDirected:
		return !hasParent
	default:
		return true
	}
}

// issueHasParentInTx reports whether the issue currently has a parent in
// the canonical hierarchy (a parent-child edge — parentIDCurrentExpr), as
// seen inside tx. Both the create path (after its inline edges are
// written) and the reopen path decide the creator rule through it, so the
// two cannot read the hierarchy differently.
func issueHasParentInTx(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	var has bool
	if err := tx.QueryRow(ctx, `
SELECT `+parentIDCurrentExpr+` IS NOT NULL
  FROM issues i
 WHERE i.id = $1`, id).Scan(&has); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("reading parent of %s: %w", id, err)
	}
	return has, nil
}

// reAddAutoWatchersOnReopen restores the automatic subscriptions that the
// personal «unwatch on close» setting took away when the issue was closed
// (sk-551.10, item 5; sk-622.21 US-2).
//
// The setting writes no opt-out: the maintainers' wording is «reopened, and you
// are the author / assignee again — the subscription may come back». So on
// a reopen, for the issue's creator and its current assignee, IF that
// person has the setting on (no prefs row reads as on), the automatic
// reason they qualify for is added back through
// notifications.AddIssueWatcher — which keeps every existing guarantee: an
// opt-out recorded by an explicit unwatch (or an old-version tombstone)
// still wins, and an existing subscription only gains the reason.
//
// The caller runs this BEFORE fanning the reopen out (sk-622.21, US-6), so
// the people who come back receive the reopen notification as subscribers,
// with its Watching card.
//
// Only people with the setting on are touched. For everyone else the close
// removed nothing, so there is nothing to restore — and re-adding for them
// would subscribe, for example, the human author of an agent's subtask,
// which the creation rule deliberately left unsubscribed.
//
// The creator qualifies by the same rule as at creation
// (creatorAutoWatches), read against the issue's CURRENT parent: a reopen
// must not grant a subscription the create would have refused.
func reAddAutoWatchersOnReopen(ctx context.Context, tx pgx.Tx, row *issueRow) error {
	creator, assigneeID, err := roleContextOf(ctx, tx, row)
	if err != nil {
		return err
	}

	var source string
	if err := tx.QueryRow(ctx, `SELECT source FROM issues WHERE id = $1`, row.ID).Scan(&source); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("reading source for reopen auto-watch of %s: %w", row.ID, err)
	}
	hasParent, err := issueHasParentInTx(ctx, tx, row.ID)
	if err != nil {
		return err
	}

	type candidate struct {
		userID uuid.UUID
		reason string
	}
	cands := make([]candidate, 0, 2)
	if creator != uuid.Nil && creatorAutoWatches(source, hasParent) {
		cands = append(cands, candidate{creator, notifications.ReasonCreator})
	}
	if assigneeID != uuid.Nil {
		cands = append(cands, candidate{assigneeID, notifications.ReasonAssignee})
	}
	for _, c := range cands {
		on, err := notifications.UnwatchOnCloseEnabled(ctx, tx, c.userID)
		if err != nil {
			return err
		}
		if !on {
			continue
		}
		if err := notifications.AddIssueWatcher(ctx, tx, row.ID, c.userID, c.reason); err != nil {
			return err
		}
	}
	return nil
}
