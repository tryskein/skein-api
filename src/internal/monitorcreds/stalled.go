package monitorcreds

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/agents"
	"skein-api/internal/authz/authzsql"
)

// StallNoticeInterval is how often one recipient session may be shown the
// same stalled monitor (sk-579 INV-7): at most once per this interval, also
// under concurrent calls through different API replicas.
const StallNoticeInterval = 5 * time.Minute

// StalledQuery asks for the stalled monitors one MCP call may be told about.
type StalledQuery struct {
	// APIKeyID is the caller's api key: only monitors minted by it qualify
	// (INV-8).
	APIKeyID uuid.UUID
	// UserID is the caller (the owner of APIKeyID).
	UserID uuid.UUID
	// RecipientSession is the caller's Mcp-Session-Id; "" for a client
	// without one (it is a recipient of its own, and equals an empty issuing
	// session).
	RecipientSession string
	// Threshold is the stall threshold (cfg.MonitorStallThreshold).
	Threshold time.Duration
	// Limit caps the returned monitors.
	Limit int
}

// StalledMonitor is one stalled monitor the caller may be told about.
type StalledMonitor struct {
	ID                  uuid.UUID
	IssueID             string
	LastPollSucceededAt *time.Time // UTC; nil = never polled successfully
	IssuedSessionID     string
	TokenFilePath       string
	Mine                bool
}

// StalledResult is a page of stalled monitors plus how many qualified.
type StalledResult struct {
	Monitors []StalledMonitor
	Total    int
}

// stalledSQL selects the stalled monitors one call may be told about
// (sk-579 T014). One read, driven by monitor_credentials_stall_candidates_idx:
// its condition (not revoked, not root_closed, issuing session known) is
// repeated verbatim below. The rest is INV-5 (the monitor is stalled) and
// INV-8 (who may be told), plus the per-recipient show throttle (INV-7).
//
// $1 api key, $2 user, $3 recipient session, $4 threshold, $5 limit,
// $6 show interval, $7 activity-clock granularity. The owner's view of the
// root's project is the access relation (authzsql, sk-674.18 T010).
//
// The issuing session's activity clocks (agents.last_seen_at,
// api_keys.sessionless_seen_at) are written at most once per $7, so a stamp
// may lag the session's last real call by up to $7. «Quiet for longer than
// the threshold» (INV-8) is therefore stamp < now() - (threshold + $7): a
// session active a moment ago is never handed to another recipient early.
// The age of the last successful poll (INV-5) is exact and is not widened.
var stalledSQL = `
SELECT mc.id, mc.scope_issue_id, mc.last_poll_succeeded_at,
       mc.issued_session_id, COALESCE(mc.token_file_path, ''),
       mc.issued_session_id = $3 AS mine,
       count(*) OVER () AS total
  FROM monitor_credentials mc
  JOIN api_keys ak ON ak.id = mc.minted_by_api_key_id
  JOIN users u     ON u.id = mc.user_id
  JOIN issues root ON root.id = mc.scope_issue_id
 WHERE mc.minted_by_api_key_id = $1
   -- the partial index condition (000057), verbatim
   AND mc.revoked_at IS NULL
   AND mc.root_closed_at IS NULL
   AND mc.issued_session_id IS NOT NULL
   -- INV-5: the token still lives, its key and owner too
   AND mc.user_id = $2
   AND mc.user_id = ak.user_id
   AND now() < mc.expires_at
   AND now() < mc.absolute_deadline
   AND ak.revoked_at IS NULL
   AND u.deactivated_at IS NULL
   -- INV-5: the root is not closed and its project is visible to the owner
   AND root.status <> 'closed'
   AND ` + authzsql.RecipientVisibleExpr("mc.user_id", "mc.project_id") + `
   -- INV-5: no successful poll (or mint) for longer than the threshold
   AND COALESCE(mc.last_poll_succeeded_at, mc.created_at) < now() - $4::interval
   -- INV-8: the issuing session itself, or any session once it went quiet
   AND (
         mc.issued_session_id = $3
      OR (mc.issued_session_id <> ''
          AND COALESCE((SELECT a.last_seen_at FROM agents a
                         WHERE a.user_id = mc.user_id AND a.session_id = mc.issued_session_id),
                       '-infinity'::timestamptz) < now() - ($4::interval + $7::interval))
      OR (mc.issued_session_id = ''
          AND COALESCE(ak.sessionless_seen_at, '-infinity'::timestamptz) < now() - ($4::interval + $7::interval))
   )
   -- INV-7: not shown to this recipient within the interval
   AND NOT EXISTS (
         SELECT 1 FROM monitor_stall_notices n
          WHERE n.monitor_id = mc.id
            AND n.recipient_session_id = $3
            AND n.shown_at > now() - $6::interval)
 ORDER BY mine DESC, COALESCE(mc.last_poll_succeeded_at, mc.created_at), mc.id
 LIMIT $5`

