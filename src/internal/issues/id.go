package issues

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// hashSuffixLength is the number of characters in the random suffix for
// hash-mode IDs (vision §6 "progressive 6→7→8" — we start with 6).
// Increase to 7 if collisions become a real problem at scale.
const hashSuffixLength = 6

// hashSuffixAlphabet excludes visually ambiguous characters (l/1/I, 0/O).
// 32 letters keeps each character ~5 bits of entropy; 6 chars × 5 bits =
// ~30 bits — collision probability 1-in-a-billion at thousands of issues
// per project. The retry loop in nextHashID protects us anyway.
const hashSuffixAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// hashMaxRetries bounds the retry loop on collision. 5 attempts at 30 bits
// of entropy each gives effectively-zero failure rate — if we ever hit it,
// the project has so many issues that we should bump alphabet size or
// suffix length, not silently keep trying.
const hashMaxRetries = 5

// ErrHashCollisionExhausted is returned by nextHashID when every retry
// landed on an existing ID. Practically unreachable; surfaced as 500 in
// the handler so we get a clean log line instead of a hang.
var ErrHashCollisionExhausted = errors.New("could not generate a unique hash id after retries")

// ClaimNextCounter atomically increments project_counters.counter and
// returns the new value. Caller must already hold an open transaction —
// we don't open one here because counter claim must commit-or-rollback
// together with the INSERT INTO issues that consumes the value (db-schema
// §2.11 "atomic claim pattern").
//
// Returns ErrCounterNotInitialised if no row exists for the project.
// Project create flow seeds a row; this branch only fires if migration
// backfill missed something or the row was deleted manually.
func ClaimNextCounter(ctx context.Context, tx pgx.Tx, projectID any) (int, error) {
	var counter int
	err := tx.QueryRow(ctx, `
UPDATE project_counters
   SET counter = counter + 1
 WHERE project_id = $1
RETURNING counter`, projectID).Scan(&counter)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrCounterNotInitialised
	}
	if err != nil {
		return 0, fmt.Errorf("claiming next counter: %w", err)
	}
	return counter, nil
}

// ErrCounterNotInitialised is returned when project_counters has no row
// for the project — should never happen in steady state because both the
// 000004 migration backfill and CreateProject seed a row.
var ErrCounterNotInitialised = errors.New("project_counters row missing — project not properly initialised")

// nextChildID computes the next hierarchical child ID for parentID using
// the F144 reconciliation rule: take MAX(stored last_child, MAX(suffix from
// existing children)) + 1. Protects against corruption from imports or
// manual fixes where last_child fell behind real children.
//
// Implementation:
//  1. UPSERT child_counters row (INSERT … ON CONFLICT) so the UPDATE below
//     always matches at least one row for first-time parents.
//  2. Single UPDATE that combines reconciliation + increment in one round-trip.
//
// Returns the full new child ID (e.g. "demo-42.5").
//
// # Hot row by construction — decision recorded sk-303.18
//
// This UPDATE advances ONE child_counters row per parent, so every
// concurrent create of a sibling under the same parent serialises through
// it: only one committed SERIALIZABLE transaction can advance the row at a
// time, and every loser gets SQLSTATE 40001 and replays via
// Store.withSerializableRetry (store.go, createMaxAttempts=6, raised from
// 3 by sk-305 after an 8-way fan-out lost a create at the old budget). That
// is a structural property of keeping `parent.1..parent.N` contiguous
// with no gaps or duplicates — not an accident to be tuned away — so the
// retry is the fix, not a workaround pending a better one.
//
// sk-303.18 measured current behaviour rather than reasoning about it:
// three separate live runs of TestCreateIssue_ConcurrentChildren_
// NoSerializationError (8 concurrent creates under one parent) needed up
// to 3, 4, and 4 retries respectively to land all 8 — comfortably inside
// the 6-attempt budget, all three green. Considered and rejected for now:
// swapping the SERIALIZABLE-abort-and-retry pattern for an explicit
// pg_advisory_xact_lock(hashtext(parentID)) held for the UPDATE's
// duration. That would turn losers into blocked waiters instead of
// retried transactions — no wasted re-executed work, no backoff jitter —
// but it trades a bounded, self-healing failure mode for one where a deep
// fan-out holds pool connections in a blocked queue instead of releasing
// them between attempts; on a small pgxpool.MaxConns in Kubernetes that
// risks starving unrelated requests under exactly the load pattern (a
// large epic fan-out) that would trigger it. Not worth that trade while
// production sees zero `internal_error` on skein_create and the retry
// count stays well under budget.
//
// Revisit when the margin actually erodes, not on a schedule: watch
// `skein_mcp_tool_errors_total{tool="skein_create",code="internal_error"}`
// staying at 0 while "serializable transaction hit a retryable abort;
// retrying" log lines with op=createIssue climb toward attempt=6 in
// production, or `skein_mcp_tool_errors_total{...,code="internal_error"}`
// actually going non-zero on skein_create.
func nextChildID(ctx context.Context, tx pgx.Tx, parentID string) (string, error) {
	// UPSERT to guarantee the row exists. ON CONFLICT DO NOTHING is enough —
	// we don't care whether INSERT or no-op happened, only that the row exists
	// before the UPDATE below.
	if _, err := tx.Exec(ctx, `
INSERT INTO child_counters (parent_id, last_child)
VALUES ($1, 0)
ON CONFLICT (parent_id) DO NOTHING`, parentID); err != nil {
		return "", fmt.Errorf("ensuring child_counter row: %w", err)
	}

	// Reconciliation: compute MAX of stored last_child and the highest suffix
	// already present in issues table for THIS parent. The suffix regex
	// '^<parent>\.(\d+)$' picks out the immediate-child segment ONLY (rejects
	// grandchildren like "demo-42.5.1").
	//
	// Postgres `substring(text from pattern)` returns the first capturing
	// group as text — non-set-returning, safe to wrap in MAX(). We use it
	// instead of regexp_matches which is set-returning and rejected inside
	// aggregates (SQLSTATE 0A000).
	//
	// sk-35: do NOT filter on `issues.parent_id = $1` — the column is
	// frozen for new rows and would silently exclude every child created
	// post-restore. The id-suffix regex is the authoritative filter:
	// any issue whose id starts with `<parent>.<N>` IS a child of this
	// parent by construction (the id was minted by nextChildID against
	// this very parent).
	regex := "^" + regexpEscape(parentID) + `\.(\d+)$`
	var newCounter int
	err := tx.QueryRow(ctx, `
WITH children AS (
    SELECT COALESCE(MAX(CAST(substring(id from $2) AS int)), 0) AS max_existing
      FROM issues
     WHERE id ~ $2
)
UPDATE child_counters
   SET last_child = GREATEST(last_child, (SELECT max_existing FROM children)) + 1
 WHERE parent_id = $1
RETURNING last_child`, parentID, regex).Scan(&newCounter)
	if err != nil {
		return "", fmt.Errorf("claiming child counter: %w", err)
	}
	return fmt.Sprintf("%s.%d", parentID, newCounter), nil
}

