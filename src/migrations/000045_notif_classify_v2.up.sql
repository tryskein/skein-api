-- 000045_notif_classify_v2 — sk-303.14, task T005.
--
-- Brings the SQL projection of classification "event × recipient → reason"
-- in line with the code registry after two reminder categories are
-- activated.
--
-- WHY A NEW MIGRATION, NOT AN EDIT OF 000041. Applied migrations are not
-- edited: on production 000041 has already run, and its text is a
-- historical record of what happened there to accumulated notification
-- history. The function, however, is a live schema object, so it is
-- replaced by the next migration.
--
-- WHAT CHANGES. Four event types and one recipient relation:
--
--   1. `review.marked` and `review.flag_changed` — private bookkeeping of
--      review (INV-2). Journal rows and nothing more: no feed, no
--      notification, no reason. They are appended to the closed list of
--      journal types, not left as "unknown": an unknown type the
--      classifier names a configuration error (`unmapped`), and privacy
--      would look like a breakage.
--
--   2. `nudge.unaccepted_digest` and `nudge.unreviewed_digest` — one
--      digest per category, each with its OWN reason (R-4: "categories
--      are not merged"), and only for the person the digest is addressed
--      to.
--
--   3. Hence the twelfth parameter — `p_is_digest_recipient`. A digest is
--      not about one issue, and the recipient's relation to any one of
--      them does not decide whether this is their digest; reusing
--      `p_is_new_assignee` or `p_is_creator` would assert something the
--      row does not have. Adding a parameter changes the SIGNATURE, so
--      this is DROP + CREATE, not CREATE OR REPLACE: the latter would
--      create an overload, and the two function instances would diverge
--      silently.
--
-- WHAT IS NOT HERE. No data row is read or written: the migration changes
-- only a schema object. So it has no entries in `migration_counters` —
-- there is nothing to count, and a zero counter for symmetry would make
-- the counters table less informative, not more.
--
-- INSTANCE EQUALITY. The function is the second instance of the
-- notifications.Classify algorithm, and it is held not by attentiveness
-- but by the shared regression corpus
-- (`internal/notifications/testdata/reason_cases.json`) that both
-- implementations execute: migrations/migration_000041_test.go checks them
-- case by case. The third implementation — the reason projection in the
-- delivery path (`internal/outbox/push.go`) — is checked against the same
-- corpus by a separate assertion in
-- `internal/outbox/push_classification_parity_test.go`.
--
-- GENERATION. sqlc.yaml reads `schema: migrations`, so a generator run is
-- mandatory and its result is committed in this same commit — even if the
-- function did not move the model. Check by the fact of the run, not by
-- reasoning.

DROP FUNCTION notif_classify(
    text, text[], boolean, boolean, boolean, boolean,
    boolean, boolean, boolean, boolean, boolean);

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

        -- Digest types. Each has its own reason: merging categories is
        -- forbidden (R-4), and unsubscribing from one must not silence the
        -- other.
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

    -- When several candidate reasons compete, the more SPECIFIC wins:
    -- "Assignment" outranks "Other edits" and "New issue". The scale is a
    -- copy of the registry scale; only the values that actually take part
    -- in a tie-break are needed here.
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
    '(sk-303.14 / T005; first revision — 000041). Second instance of the '
    'notifications.Classify algorithm: a migration does not call application '
    'code, so instance equality is held by the shared corpus that both execute. '
    'The review-and-nudges release added two private journal types, two digest types with their own '
    'reasons, and the recipient relation "digest recipient".';
