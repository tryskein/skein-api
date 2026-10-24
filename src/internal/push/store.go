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
}

// Store is the push-subscriptions repository over a pgx pool.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wires a Store around the shared pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const subscriptionColumns = `id, user_id, endpoint, p256dh, auth, ua, created_at, last_seen_at`

// scanSubscription reads one row into a Subscription.
func scanSubscription(row pgx.Row) (Subscription, error) {
	var (
		s  Subscription
		ua *string
	)
	if err := row.Scan(&s.ID, &s.UserID, &s.Endpoint, &s.P256dh, &s.Auth, &ua, &s.CreatedAt, &s.LastSeenAt); err != nil {
		return Subscription{}, err
	}
	s.UA = ua
	return s, nil
}

// Upsert inserts a subscription for userID, or — when a row already exists
// for the same endpoint — re-homes it to userID and refreshes its keys /
// User-Agent / last_seen_at. The endpoint unique index is the conflict
// target: a browser re-subscribing sends the same endpoint, so this is the
// idempotent "register this device" path. Returns the stored row.
func (s *Store) Upsert(ctx context.Context, userID uuid.UUID, endpoint, p256dh, auth, ua string) (Subscription, error) {
	var uaArg any
	if ua != "" {
		uaArg = ua
	}
	row := s.pool.QueryRow(ctx, `
INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth, ua, created_at, last_seen_at)
VALUES ($1, $2, $3, $4, $5, now(), now())
ON CONFLICT (endpoint) DO UPDATE
   SET user_id      = EXCLUDED.user_id,
       p256dh       = EXCLUDED.p256dh,
       auth         = EXCLUDED.auth,
       ua           = EXCLUDED.ua,
       last_seen_at = now()
RETURNING `+subscriptionColumns,
		userID, endpoint, p256dh, auth, uaArg)
	sub, err := scanSubscription(row)
	if err != nil {
		return Subscription{}, fmt.Errorf("upserting push subscription: %w", err)
	}
	return sub, nil
}

// DeleteByEndpoint removes the caller's subscription identified by endpoint.
// Scoped to userID so a caller can only unsubscribe their own device. A
// missing row is a silent no-op (idempotent unsubscribe) — DELETE returns no
// error when zero rows match.
func (s *Store) DeleteByEndpoint(ctx context.Context, userID uuid.UUID, endpoint string) error {
	_, err := s.pool.Exec(ctx, `
DELETE FROM push_subscriptions WHERE user_id = $1 AND endpoint = $2`, userID, endpoint)
	if err != nil {
		return fmt.Errorf("deleting push subscription: %w", err)
	}
	return nil
}

// DeleteByIDForUser removes the caller's subscription identified by its row
// id. Scoped to userID so a caller can only delete a device they own — a row
// belonging to another user (or a missing id) leaves the table untouched.
// Returns whether a row was actually deleted so the handler can answer 404 on
// a miss while keeping the SQL single-statement. This is the id-keyed sibling
// of DeleteByEndpoint: the management UI lists devices by id and deletes any
// of them by id, without the browser having to hold the matching endpoint.
func (s *Store) DeleteByIDForUser(ctx context.Context, userID uuid.UUID, id uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
DELETE FROM push_subscriptions WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return false, fmt.Errorf("deleting push subscription by id: %w", err)
	}
	return tag.RowsAffected() > 0, nil
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
