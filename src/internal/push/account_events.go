package push

// account_events.go — account revocation of push subscriptions. An account
// event (password reset by link, deactivation on any path, removal of an
// external user from the last project, account deletion) deletes every
// subscription of the user in the event's own transaction, after the event
// has written the user's row: a push write of the user locks that row FOR
// SHARE first, so a write overlapping the event either waits for it and is
// refused, or is ordered before it and its subscription is deleted here.
// Device labels are not revoked: the event is not a device revocation.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Reasons of an account event that deletes push subscriptions — the
// "reason" field of the log record.
const (
	// ReasonPasswordReset — the password was reset by an emailed link.
	ReasonPasswordReset = "password_reset"
	// ReasonDeactivation — the user was deactivated (administrator,
	// operator, an agent with the right).
	ReasonDeactivation = "deactivation"
	// ReasonExternalOrphaned — an external user lost the last project and
	// was deactivated with it.
	ReasonExternalOrphaned = "external_orphaned"
	// ReasonAccountDeleted — the account was deleted.
	ReasonAccountDeleted = "account_deleted"
)

// DeleteAllForUserInTx deletes every push subscription of userID inside tx
// and returns how many were deleted. Call it after the event has written
// the users row, in the same transaction; reason is one of the Reason*
// constants. It does not log: the caller logs with LogAccountEventRemoval
// after its transaction committed, with the number of the last attempt.
func DeleteAllForUserInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, reason string) (int, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM push_subscriptions WHERE user_id = $1`, userID)
	if err != nil {
		return 0, fmt.Errorf("deleting push subscriptions on %s: %w", reason, err)
	}
	return int(tag.RowsAffected()), nil
}

// LogAccountEventRemoval writes the Information record of push
// subscriptions deleted by a committed account event: the user, the reason
// and the number deleted.
func LogAccountEventRemoval(ctx context.Context, userID uuid.UUID, reason string, removed int) {
	slog.InfoContext(ctx, "push subscriptions removed by an account event",
		"userId", userID.String(), "reason", reason, "removed", removed)
}
