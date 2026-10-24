-- 000050_close_outcome — sk-489.
--
-- WHY. An issue has only the status "closed" and a free-text reason.
-- Everything that computes progress treats "closed" as "done": an epic, some of
-- whose children are closed as "won't do" or as a duplicate, looks more
-- done than it is, and cannot reach an honest 100 % while such children
-- sit in the denominator. The reason text cannot be parsed — it is free-form and
-- bilingual; a machine-distinguishable property is needed.
--
-- WHAT THIS MIGRATION DOES. It puts the closing OUTCOME next to the status — one of
-- four values (`done`, `wont_do`, `duplicate`, `superseded`) — seeds
-- it for all history and adds two guards: a CHECK "the outcome exists if and only
-- if the issue is closed" and a normalizing trigger that
-- keeps this invariant even under a write that knows nothing about the column.
--
-- Steps:
--   1. the column issues.close_outcome + a CHECK on the closed value set;
--   2. seeding history in one pass + a named counter in migration_counters;
--   3. the consistency CHECK (AFTER the seed — it would not pass before it);
--   4. the function and the BEFORE INSERT OR UPDATE normalizing trigger.
--
-- ⚠ WHY A TRIGGER, IF THERE IS A CHECK AND THERE IS CODE. Because the rollout is not
-- instantaneous. While the OLD api version runs on the NEW schema, its close
-- writes exactly `status`, `closed_at`, `closed_reason` and does not know the column —
-- and it would hit the consistency CHECK, that is closing an issue would start
-- failing on every old pod. Its reopen would leave an outcome on an
-- unclosed issue. The trigger turns both cases into the right state:
-- closed without an outcome → "done", not closed → no outcome. Hence the decision not to
-- require stopping all instances for THIS migration: the schema tolerates
-- old code.
--
-- The same mechanism fixes the second class of writes bypassing the code — test fixtures
-- that seed closed issues with a raw INSERT (`internal/testsupport`). A BEFORE
-- ROW trigger in Postgres fires BEFORE constraint checks, so the
-- INSERT branch here is not decorative symmetry: without it every such INSERT would hit
-- the consistency CHECK.
--
-- ⚠ WHAT IS DELIBERATELY NOT HERE. The registry of notification reasons and the revision of
-- `notif_classify` — the new reason "outcome changed" ships in a separate migration
-- 000051 together with the registry entry in code: the registry copies are checked by tests
-- in both directions, and they must appear in one commit.

-- ---------------------------------------------------------------------------
-- 1. The column and the closed value set
-- ---------------------------------------------------------------------------
--
-- text + CHECK, not an enum — like status / type / source of this same table:
-- a type-level enumeration would force ALTER TYPE for every future value and
-- gives nothing that CHECK does not.
ALTER TABLE issues
    ADD COLUMN close_outcome text
        CHECK (close_outcome IS NULL
               OR close_outcome IN ('done', 'wont_do', 'duplicate', 'superseded'));

COMMENT ON COLUMN issues.close_outcome IS
    'sk-489: closing outcome — how the issue ended, as opposed to the free '
    'text of closed_reason. One of four values: done (done), wont_do '
    '(will not do), duplicate (duplicate), superseded (superseded). '
    'Present if and only if status = ''closed'' — this is guarded by the '
    'CHECK issues_close_outcome_consistent_chk and kept by the trigger '
    'issues_normalize_close_outcome even under a write that does not know the column. '
    'Progress (epic ring, branch progress, row chips) counts as done '
    'only done; the other three — "closed without work" — drop out of the '
    'progress denominator and are shown separately.';

