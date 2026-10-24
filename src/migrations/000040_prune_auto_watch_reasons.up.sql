-- 000040_prune_auto_watch_reasons — sk-303.13 / T018, migration 2 of three.
--
-- What it does and why this way.
--
-- The wave narrows the set of reasons for which an issue watch is created
-- AUTOMATICALLY: creator and assignee stay, "for a comment" (commenter) and
-- "for a mention" (mention) go. The code narrowing (T008) only applies
-- going forward — it does not touch accumulated watches, so a person who
-- once commented on someone else's issue would keep receiving its feed.
-- This migration strips the already-accumulated reasons.
--
-- The invariant is stated as a GENERAL RULE, not a list of special cases
-- (US-8, SC-007): exactly two forbidden reasons are stripped, and the
-- watch row is deleted if and only if the reasons array is EMPTY after
-- that strip. Any other reason keeps the watch alive — 'manual',
-- 'creator', 'assignee', and the sticky 'unwatched' mark alike. The last
-- especially: a person who unwatched by hand, whose row looks like
-- {unwatched, commenter}, must survive the migration as an unwatch, not
-- become the absence of a row, because absence of a row means "not
-- unwatched" and the feed would come back to them on its own.
--
-- The two steps below are ordered so a temp table is unnecessary: first
-- the rows consisting ONLY of forbidden reasons are DELETED (predicate
-- "array is contained in the forbidden set AND intersects it" — the
-- second condition cuts already-empty arrays, which the migration does
-- not touch at all), and only then are reasons cleaned from the remaining
-- rows. After the first step an "empty array" can no longer arise, so the
-- second step does not delete anything by accident.
--
-- Bounds. project_watchers are not touched: a project watch is created
-- only by hand ('manual' is the column default), and automatic
-- "comment"/"mention" reasons do not exist on that path — no code path
-- writes them there. The spec changes "the set of reasons for which a
-- watch is created automatically" on the issue specifically.
--
-- Irreversibility. Stripped reasons are not restored: there is nowhere to
-- restore them from (only the deleted row knew which ones had accumulated),
-- and "give them back to everyone" would return exactly the noise this
-- wave exists to remove. The down migration is honest about this — see
-- .down.sql.

-- ---------------------------------------------------------------------------
-- Counters are TAKEN BEFORE the mutation: after it, "how many there were"
-- cannot be reproduced, and migration observability requires exactly these
-- numbers.
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    forbidden       CONSTANT text[] := ARRAY['commenter', 'mention'];
    v_accounts      bigint;
    v_touched       bigint;
    v_deleted       bigint;
    v_kept          bigint;
    v_leftover      bigint;
BEGIN
    SELECT count(DISTINCT user_id), count(*)
      INTO v_accounts, v_touched
      FROM issue_watchers
     WHERE reasons && forbidden;

    -- Rows where EVERY reason is forbidden: reasons <@ forbidden means
    -- "contained in", and reasons && forbidden cuts the empty array (the
    -- empty set is contained in any set, but the migration does not touch
    -- it).
    SELECT count(*)
      INTO v_deleted
      FROM issue_watchers
     WHERE reasons && forbidden
       AND reasons <@ forbidden;

    v_kept := v_touched - v_deleted;

    -- Step 1: orphaned watches.
    DELETE FROM issue_watchers
     WHERE reasons && forbidden
       AND reasons <@ forbidden;

    -- Step 2: on the rest, only the forbidden reasons are stripped; order of
    -- the others is preserved (WITH ORDINALITY) so row diffs stay readable.
    UPDATE issue_watchers
       SET reasons = ARRAY(
               SELECT r
                 FROM unnest(reasons) WITH ORDINALITY AS t(r, ord)
                WHERE NOT (r = ANY(forbidden))
                ORDER BY ord)
     WHERE reasons && forbidden;

    -- Post-condition: no forbidden reason remains anywhere. The check lives
    -- here, not in a test, because the migration is irreversible: a mismatch
    -- must stop the deploy before the next migration lands on top.
    SELECT count(*) INTO v_leftover FROM issue_watchers WHERE reasons && forbidden;
    IF v_leftover > 0 THEN
        RAISE EXCEPTION
            'migration 000040: % issue_watchers row(s) still carry a pruned reason',
            v_leftover;
    END IF;

    INSERT INTO migration_counters (migration, metric, value) VALUES
        ('000040_prune_auto_watch_reasons', 'watch_accounts_affected',        v_accounts),
        ('000040_prune_auto_watch_reasons', 'watch_rows_with_pruned_reason',  v_touched),
        ('000040_prune_auto_watch_reasons', 'watch_rows_deleted',             v_deleted),
        ('000040_prune_auto_watch_reasons', 'watch_rows_kept_other_reason',   v_kept);

    RAISE LOG 'migration 000040: watch_accounts_affected=% watch_rows_with_pruned_reason=% watch_rows_deleted=% watch_rows_kept_other_reason=%',
        v_accounts, v_touched, v_deleted, v_kept;
END
$$;
