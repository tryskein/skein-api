package invitations

// notify.go — what an invitation tells people, in the transaction of the
// invitation (sk-674.18 T040, US-5, US-19, INV-25 by analogy: a rolled-back
// action tells nobody):
//
//   - the push (notifications.ReasonKeySpaceInvitation, a direct-writer
//     reason): an already published event plus a push-only notification row
//     of type "space_invitation" — to the invited person when the invitation
//     is sent (created or sent again), to the sender when it is accepted. The
//     push tract re-checks at send time that the invitation still holds
//     (outbox/push_invitation.go). Written only for an active, non-external
//     account (a blocked person is no addressee of an Inbox; the lead's
//     decision 2026-10-14) and only while the recipient's push channel for
//     the reason is on — no row at all otherwise (INV-1 of notifications);
//   - the personal signals (K-18): `spaces` with the reason of the contract
//     and `inbox` where the Inbox changes — published after commit by
//     signals.BeginTxFunc.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/notifications"
	"skein-api/internal/signals"
	"skein-api/internal/spaces"
)

// pushEnabledInTx reads whether recipient's push channel for space
// invitations is on: his stored choice, or the registry's default.
func pushEnabledInTx(ctx context.Context, tx pgx.Tx, recipient uuid.UUID) (bool, error) {
	spec, ok := notifications.LookupReason(notifications.ReasonKeySpaceInvitation)
	if !ok || !spec.Push.Applicable {
		return false, nil
	}
	var on bool
	err := tx.QueryRow(ctx, `
SELECT push_enabled FROM user_notification_reason_prefs WHERE user_id = $1 AND reason = $2`,
		recipient, string(notifications.ReasonKeySpaceInvitation)).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return spec.Push.DefaultOn, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading the push preference: %w", err)
	}
	return on, nil
}

// pushInTx writes the push of eventType (EventTypeSpaceInvitationSent or
// EventTypeSpaceInvitationAccepted) about invitationID to recipient: the
// event and its push-only notification row, unless the recipient's channel
// is off.
func pushInTx(ctx context.Context, tx pgx.Tx, recipient uuid.UUID, eventType string, spaceID uuid.UUID, spaceShort string,
	invitationID uuid.UUID, actor spaces.Actor) error {
	on, err := pushEnabledInTx(ctx, tx, recipient)
	if err != nil || !on {
		return err
	}
	payload, err := json.Marshal(map[string]string{"invitationId": invitationID.String(), "spaceId": spaceShort})
	if err != nil {
		return err
	}
	var userArg, sessionArg any
	if actor.UserID != uuid.Nil {
		userArg = actor.UserID
	}
	if actor.SessionID != "" {
		sessionArg = actor.SessionID
	}
	actorType := actor.Type
	if actorType == "" {
		actorType = "system"
	}
	// Already published: the fan-out never reads it (Reason.DirectWriter) —
	// the row below is this event's only delivery.
	var eventID int64
	if err := tx.QueryRow(ctx, `
INSERT INTO events (type, actor, actor_user_id, actor_session_id, actor_type,
                    entity_type, entity_id, project_id, payload, published_at)
VALUES ($1, $2, $3, $4, $5, 'space', $6, NULL, $7::jsonb, now())
RETURNING id`, eventType, actor.Username, userArg, sessionArg, actorType, spaceID.String(), string(payload)).Scan(&eventID); err != nil {
		return fmt.Errorf("writing the invitation event: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO notifications (
    user_id, event_id, type, project_id,
    reason, feed_channel,
    push_state, push_next_attempt_at, push_deadline_at,
    card_resolved)
VALUES ($1, $2, $3, NULL,
        $4, false,
        'pending', now(), now() + ($5::bigint * interval '1 second'),
        true)
ON CONFLICT (user_id, event_id) DO NOTHING`,
		recipient, eventID, NotificationType, string(notifications.ReasonKeySpaceInvitation),
		int64(notifications.PushDeliveryDeadline/time.Second)); err != nil {
		return fmt.Errorf("writing the invitation push: %w", err)
	}
	return nil
}

// NotificationType is the `type` of an invitation's notification row
// (spaces/contract.go: notification rows · type · "space_invitation").
const NotificationType = "space_invitation"

// signalInvited tells the invited account: `spaces` (invited, with the
// space's name for the toast) and `inbox` (the card and the badge).
func signalInvited(tx pgx.Tx, user uuid.UUID, spaceShort, spaceName string) {
	signals.AddSpaceEvent(tx, user, signals.SpaceEvent{Reason: signals.SpaceInvited, SpaceID: spaceShort, SpaceName: spaceName})
	signals.Add(tx, user, signals.AreaInbox)
}

// signalManagers tells the space's owner and the sender of an invitation —
// those who see its row in the space settings — with reason ("" for a plain
// `spaces` re-read), except the user who acted.
func signalManagers(ctx context.Context, tx pgx.Tx, spaceID uuid.UUID, spaceShort string, sender, actor uuid.UUID, reason signals.SpaceReason) error {
	var owner uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT owner_id FROM spaces WHERE id = $1`, spaceID).Scan(&owner); err != nil {
		return fmt.Errorf("reading the space owner: %w", err)
	}
	for _, u := range []uuid.UUID{owner, sender} {
		if u == actor {
			continue
		}
		if reason == "" {
			signals.Add(tx, u, signals.AreaSpaces)
			continue
		}
		signals.AddSpaceEvent(tx, u, signals.SpaceEvent{Reason: reason, SpaceID: spaceShort})
	}
	return nil
}
