-- 000044_review_and_nudges — sk-303.14, task T003.
--
-- Data foundation for the wave's two mechanics: "creator review" and
-- "repeating reminders". One migration, because all of its parts are the
-- state of ONE feature, and half the schema without the other half is
-- untestable.
--
-- What is introduced:
--   1. issues: the "review required" flag, the review mark (author and
--      time), and the burst schedule base — the moment of the last
--      assignee assignment, stamped by a trigger.
--   2. user_notification_prefs: the mechanic kill switch, the personal
--      flag default, and three schedule parameters with bounds from
--      internal/review/schedule.go.
--   3. reminder_rate_limit: rate-limit state on the pair "user and
--      category".
--   4. notifications: a new terminal delivery outcome — 'superseded',
--      for a superseded notification.
--   5. migration_counters: three named counters of this migration.
--
-- WHAT IS DELIBERATELY ABSENT — PARTIAL INDEXES. An index without its
-- query is untestable: the set's semantic contract states membership, but
-- not join shape, column order, or selectivity. A guessed-ahead index then
-- forces a correct query to be distorted so the planner will pick it.
-- Indexes are introduced by the tasks of their queries — unreviewed
-- (000046) and unaccepted (000047).
--
-- GENERATION. sqlc.yaml reads `schema: migrations`, so this migration
-- moves internal/storage/generated/. The result of `sqlc generate` is
-- committed in the same commit — otherwise the consistency gate (the
-- sqlc-generated job, sk-339) goes red on every later task of the wave.

-- ---------------------------------------------------------------------------
-- 1. issues: flag, mark, schedule base
-- ---------------------------------------------------------------------------
--
-- review_required — "review required". DEFAULT false, and this is the
-- migration's key decision: the flag value for issues created BEFORE
-- release is "not required" (SC-015). Otherwise on release day the entire
-- historical archive of closed delegated issues would flood counters and
-- reminders at once. Issue-create paths stamp the flag EXPLICITLY, from
-- the author's personal default (the factory personal default is
-- "required", US-3), so the narrow schema default remains a safety net
-- for paths that do not yet know about the column, not the working value.
--
-- reviewed_at / reviewed_by — the review mark: time and author. The pair
-- repeats the accept construction (accepted_at / accepted_by, 000017),
-- including ON DELETE SET NULL: on hard-delete of an account the mark
-- time remains (the issue WAS reviewed) and the identity is lost. There
-- is deliberately no linking CHECK between them — it would make user
-- deletion impossible.
--
-- assignee_set_at — burst schedule base (R-5). The moment the issue was
-- assigned to THIS CURRENT assignee. Issue creation time is not that
-- moment: an issue can be created long before assignment (SC-019), and
-- reconstructing the moment from the event journal in the worker's hot
-- query is unacceptable in cost. Hence stored state.
--
-- NULL on existing rows is MEANINGFUL and means exactly one thing: the
-- assignment moment of this issue was not observed, because assignment
-- happened before release. There is nothing to restore it from, and it
-- must not be invented: any substitution (created_at, updated_at, deploy
-- time) would be a fabricated fact from which the schedule would compute
-- real marks. For the schedule, a row without a base is a row without a
-- base (internal/review: ErrNoBase); the decision about query behaviour
-- on such rows belongs to the task of that query, not this migration.
-- The issues_assigned_without_base counter below makes their count
-- observable at deploy.
ALTER TABLE issues
    ADD COLUMN review_required  boolean NOT NULL DEFAULT false,
    ADD COLUMN reviewed_at      timestamptz,
    ADD COLUMN reviewed_by      uuid REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN assignee_set_at  timestamptz;

COMMENT ON COLUMN issues.review_required IS
    'Whether the creator must review the result. Private bookkeeping of '
    'the author: read and changed only by them (INV-2, INV-4). Issues created '
    'before the review workflow release have false — "not required" (SC-015).';
COMMENT ON COLUMN issues.reviewed_at IS
    'Moment of the review mark. NULL — the result has not been reviewed. The '
    'mark does not change issue status and gates nothing (INV-1).';