// Stalled returns up to q.Limit stalled monitors the caller may be told
// about — its own issuing session's first, then the longest-silent — and the
// number that qualified (sk-579 US-5, INV-5, INV-8). One read; it writes
// nothing (the show marks are ClaimStallNotices).
func (s *Store) Stalled(ctx context.Context, q StalledQuery) (StalledResult, error) {
	rows, err := s.pool.Query(ctx, stalledSQL,
		q.APIKeyID, q.UserID, q.RecipientSession, q.Threshold, q.Limit, StallNoticeInterval, agents.LastSeenGranularity)
	if err != nil {
		return StalledResult{}, fmt.Errorf("selecting stalled monitors: %w", err)
	}
	defer rows.Close()
	var res StalledResult
	for rows.Next() {
		var m StalledMonitor
		if err := rows.Scan(&m.ID, &m.IssueID, &m.LastPollSucceededAt,
			&m.IssuedSessionID, &m.TokenFilePath, &m.Mine, &res.Total); err != nil {
			return StalledResult{}, fmt.Errorf("scanning stalled monitor: %w", err)
		}
		if m.LastPollSucceededAt != nil {
			utc := m.LastPollSucceededAt.UTC()
			m.LastPollSucceededAt = &utc
		}
		res.Monitors = append(res.Monitors, m)
	}
	if err := rows.Err(); err != nil {
		return StalledResult{}, fmt.Errorf("reading stalled monitors: %w", err)
	}
	return res, nil
}

// claimStallNoticesSQL is the conditional upsert of the show marks: a pair
// (monitor, recipient session) is claimed when it has no mark yet or its mark
// is older than the interval. Concurrent claims of one pair serialise on the
// conflicting row; the loser sees the winner's fresh shown_at and claims
// nothing (INV-7). An empty recipient session is the key ” — never NULL.
const claimStallNoticesSQL = `
INSERT INTO monitor_stall_notices (monitor_id, recipient_session_id, shown_at)
SELECT m, $2, now() FROM unnest($1::uuid[]) AS m
ON CONFLICT (monitor_id, recipient_session_id)
DO UPDATE SET shown_at = EXCLUDED.shown_at
      WHERE monitor_stall_notices.shown_at <= now() - $3::interval
RETURNING monitor_id`

// ClaimStallNotices marks monitorIDs as shown to recipientSession and returns
// the ids this call actually claimed — only those may be shown (sk-579 INV-7).
func (s *Store) ClaimStallNotices(ctx context.Context, recipientSession string, monitorIDs []uuid.UUID) ([]uuid.UUID, error) {
	if len(monitorIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, claimStallNoticesSQL, monitorIDs, recipientSession, StallNoticeInterval)
	if err != nil {
		return nil, fmt.Errorf("claiming stall notices: %w", err)
	}
	defer rows.Close()
	var claimed []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning claimed notice: %w", err)
		}
		claimed = append(claimed, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading claimed notices: %w", err)
	}
	return claimed, nil
}

// TouchSessionless records that apiKeyID made an MCP tool call without an
// Mcp-Session-Id (sk-579 US-5): the only clock that tells whether an EMPTY
// issuing session has gone quiet. Written at most once per granularity per
// key — the same throttle as agents.last_seen_at (pass
// agents.LastSeenGranularity). A non-positive granularity is refused rather
// than turned into a write on every call.
func (s *Store) TouchSessionless(ctx context.Context, apiKeyID uuid.UUID, granularity time.Duration) error {
	if granularity <= 0 {
		return fmt.Errorf("touching sessionless activity: granularity must be > 0, got %s", granularity)
	}
	_, err := s.pool.Exec(ctx, `
UPDATE api_keys
   SET sessionless_seen_at = now()
 WHERE id = $1
   AND (sessionless_seen_at IS NULL OR sessionless_seen_at < now() - $2::interval)`,
		apiKeyID, granularity)
	if err != nil {
		return fmt.Errorf("touching sessionless activity of key %s: %w", apiKeyID, err)
	}
	return nil
}
