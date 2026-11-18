package mail

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// APIKeyNotifier queues the KindAPIKeyCreated notice of a new API key
// (sk-977.7.49) in the transaction that creates the key; it is the
// apikeys.CreatedNotifier of a mail-enabled instance. The notice goes to the
// owner's account address. An owner without an address, or whose account
// is deleted or deactivated, gets none — the key is created all the same.
type APIKeyNotifier struct {
	// DeliveryTTL is SKEIN_EMAIL_DELIVERY_TTL.
	DeliveryTTL time.Duration
}

// KeyCreatedInTx queues the notice about keyID to its owner userID in lang
// (ru or en) inside tx.
func (n APIKeyNotifier) KeyCreatedInTx(ctx context.Context, tx pgx.Tx, userID, keyID uuid.UUID, lang string) error {
	var email string
	err := tx.QueryRow(ctx, `
SELECT coalesce(btrim(email), '') FROM users
 WHERE id = $1 AND deleted_at IS NULL AND deactivated_at IS NULL`, userID).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && email == "") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the address of the key owner: %w", err)
	}
	if _, err := Enqueue(ctx, tx, Message{
		Kind: KindAPIKeyCreated, To: email, Lang: lang, UserID: userID, APIKeyID: keyID,
	}, n.DeliveryTTL); err != nil {
		return fmt.Errorf("queueing the api key notice: %w", err)
	}
	return nil
}