COMMENT ON COLUMN issues.reviewed_by IS
    'Author of the review mark. Always the issue author (INV-4). NULL after '
    'hard-delete of an account — the mark time is preserved.';
COMMENT ON COLUMN issues.assignee_set_at IS
    'Burst schedule base (R-5): the moment the issue was assigned to the '
    'current assignee. Stamped by trigger issues_stamp_assignee_set_at on '
    'every assignee-change path. NULL — the assignment moment was not '
    'observed (issue assigned before the review workflow release) or there is no '
    'assignee; such a row has no schedule base.';

-- ---------------------------------------------------------------------------
-- 1a. Trigger that stamps the schedule base
-- ---------------------------------------------------------------------------
--
-- WHY A TRIGGER, NOT A STAMP IN CODE. Requirement R-5 — "stamped on EVERY
-- assignee-change path, including ones that appear later". There are
-- several paths today, and naming them is not a contract: a forgotten
-- path silently gives a wrong burst, which in production looks like
-- "reminders arrive at the wrong time". The trigger forgets no path,
-- including raw SQL and import. There is a trigger precedent in the
-- project — 000034.
--
-- WHY A NAIVE TRIGGER IS WRONG HERE — three separate requirements:
--
--   (a) INSERT. The main reminder-scenario path is creating an issue WITH
--       an assignee immediately; a trigger only on UPDATE does not see
--       it, and the burst never starts (SC-019).
--
--   (b) ACTUAL change, not mentioning the column. Existing write paths
--       (PatchIssue and neighbours) always name assignee in the
--       assignment, usually as COALESCE(:new, assignee). A trigger
--       "UPDATE fired — stamp" would restart the burst on every title
--       edit and on every claim of an already-assigned issue. So the
--       condition compares VALUES.
--
--   (c) COMPARISON IS CASE-INSENSITIVE. The domain compares assignees
--       that way — LOWER(assignee) sits in every delegated, counter, and
--       unaccepted predicate. A case change is not an assignee change,
--       and the schedule base must not move because of it.
--
-- IS DISTINCT FROM instead of <> covers NULL on both sides in one
-- condition: assign (NULL → name), reassign (name → other name) and
-- clear assignee (name → NULL) all pass as "the value changed", and
-- identical NULLs do not.
--
-- CLEARING THE ASSIGNEE CLEARS THE BASE. The column means "the moment
-- the CURRENT assignee was assigned"; there is no current assignee —
-- so there is no moment. A leftover stamp would be a lie from which
-- real marks would later be computed. Re-assigning the same person
-- stamps a fresh value.
--
-- INSERT respects an explicitly passed value (condition assignee_set_at
-- IS NULL): import and data restore must be able to bring in the real
-- assignment moment, not get the import moment. UPDATE gets no such
-- exception: there the stamp is set exactly when the assignee actually
-- changed, and that is the trigger's purpose.
--
-- search_path is pinned (pg_catalog, public) — the same security default
-- as the project's first trigger.
CREATE FUNCTION skein_stamp_assignee_set_at() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.assignee IS NOT NULL AND NEW.assignee_set_at IS NULL THEN
            NEW.assignee_set_at := now();
        END IF;
        RETURN NEW;
    END IF;

    -- UPDATE: only an actual change of assignee as a person.
    IF lower(NEW.assignee) IS DISTINCT FROM lower(OLD.assignee) THEN
        IF NEW.assignee IS NULL THEN
            NEW.assignee_set_at := NULL;
        ELSE
            NEW.assignee_set_at := now();
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER issues_stamp_assignee_set_at
    BEFORE INSERT OR UPDATE ON issues
    FOR EACH ROW
    EXECUTE FUNCTION skein_stamp_assignee_set_at();