// regexpEscape escapes the metacharacters that have meaning in Postgres
// regex syntax (POSIX ERE). We control parent IDs (validation regex limits
// them to [a-z0-9._-]), so realistically only '.' '-' need escaping; we
// escape the broader set to stay safe if validation rules change.
func regexpEscape(s string) string {
	const meta = `\.+*?()[]{}|^$`
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		if strings.ContainsRune(meta, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// nextCounterID claims the next per-project counter and returns the full
// "{prefix}-{N}" form. Caller passes the project's ID and id_prefix (we
// take both because the counter table is keyed on uuid, but the ID we mint
// uses the human-readable prefix).
func nextCounterID(ctx context.Context, tx pgx.Tx, projectID any, prefix string) (string, error) {
	n, err := ClaimNextCounter(ctx, tx, projectID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%d", prefix, n), nil
}

// nextHashID generates a {prefix}-{6-char random alphanum} ID and probes
// issues table for collisions. On hit, regenerates up to hashMaxRetries
// times before giving up with ErrHashCollisionExhausted.
//
// The probe is a separate SELECT, not the eventual INSERT — that keeps
// this helper independent of the rest of the issue create transaction. The
// real INSERT can still race and lose; caller is expected to translate a
// PG unique-violation on issues_pkey into a 422 / retry from above.
func nextHashID(ctx context.Context, tx pgx.Tx, prefix string) (string, error) {
	for attempt := 0; attempt < hashMaxRetries; attempt++ {
		suffix, err := randomHashSuffix(hashSuffixLength)
		if err != nil {
			return "", fmt.Errorf("generating hash suffix: %w", err)
		}
		candidate := fmt.Sprintf("%s-%s", prefix, suffix)

		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM issues WHERE id = $1)`, candidate).Scan(&exists); err != nil {
			return "", fmt.Errorf("probing for hash collision: %w", err)
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", ErrHashCollisionExhausted
}

// randomHashSuffix draws `length` characters from hashSuffixAlphabet using
// crypto/rand. The modulo bias is mathematically present but the alphabet
// is exactly 32 chars — a power of two — so each random byte's low 5 bits
// give a uniform distribution. We do the cleaner approach (one byte per
// char, mod the alphabet length) since 32 divides 256 evenly.
func randomHashSuffix(length int) (string, error) {
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, length)
	for i, b := range buf {
		out[i] = hashSuffixAlphabet[int(b)%len(hashSuffixAlphabet)]
	}
	return string(out), nil
}
