-- sk-31: Drop the manual `status='blocked'` flag. Blocked-ness is now
-- derived at query time from the dependency graph (an issue is blocked
-- when it has an outgoing `blocks` or `waits-for` edge to an unclosed
-- issue — same algorithm skein_ready already uses).
--
-- For every issue currently in status='blocked' we preserve the reason
-- text from the latest issue.status_changed event that flipped status
-- to 'blocked' as a system comment, so the historical context isn't
-- lost. The events row itself stays — this only adds a human-visible
-- comment so the audit trail is reachable from the issue timeline.
--
-- comments table columns (000004_issues.up.sql §line 126): id, issue_id,
-- author, author_user_id, author_type, body, created_at, updated_at.
-- author is the human-readable handle ("system"); author_type='system'
-- per the CHECK constraint; author_user_id stays NULL (no real user).
INSERT INTO comments (
    id, issue_id, author, author_user_id, author_type,
    body, created_at, updated_at
)
SELECT
    gen_random_uuid(),
    i.id,
    'system',
    NULL,
    'system',
    '_(system)_ Status was previously **blocked**. Reason from audit log: ' ||
        COALESCE(NULLIF(e.reason, ''), '_(not recorded)_'),
    now(),
    now()
FROM issues i
LEFT JOIN LATERAL (
    SELECT reason
      FROM events
     WHERE entity_type = 'issue'
       AND entity_id = i.id
       AND type = 'issue.status_changed'
       AND (new_data->>'status') = 'blocked'
     ORDER BY id DESC
     LIMIT 1
) e ON TRUE
WHERE i.status = 'blocked';

-- Flip every blocked row back to open. Callers that want "blocked"
-- now have to add a typed-deps edge (skein_dep_add type='blocks' or
-- 'waits-for') which the SELECT-time EXISTS predicate picks up.
UPDATE issues
   SET status = 'open',
       updated_at = now()
 WHERE status = 'blocked';

-- Drop the 'blocked' value from the status CHECK constraint. The
-- previous constraint name comes from the issues init migration
-- (000004_issues.up.sql §§47-54) — Postgres auto-names it
-- `issues_status_check`.
ALTER TABLE issues
    DROP CONSTRAINT IF EXISTS issues_status_check;

ALTER TABLE issues
    ADD CONSTRAINT issues_status_check
    CHECK (status IN ('open', 'in_progress', 'deferred', 'closed'));
