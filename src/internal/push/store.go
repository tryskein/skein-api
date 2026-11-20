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

// DeleteGone removes the subscription the push service reported dead
// (404/410) — only while it is still the record that was sent to: the same
// id at the same version. A record registered again after the send (its
// version moved) or created anew at the same endpoint (another id) is a
// newer registration the answer does not speak about, and it stays.
// It reports whether a record was removed.
func (s *Store) DeleteGone(ctx context.Context, id uuid.UUID, version int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
DELETE FROM push_subscriptions WHERE id = $1 AND version = $2`, id, version)
	if err != nil {
		return false, fmt.Errorf("deleting gone push subscription: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListDeliverableForUser returns the subscriptions a delivery attempt may
// use: those of userID while the user is neither deactivated nor deleted,
// read in one statement, so the activity check is made once per attempt and
// the attempt sees the user's state either before or after a concurrent
// account event, never a mix. Records without a device label are included.
func (s *Store) ListDeliverableForUser(ctx context.Context, userID uuid.UUID) ([]Subscription, error) {
	rows, err := s.pool.Query(ctx, `
SELECT ps.id, ps.user_id, ps.endpoint, ps.p256dh, ps.auth, ps.ua, ps.created_at,
       ps.last_seen_at, ps.device_label, ps.version
  FROM push_subscriptions ps
  JOIN users u ON u.id = ps.user_id
 WHERE ps.user_id = $1
   AND u.deactivated_at IS NULL
   AND u.deleted_at IS NULL
 ORDER BY ps.created_at DESC, ps.id ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing deliverable push subscriptions: %w", err)
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
		return nil, fmt.Errorf("iterating deliverable push subscriptions: %w", err)
	}
	return out, nil
}

// PurgeInactiveOwners deletes every subscription whose owner is deactivated
// or deleted and returns how many were deleted. It runs at startup after the
// migrations: a release deployed again over a schema whose migration already
// ran does not repeat that migration's cleanup, so subscriptions an older
// release kept for users deactivated meanwhile are removed here. Device
// revocations are not touched. Idempotent.
func (s *Store) PurgeInactiveOwners(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
DELETE FROM push_subscriptions ps
 USING users u
 WHERE u.id = ps.user_id
   AND (u.deactivated_at IS NOT NULL OR u.deleted_at IS NOT NULL)`)
	if err != nil {
		return 0, fmt.Errorf("purging push subscriptions of inactive users: %w", err)
	}
	return int(tag.RowsAffected()), nil
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
