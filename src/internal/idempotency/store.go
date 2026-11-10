// Package idempotency keeps the outcomes of public REST requests sent with
// an Idempotency-Key (US-7, INV-8, plan D-8), in the table api_idempotency.
//
// A row belongs to one API key and one Idempotency-Key. Its life:
//
//  1. Begin writes the in-progress marker with the executor's own token in a
//     short transaction; the marker lives the write timeout plus 30 s, so by
//     its end the first execution has finished or been cancelled.
//  2. An outcome produced by a writing transaction is recorded inside that
//     transaction by RecordInTx — status, id, version and the objects of the
//     response — as a compare-and-swap on the token: when a repeat took the
//     marker over, the record fails and the operation rolls back.
//  3. Finish writes the body after the commit by a separate UPDATE on the
//     token, or — when no writing transaction recorded the outcome — stores
//     a storable outcome with its body, or deletes the marker of an outcome
//     that is not kept.
//
// The database clock decides every expiry, so tests move time by writing the
// row, never by waiting.
package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/publicapi"
)

// Retention is how long the outcome of a request is kept (spec «Data:
// Idempotency»): after it the key is forgotten and a repeat runs again.
const Retention = 24 * time.Hour

// MarkerGrace is added to the write timeout to make the life of the
// in-progress marker (ОСТ-2-6).
const MarkerGrace = 30 * time.Second

// CleanupInterval is how often the expired outcomes are deleted. A repeat
// never gets an outcome past its retention whether or not it was deleted
// yet: Begin treats it as forgotten.
const CleanupInterval = 15 * time.Minute

// ErrMarkerLost reports that the marker of this execution is no longer its
// own: a repeat took it over after it expired. The writing transaction that
// got it must roll back.
var ErrMarkerLost = errors.New("idempotency: the in-progress marker was taken over")

// Identity is what makes two requests the same request (US-7): the API key,
// the Idempotency-Key, the operation, the target of the path, the body, the
// Prefer header and the force mode of a guarded operation (the lead's
// decision in sk-878.3.1: the key with another mode is reused, 422). The mode
// is also what a repeat re-checks the owner's role and the key right in.
type Identity struct {
	KeyID       uuid.UUID
	Key         string
	OperationID string
	Target      string
	Fingerprint []byte
	// Prefer is the Prefer header of the request, "" when absent.
	Prefer string
	Force  bool
}

// Verdict is what Begin decided for a request.
type Verdict uint8

// The verdicts.
const (
	// Execute — the marker is this request's: run the operation under
	// Decision.Token.
	Execute Verdict = iota + 1
	// Independent — the key holds the outcome of another operation, target
	// or Prefer: the request is a different one and runs without being
	// recorded, leaving the stored outcome alone.
	Independent
	// InProgress — the first request still runs (or its body is not yet
	// written while its marker lives): 409.
	InProgress
	// KeyReused — the same operation, target and Prefer with another body
	// or another force mode: 422.
	KeyReused
	// Replay — the outcome is stored: answer it (Decision.Stored).
	Replay
)

// Refs are the objects an outcome refers to: the target the owner's role is
// checked on and the objects of the response, both read again on a repeat.
type Refs struct {
	Target  *publicapi.ObjectRef  `json:"target,omitempty"`
	Objects []publicapi.ObjectRef `json:"objects,omitempty"`
}

// Stored is the recorded outcome of a request.
type Stored struct {
	Status      int
	ID          *string
	Version     *int64
	Refs        Refs
	ContentType string
	// Body is nil when the body was not written after the commit.
	Body []byte
	// Force is the mode of the request that produced the outcome.
	Force bool
}

// Decision is the result of Begin.
type Decision struct {
	Verdict Verdict
	// Token is the marker of an Execute verdict.
	Token uuid.UUID
	// Stored is the outcome of a Replay verdict.
	Stored Stored
}

