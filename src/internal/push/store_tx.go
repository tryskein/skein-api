package push

// store_tx.go — the transactional write path of push subscriptions.
//
// Every write runs in one SERIALIZABLE transaction (spaces.WithRetry: up to
// three attempts on a serialisation failure or deadlock). Its first command
// takes the caller's users row FOR SHARE and checks, at the moment of the
// write, that the caller is active and that the session epoch passed by the
// caller equals users.session_epoch; otherwise the write is refused with
// ErrSessionStale and nothing changes. An account event (deactivation,
// password reset, deletion) updates that row first, so a write overlapping
// it either waits for it and is refused, or is ordered before it and its
// subscriptions are removed by the event.
//
// The store does not log; the handler logs a write after it returns
// successfully. Error texts never carry an endpoint or keys.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/spaces"
)

var (
	// ErrSessionStale — at the moment of the write the caller is inactive
	// (deactivated or deleted, or no such user) or the session epoch
	// passed in differs from users.session_epoch (CodeSessionStale).
	ErrSessionStale = errors.New("push: session is stale")
	// ErrDeviceRevoked — the device label of the registration is revoked
	// for the caller (CodeDeviceRevoked).
	ErrDeviceRevoked = errors.New("push: device label is revoked")
	// ErrEndpointTaken — the endpoint belongs to another user's
	// subscription; that subscription is left unchanged (CodeEndpointTaken).
	ErrEndpointTaken = errors.New("push: endpoint belongs to another user")
	// ErrSubscriptionNotFound — the caller has no such subscription
	// (CodeSubscriptionNotFound).
	ErrSubscriptionNotFound = errors.New("push: subscription not found")
)

// RegisterInput is one registration of a browser push channel.
//
// The new form carries DeviceLabel and Mode (ModeUpsert or ModeUpdateOnly).
// The old form — a page or service worker that predates device labels —
// carries neither: it only refreshes the keys of the caller's existing
// subscription with that endpoint and never touches its label.
type RegisterInput struct {
	// UserID is the caller, the owner of the subscription.
	UserID uuid.UUID
	// SessionEpoch is the epoch of the caller's session; the write is
	// refused with ErrSessionStale unless it equals users.session_epoch.
	SessionEpoch int64
	// Endpoint is the push service URL of the channel.
	Endpoint string
	// P256dh and Auth are the encryption keys of the channel.
	P256dh string
	Auth   string
	// UA is the User-Agent recorded with the subscription; empty stores
	// NULL.
	UA string
	// DeviceLabel is the device consent label (new form); nil is the old
	// form.
	DeviceLabel *uuid.UUID
	// Mode is ModeUpsert or ModeUpdateOnly for the new form and empty for
	// the old form.
	Mode string
}

// VersionGuard makes a channel removal conditional: only the subscription
// with this id and this version is removed.
type VersionGuard struct {
	// ID is the subscription id.
	ID uuid.UUID
	// Version is the subscription version.
	Version int64
}

// withPushTx runs fn in one SERIALIZABLE transaction whose first command
// locks the caller's users row FOR SHARE and checks that the caller is
// active and that sessionEpoch is the current session epoch. fn runs once
// per attempt; values it computes must be assigned afresh on every attempt.
func (s *Store) withPushTx(ctx context.Context, callerID uuid.UUID, sessionEpoch int64, fn func(pgx.Tx) error) error {
	return spaces.WithRetry(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			active bool
			epoch  int64
		)
		err := tx.QueryRow(ctx, `
SELECT deactivated_at IS NULL AND deleted_at IS NULL, session_epoch
  FROM users
 WHERE id = $1
   FOR SHARE`, callerID).Scan(&active, &epoch)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSessionStale
		}
		if err != nil {
			return fmt.Errorf("locking push caller %s: %w", callerID, err)
		}
		if !active || epoch != sessionEpoch {
			return ErrSessionStale
		}
		return fn(tx)
	})
}

