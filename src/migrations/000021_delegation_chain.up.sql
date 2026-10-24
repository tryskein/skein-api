-- 000021 — Delegated chain columns: previous_assignees + dismissed_by.
--
-- sk-34: extend the Delegated lens beyond "createdBy = me AND assignee !=
-- me" to "I'm somewhere in this issue's chain of responsibility". Two
-- new uuid[] columns on issues drive the new predicate:
--
--   - previous_assignees uuid[]
--       Every user UUID that was ever this issue's assignee, in append-
--       order (with no dupes — the PatchIssue assignee-change branch
--       enforces "append iff not already present"). When Alice creates a
--       task, assigns to Bob, Bob reassigns to Charlie, Bob ends up in
--       this array. Lets us surface the issue in Bob's Delegated lens
--       even though he's no longer the current assignee.
--
--   - dismissed_by uuid[]
--       Users who explicitly opted out of this issue (via the new "Drop
--       from Delegated" affordance / skein_drop_delegated MCP tool).
--       Sticky — removed from the array only when the user becomes the
--       current assignee again (the PatchIssue assignee-change branch
--       clears the entry on re-assignment). The Delegated filter excludes
--       any issue where the caller is in dismissed_by.
--
-- The new Delegated predicate (single source for /me + project-scoped +
-- cross-project paths):
--
--     created_by = me
--     OR (me = ANY(previous_assignees)
--         AND NOT me = ANY(dismissed_by)
--         AND LOWER(assignee) <> me_username)
--
-- Storage: uuid[] (Postgres native, no JSON wrapping). Both columns are
-- NOT NULL with default '{}' so existing rows backfill to empty arrays
-- without a separate UPDATE.
--
-- Indexes: GIN on each array. The Delegated predicate uses `= ANY(...)`
-- which lowers to the `<@>` containment expressions a GIN array index
-- can serve directly. Without these the cross-project Delegated lens
-- would do a seq-scan on issues — fine at 1k rows, painful at 1M.
--
-- Lock cost: ADD COLUMN with a constant default on uuid[] is metadata-
-- only in PostgreSQL 11+ — no rewrite. Index build runs a write lock
-- for the duration; sub-second on current DB size.
--
-- No data backfill: existing rows start with empty arrays (the column
-- defaults). Any future re-assignment will start populating them via the
-- PatchIssue branch.
ALTER TABLE issues
    ADD COLUMN previous_assignees uuid[] NOT NULL DEFAULT '{}',
    ADD COLUMN dismissed_by       uuid[] NOT NULL DEFAULT '{}';

-- GIN indexes for `<caller> = ANY(previous_assignees)` and the matching
-- check on `dismissed_by`. Both lookups are O(log N) on the operator
-- class for uuid[] (gin_array_ops) once the index is in place.
CREATE INDEX idx_issues_previous_assignees ON issues USING GIN (previous_assignees);
CREATE INDEX idx_issues_dismissed_by       ON issues USING GIN (dismissed_by);
