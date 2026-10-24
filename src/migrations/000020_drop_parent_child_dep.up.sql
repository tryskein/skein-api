-- sk-33: drop parent-child as a dep type; single hierarchy mechanism via
-- issues.parent_id. The dependencies edge was a redundant mirror of
-- parent_id, with a broken auto-mirror invariant in practice (272 issues
-- with parent_id, only 151 mirror edges in prod). This migration removes
-- the duplication entirely.

-- Step 1: preserve divergent rows. A "divergent" row has a parent-child
-- dep edge but the child has NO parent_id — the loose association would
-- be silently dropped if we just DELETE'd the type. Convert these to
-- `related` so the link survives, with a metadata note for audit.
UPDATE dependencies
SET type = 'related',
    metadata = jsonb_set(
      COALESCE(metadata, '{}'::jsonb),
      '{migration_note}',
      '"converted from parent-child by 000020"'
    ),
    version = version + 1
WHERE type = 'parent-child'
  AND from_id NOT IN (
    SELECT id FROM issues WHERE parent_id IS NOT NULL
  );

-- Step 2: drop all remaining parent-child edges. These are pure mirrors
-- of issues.parent_id — the column already expresses the relationship,
-- and after sk-33 the parent_id column is the ONLY source of truth.
DELETE FROM dependencies WHERE type = 'parent-child';

-- Step 3: tighten the CHECK so the type can never be inserted again.
-- The constraint name was created by 000005_dependencies as
-- `dependencies_type_check` (default name for column CHECK).
ALTER TABLE dependencies DROP CONSTRAINT dependencies_type_check;
ALTER TABLE dependencies ADD CONSTRAINT dependencies_type_check
  CHECK (type IN ('blocks', 'waits-for', 'related', 'discovered-from', 'supersedes', 'duplicates'));
