// Package push implements the Web Push foundation: storage of browser push
// subscriptions and a Send helper that signs payloads with VAPID and POSTs
// them to the browser's push service. This is infrastructure only — wiring
// push into the event/notification fan-out is a separate ticket.
package push

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Subscription is the wire + storage shape of a browser push registration.
// JSON tags match the camelCase convention used across the API surface.
// p256dh / auth are the RFC 8291 encryption material; they are NOT secrets
// in the credential sense (they're browser-public ECDH material), but we
// still avoid logging them in full from the Send path.
type Subscription struct {
	ID         uuid.UUID `json:"id"`
	UserID     uuid.UUID `json:"userId"`
	Endpoint   string    `json:"endpoint"`
	P256dh     string    `json:"p256dh"`
	Auth       string    `json:"auth"`
	UA         *string   `json:"ua,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	// DeviceLabel is the device consent label created by the browser; nil
	// for a subscription registered without one.
	DeviceLabel *uuid.UUID `json:"deviceLabel"`
	// Version grows with every write of the record; conditional removals
	// match on id and version.
	Version int64 `json:"version"`
}

// Store is the push-subscriptions repository over a pgx pool.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wires a Store around the shared pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const subscriptionColumns = `id, user_id, endpoint, p256dh, auth, ua, created_at, last_seen_at, device_label, version`

// scanSubscription reads one row into a Subscription.
func scanSubscription(row pgx.Row) (Subscription, error) {
	var (
		s  Subscription
		ua *string
	)
	if err := row.Scan(&s.ID, &s.UserID, &s.Endpoint, &s.P256dh, &s.Auth, &ua, &s.CreatedAt, &s.LastSeenAt, &s.DeviceLabel, &s.Version); err != nil {
		return Subscription{}, err
	}
	s.UA = ua
	return s, nil
}

// DeleteByEndpointAny removes a subscription by endpoint regardless of owner.
// Used by Send when the push service reports the endpoint is gone (404/410):
// the registration is dead, so we prune it no matter which user it belongs
// to. Idempotent.
func (s *Store) DeleteByEndpointAny(ctx context.Context, endpoint string) error {
	_, err := s.pool.Exec(ctx, `
DELETE FROM push_subscriptions WHERE endpoint = $1`, endpoint)
	if err != nil {
		return fmt.Errorf("deleting push subscription by endpoint: %w", err)
	}
	return nil
}

// ListForUser returns every subscription owned by userID, newest-first, so a
// user can review and manage the devices registered for push.
func (s *Store) ListForUser(ctx context.Context, userID uuid.UUID) ([]Subscription, error) {
	rows, err := s.pool.Query(ctx, `
SELECT `+subscriptionColumns+`
  FROM push_subscriptions
 WHERE user_id = $1
 ORDER BY created_at DESC, id ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing push subscriptions: %w", err)
	}
	defer rows.Close()

	out := make([]Subscription, 0, 4)
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning push subscription: %w", err)
		}
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating push subscriptions: %w", err)
	}
	return out, nil
}
