-- 000051_outcome_changed_reason — sk-489, US-3.
--
-- WHY. A closed issue got an OUTCOME (000050), and the outcome can be changed: by editing
-- the closed issue or by closing it again with a different value. The ISSUE AUTHOR
-- must learn about it — they are the only one who could mark the result
-- as reviewed, and the only one for whom "the result no longer stands" is
-- news. News cannot be delivered without a reason in the registry: the classifier
-- calls a type that has no reason mapped `unmapped`, that is a CONFIGURATION
-- ERROR, with an ERROR-level record and an increment of an alerting metric.
-- This migration closes exactly that window.
--
-- WHAT IT DOES, step by step:
--   1. extends the CHECK of the column user_notification_reason_prefs.reason
--      with a fourteenth value `outcome_changed`;
--   2. seeds a preference row of the new reason for EVERY account — feed and push
--      enabled — and writes a named counter to migration_counters;
--   3. replaces notif_classify with revision 3: the thirteenth parameter
--      `p_outcome_changed` and two branches of rule R-7.
--
-- ⚠ WHY ALL THREE STEPS ARE IN ONE MIGRATION AND IN ONE COMMIT WITH THE Go REGISTRY.
-- The reason registry exists in SEVERAL copies: the table in code
-- (internal/notifications/reasons.go), this CHECK, the preference seed and the SQL
-- projection of the classifier. Guards compare them pairwise and in BOTH directions
-- (migrations/migrations_test.go#TestMigration000039_CheckConstraintMatchesRegistry
-- and neighbors), so copies that drifted apart by one commit turn the build red not
-- where the defect is. These steps cannot be separated — they can only be released
-- together.
--
-- ⚠ WHY DROP + CREATE, NOT CREATE OR REPLACE. A PARAMETER is added, that
-- is the signature changes. CREATE OR REPLACE would create an OVERLOAD: two
-- functions with the same name, and calls would silently diverge between them. Revision 2
-- made the same decision for the same reason.
--
-- ⚠ WHY THE THIRTEENTH PARAMETER IS APPENDED AT THE END, NOT INSERTED AMONG THE EVENT
-- FACTS. All parameters after `p_changed_fields` are boolean, so
-- INSERTING in the middle would not make resolution fail for a caller that
-- does not know about it: the arguments would just shift by position and bind to
-- the wrong names. Appending at the end makes the old 12-argument call
-- UNRESOLVABLE, that is loud.
--
-- WHAT RULE R-7 DOES (one close or one outcome change — one
-- notification to the author):
--   * `issue.outcome_changed` → reason `outcome_changed`, and ONLY to the author;
--   * `issue.status_changed` where `p_outcome_changed` is true (that is a
--     repeated close CHANGED the outcome) gives the author NO reason — otherwise they
--     would get two rows for one action. The condition is checked BEFORE the general
--     branch: today's branch reads "subscriber OR author", and an author
--     subscribed to their own issue would enter it as a subscriber.
--     A subscriber who is not the author gets the status row as usual.
--
-- EQUALITY OF THE COPIES rests not on vigilance: both implementations of the rule
-- execute a SHARED corpus `internal/notifications/testdata/reason_cases.json`,
-- and migrations/migration_000041_test.go compares them case by case. The third
-- implementation — the projection of the reason in the delivery path (internal/outbox/push.go) —
-- is checked against the same corpus in
-- internal/outbox/push_classification_parity_test.go.
--
-- GENERATION. sqlc.yaml reads `schema: migrations`, so a sqlc run
-- is mandatory and its result is committed in this same commit, even if the model did not
-- change. It is verified by the fact of the run, not by reasoning.

-- ---------------------------------------------------------------------------
-- 1. The fourteenth value in the closed set of reasons
-- ---------------------------------------------------------------------------
--
-- The list is written out in full, not "added" to the previous one: a CHECK in Postgres
-- is replaced, not extended, and the full list next to the migration reads
-- as a statement of what the set has become.
ALTER TABLE user_notification_reason_prefs
    DROP CONSTRAINT user_notification_reason_prefs_reason_check;