-- ---------------------------------------------------------------------------
-- 2. user_notification_prefs: kill switch, flag default, schedule
-- ---------------------------------------------------------------------------
--
-- Rows of this table are created lazily (one per user, insert-on-first-use),
-- so absence of a row means "everything factory".
--
-- The kill switch and the personal flag default are NOT NULL with default
-- true: the review workflow is on, and the factory personal flag default is
-- "required" (US-3). Turning it off is a deliberate human action (INV-6),
-- not a starting state.
--
-- The three schedule parameters are NULLABLE, and that is not sloppiness:
-- NULL here means "empty, take factory" and is distinct from an explicitly
-- set value. For burst reminder count the difference matters: 0 is a
-- legitimate "no burst" setting, and merging it with "unset" would take
-- away the person's ability to turn the burst off.
--
-- BOUNDS ARE TAKEN FROM THE SCHEDULE CONTRACT, not invented here:
-- internal/review/schedule.go — MinBurstCount=0, MaxBurstCount=6,
-- MinIntervalMinutes=15, MaxIntervalMinutes=527040, factory 3 / 180 / 720.
-- The CHECK exists as a second instance of these numbers (a migration
-- cannot call application code); their equivalence is checked by class
-- T004, not by hoping someone notices. A value outside the bounds is
-- REJECTED, not silently rewritten (SC-022, SC-027) — at this level by a
-- database refusal, at the API level by 422.
--
-- THE UPPER BOUND OF INTERVALS — 527040 minutes (366 days) — is not
-- decoration. int4 itself allows 2 147 483 647 minutes, and the schedule
-- computes marks by multiplying the interval by the number and holds them
-- in int64 nanoseconds (time.Duration, ceiling ~292 years). A value above
-- the ceiling overflows that multiplication and yields a NEGATIVE offset
-- — a mark BEFORE the base, i.e. a forever-overdue reminder instead of a
-- rare one. The schema must refuse such a value here, before it reaches
-- the arithmetic: the ceiling's justification is in the comment on
-- review.MaxInterval.
ALTER TABLE user_notification_prefs
    ADD COLUMN review_mechanic_enabled     boolean NOT NULL DEFAULT true,
    ADD COLUMN review_required_default     boolean NOT NULL DEFAULT true,
    ADD COLUMN reminder_burst_count        int,
    ADD COLUMN unaccepted_interval_minutes int,
    ADD COLUMN unreviewed_interval_minutes int;

ALTER TABLE user_notification_prefs
    ADD CONSTRAINT user_prefs_reminder_burst_count_chk
        CHECK (reminder_burst_count IS NULL
               OR reminder_burst_count BETWEEN 0 AND 6),
    ADD CONSTRAINT user_prefs_unaccepted_interval_chk
        CHECK (unaccepted_interval_minutes IS NULL
               OR unaccepted_interval_minutes BETWEEN 15 AND 527040),
    ADD CONSTRAINT user_prefs_unreviewed_interval_chk
        CHECK (unreviewed_interval_minutes IS NULL
               OR unreviewed_interval_minutes BETWEEN 15 AND 527040);

COMMENT ON COLUMN user_notification_prefs.review_mechanic_enabled IS
    'Master kill switch of the review workflow. false — the unreviewed set, '
    'its display and reminders on it disappear for the user (INV-6). Data is '
    'preserved and returns when the switch is turned back on.';
COMMENT ON COLUMN user_notification_prefs.review_required_default IS
    'Personal default of the "review required" flag, applied on EVERY issue '
    'create path where the flag is not passed explicitly (US-3). Factory is '
    'true.';
COMMENT ON COLUMN user_notification_prefs.reminder_burst_count IS
    'Number of reminders in the first hour for unaccepted issues, 0..6. '
    'NULL — empty, factory is taken (3). 0 — no burst. Bounds from '
    'internal/review/schedule.go.';
COMMENT ON COLUMN user_notification_prefs.unaccepted_interval_minutes IS
    'Background interval for unaccepted issues, minutes, 15..527040 '
    '(366 days). NULL — empty, factory is taken (180). Bounds from '
    'internal/review/schedule.go; the upper is the representability limit '
    'of a mark.';
COMMENT ON COLUMN user_notification_prefs.unreviewed_interval_minutes IS
    'Background interval for unreviewed results, minutes, 15..527040 '
    '(366 days). NULL — empty, factory is taken (720). Bounds from '
    'internal/review/schedule.go; the upper is the representability limit '
    'of a mark.';

