package spaces

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Invitations addressed to a person — the second half of his Inbox (US-13:
// the Inbox badge is unaccepted issues plus the pending invitations to me;
// US-5: an agent sees them through MCP, without the token or the link).
// sk-674.18 T029. An invitation is addressed by email; the person's email is
// read from users by id, never taken from the request.

// PendingToMeWhere is the row predicate over space_invitations aliased si
// for the user bound at userPh: a valid pending invitation (it still holds a
// seat — the predicate of validPendingExpr) to his email, in a space that is
// not yet his. Its own alias: the header counts fold it into a statement
// whose issues reads use i.
func PendingToMeWhere(userPh string) string {
	return `si.status = 'pending' AND si.expires_at > now()
   AND si.email = (SELECT lower(btrim(me.email)) FROM users me WHERE me.id = ` + userPh + `)
   AND NOT EXISTS (SELECT 1 FROM space_members sm_me
                    WHERE sm_me.space_id = si.space_id AND sm_me.user_id = ` + userPh + `)`
}

// PendingInvitationsCountExpr is the number of pending invitations to the
// user bound at userPh, as a scalar subquery for a statement that folds it
// into a larger one (the header counts: inbox = unaccepted issues + this).
func PendingInvitationsCountExpr(userPh string) string {
	return `(SELECT count(*) FROM space_invitations si WHERE ` + PendingToMeWhere(userPh) + `)`
}

// PendingInvitation is one pending invitation to the caller, as an agent
// sees it in its Inbox: who invites to which space and until when — never
// the token or the link (INV-17: accepting is a person's act in the web UI).
type PendingInvitation struct {
	// ID is the invitation id (UUID).
	ID string `json:"id"`
	// SpaceID is the space short id.
	SpaceID string `json:"spaceId"`
	// SpaceName is the space name.
	SpaceName string `json:"spaceName"`
	// InvitedBy is the sender's username.
	InvitedBy string `json:"invitedBy"`
	// ExpiresAt is the expiry (RFC 3339, UTC).
	ExpiresAt string `json:"expiresAt"`
}

// PendingInvitationsTo lists the pending invitations to user, newest first.
func PendingInvitationsTo(ctx context.Context, q querier, user uuid.UUID) ([]PendingInvitation, error) {
	rows, err := q.Query(ctx, `
SELECT si.id, s.short_id, s.name, u.username,
       to_char(si.expires_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
  FROM space_invitations si
  JOIN spaces s ON s.id = si.space_id
  JOIN users u ON u.id = si.invited_by
 WHERE `+PendingToMeWhere("$1::uuid")+`
 ORDER BY si.created_at DESC, si.id`, user)
	if err != nil {
		return nil, fmt.Errorf("reading the invitations to %s: %w", user, err)
	}
	defer rows.Close()
	out := []PendingInvitation{}
	for rows.Next() {
		var p PendingInvitation
		var id uuid.UUID
		if err := rows.Scan(&id, &p.SpaceID, &p.SpaceName, &p.InvitedBy, &p.ExpiresAt); err != nil {
			return nil, fmt.Errorf("reading the invitations to %s: %w", user, err)
		}
		p.ID = id.String()
		out = append(out, p)
	}
	return out, rows.Err()
}

// IsInvitationTo reports whether id is an invitation addressed to user,
// whatever its state — the MCP accept path refuses it with
// CodeInvitationAgentForbidden instead of answering «no such issue»
// (INV-17).
func IsInvitationTo(ctx context.Context, q querier, id, user uuid.UUID) (bool, error) {
	var ok bool
	if err := q.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM space_invitations i
                WHERE i.id = $1
                  AND i.email = (SELECT lower(btrim(me.email)) FROM users me WHERE me.id = $2))`,
		id, user).Scan(&ok); err != nil {
		return false, fmt.Errorf("reading invitation %s: %w", id, err)
	}
	return ok, nil
}