ALTER TABLE user_notification_reason_prefs
    ADD CONSTRAINT user_notification_reason_prefs_reason_check
        CHECK (reason IN (
            'assignment',
            'accepted',
            'delegated_status',
            'reminder',
            'mention',
            'issue_created',
            'comment',
            'attachment',
            'reparent',
            'auto_unclaim',
            'other_edits',
            'unaccepted_reminder',
            'review_reminder',
            'outcome_changed'
        ));

-- ---------------------------------------------------------------------------
-- 2. Seeding the preference row of the new reason
-- ---------------------------------------------------------------------------
--
-- ENABLED by default on both channels (owner decision, US-3): an outcome change
-- of a closed issue is news of the same class as "Status of a delegated
-- issue", and the person who set the issue learns about it until they opt out.
--
-- ON CONFLICT DO NOTHING is deliberate: a row could appear if the
-- migration is applied to a database where the new code has already managed to write a preference
-- (the mixed-version window). Overwriting would wipe an EXPLICIT choice of the person
-- with the factory value.
--
-- The counter is checked against an independent query right here, as in 000039: the number
-- seeded must equal the number of accounts. The guard catches not a race (the ALTER
-- above rules it out) but a DRIFT OF TWO PREDICATES — the seed and the count — if
-- a future edit touches one and forgets the other.
DO $$
DECLARE
    v_rows     bigint;
    v_accounts bigint;
BEGIN
    INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled)
    SELECT u.id, 'outcome_changed', true, true
      FROM users u
    ON CONFLICT (user_id, reason) DO NOTHING;
    GET DIAGNOSTICS v_rows = ROW_COUNT;

    SELECT count(*) INTO v_accounts FROM users;
    IF v_rows <> v_accounts THEN
        RAISE EXCEPTION
            'migration 000051: seeded % rows but % accounts exist', v_rows, v_accounts;
    END IF;

    INSERT INTO migration_counters (migration, metric, value) VALUES
        ('000051_outcome_changed_reason', 'outcome_changed_rows_written', v_rows),
        ('000051_outcome_changed_reason', 'accounts_defaulted',           v_accounts);

    RAISE LOG 'migration 000051: outcome_changed_rows_written=% accounts_defaulted=%',
        v_rows, v_accounts;
END
$$;

-- ---------------------------------------------------------------------------
-- 3. notif_classify, revision 3
-- ---------------------------------------------------------------------------

DROP FUNCTION notif_classify(
    text, text[], boolean, boolean, boolean, boolean,
    boolean, boolean, boolean, boolean, boolean, boolean);