-- ---------------------------------------------------------------------------
-- 3. reminder_rate_limit: rate-limit state
-- ---------------------------------------------------------------------------
--
-- A separate table, not columns in user prefs: prefs are a human choice,
-- and this is delivery state. Mixing them would mean rewriting the human
-- choice on every worker tick.
--
-- The key is the pair "user and category" (INV-8: "no more than one
-- reminder of EACH category per window"; the rate limit is on the user,
-- not the issue). The primary key IS the mechanism: a window is consumed
-- by one conditional UPSERT with RETURNING, and zero returned rows means
-- "the window was already eaten by a neighbouring instance". Crash
-- resilience between steps comes from the same place — state lives in
-- the database, not in the worker's memory.
--
-- last_formed_at, not last_sent_at: INV-10 requires naming by the
-- checkable fact. The notification was FORMED — that is what we observe;
-- delivery is asynchronous and may end in failure, and it does not return
-- the window.
--
-- WHY next_due_at SITS NEXT TO last_formed_at — A FROZEN DUE TIME.
-- The spec requires: a prefs change takes effect from the NEXT window;
-- an already-started one is not recomputed after the fact — neither by
-- shortening nor by stretching. The moment of the last reminder is
-- physically not enough for this rule: it does not remember which prefs
-- opened the window, and any recompute from it takes the CURRENT ones.
-- On the factory scale with the last mark at base+4h and a three-hour
-- background, the next is base+7h; if the person changes the interval
-- to one hour, a recompute would give base+5h (the running window is
-- cut), if to 12 hours — base+13h (stretched). Both outcomes contradict
-- the rule, and neither is visible in the state: the row looks the same.
--
-- So the due time is STORED, not derived. Working order with the pair:
--   1. the window is consumed by a conditional UPSERT on
--      next_due_at <= now();
--   2. the same operator writes a new next_due_at, computed from CURRENT
--      prefs (internal/review: Settings.NextReminderAt) — that is
--      "from the next window";
--   3. the "is it due" check on next_due_at ONLY READS it and never
--      recomputes it on a prefs change.
-- Task T018 (sk-303.14.2.8.2) implements this rule on this column.
--
-- ⚠ THERE IS NO CATCH-UP. Whether the worker slept through one window or
-- a hundred — marks are NOT replayed: NextReminderAt skips the missed
-- ones and returns a strictly future due time. This is not an
-- implementation detail, it is a defence: replay would give a recovered
-- worker a barrage of reminders for every slept-through window — exactly
-- the pounding this table exists to prevent. Computing the next due time
-- by shifting from the previous is FORBIDDEN; compute it only via
-- NextReminderAt from the base and the formation moment.
--
-- So under the prescribed protocol next_due_at is always strictly later
-- than last_formed_at. There is nevertheless no linking CHECK — but for
-- a different reason: an operator repairing a stuck row by hand may
-- legitimately need to set the due time into the past to force a
-- reminder. A schema constraint would take that move away, and would not
-- save from an implementation error: catch-up is visible not as one row
-- but as a sequence.
--
-- NOT NULL without DEFAULT: the due time must be computed from the prefs
-- that were in force at window open, and now() or any other schema
-- default would be a fabricated due time, tied to no prefs. The table is
-- new — there is nothing to fill, no backfill.
--
-- The category set is closed by a CHECK and repeats
-- internal/review.Categories().
CREATE TABLE reminder_rate_limit (
    user_id        uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    category       text        NOT NULL CHECK (category IN ('unaccepted', 'unreviewed')),
    last_formed_at timestamptz NOT NULL,
    next_due_at    timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, category)
);

COMMENT ON TABLE reminder_rate_limit IS
    'Rate-limit state of repeating reminders on the pair "user and '
    'category" (INV-8). Not a preference, but delivery state: written by '
    'the reminder worker, not edited by a human.';