// Register writes a registration of the caller's channel and returns the
// stored subscription. In order, inside one transaction:
//
//  1. the caller is active and the session epoch is current, else
//     ErrSessionStale;
//  2. a new-form label revoked by the caller → ErrDeviceRevoked, whatever
//     the endpoint;
//  3. the endpoint belongs to another user → ErrEndpointTaken; that
//     subscription keeps its owner, keys, label and version;
//  4. the caller's own subscription with the endpoint gets the new keys and
//     User-Agent, the new-form label (the old form keeps the label),
//     version + 1 and last_seen_at = now();
//  5. no subscription with the endpoint: ModeUpsert creates one with the
//     label at version 1; ModeUpdateOnly and the old form →
//     ErrSubscriptionNotFound, nothing is created.
func (s *Store) Register(ctx context.Context, in RegisterInput) (Subscription, error) {
	newForm := in.DeviceLabel != nil
	switch {
	case newForm && in.Mode != ModeUpsert && in.Mode != ModeUpdateOnly:
		return Subscription{}, fmt.Errorf("registering push subscription: unknown mode %q", in.Mode)
	case !newForm && in.Mode != "":
		return Subscription{}, errors.New("registering push subscription: mode without a device label")
	}
	var uaArg any
	if in.UA != "" {
		uaArg = in.UA
	}

	var sub Subscription
	err := s.withPushTx(ctx, in.UserID, in.SessionEpoch, func(tx pgx.Tx) error {
		if newForm {
			var revoked bool
			if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM push_device_revocations WHERE user_id = $1 AND device_label = $2)`,
				in.UserID, *in.DeviceLabel).Scan(&revoked); err != nil {
				return fmt.Errorf("reading device revocation: %w", err)
			}
			if revoked {
				return ErrDeviceRevoked
			}
		}

		var (
			id    uuid.UUID
			owner uuid.UUID
		)
		err := tx.QueryRow(ctx, `
SELECT id, user_id FROM push_subscriptions WHERE endpoint = $1 FOR UPDATE`, in.Endpoint).Scan(&id, &owner)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if !newForm || in.Mode != ModeUpsert {
				return ErrSubscriptionNotFound
			}
			created, err := scanSubscription(tx.QueryRow(ctx, `
INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth, ua, device_label, version, created_at, last_seen_at)
VALUES ($1, $2, $3, $4, $5, $6, 1, now(), now())
RETURNING `+subscriptionColumns,
				in.UserID, in.Endpoint, in.P256dh, in.Auth, uaArg, *in.DeviceLabel))
			if err != nil {
				return fmt.Errorf("creating push subscription: %w", err)
			}
			sub = created
			return nil
		case err != nil:
			return fmt.Errorf("reading push subscription by endpoint: %w", err)
		case owner != in.UserID:
			return ErrEndpointTaken
		}

		// $5 is the new-form label; NULL keeps the label (old form).
		var labelArg any
		if newForm {
			labelArg = *in.DeviceLabel
		}
		updated, err := scanSubscription(tx.QueryRow(ctx, `
UPDATE push_subscriptions
   SET p256dh       = $2,
       auth         = $3,
       ua           = $4,
       device_label = COALESCE($5::uuid, device_label),
       version      = version + 1,
       last_seen_at = now()
 WHERE id = $1
RETURNING `+subscriptionColumns,
			id, in.P256dh, in.Auth, uaArg, labelArg))
		if err != nil {
			return fmt.Errorf("updating push subscription: %w", err)
		}
		sub = updated
		return nil
	})
	if err != nil {
		return Subscription{}, err
	}
	return sub, nil
}

// RevokeDevice revokes the caller's device labels and removes the
// caller's subscriptions of that device, in one transaction: each label is
// recorded as revoked for the caller (also when no subscription carries it;
// an already revoked label stays as it is), every subscription of the
// caller carrying one of the labels is deleted, and so is the caller's
// unlabelled subscription with endpoint (endpoint "" names none). Other
// users' subscriptions and revocations are never touched. A revoked label
// refuses every later registration of the caller with it, whatever the
// endpoint. Returns the number of subscriptions deleted.
func (s *Store) RevokeDevice(ctx context.Context, userID uuid.UUID, sessionEpoch int64, labels []uuid.UUID, endpoint string) (int, error) {
	if labels == nil {
		labels = []uuid.UUID{}
	}
	var removed int
	err := s.withPushTx(ctx, userID, sessionEpoch, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO push_device_revocations (user_id, device_label)
SELECT $1, label FROM unnest($2::uuid[]) AS label
ON CONFLICT (user_id, device_label) DO NOTHING`, userID, labels); err != nil {
			return fmt.Errorf("recording device revocations: %w", err)
		}
		tag, err := tx.Exec(ctx, `
DELETE FROM push_subscriptions
 WHERE user_id = $1
   AND (device_label = ANY($2::uuid[])
        OR (device_label IS NULL AND $3 <> '' AND endpoint = $3))`, userID, labels, endpoint)
		if err != nil {
			return fmt.Errorf("deleting revoked device subscriptions: %w", err)
		}
		removed = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// RemoveChannel removes the subscription with endpoint. With anyOwner (the
// new form) it is removed whoever owns it; without (the old form) only when
// it is the caller's. With guard it is removed only when its id and version
// both equal the guard's: a subscription written after the guard was taken
// stays. Nothing is revoked. No such subscription is the same success.
// Returns the owner of the removed subscription — for the handler's log
// only, never for a response — or nil when nothing was removed.
func (s *Store) RemoveChannel(ctx context.Context, callerID uuid.UUID, sessionEpoch int64, endpoint string, guard *VersionGuard, anyOwner bool) (*uuid.UUID, error) {
	var (
		guardID      any
		guardVersion any
	)
	if guard != nil {
		guardID, guardVersion = guard.ID, guard.Version
	}
	var ownerID *uuid.UUID
	err := s.withPushTx(ctx, callerID, sessionEpoch, func(tx pgx.Tx) error {
		ownerID = nil
		var owner uuid.UUID
		err := tx.QueryRow(ctx, `
DELETE FROM push_subscriptions
 WHERE endpoint = $1
   AND ($2 OR user_id = $3)
   AND ($4::uuid IS NULL OR (id = $4 AND version = $5::bigint))
RETURNING user_id`, endpoint, anyOwner, callerID, guardID, guardVersion).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("removing push channel: %w", err)
		}
		ownerID = &owner
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ownerID, nil
}

// RemoveDeviceByID removes the caller's subscription id. When it carries a
// device label, the label is revoked for the caller and every subscription
// of the caller with that label is deleted too. Not the caller's or no such
// subscription → ErrSubscriptionNotFound, nothing changes. Returns the
// number of subscriptions deleted.
func (s *Store) RemoveDeviceByID(ctx context.Context, userID uuid.UUID, sessionEpoch int64, id uuid.UUID) (int, error) {
	var removed int
	err := s.withPushTx(ctx, userID, sessionEpoch, func(tx pgx.Tx) error {
		var label *uuid.UUID
		err := tx.QueryRow(ctx, `
DELETE FROM push_subscriptions WHERE id = $1 AND user_id = $2
RETURNING device_label`, id, userID).Scan(&label)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSubscriptionNotFound
		}
		if err != nil {
			return fmt.Errorf("removing push subscription by id: %w", err)
		}
		removed = 1
		if label == nil {
			return nil
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO push_device_revocations (user_id, device_label) VALUES ($1, $2)
ON CONFLICT (user_id, device_label) DO NOTHING`, userID, *label); err != nil {
			return fmt.Errorf("recording device revocation: %w", err)
		}
		tag, err := tx.Exec(ctx, `
DELETE FROM push_subscriptions WHERE user_id = $1 AND device_label = $2`, userID, *label)
		if err != nil {
			return fmt.Errorf("deleting device subscriptions: %w", err)
		}
		removed += int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}
