package issues

// creator_projection.go — sk-565 (P3, P13): the creator's data an issue
// create reads, gathered in ONE primary-key read per transaction attempt.
//
// WHY ONE READ. Every issue of a create call (a single issue, or an epic
// with up to 50 children) needs the creator's personal defaults. Reading
// them per issue was 51 reads for a full epic; the spec allows one read of
// the settings row per call, inside the create transaction ("Perf", D4,
// INV-5: every issue of the call is decided by the same value).
//
// WHY MODE-DEPENDENT. What is selected depends on what the call needs:
//   - username and assign_agent_issues_to_me — only when at least one issue
//     of the call has AssignCreatorByDefault (P1, the MCP paths). A REST
//     create never names the new column (INV-4 to the letter);
//   - review_required_default — only when at least one issue of the call
//     has no explicit ReviewRequired; with an explicit value on every issue
//     nothing is read for it (P13 keeps sk-303.14's behaviour);
//   - nothing needed — nothing is read.
//
// WHY PER ATTEMPT. The projection is built inside the withSerializableRetry
// closure, so an aborted attempt's values never reach the attempt that
// commits: the committed attempt applies what ITS transaction sees.
//
// The read never writes: a creator without a prefs row gets the factory
// values (notifications.DefaultAssignAgentIssuesToMe,
// review.DefaultReviewRequired) and still has no row afterwards (INV-10).

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/notifications"
	"skein-api/internal/review"
)

// creatorProjection is the per-attempt, lazily loaded view of the creator.
// Not safe for concurrent use; one attempt runs on one goroutine.
type creatorProjection struct {
	creatorID uuid.UUID

	// What the call needs, fixed at construction.
	wantAssign bool
	wantReview bool

	loaded                bool
	username              string
	assignByDefault       bool
	reviewRequiredDefault bool
}

// newCreatorProjection decides, from every input of one create call, which
// creator data the call will need. Nothing is read here.
func newCreatorProjection(creatorID uuid.UUID, inputs ...*CreateIssueInput) *creatorProjection {
	p := &creatorProjection{creatorID: creatorID}
	for _, in := range inputs {
		if in.AssignCreatorByDefault {
			p.wantAssign = true
		}
		if in.ReviewRequired == nil {
			p.wantReview = true
		}
	}
	return p
}

// creatorProjectionAssignSQL / ...AssignReviewSQL are the MCP-mode reads:
// the creator's users row joined to the (possibly absent) prefs row, by
// primary key. LEFT JOIN, because the prefs row is created lazily.
const (
	creatorProjectionAssignSQL = `
SELECT u.username, p.assign_agent_issues_to_me
  FROM users u
  LEFT JOIN user_notification_prefs p ON p.user_id = u.id
 WHERE u.id = $1`

	creatorProjectionAssignReviewSQL = `
SELECT u.username, p.assign_agent_issues_to_me, p.review_required_default
  FROM users u
  LEFT JOIN user_notification_prefs p ON p.user_id = u.id
 WHERE u.id = $1`

	// creatorProjectionReviewSQL is the review-only read — the statement
	// sk-303.14 has always executed on the REST path.
	creatorProjectionReviewSQL = `
SELECT review_required_default
  FROM user_notification_prefs
 WHERE user_id = $1`
)

// load performs the single read, once per attempt.
func (p *creatorProjection) load(ctx context.Context, tx pgx.Tx) error {
	if p.loaded {
		return nil
	}
	p.assignByDefault = notifications.DefaultAssignAgentIssuesToMe
	p.reviewRequiredDefault = review.DefaultReviewRequired

	switch {
	case p.wantAssign:
		var assign, reviewDefault *bool
		var err error
		if p.wantReview {
			err = tx.QueryRow(ctx, creatorProjectionAssignReviewSQL, p.creatorID).
				Scan(&p.username, &assign, &reviewDefault)
		} else {
			err = tx.QueryRow(ctx, creatorProjectionAssignSQL, p.creatorID).
				Scan(&p.username, &assign)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("creator %s does not exist", p.creatorID)
		}
		if err != nil {
			return fmt.Errorf("reading creator projection for %s: %w", p.creatorID, err)
		}
		if assign != nil {
			p.assignByDefault = *assign
		}
		if reviewDefault != nil {
			p.reviewRequiredDefault = *reviewDefault
		}

	case p.wantReview:
		var personal bool
		err := tx.QueryRow(ctx, creatorProjectionReviewSQL, p.creatorID).Scan(&personal)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// No prefs row: the factory value set above stands.
		case err != nil:
			return fmt.Errorf("reading review_required_default for %s: %w", p.creatorID, err)
		default:
			p.reviewRequiredDefault = personal
		}
	}
	p.loaded = true
	return nil
}

// ReviewRequiredDefault returns the creator's personal «review required»
// default. Valid only when the projection was built for an input without
// an explicit ReviewRequired.
func (p *creatorProjection) ReviewRequiredDefault(ctx context.Context, tx pgx.Tx) (bool, error) {
	if !p.wantReview {
		return false, fmt.Errorf("creator projection: review default requested but not projected")
	}
	if err := p.load(ctx, tx); err != nil {
		return false, err
	}
	return p.reviewRequiredDefault, nil
}

// AssignDefault returns the creator's username and whether their personal
// setting assigns agent-created issues to them. Valid only when the
// projection was built for an input with AssignCreatorByDefault.
func (p *creatorProjection) AssignDefault(ctx context.Context, tx pgx.Tx) (username string, on bool, err error) {
	if !p.wantAssign {
		return "", false, fmt.Errorf("creator projection: assign default requested but not projected")
	}
	if err := p.load(ctx, tx); err != nil {
		return "", false, err
	}
	return p.username, p.assignByDefault, nil
}

// createAttempt is the state one withSerializableRetry attempt of a create
// call shares between the issues it inserts (the epic and its children).
// Built afresh inside the retry closure: nothing in it survives an aborted
// attempt.
type createAttempt struct {
	creator *creatorProjection
	// visible caches, per project, whether the creator sees it — for the
	// parent-chain walk of the default assignee (P5). Per attempt only.
	visible map[uuid.UUID]bool
}

// newCreateAttempt builds the per-attempt state for one create call.
func newCreateAttempt(creatorID uuid.UUID, inputs ...*CreateIssueInput) *createAttempt {
	return &createAttempt{creator: newCreatorProjection(creatorID, inputs...)}
}