// Outcome is what a writing transaction records (RecordInTx).
type Outcome struct {
	// Status is the HTTP status the operation answers on success.
	Status int
	// ID and Version are those of the resource of the response; Version 0
	// means the resource has none.
	ID      string
	Version int
	Refs    Refs
}

// Store reads and writes api_idempotency.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore is the store over pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// micros is d as a whole number of microseconds, the unit the SQL below
// turns into an interval.
func micros(d time.Duration) int64 { return d.Microseconds() }

const insertMarkerSQL = `
INSERT INTO api_idempotency (api_key_id, idempotency_key, operation_id, target, body_fingerprint,
                             prefer, force, state, marker_token, in_progress_until, expires_at)
VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, 'in_progress', $8,
        now() + $9::bigint * interval '1 microsecond',
        now() + $10::bigint * interval '1 microsecond')
ON CONFLICT (api_key_id, idempotency_key) DO NOTHING`

const selectRowSQL = `
SELECT operation_id, target, body_fingerprint, COALESCE(prefer, ''), force, state,
       in_progress_until > now(), expires_at > now(),
       response_status, response_id, response_version, response_refs,
       COALESCE(response_content_type, ''), response_body
FROM api_idempotency
WHERE api_key_id = $1 AND idempotency_key = $2
FOR UPDATE`

// replaceExpiredSQL starts a new life of a row whose retention ended: the
// key is forgotten, so the request is a first one.
const replaceExpiredSQL = `
UPDATE api_idempotency
SET operation_id = $3, target = $4, body_fingerprint = $5, prefer = NULLIF($6, ''), force = $7,
    state = 'in_progress', marker_token = $8,
    in_progress_until = now() + $9::bigint * interval '1 microsecond',
    expires_at = now() + $10::bigint * interval '1 microsecond',
    response_status = NULL, response_id = NULL, response_version = NULL, response_refs = NULL,
    response_content_type = NULL, response_body = NULL, created_at = now()
WHERE api_key_id = $1 AND idempotency_key = $2`

// takeOverSQL gives the marker of a first execution whose marker expired to
// the repeat: it runs the operation again, exactly once.
const takeOverSQL = `
UPDATE api_idempotency
SET marker_token = $3,
    in_progress_until = now() + $4::bigint * interval '1 microsecond'
WHERE api_key_id = $1 AND idempotency_key = $2`

// Begin decides what to do with a request of identity id: write the
// in-progress marker (Execute), or answer from the row the key already has.
// markerTTL is the life of the marker (the write timeout plus MarkerGrace).
func (s *Store) Begin(ctx context.Context, id Identity, markerTTL time.Duration) (Decision, error) {
	var d Decision
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		d, err = begin(ctx, tx, id, markerTTL)
		return err
	})
	if err != nil {
		return Decision{}, fmt.Errorf("idempotency: beginning %s: %w", id.OperationID, err)
	}
	return d, nil
}

