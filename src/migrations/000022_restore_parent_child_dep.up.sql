-- sk-35: restore parent-child as a typed dep edge — beads-style canonical
-- hierarchy. Reverses the sk-33 (000020) deletion. After this migration
-- the dependencies table is the source of truth for parent-child links;
-- `issues.parent_id` stays as a frozen column for backward-compat reads
-- until the next phase (sk-35.1 drops the column).
--
-- Three steps:
--   1. Re-open the CHECK constraint to allow `parent-child` again.
--   2. Backfill one parent-child dep edge per row in `issues` that still
--      has a non-NULL `parent_id`. Skips rows that already have an edge
--      (cheap defense — also catches re-runs and any manually inserted
--      rows post-restore).
--   3. Make sure `child_counters.last_child` is at least as high as the
--      max suffix already in issues for that parent. After phase 2 of
--      sk-35, new children no longer set issues.parent_id, so the
--      counter must be the only fallback for `nextChildID`.

-- Step 1: re-open CHECK to allow parent-child again.
ALTER TABLE dependencies DROP CONSTRAINT dependencies_type_check;
ALTER TABLE dependencies ADD CONSTRAINT dependencies_type_check
  CHECK (type IN ('blocks','waits-for','related','discovered-from',
                  'supersedes','duplicates','parent-child'));

-- Step 2: backfill parent-child edges from issues.parent_id. Skip pairs
-- with any existing dep row (composite PK is (from_id,to_id,type)) so
-- a re-run is a no-op and the 'related' rows the sk-33 step-1
-- converter left behind aren't disturbed.
--
-- The dependencies table has no id column — composite PK is the
-- identity. The `migration_note` lives in metadata so a future audit
-- can find every row this migration emitted.
INSERT INTO dependencies (
  from_id, to_id, type, metadata, version, created_by, created_at
)
SELECT
  i.id, i.parent_id, 'parent-child',
  jsonb_build_object('migration_note','backfilled from issues.parent_id by 000022'),
  1,
  COALESCE(i.created_by, '00000000-0000-0000-0000-000000000000'::uuid),
  COALESCE(i.created_at, now())
FROM issues i
WHERE i.parent_id IS NOT NULL
ON CONFLICT (from_id, to_id, type) DO NOTHING;

-- Step 3: ensure child_counters has the right last_child for every parent
-- that has hierarchical children. Extract the LAST dotted segment only.
INSERT INTO child_counters (parent_id, last_child)
SELECT
  regexp_replace(i.id, '\.[0-9]+$', '')                AS parent,
  MAX((regexp_match(i.id, '\.([0-9]+)$'))[1]::int)     AS last_child
FROM issues i
WHERE i.id ~ '\.[0-9]+$'
GROUP BY 1
ON CONFLICT (parent_id) DO UPDATE
  SET last_child = GREATEST(child_counters.last_child, EXCLUDED.last_child);
