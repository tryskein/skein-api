-- 000085_events_actor_session_seen - sk-740 (epic sk-750).
--
-- WHY. The dashboard Agents zone for a non-admin (internal/dashboard/store.go#Agents,
-- the scope branch) takes, for each session, the time of its last event in
-- the scope projects - with one grouping:
--
--     SELECT actor_user_id, actor_session_id, max(occurred_at)
--       FROM events
--      WHERE project_id = ANY($1) AND actor_user_id IS NOT NULL
--        AND actor_session_id IS NOT NULL
--      GROUP BY actor_user_id, actor_session_id
--
-- Without the index this is a parallel Seq Scan of all of events and an external sort.
-- The key (actor_user_id, actor_session_id, project_id, occurred_at DESC) returns
-- rows already in group order, project_id = ANY is checked via the index, and
-- max(occurred_at) is read by an Index Only Scan without sorting.
--
-- Measurement on the sqlperf 1M stand (events 6.87 million rows, 1.63 million in the index
-- predicate; a non-admin with a scope of 2239 projects), EXPLAIN (ANALYZE, BUFFERS):
--   without the index - 505,307 buffers + temp 17,405, 1.54 s;
--   with the index    - 391,989 buffers, no temp, 0.82 s.
-- The (project_id, ...) order first is worse on the same stand (analysis sk-750, section 1).
-- Index size on the stand - 157 MB, build - 1.8 s.
--
-- Partial: rows without a session or without a project are not read by this query.
--
-- A plain CREATE INDEX, not CONCURRENTLY. The golang-migrate pgx/v5 driver
-- runs this file as one statement without BeginTx, i.e. outside a
-- transaction block, so CONCURRENTLY would be allowed. But when any
-- migration fails the version stays dirty, and api does not start until it is sorted out
-- by hand. A failed CONCURRENTLY also leaves an INVALID index with the same
-- name: it must be found and dropped, otherwise a retry fails on "already exists".
-- A failed plain index build leaves nothing. The price - writes to events
-- are blocked for the duration of the build.
-- An estimate for prod, not a measurement: about 311 thousand rows fall under the predicate, the build
-- takes about 0.35 s (proportional to 1.76 s for 1.63 million rows on the stand, sk-740).

CREATE INDEX idx_events_actor_session_seen
    ON events (actor_user_id, actor_session_id, project_id, occurred_at DESC)
    WHERE actor_session_id IS NOT NULL AND project_id IS NOT NULL;