func begin(ctx context.Context, tx pgx.Tx, id Identity, markerTTL time.Duration) (Decision, error) {
	token := uuid.New()
	args := []any{id.KeyID, id.Key, id.OperationID, id.Target, id.Fingerprint, id.Prefer, id.Force,
		token, micros(markerTTL), micros(Retention)}
	tag, err := tx.Exec(ctx, insertMarkerSQL, args...)
	if err != nil {
		return Decision{}, fmt.Errorf("writing the marker: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return Decision{Verdict: Execute, Token: token}, nil
	}

	var (
		row          Stored
		opID, target string
		fingerprint  []byte
		prefer       string
		state        string
		markerAlive  bool
		live         bool
		status       *int32
		refs         []byte
	)
	err = tx.QueryRow(ctx, selectRowSQL, id.KeyID, id.Key).Scan(&opID, &target, &fingerprint, &prefer, &row.Force,
		&state, &markerAlive, &live, &status, &row.ID, &row.Version, &refs, &row.ContentType, &row.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		// Deleted by the cleanup between the two statements: the key is
		// forgotten, the request is a first one.
		if _, err := tx.Exec(ctx, insertMarkerSQL, args...); err != nil {
			return Decision{}, fmt.Errorf("writing the marker after a cleanup: %w", err)
		}
		return Decision{Verdict: Execute, Token: token}, nil
	}
	if err != nil {
		return Decision{}, fmt.Errorf("reading the stored outcome: %w", err)
	}

	switch {
	case !live:
		if _, err := tx.Exec(ctx, replaceExpiredSQL, args...); err != nil {
			return Decision{}, fmt.Errorf("replacing an expired outcome: %w", err)
		}
		return Decision{Verdict: Execute, Token: token}, nil
	case opID != id.OperationID || target != id.Target || prefer != id.Prefer:
		return Decision{Verdict: Independent}, nil
	case string(fingerprint) != string(id.Fingerprint) || row.Force != id.Force:
		return Decision{Verdict: KeyReused}, nil
	case state == "in_progress" && markerAlive:
		return Decision{Verdict: InProgress}, nil
	case state == "in_progress":
		if _, err := tx.Exec(ctx, takeOverSQL, id.KeyID, id.Key, token, micros(markerTTL)); err != nil {
			return Decision{}, fmt.Errorf("taking the marker over: %w", err)
		}
		return Decision{Verdict: Execute, Token: token}, nil
	case row.Body == nil && markerAlive:
		// Recorded, but the body may still be written by the first
		// execution (D-8 rule 6).
		return Decision{Verdict: InProgress}, nil
	}
	if status == nil {
		return Decision{}, fmt.Errorf("completed outcome of %s without a status", opID)
	}
	row.Status = int(*status)
	if len(refs) > 0 {
		if err := json.Unmarshal(refs, &row.Refs); err != nil {
			return Decision{}, fmt.Errorf("decoding the objects of the stored outcome: %w", err)
		}
	}
	return Decision{Verdict: Replay, Stored: row}, nil
}

const recordSQL = `
UPDATE api_idempotency
SET state = 'completed', response_status = $4, response_id = NULLIF($5, ''),
    response_version = NULLIF($6::bigint, 0), response_refs = $7
WHERE api_key_id = $1 AND idempotency_key = $2 AND marker_token = $3 AND state = 'in_progress'`

// Record writes o in tx, the writing transaction of the execution under
// token, as a compare-and-swap on the token (D-8 rule 2): ErrMarkerLost when
// the marker is no longer that execution's, and tx must then roll back.
// Operations reach it through RecordInTx.
func Record(ctx context.Context, tx pgx.Tx, keyID uuid.UUID, key string, token uuid.UUID, o Outcome) error {
	refs, err := json.Marshal(o.Refs)
	if err != nil {
		return fmt.Errorf("idempotency: encoding the objects of the outcome: %w", err)
	}
	tag, err := tx.Exec(ctx, recordSQL, keyID, key, token, o.Status, o.ID, o.Version, refs)
	if err != nil {
		return fmt.Errorf("idempotency: recording the outcome: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrMarkerLost
	}
	return nil
}

// Response is the response an execution produced, as Finish keeps it.
type Response struct {
	Status      int
	ContentType string
	Body        []byte
	// ID and Version are those the response names, when it names them; they
	// are stored with an outcome no writing transaction recorded.
	ID      string
	Version int
}

const lockOwnSQL = `
SELECT state, response_status
FROM api_idempotency
WHERE api_key_id = $1 AND idempotency_key = $2 AND marker_token = $3
FOR UPDATE`

const writeBodySQL = `
UPDATE api_idempotency
SET response_content_type = $4, response_body = $5
WHERE api_key_id = $1 AND idempotency_key = $2 AND marker_token = $3 AND state = 'completed'`

const storeOutcomeSQL = `
UPDATE api_idempotency
SET state = 'completed', response_status = $4, response_id = NULLIF($5, ''),
    response_version = NULLIF($6::bigint, 0), response_refs = $7,
    response_content_type = $8, response_body = $9
WHERE api_key_id = $1 AND idempotency_key = $2 AND marker_token = $3 AND state = 'in_progress'`

const releaseSQL = `
DELETE FROM api_idempotency
WHERE api_key_id = $1 AND idempotency_key = $2 AND marker_token = $3`

// Finish ends the execution under token, after the operation returned:
//
//   - the outcome was recorded by its writing transaction (completed on this
//     token): the body is written (D-8 rule 3) when resp is the response of
//     that outcome;
//   - nothing was recorded and storable is set: the outcome is stored with
//     its body and refs in this one transaction (D-8 rules 4, 5);
//   - nothing was recorded and the outcome is not kept: the marker is
//     deleted, so a repeat runs again.
//
// A marker taken over by a repeat is left alone.
func (s *Store) Finish(ctx context.Context, keyID uuid.UUID, key string, token uuid.UUID, resp Response, storable bool, refs Refs) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			state  string
			status *int32
		)
		err := tx.QueryRow(ctx, lockOwnSQL, keyID, key, token).Scan(&state, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("idempotency: reading the own marker: %w", err)
		}
		if state == "completed" {
			if status == nil || int(*status) != resp.Status {
				// The response is not the recorded outcome (the recorded
				// write answered otherwise): a repeat reads the objects.
				return nil
			}
			if _, err := tx.Exec(ctx, writeBodySQL, keyID, key, token, resp.ContentType, resp.Body); err != nil {
				return fmt.Errorf("idempotency: writing the body: %w", err)
			}
			return nil
		}
		if !storable {
			if _, err := tx.Exec(ctx, releaseSQL, keyID, key, token); err != nil {
				return fmt.Errorf("idempotency: releasing the marker: %w", err)
			}
			return nil
		}
		raw, err := json.Marshal(refs)
		if err != nil {
			return fmt.Errorf("idempotency: encoding the objects of the outcome: %w", err)
		}
		if _, err := tx.Exec(ctx, storeOutcomeSQL, keyID, key, token, resp.Status, resp.ID, resp.Version,
			raw, resp.ContentType, resp.Body); err != nil {
			return fmt.Errorf("idempotency: storing the outcome: %w", err)
		}
		return nil
	})
}

