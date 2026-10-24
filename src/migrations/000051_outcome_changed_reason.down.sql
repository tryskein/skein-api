-- 000051_outcome_changed_reason (down) — sk-489.
--
-- Returns the three registry copies to the state before the new reason. THE ORDER
-- OF STEPS MATTERS and is the reverse of the apply:
--
--   1. notif_classify returns to revision 2 (twelve parameters);
--   2. FIRST the `outcome_changed` preference rows are deleted, and only THEN is
--      the CHECK narrowed. The reverse is impossible: ADD CONSTRAINT validates the table, and
--      on a database where the seed ran, the narrowing would fail on the very first row —
--      the rollback would stop halfway, leaving the function already rolled back and the
--      CHECK not yet.
--
-- WHAT IS LOST, verbatim:
--   * the preference rows of the new reason are deleted — including those where a person
--      EXPLICITLY turned a channel off. A re-apply will bring back the factory "enabled", not
--      the choice that was made: after the row is deleted there is no way to tell "turned off" from
--      "never touched";
--   * `issue.outcome_changed` becomes `unmapped`, that is an outcome change
--      starts to look like a configuration error — with an ERROR record and
--      a metric increment on every change. This is acceptable exactly when
--      the whole feature is rolled back: without its code no events of this type arise.
--      Rolling back THIS ONE migration while the sk-489 code is running is not routine, but
--      exactly the state that the "Delivery strategy" of the plan forbids;
--   * the thirteenth parameter disappears, so a 13-argument call will stop
--      resolving. The function has no calls of its own — the only
--      production consumer, the DO block of migration 000041, ran in
--      its window and no longer executes.
--
-- The function text below is a VERBATIM copy of revision 2 (000045), not a retelling:
-- a rollback that restores "roughly the same thing" would leave the schema in a
-- state that existed neither before nor after.

-- ---------------------------------------------------------------------------
-- 1. notif_classify, return to revision 2
-- ---------------------------------------------------------------------------

DROP FUNCTION notif_classify(
    text, text[], boolean, boolean, boolean, boolean,
    boolean, boolean, boolean, boolean, boolean, boolean, boolean);

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
    p_is_digest_recipient boolean
) RETURNS notif_classification
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE
AS $fn$
DECLARE
    -- A closed list of types that live ONLY as journal records. Deliberately duplicates
    -- notifications.auditOnlyEventTypes: the migration does not call application
    -- code. Equality of the two copies is checked by a test, not by
    -- vigilance.
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
            IF subscriber OR creator THEN candidates := candidates || 'delegated_status'::text; END IF;

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

        -- Digest types. Each has its own reason: merging categories is forbidden
        -- (R-4), and unsubscribing from one must not silence another.
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
    'SQL projection of the rule "event × recipient → reason", revision 2 '
    '(sk-303.14 / T005; the first revision is 000041). The second copy of the algorithm '
    'notifications.Classify: the migration does not call application code, so '
    'equality of the copies rests on a shared corpus that both execute. '
    'The review-and-nudges release added two private journal types, two digest types with '
    'their own reasons and the recipient relation "digest addressee".';

-- ---------------------------------------------------------------------------
-- 2. Preference rows of the new reason — BEFORE narrowing the CHECK
-- ---------------------------------------------------------------------------

DELETE FROM user_notification_reason_prefs WHERE reason = 'outcome_changed';

-- ---------------------------------------------------------------------------
-- 3. The CHECK returns to thirteen values
-- ---------------------------------------------------------------------------

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
            'review_reminder'
        ));

COMMENT ON COLUMN user_notification_reason_prefs.reason IS
    'Notification reason from the closed registry (internal/notifications/reasons.go).';
