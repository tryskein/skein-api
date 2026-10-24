package notifications

// issue_create_prefs.go — sk-565 (D3, D8, INV-8, INV-10): the personal
// «assign the issues my agent creates to me» setting,
// user_notification_prefs.assign_agent_issues_to_me (migration 000058).
//
// This file is the store behind GET/PUT /api/v1/me/issue-create-prefs. It
// is NOT the read on the issue-create path: that one happens inside the
// creation transaction (D4) and lives with the issues store.
//
// A contract of its own rather than a field of review-prefs (D8): the
// review contract is the acknowledgement mechanic (its switch, its
// schedule); self-assignment on create is not part of it.
//
// THE LAZY ROW. Prefs rows are created on first write, so most people have
// none, and for them the reader answers the factory value (INV-8). Reading
// never creates a row (INV-10, SC-008).

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DefaultAssignAgentIssuesToMe is the factory value of the setting: on, for
// everyone, existing users included. It is what the reader substitutes for
// a missing prefs row, and it matches the column DEFAULT of 000058, so a row
// created by another setting's upsert reads the same.
const DefaultAssignAgentIssuesToMe = true

// IssueCreatePrefsDTO is the response body of GET and PUT
// /api/v1/me/issue-create-prefs.
type IssueCreatePrefsDTO struct {
	// AssignAgentIssuesToMe — when true, an issue the caller's agent creates
	// over MCP (skein_create, skein_epic_create) without an assignee is
	// assigned to the caller; standing issues and issues under someone
	// else's parent are excepted. REST issue creation does not read it.
	// Factory value: true.
	AssignAgentIssuesToMe bool `json:"assignAgentIssuesToMe"`
}

// UpdateIssueCreatePrefsInput is a partial update of the issue-create
// settings. A nil field means «not named — leave as is».
type UpdateIssueCreatePrefsInput struct {
	AssignAgentIssuesToMe *bool
}

// upsertAssignAgentIssuesToMe writes the flag and nothing else.
//
// One statement, no read beforehand: the merge happens on the row the
// statement locks, so a concurrent write of ANOTHER personal setting on the
// same row is kept (INV-10), and of two concurrent writes of this flag the
// one committed last wins.
//
// The INSERT branch names only the key, this flag and updated_at. Unlike
// upsertUnwatchOnClose it does not write suppress_agent_actions = true: the
// columns it leaves out take their schema DEFAULTs (000043, 000044, 000053),
// which match what their own readers substitute for a missing row — so
// creating the row here does not change the effective value of any other
// setting.
const upsertAssignAgentIssuesToMe = `
INSERT INTO user_notification_prefs (user_id, assign_agent_issues_to_me, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (user_id) DO UPDATE
   SET assign_agent_issues_to_me = EXCLUDED.assign_agent_issues_to_me,
       updated_at                = now()
RETURNING assign_agent_issues_to_me`

// GetIssueCreatePrefs returns the caller's issue-create settings. No prefs
// row → the factory value; the row is not created.
func (s *Store) GetIssueCreatePrefs(ctx context.Context, userID uuid.UUID) (IssueCreatePrefsDTO, error) {
	var on bool
	err := s.pool.QueryRow(ctx,
		`SELECT assign_agent_issues_to_me FROM user_notification_prefs WHERE user_id = $1`,
		userID).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return IssueCreatePrefsDTO{AssignAgentIssuesToMe: DefaultAssignAgentIssuesToMe}, nil
	}
	if err != nil {
		return IssueCreatePrefsDTO{}, fmt.Errorf("reading assign_agent_issues_to_me for %s: %w", userID, err)
	}
	return IssueCreatePrefsDTO{AssignAgentIssuesToMe: on}, nil
}

// UpdateIssueCreatePrefs applies a partial update and returns the
// post-state. An input naming nothing writes nothing — no row is created —
// and answers the current settings.
func (s *Store) UpdateIssueCreatePrefs(ctx context.Context, userID uuid.UUID, in UpdateIssueCreatePrefsInput) (IssueCreatePrefsDTO, error) {
	if in.AssignAgentIssuesToMe == nil {
		return s.GetIssueCreatePrefs(ctx, userID)
	}
	var on bool
	if err := s.pool.QueryRow(ctx, upsertAssignAgentIssuesToMe,
		userID, *in.AssignAgentIssuesToMe).Scan(&on); err != nil {
		return IssueCreatePrefsDTO{}, fmt.Errorf("upserting assign_agent_issues_to_me for %s: %w", userID, err)
	}
	return IssueCreatePrefsDTO{AssignAgentIssuesToMe: on}, nil
}
