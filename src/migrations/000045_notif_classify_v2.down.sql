-- 000045_notif_classify_v2 (down) — sk-303.14 / T005.
--
-- Returns the SQL classification projection to the 000041 revision: eleven
-- parameters, without the "digest recipient" relation.
--
-- WHAT IS LOST ON ROLLBACK, verbatim:
--   * `review.marked` and `review.flag_changed` stop being journal types
--     and become `unmapped` — i.e. private events will start looking like
--     a configuration error. That is acceptable precisely because rolling
--     this migration back only makes sense together with rolling the wave
--     back: their producer (T007) is absent on the rolled-back schema, and
--     events of these types do not exist;
--   * `nudge.unaccepted_digest` / `nudge.unreviewed_digest` also become
--     `unmapped`;
--   * the twelfth parameter disappears, so any call with twelve arguments
--     will stop resolving. The function has no calls of its own: the only
--     production consumer is the DO-block of migration 000041, which ran
--     in its window and is no longer executed.
--
-- The function text below is a verbatim copy of the 000041 revision, not
-- a paraphrase: a rollback that restored "roughly the same thing" would
-- leave the schema in a state that existed neither before nor after.

DROP FUNCTION notif_classify(
    text, text[], boolean, boolean, boolean, boolean,
    boolean, boolean, boolean, boolean, boolean, boolean);

CREATE FUNCTION notif_classify(
    p_event_type         text,
    p_changed_fields     text[],
    p_explicit_accept    boolean,
    p_stale_lock_release boolean,
    p_is_subscriber      boolean,
    p_is_new_assignee    boolean,
    p_is_assignee        boolean,
    p_is_creator         boolean,
    p_is_actor           boolean,
    p_is_mentioned       boolean,
    p_is_reminder_owner  boolean
) RETURNS notif_classification
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE
AS $fn$
DECLARE
    -- Closed list of types that live ONLY as journal rows. Duplicates
    -- notifications.auditOnlyEventTypes on purpose: a migration does not
    -- call application code. Equality of the two instances is checked by
    -- a test, not by attentiveness.
    audit_only  CONSTANT text[] := ARRAY[
        'issue.claimed', 'issue.closed', 'issue.archived',
        'dependency.created', 'dependency.deleted',
        'issue.assigned', 'issue.priority_changed', 'issue.field_changed',
        'issue.label_added', 'issue.label_removed', 'issue.components_changed',
        'issue.commits_attached', 'issue.pinned', 'issue.unpinned',
        'issue.delegation_dropped', 'issue.delegation_resumed',
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
            -- Exactly two edited fields carry a delivery path: assignee and
            -- priority. Editing any other field lives in the journal, and
            -- that is not a failure.
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
            -- An accept stamped automatically (claim, self-assign) gets no
            -- reason: it is written only to the journal.
            IF NOT COALESCE(p_explicit_accept, false) THEN
                RETURN ROW('audit_only', NULL)::notif_classification;
            END IF;
            IF subscriber OR creator THEN candidates := candidates || 'accepted'::text; END IF;

        WHEN 'issue.unclaimed' THEN
            -- A human unclaim carries no reason: the neighbouring
            -- issue.status_changed already delivered the signal, and two
            -- notifications for one action are forbidden (INV-6).
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

        ELSE
            RETURN ROW('unmapped', NULL)::notif_classification;
    END CASE;

    IF cardinality(candidates) = 0 THEN
        RETURN ROW('no_reason_for_recipient', NULL)::notif_classification;
    END IF;

    -- When several candidate reasons compete, the more SPECIFIC wins:
    -- "Assignment" outranks "Other edits" and "New issue". The scale is a
    -- copy of the registry scale; only the values that actually take part
    -- in a tie-break are needed here.
    FOREACH cand IN ARRAY candidates LOOP
        prio := CASE cand
            WHEN 'mention'          THEN 100
            WHEN 'reminder'         THEN 100
            WHEN 'assignment'       THEN 90
            WHEN 'accepted'         THEN 80
            WHEN 'delegated_status' THEN 70
            WHEN 'auto_unclaim'     THEN 60
            WHEN 'comment'          THEN 40
            WHEN 'attachment'       THEN 40
            WHEN 'reparent'         THEN 40
            WHEN 'other_edits'      THEN 20
            WHEN 'issue_created'    THEN 10
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
    'SQL projection of the rule "event × recipient → reason" (sk-303.13 / T020). Second '
    'instance of the notifications.Classify algorithm: a migration does not call '
    'application code, so equality of the two instances is held by the shared corpus T002 '
    'that both execute. Used by migration 000041 to classify accumulated history.';
