// Package agents implements the lazy MCP-session registry (R2.1, F227).
//
// Every authenticated /mcp request that carries an Mcp-Session-Id triggers
// an UPSERT into the agents table. The first time we see a (user_id,
// session_id) pair we INSERT; subsequent requests bump last_seen_at.
//
// The registry is best-effort: a failed UPSERT is logged at WARN but does
// NOT abort the request — agent identification is convenience, not auth.
//
// TODO(R5.3): a cleanup goroutine with a Postgres advisory lock will
// remove rows older than the agent retention window (~1 year). Today we
// just keep growing; volume is dominated by MCP session churn so even a
// year of dev usage stays in the low thousands.
package agents

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LastSeenGranularity is the write throttle of agents.last_seen_at: an
// existing row's stamp is only bumped when it is older than this. Readers of
// last_seen_at therefore cannot tell activity apart at a finer resolution —
// which is why config rejects a monitor stall threshold that does not exceed
// it (sk-579, plan decision "Stalled threshold", condition (b)). Callers pass it
// to Upsert.
const LastSeenGranularity = 60 * time.Second

// Upsert inserts a new (user_id, session_id) row or updates last_seen_at
// on an existing one.
//
// last_seen_at is throttled at the SQL layer: we only bump it when the
// previous value is older than granularity (production passes
// LastSeenGranularity, 60 seconds). This keeps audit accuracy at that
// granularity while cutting UPSERT writes on the MCP hot path
// to ~1/(user, session)/min — the row stays cheap to update even when a
// chatty agent fires many tools/call requests in quick succession.
//
// Caller is expected to skip this call entirely when sessionID is empty
// (initialize request) — we still defensively return early to keep
// callers honest.
func Upsert(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, sessionID string, granularity time.Duration) error {
	if sessionID == "" {
		return nil
	}

	_, err := pool.Exec(ctx, `
INSERT INTO agents (user_id, session_id, first_seen_at, last_seen_at)
VALUES ($1, $2, now(), now())
ON CONFLICT (user_id, session_id) DO UPDATE
   SET last_seen_at = CASE
           WHEN agents.last_seen_at IS NULL OR now() - agents.last_seen_at > make_interval(secs => $3)
               THEN now()
           ELSE agents.last_seen_at
       END
`, userID, sessionID, granularity.Seconds())
	if err != nil {
		return fmt.Errorf("upserting agent (%s/%s): %w", userID, sessionID, err)
	}
	return nil
}