CREATE FUNCTION notif_classify(
    p_event_type          text,
    p_changed_fields      text[],
    p_explicit_accept     boolean,
    p_stale_lock_release  boolean,
    p_is_subscriber       boolean,
    p_is_new_assignee     boolean,
    p_is_assignee         boolean,
    p_is_creator          boolean,
    p_is_actor            boolean,
    p_is_mentioned        boolean,
    p_is_reminder_owner   boolean,
    p_is_digest_recipient boolean,
    p_outcome_changed     boolean
) RETURNS notif_classification
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE
AS $fn$
DECLARE
    -- A closed list of types that live ONLY as journal records. Deliberately duplicates
    -- notifications.auditOnlyEventTypes: the migration does not call application
    -- code. Equality of the two copies is checked by a test, not by
    -- vigilance. `issue.outcome_changed` is NOT here and must not be:
    -- it is an ordinary public type with a reason and an addressee.
    audit_only  CONSTANT text[] := ARRAY[
        'issue.claimed', 'issue.closed', 'issue.archived',
        'dependency.created', 'dependency.deleted',
        'issue.assigned', 'issue.priority_changed', 'issue.field_changed',
        'issue.label_added', 'issue.label_removed', 'issue.components_changed',
        'issue.commits_attached', 'issue.pinned', 'issue.unpinned',
        'issue.delegation_dropped', 'issue.delegation_resumed',
        'review.marked', 'review.flag_changed',
        'comment.edited', 'comment.deleted', 'attachment.removed',
        'reminder.set', 'reminder.snoozed', 'reminder.dismissed',
        'project.created', 'project.updated', 'project.archived',
        'project.unarchived', 'project.field_changed', 'project.alias_added',
        'project.alias_removed', 'project.repo_added', 'project.repo_removed',
        'project.repo_updated', 'project.member_added', 'project.member_removed',
        'project.member_role_changed',
        'component.created', 'component.updated', 'component.archived',
        'component.unarchived', 'component.field_changed',
        'user.created', 'user.updated', 'user.field_changed',
        'user.deactivated', 'user.reactivated', 'user.password_changed'
    ];
    fields      text[]  := COALESCE(p_changed_fields, '{}'::text[]);
    subscriber  boolean := COALESCE(p_is_subscriber, false);
    new_assign  boolean := COALESCE(p_is_new_assignee, false);
    creator     boolean := COALESCE(p_is_creator, false);
    mentioned   boolean := COALESCE(p_is_mentioned, false);
    rem_owner   boolean := COALESCE(p_is_reminder_owner, false);
    digest_to   boolean := COALESCE(p_is_digest_recipient, false);
    outcome_chg boolean := COALESCE(p_outcome_changed, false);
    candidates  text[]  := '{}'::text[];
    best        text;
    best_prio   int     := -1;
    cand        text;
    prio        int;
    fld_assignee boolean;
    fld_priority boolean;