-- ---------------------------------------------------------------------------
-- 2. Seeding history + a named counter
-- ---------------------------------------------------------------------------
--
-- All previously closed issues get the outcome "done", including those closed by archiving in
-- the past: we do not sort them out retroactively (owner decision). There would be
-- nothing to sort them by — the only trace of intent lies in the free reason text.
--
-- One pass over the table, as the spec non-functional constraint requires. The counter
-- `issues_seeded_done` is what lets acceptance be checked with an SQL query rather than
-- by sampling data: the number seeded must equal the number
-- closed at apply time.
--
-- The postcondition is checked HERE, not in a test, for the same reason as in
-- 000044: a mismatch must stop the rollout before the
-- next migration lands on top.
--
-- WHAT THIS POSTCONDITION DOES NOT DO — stated after the review of wave sk-489/A/1
-- (critic self), so the next reader does not take it for a rollout gate,
-- which it is not. TODAY it cannot fire: both numbers are taken by
-- ONE predicate from ONE snapshot, and the ALTER TABLE above holds ACCESS
-- EXCLUSIVE, so a closed issue cannot appear between the seed and the
-- count. Its real value is as a guard against TWO PREDICATES DRIFTING APART: if
-- a future edit changes the seed condition and forgets the count condition (or
-- vice versa), the migration fails here instead of leaving a partially seeded history.
-- That is why the predicates are written side by side and not factored into a shared expression:
-- a shared expression would make the guard a tautology. It is also the reason the consistency CHECK
-- is added below and not above: before the seed no closed
-- issue would pass it, and the migration would fail on the very first one.
DO $$
DECLARE
    v_seeded bigint;
    v_closed bigint;
BEGIN
    UPDATE issues SET close_outcome = 'done' WHERE status = 'closed';
    GET DIAGNOSTICS v_seeded = ROW_COUNT;

    SELECT count(*) INTO v_closed FROM issues WHERE status = 'closed';
    IF v_seeded <> v_closed THEN
        RAISE EXCEPTION
            'migration 000050: seeded % rows but % issues are closed', v_seeded, v_closed;
    END IF;

    INSERT INTO migration_counters (migration, metric, value) VALUES
        ('000050_close_outcome', 'issues_seeded_done', v_seeded);

    RAISE LOG 'migration 000050: issues_seeded_done=%', v_seeded;
END
$$;

-- ---------------------------------------------------------------------------
-- 3. Consistency CHECK (INV-1)
-- ---------------------------------------------------------------------------
--
-- An equality of two predicates, not a pair of one-way conditions: it reads as
-- the statement itself — "outcome exists ⇔ issue is closed" — and closes both errors
-- with one expression (closed without an outcome, unclosed with an outcome). status is declared
-- NOT NULL, so no three-valued logic arises here.
ALTER TABLE issues
    ADD CONSTRAINT issues_close_outcome_consistent_chk
        CHECK ((status = 'closed') = (close_outcome IS NOT NULL));

-- ---------------------------------------------------------------------------
-- 4. The normalizing trigger
-- ---------------------------------------------------------------------------
--
-- It is not "insurance against a bug" — it is a load-bearing part of compatibility
-- during the rollout (see the header). There is exactly one rule, in both directions:
--
--   issue is closed, no outcome  → "done";
--   issue is not closed          → no outcome.
--
-- What it does NOT do, and this is just as important: for an already closed issue with an
-- outcome set it does NOT touch the outcome. An old pod rewriting the close reason of an
-- already closed issue does not know the column and does not name it in its UPDATE — so
-- NEW.close_outcome carries the previous value, and "done" does not
-- overwrite it. A `wont_do` set by a person is not lost because of someone else's edit of
-- the reason.
--
-- search_path is pinned (pg_catalog, public) — the same security default as the
-- neighboring trigger of this table (000044).
CREATE FUNCTION skein_normalize_close_outcome() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public
    AS $$
BEGIN
    IF NEW.status = 'closed' THEN
        IF NEW.close_outcome IS NULL THEN
            NEW.close_outcome := 'done';
        END IF;
    ELSE
        NEW.close_outcome := NULL;
    END IF;
    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION skein_normalize_close_outcome() IS
    'sk-489: keeps INV-1 (outcome exists ⇔ issue is closed) under a write that does not '
    'know the column close_outcome — the old api version in the rollout window and '
    'raw fixture INSERTs. Closed without an outcome → done; not closed → NULL; for an already '
    'closed issue it does not touch the outcome that is set.';

CREATE TRIGGER issues_normalize_close_outcome
    BEFORE INSERT OR UPDATE ON issues
    FOR EACH ROW
    EXECUTE FUNCTION skein_normalize_close_outcome();

-- NO INDEX, ON PURPOSE. There is no entry into the table by outcome: it is read
-- together with the issue row that has already been found, and takes part in aggregates over
-- the children of an epic, where selection goes by the parent-child edge and the outcome is only
-- summed. An index would serve a nonexistent query and cost more on
-- every close.
