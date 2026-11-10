// Package agents implements the lazy registry of lock holders: MCP
// sessions (R2.1, F227) and REST executors of API keys (D-6).
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
	"strings"
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

// RESTHolderPrefix opens the lock-holder namespace of the public REST API
// (D-6, ADR-2): a REST executor holds a lock as api:<keyId> (the key
// itself) or api:<keyId>:<workerId> (X-Skein-Worker-Id). No MCP session id
// lives in it — MCPSessionHolder moves a client-sent one out.
const RESTHolderPrefix = "api:"

// mcpForeignPrefix is put in front of an Mcp-Session-Id that a client sent
// inside the REST namespace, so the session keeps a stable holder of its
// own that no REST executor shares.
const mcpForeignPrefix = "mcp:"

// RESTHolder is the lock holder and agents.session_id of a REST executor:
// api:<keyId> without a worker id, api:<keyId>:<workerId> with one.
func RESTHolder(keyID uuid.UUID, workerID string) string {
	if workerID == "" {
		return RESTHolderPrefix + keyID.String()
	}
	return RESTHolderPrefix + keyID.String() + ":" + workerID
}

// IsRESTHolder reports whether a lock holder or agents.session_id belongs
// to the REST namespace.
func IsRESTHolder(sessionID string) bool {
	return strings.HasPrefix(sessionID, RESTHolderPrefix)
}

// MCPSessionHolder is the holder an MCP session gets for the
// Mcp-Session-Id it sent. The transport issues its own ids and refuses an
// unknown one, so an id shaped like a REST executor's does not reach a tool
// today; the auth layer still moves such an id out of that namespace, so
// the holder rule (INV-10: holders of REST and MCP never coincide) does not
// rest on the transport's session settings. Every other id is kept as it
// is.
func MCPSessionHolder(sessionID string) string {
	if IsRESTHolder(sessionID) {
		return mcpForeignPrefix + sessionID
	}
	return sessionID
}

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
	return UpsertWithKey(ctx, pool, userID, sessionID, uuid.Nil, granularity)
}

// UpsertWithKey is Upsert for a session that called with an API key: the
// row records the key (agents.api_key_id), because the stale-lock reaper
// keeps a holder alive only while its key is active, has the write right
// and covers the issue (D-6). uuid.Nil means no key and leaves a recorded
// key as it is.
func UpsertWithKey(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, sessionID string, keyID uuid.UUID, granularity time.Duration) error {
	if sessionID == "" {
		return nil
	}
	var key any
	if keyID != uuid.Nil {
		key = keyID
	}
	_, err := pool.Exec(ctx, `
INSERT INTO agents (user_id, session_id, first_seen_at, last_seen_at, api_key_id)
VALUES ($1, $2, now(), now(), $4)
ON CONFLICT (user_id, session_id) DO UPDATE
   SET last_seen_at = CASE
           WHEN agents.last_seen_at IS NULL OR now() - agents.last_seen_at > make_interval(secs => $3)
               THEN now()
           ELSE agents.last_seen_at
       END,
       api_key_id = COALESCE(EXCLUDED.api_key_id, agents.api_key_id)
`, userID, sessionID, granularity.Seconds(), key)
	if err != nil {
		return fmt.Errorf("upserting agent (%s/%s): %w", userID, sessionID, err)
	}
	return nil
}

// TouchRESTExecutor registers a REST executor in the registry, or extends
// its activity, and answers its holder string (D-6). The executor is the
// key itself (workerID empty) or the key with X-Skein-Worker-Id; each has
// its own row, so the activity of one keeps no other executor's locks
// alive. last_seen_at is throttled by granularity exactly as for an MCP
// session — production passes LastSeenGranularity.
func TouchRESTExecutor(ctx context.Context, pool *pgxpool.Pool, userID, keyID uuid.UUID, workerID string, granularity time.Duration) (string, error) {
	if keyID == uuid.Nil {
		return "", fmt.Errorf("touching a REST executor of user %s: no key", userID)
	}
	holder := RESTHolder(keyID, workerID)
	var worker any
	if workerID != "" {
		worker = workerID
	}
	_, err := pool.Exec(ctx, `
INSERT INTO agents (user_id, session_id, first_seen_at, last_seen_at, api_key_id, worker_id)
VALUES ($1, $2, now(), now(), $4, $5)
ON CONFLICT (user_id, session_id) DO UPDATE
   SET last_seen_at = CASE
           WHEN agents.last_seen_at IS NULL OR now() - agents.last_seen_at > make_interval(secs => $3)
               THEN now()
           ELSE agents.last_seen_at
       END
`, userID, holder, granularity.Seconds(), keyID, worker)
	if err != nil {
		return "", fmt.Errorf("touching REST executor (%s/%s): %w", userID, holder, err)
	}
	return holder, nil
}