BEGIN
    IF p_event_type = ANY (audit_only) THEN
        RETURN ROW('audit_only', NULL)::notif_classification;
    END IF;

    CASE p_event_type
        WHEN 'issue.created' THEN
            IF new_assign THEN candidates := candidates || 'assignment'::text;    END IF;
            IF subscriber THEN candidates := candidates || 'issue_created'::text; END IF;

        WHEN 'issue.updated' THEN
            -- Exactly two edit fields carry a delivery path: assignee and priority.
            -- An edit of any other field lives in the journal, and that is not a refusal.
            fld_assignee := 'assignee' = ANY (fields);
            fld_priority := 'priority' = ANY (fields);
            IF NOT fld_assignee AND NOT fld_priority THEN
                RETURN ROW('audit_only', NULL)::notif_classification;
            END IF;
            IF fld_assignee AND (new_assign OR subscriber) THEN
                candidates := candidates || 'assignment'::text;
            END IF;
            IF fld_priority AND subscriber THEN
                candidates := candidates || 'other_edits'::text;
            END IF;

        WHEN 'issue.status_changed' THEN
            -- R-7: a close that INCIDENTALLY changed the outcome leaves the author exactly
            -- one notification — about the outcome change. The check goes BEFORE the general
            -- branch, because the general one reads "subscriber OR author", and the author
            -- of their own issue would enter it as a subscriber.
            IF outcome_chg AND creator THEN
                NULL;  -- no candidates → no_reason_for_recipient
            ELSIF subscriber OR creator THEN
                candidates := candidates || 'delegated_status'::text;
            END IF;

        WHEN 'issue.outcome_changed' THEN
            -- There is one addressee — the issue author. A subscriber is told about the same action
            -- by the status row (on a repeated close) or is told
            -- nothing (on an outcome edit).
            IF creator THEN candidates := candidates || 'outcome_changed'::text; END IF;

        WHEN 'issue.accepted' THEN
            -- An acceptance set automatically (claim, self-assignment) gets no
            -- reason: it is written only to the journal.
            IF NOT COALESCE(p_explicit_accept, false) THEN
                RETURN ROW('audit_only', NULL)::notif_classification;
            END IF;
            IF subscriber OR creator THEN candidates := candidates || 'accepted'::text; END IF;

        WHEN 'issue.unclaimed' THEN
            -- A human unlock carries no reason: the adjacent issue.status_changed has already
            -- delivered the signal, and two notifications for one action are forbidden (INV-6).
            IF NOT COALESCE(p_stale_lock_release, false) THEN
                RETURN ROW('audit_only', NULL)::notif_classification;
            END IF;
            IF subscriber THEN candidates := candidates || 'auto_unclaim'::text; END IF;

        WHEN 'issue.reparented' THEN
            IF subscriber THEN candidates := candidates || 'reparent'::text; END IF;

        WHEN 'comment.created' THEN
            IF subscriber THEN candidates := candidates || 'comment'::text; END IF;

        WHEN 'attachment.added' THEN
            IF subscriber THEN candidates := candidates || 'attachment'::text; END IF;

        WHEN 'comment.mention', 'issue.mention' THEN
            IF mentioned THEN candidates := candidates || 'mention'::text; END IF;

        WHEN 'reminder.rang' THEN
            IF rem_owner THEN candidates := candidates || 'reminder'::text; END IF;

        -- Reminder digests. Each has its own reason: merging categories
        -- is forbidden, and unsubscribing from one must not silence another.
        WHEN 'nudge.unaccepted_digest' THEN
            IF digest_to THEN candidates := candidates || 'unaccepted_reminder'::text; END IF;

        WHEN 'nudge.unreviewed_digest' THEN
            IF digest_to THEN candidates := candidates || 'review_reminder'::text; END IF;

        ELSE
            RETURN ROW('unmapped', NULL)::notif_classification;
    END CASE;

    IF cardinality(candidates) = 0 THEN
        RETURN ROW('no_reason_for_recipient', NULL)::notif_classification;
    END IF;

    -- When there are several candidate reasons, the more SPECIFIC one wins: "Issue assignment"
    -- outranks "Other edits" and "New issue". The scale is a copy of the registry scale; only
    -- the values that actually take part in resolving a tie are needed here.
    FOREACH cand IN ARRAY candidates LOOP
        prio := CASE cand
            WHEN 'mention'             THEN 100
            WHEN 'reminder'            THEN 100
            WHEN 'unaccepted_reminder' THEN 100
            WHEN 'review_reminder'     THEN 100
            WHEN 'assignment'          THEN 90
            WHEN 'accepted'            THEN 80
            WHEN 'outcome_changed'     THEN 75
            WHEN 'delegated_status'    THEN 70
            WHEN 'auto_unclaim'        THEN 60
            WHEN 'comment'             THEN 40
            WHEN 'attachment'          THEN 40
            WHEN 'reparent'            THEN 40
            WHEN 'other_edits'         THEN 20
            WHEN 'issue_created'       THEN 10
        END;
        IF prio > best_prio THEN
            best_prio := prio;
            best      := cand;
        END IF;
    END LOOP;

    RETURN ROW('reason', best)::notif_classification;
END
$fn$;

COMMENT ON FUNCTION notif_classify IS
    'SQL projection of the rule "event × recipient → reason", revision 3 '
    '(sk-489; revision 1 is 000041, revision 2 is 000045). The second copy '
    'of the algorithm notifications.Classify: the migration does not call application code, '
    'so equality of the copies rests on a shared corpus that '
    'both execute. Revision 3 added the type "outcome of a closed issue changed" with '
    'the author as addressee and rule R-7: a close that changed the outcome does not give '
    'the author a second status notification.';

COMMENT ON COLUMN user_notification_reason_prefs.reason IS
    'Notification reason from the closed registry (internal/notifications/reasons.go). '
    'The fourteenth value is outcome_changed (sk-489): the outcome of a closed '
    'issue changed, the addressee is the issue author.';
