package invitations

// expiry.go — the invitation expiry worker (sk-674.18 T040, US-5, US-19).
//
// A pending invitation past its expires_at already holds no seat and has no
// live link — every predicate reads `expires_at > now()`. The worker gives
// it its outcome: status expired, its tokens and unsent emails retired
// (tokens.Cancel), and the signal `expired` to the space owner and the
// sender (signals contract), `inbox` to the addressee's account (the card
// leaves). It locks invitation rows only and skips locked ones (an answer
// in progress holds the row; it decides first).
//
// The same tick deletes the journal of sends older than the daily window
// (space_invitation_sends, 000083): no count reads them, and without the
// sweep the table would grow without bound. Bounded batches, so a pass never
// holds a long lock.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/signals"
	"skein-api/internal/tokens"
)

// Expiry tuning.
const (
	// defaultExpiryInterval — invitations live days (SKEIN_SPACE_INVITE_TTL);
	// a minute's lag of the status behind the expiry changes no answer.
	defaultExpiryInterval = time.Minute
	// defaultExpiryBatch bounds one pass's transaction.
	defaultExpiryBatch = 100
	// defaultSendsBatch bounds one DELETE of the journal of sends.
	defaultSendsBatch = 1000
)

// ExpiryOptions tune the worker; zero values take the defaults.
type ExpiryOptions struct {
	Interval   time.Duration
	Batch      int
	SendsBatch int
}

// Expirer is the expiry worker.
type Expirer struct {
	pool *pgxpool.Pool
	opts ExpiryOptions
}

// NewExpirer builds the worker.
func NewExpirer(pool *pgxpool.Pool, opts ExpiryOptions) *Expirer {
	if opts.Interval <= 0 {
		opts.Interval = defaultExpiryInterval
	}
	if opts.Batch <= 0 {
		opts.Batch = defaultExpiryBatch
	}
	if opts.SendsBatch <= 0 {
		opts.SendsBatch = defaultSendsBatch
	}
	return &Expirer{pool: pool, opts: opts}
}

// Run passes once at start and then every Interval until ctx is cancelled.
// A failed pass is logged; the next one comes on schedule.
func (e *Expirer) Run(ctx context.Context) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := e.RunOnce(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "invitation expiry pass failed", "error", err.Error())
		}
		t.Reset(e.opts.Interval)
	}
}

// RunOnce expires the overdue pending invitations, batch after batch, and
// sweeps one batch of the old journal of sends. It returns how many
// invitations it expired.
func (e *Expirer) RunOnce(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := e.expireBatch(ctx)
		total += n
		if err != nil {
			return total, err
		}
		if n < e.opts.Batch {
			break
		}
	}
	if _, err := e.pool.Exec(ctx, `
DELETE FROM space_invitation_sends
 WHERE id IN (SELECT id FROM space_invitation_sends
               WHERE sent_at < now() - $1::bigint * interval '1 second'
               LIMIT $2)`, int64(dailyWindow/time.Second), e.opts.SendsBatch); err != nil {
		return total, fmt.Errorf("deleting the old sends: %w", err)
	}
	return total, nil
}

// expireBatch expires one batch in one transaction.
func (e *Expirer) expireBatch(ctx context.Context) (int, error) {
	n := 0
	err := signals.BeginTxFunc(ctx, e.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		n = 0
		rows, err := tx.Query(ctx, `
SELECT i.id, i.space_id, i.invited_by, i.email, s.short_id, s.owner_id
  FROM space_invitations i
  JOIN spaces s ON s.id = i.space_id
 WHERE i.status = 'pending' AND i.expires_at <= now()
 ORDER BY i.expires_at
 LIMIT $1
   FOR NO KEY UPDATE OF i SKIP LOCKED`, e.opts.Batch)
		if err != nil {
			return fmt.Errorf("selecting the overdue invitations: %w", err)
		}
		type overdue struct {
			id, spaceID, sender, owner uuid.UUID
			email, short               string
		}
		var batch []overdue
		for rows.Next() {
			var o overdue
			if err := rows.Scan(&o.id, &o.spaceID, &o.sender, &o.email, &o.short, &o.owner); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, o)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, o := range batch {
			if err := resolve(ctx, tx, o.id, "expired"); err != nil {
				return err
			}
			if err := tokens.Cancel(ctx, tx, tokens.InvitationSubject(o.id)); err != nil {
				return err
			}
			// The owner and the sender — once each, also when the owner
			// sent it himself.
			signals.AddSpaceEvent(tx, o.owner, signals.SpaceEvent{Reason: signals.SpaceExpired, SpaceID: o.short})
			if o.sender != o.owner {
				signals.AddSpaceEvent(tx, o.sender, signals.SpaceEvent{Reason: signals.SpaceExpired, SpaceID: o.short})
			}
			to, found, err := personByEmail(ctx, tx, o.email)
			if err != nil {
				return err
			}
			if found {
				signals.Add(tx, to.ID, signals.AreaInbox, signals.AreaSpaces)
			}
			n++
		}
		return nil
	})
	return n, err
}
