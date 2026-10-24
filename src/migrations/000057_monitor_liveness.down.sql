-- 000057_monitor_liveness (down) — sk-579 (T004).
-- Removes only what up added; the monitor_credentials and api_keys rows
-- stay intact.
ALTER TABLE api_keys
    DROP COLUMN IF EXISTS sessionless_seen_at;

DROP TABLE IF EXISTS monitor_stall_notices;

DROP INDEX IF EXISTS monitor_credentials_stall_candidates_idx;

ALTER TABLE monitor_credentials
    DROP COLUMN IF EXISTS root_closed_at,
    DROP COLUMN IF EXISTS last_poll_succeeded_at,
    DROP COLUMN IF EXISTS token_file_path,
    DROP COLUMN IF EXISTS issued_session_id;
