package outbox

// push_invitation.go — the third payload kind of the push tract: a space
// invitation (sk-674.18 T040, US-5: "email, push and an Inbox card"
// to an existing addressee; "Acceptance — push … to the inviter").
//
// The row is written by the invitations package in the transaction of the
// invitation (notifications.Reason.DirectWriter), with an already published
// event of type space.invitation_sent or space.invitation_accepted whose
// payload names the invitation. It has no project and no issue: the
// row-level access gate of the issue kind does not apply, and the re-check
// here is the invitation's own — at send time, as for every other kind:
//
//   - sent: the invitation is still pending and unexpired, its space exists,
//     and the recipient is still its addressee (an active, non-external
//     account with the invited email — an external person is no party of a
//     space, K12). Otherwise the invitation lost its force — accepted,
//     declined, revoked, expired, its space deleted — and «you are invited»
//     would invite to nothing: finished as configuration_no_send /
//     invitation_stale, never retried.
//   - accepted: the recipient is still the invitation's sender, active and
//     not external.
//
// The texts the users typed — the space name, the usernames — are cut to a
// bounded length and stripped of control characters (spec "Constraints":
// user text in an email or a push is escaped and bounded; a push is plain
// text, nothing in it is a link). The URL carries no token: it opens the
// web UI, where the Inbox shows the invitation card.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"skein-api/internal/notifications"
)

// invitationTextMax bounds a user-typed name in an invitation push.
const invitationTextMax = 80

// invitationFacts is what the send-time re-check reads.
type invitationFacts struct {
	eventType string
	// actor is events.actor — the username of whoever accepted.
	actor     string
	live      bool // pending, unexpired, the space exists
	addressee bool // the recipient is the invited email's active account
	sender    bool // the recipient is the sender, active
	spaceName string
	inviter   string
}

// buildInvitation re-checks the invitation of the row and renders its
// payload. ok=false means the row is already finalised, or deliberately left
// leased after a failed read (the lease expiry retries it).
func (r *PushRunner) buildInvitation(ctx, work context.Context, c pushClaim) ([]byte, bool) {
	var f invitationFacts
	var spaceName *string
	err := r.pool.QueryRow(work, `
SELECT e.type, e.actor,
       i.status = 'pending' AND i.expires_at > now() AND i.space_id IS NOT NULL,
       u.deleted_at IS NULL AND u.deactivated_at IS NULL AND NOT u.is_external AND lower(btrim(u.email)) = i.email,
       u.deleted_at IS NULL AND u.deactivated_at IS NULL AND NOT u.is_external AND i.invited_by = u.id,
       s.name, inv.username
  FROM events e
  JOIN space_invitations i ON i.id = (e.payload->>'invitationId')::uuid
  JOIN users inv ON inv.id = i.invited_by
  JOIN users u ON u.id = $2
  LEFT JOIN spaces s ON s.id = i.space_id
 WHERE e.id = $1 AND e.type IN ($3, $4)`,
		c.eventID, c.userID,
		notifications.EventTypeSpaceInvitationSent, notifications.EventTypeSpaceInvitationAccepted,
	).Scan(&f.eventType, &f.actor, &f.live, &f.addressee, &f.sender, &spaceName, &f.inviter)
	if errors.Is(err, pgx.ErrNoRows) {
		slog.WarnContext(ctx, "push not sent: the event resolves to no invitation",
			"notificationId", c.id, "eventId", c.eventID, "userId", c.userID.String())
		r.finalise(ctx, c, outcomeConfigurationNoSend, causeEventUnresolvable)
		return nil, false
	}
	if err != nil {
		slog.WarnContext(ctx, "push deferred: invitation lookup failed",
			"notificationId", c.id, "eventId", c.eventID, "error", err.Error())
		return nil, false
	}
	if spaceName != nil {
		f.spaceName = *spaceName
	}
	var holds bool
	switch f.eventType {
	case notifications.EventTypeSpaceInvitationSent:
		holds = f.live && f.addressee
	default:
		holds = f.sender
	}
	if !holds {
		r.finalise(ctx, c, outcomeConfigurationNoSend, causeInvitationStale)
		return nil, false
	}
	payload, err := json.Marshal(buildInvitationPayload(f, r.appBaseURL))
	if err != nil {
		slog.ErrorContext(ctx, "push not sent: payload marshal failed",
			"notificationId", c.id, "error", err.Error())
		r.retryOrExhaust(ctx, c)
		return nil, false
	}
	return payload, true
}

// buildInvitationPayload renders an invitation push: the bold line says what
// happened and who did it, the body names the space; the URL is the web UI's
// root, where the Inbox carries the invitation card. IssueID is empty — the
// push is about no issue.
func buildInvitationPayload(f invitationFacts, appBaseURL string) pushPayload {
	var title string
	switch f.eventType {
	case notifications.EventTypeSpaceInvitationSent:
		title = "✉️ Invitation to a space · " + plainText(f.inviter)
	default:
		title = "🤝 Invitation accepted · " + plainText(f.actor)
	}
	url := "/"
	if base := strings.TrimRight(appBaseURL, "/"); base != "" {
		url = base + "/"
	}
	return pushPayload{
		Title:    title,
		Body:     plainText(f.spaceName),
		URL:      url,
		Category: string(notifications.ReasonKeySpaceInvitation),
	}
}

// plainText is a user-typed name as a push may show it: control characters
// dropped, runs of whitespace folded into one space, at most
// invitationTextMax characters (an ellipsis marks a cut).
func plainText(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case unicode.IsControl(r) || r == utf8.RuneError:
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	out := b.String()
	if utf8.RuneCountInString(out) > invitationTextMax {
		out = string([]rune(out)[:invitationTextMax-1]) + "…"
	}
	return out
}
