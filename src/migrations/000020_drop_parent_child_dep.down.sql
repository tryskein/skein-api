-- sk-33 down: forward-compat revert ONLY. The data is unrecoverable:
--   - auto-mirror parent-child edges were DELETE'd in up (the parent_id
--     column on issues still expresses the relationship — no data lost in
--     terms of hierarchy, just the redundant mirror is gone)
--   - divergent rows were UPDATE'd to type='related' with a metadata note
--     ('converted from parent-child by 000020') — these CANNOT be
--     mechanically distinguished from edges originally created as
--     'related' without external state we don't keep
--
-- Re-add `parent-child` to the CHECK constraint so a hand-rolled INSERT
-- by an operator post-revert is at least syntactically valid. We do NOT
-- attempt to re-create the deleted edges — issue any new ones manually if
-- needed, or roll forward and re-apply 000020 with adjusted policy.

ALTER TABLE dependencies DROP CONSTRAINT dependencies_type_check;
ALTER TABLE dependencies ADD CONSTRAINT dependencies_type_check
  CHECK (type IN ('blocks', 'waits-for', 'related', 'parent-child', 'discovered-from', 'supersedes', 'duplicates'));