COMMENT ON COLUMN reminder_rate_limit.last_formed_at IS
    'Moment of the last FORMED reminder of this category (INV-10). The '
    'window is consumed by a created notification, not by a successful '
    'delivery.';
COMMENT ON COLUMN reminder_rate_limit.next_due_at IS
    'Frozen moment of the next reminder of this category, computed from the '
    'prefs that were in force at OPEN of the current window. Stored, not '
    'derived from last_formed_at: derivation would take current prefs and '
    'thereby recompute an already-started window — cut it on interval '
    'shortening and stretch it on lengthening, and the spec requires a '
    'prefs change to take effect only from the next window. The "is it due" '
    'check reads this moment; a new value is written from CURRENT prefs at '
    'the same moment the window is consumed.';

-- ---------------------------------------------------------------------------
-- 4. notifications: new terminal outcome — supersession
-- ---------------------------------------------------------------------------
--
-- Spec ("Supersession on prolonged unavailability"): if transport is
-- unavailable longer than a window, several notifications of one
-- "addressee and category" pair will legitimately form in that time.
-- Only the freshest is delivered; earlier undelivered ones are SUPERSEDED
-- and finish with their OWN outcome.
--
-- Reusing an existing outcome is not allowed: supersession would stop
-- being distinguishable from a configuration refusal in the row state
-- and would remain observable only in a measurement label — and the
-- requirement is about the state. The outcome set is closed by a schema
-- constraint (000039), so the new value is introduced by a migration, not
-- a constant in code.
--
-- Recreating the CHECK, not ADD: the closed set exists as exactly one
-- constraint, and a second one next to it would mean two sources of
-- truth.
ALTER TABLE notifications
    DROP CONSTRAINT notifications_push_outcome_chk;

ALTER TABLE notifications
    ADD CONSTRAINT notifications_push_outcome_chk
        CHECK (push_outcome IS NULL OR push_outcome IN (
            'success',
            'no_device',
            'access_revoked',
            'prefs_unavailable',
            'configuration_no_send',
            'transport_exhausted',
            'superseded'
        ));

-- ---------------------------------------------------------------------------
-- 5. Migration counters
-- ---------------------------------------------------------------------------
--
-- Observability requirement: the migration must leave in the journal named
-- numbers of affected rows — otherwise "did it apply" is checked only by
-- sampling data. Three separate metrics, not one total: a single number
-- is useless to the operator.
--
--   issues_defaulted_no_review    — issues created before release: their
--                                   flag means "not required" (SC-015).
--   issues_assigned_without_base  — of those, the assigned ones: these
--                                   rows have no burst schedule base and
--                                   it has nowhere to come from (see the
--                                   comment on assignee_set_at). The
--                                   operator must see this number at
--                                   deploy.
--   prefs_rows_defaulted          — existing user-prefs rows that received
--                                   the mechanic's values.
DO $$
DECLARE
    v_issues        bigint;
    v_assigned      bigint;
    v_prefs         bigint;
BEGIN
    SELECT count(*) INTO v_issues   FROM issues;
    SELECT count(*) INTO v_assigned FROM issues WHERE assignee IS NOT NULL;
    SELECT count(*) INTO v_prefs    FROM user_notification_prefs;

    -- Post-condition: no historical issue is left with the flag on. The
    -- check lives here, not in a test: a mismatch must stop the deploy
    -- before the next migration lands on top.
    IF EXISTS (SELECT 1 FROM issues WHERE review_required) THEN
        RAISE EXCEPTION
            'migration 000044: historical issues carry review_required = true';
    END IF;

    INSERT INTO migration_counters (migration, metric, value) VALUES
        ('000044_review_and_nudges', 'issues_defaulted_no_review',   v_issues),
        ('000044_review_and_nudges', 'issues_assigned_without_base', v_assigned),
        ('000044_review_and_nudges', 'prefs_rows_defaulted',         v_prefs);

    RAISE LOG 'migration 000044: issues_defaulted_no_review=% issues_assigned_without_base=% prefs_rows_defaulted=%',
        v_issues, v_assigned, v_prefs;
END
$$;