// Release deletes the marker under token, when the execution ended without
// any outcome to keep before the operation could answer.
func (s *Store) Release(ctx context.Context, keyID uuid.UUID, key string, token uuid.UUID) error {
	if _, err := s.pool.Exec(ctx, releaseSQL, keyID, key, token); err != nil {
		return fmt.Errorf("idempotency: releasing the marker: %w", err)
	}
	return nil
}

// cleanupBatch is how many expired rows one statement of DeleteExpired
// removes, so a large backlog never holds one long transaction.
const cleanupBatch = 1000

const deleteExpiredSQL = `
DELETE FROM api_idempotency
WHERE ctid IN (SELECT ctid FROM api_idempotency WHERE expires_at <= now() LIMIT $1)`

// DeleteExpired removes every row whose retention ended and returns how many
// it removed.
func (s *Store) DeleteExpired(ctx context.Context) (int64, error) {
	var total int64
	for {
		tag, err := s.pool.Exec(ctx, deleteExpiredSQL, cleanupBatch)
		if err != nil {
			return total, fmt.Errorf("idempotency: deleting expired outcomes: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < cleanupBatch {
			return total, nil
		}
	}
}

// RunCleanup deletes the expired outcomes every interval until ctx ends,
// starting with one sweep at once. A failed sweep is logged and the next
// tick retries; several replicas may run it side by side.
func RunCleanup(ctx context.Context, store *Store, interval time.Duration) {
	if interval <= 0 {
		panic("idempotency: cleanup interval must be > 0")
	}
	sweep := func() {
		n, err := store.DeleteExpired(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.WarnContext(ctx, "idempotency cleanup: sweep failed", "deleted", n, "error", err.Error())
			}
			return
		}
		if n > 0 {
			slog.InfoContext(ctx, "idempotency cleanup", "deleted", n)
		}
	}
	sweep()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}
