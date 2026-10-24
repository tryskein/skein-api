-- 000041_classify_prune_notification_history — sk-303.13 / T020, migration 3 of three.
--
-- ⚠ THIS MIGRATION IRREVERSIBLY DELETES `notifications` ROWS.
--   Run only after the release's backup gate: read-only preflight counters of
--   the classes below taken while the database is quiescent, a copy of the
--   WHOLE database (not of this table), a restore of that copy into an
--   isolated database proving it unfolds, and a recorded date until which the
--   artifact is guaranteed to exist — because the artifact's lifetime is the
--   window in which a data rollback is possible at all.
--   The gate is EXTERNAL: it cannot be checked from inside SQL (the migration
--   has no way to know whether a backup was taken), and trying to ask would
--   turn a deploy refusal into a riddle. Order is enforced by the operator,
--   not by a predicate here.
--
-- WHAT IT DOES. Two things, in this order:
--
--   1. CLASSIFIES accumulated history — stamps `notifications.reason` on every
--      historical row. Without this step history would stay reason-less, and
--      reading surfaces could not tell "assignment" from "comment".
--   2. DELETES three disjoint classes of rows that must not be visible after
--      deploy, and DOES NOT TOUCH the rest — neither their visibility, nor
--      read state, nor archive (SC-021).
--
-- The rule it implements is one: "after deploy the feed shows exactly what
-- it would have shown had the new prefs been in force from the start".
--
-- ---------------------------------------------------------------------------
-- WHY CLASSIFY BY THE PAIR "event × recipient", NOT BY EVENT TYPE
-- ---------------------------------------------------------------------------
--
-- Storage holds one row per (recipient, event) pair, so ONE event means
-- different reasons for different recipients. A historical issue-created
-- event for the new assignee is "Assignment" (reason on, the row stays),
-- and for a watcher it is "New issue" (reason off, the row is deleted).
-- Classifying by event type would either wipe assignment history or leave
-- a turned-off reason in the feed.
--
-- This is not a theoretical danger: on the production database 2026-08-30
-- ALL 459 rows of type `issue.created` are addressed to the new assignee.
-- Classifying by event type would have deleted all 459 assignment
-- notifications.
--
-- There CANNOT be one executable function for two consumers: a migration
-- does not call application code. So the algorithm exists as a second
-- instance — the `notif_classify` function below — and equivalence of the
-- two instances is held by the shared regression set (corpus T002,
-- internal/notifications/testdata/reason_cases.json) that both execute.
--
-- ---------------------------------------------------------------------------
-- HOW RECIPIENT FACTS ARE RECONSTRUCTED
-- ---------------------------------------------------------------------------
--
--   is_mentioned       — event type 'comment.mention' / 'issue.mention': such
--                        rows are written by the point mention path, the
--                        addressee is one and is the mentioned person.
--   is_reminder_owner  — event type 'reminder.rang': the same point path, the
--                        ring row is created only for the reminder owner.
--   is_subscriber      — everything else. The role delivery path (push to the
--                        new assignee and the creator) appeared ONLY in this
--                        wave, so every historical row arrived through the
--                        watcher UNION; for the two point types above the
--                        classifier does not read the watch at all.
--   is_new_assignee    — the assignee THIS event set: the snapshot
--                        events.new_data->>'assignee' for 'issue.created' and
--                        the newValue of field 'assignee' in
--                        events.payload->'changes' for 'issue.updated'.
--                        Matching against users repeats the runtime
--                        (ResolveAssigneeUserID): username, email, or text
--                        id, case-insensitive.
--   is_creator         — the recipient created the issue (issues.created_by).
--   is_assignee        — the recipient is the CURRENT assignee. There is no
--                        historical value in the database; this fact does not
--                        affect reason choice — the classifier reads it only
--                        in the push-audience predicate.
--   is_actor           — the recipient performed the action
--                        (events.actor_user_id).
--   explicit_accept    — TRUE for 'issue.accepted': the accept event is
--                        written by exactly one code path (the explicit
--                        accept tool), and only it fanned a notification;
--                        automatic accept produced no fan-out.
--   stale_lock_release — TRUE for 'issue.unclaimed': lock-release fan-out
--                        was done only by the stale-lock sweeper; a manual
--                        unclaim created no notification.
--
-- ---------------------------------------------------------------------------
-- THREE CLASSES AND THEIR PRIORITY
-- ---------------------------------------------------------------------------
--
--   1 legacy_hidden     — hidden by the user's previous prefs
--                         (`user_notification_prefs.notify_event_types` is not
--                         NULL and does not contain the row's type). The
--                         filter historically applied ON READ, and the wave
--                         lifts it from the read — without deletion such
--                         rows would RESURFACE (INV-12).
--   3 feed_inapplicable — the "Reminder" reason: it stays ON, but its feed
--                         channel became inapplicable, so old rings stop
--                         being visible on a par with turned-off ones
--                         (SC-022).
--   2 new_default_off   — the row belongs to no reason that the new default
--                         shows in the feed. Two subgroups, counted
--                         separately:
--                         2a default_off_reason — a reason is determined,
--                            the feed channel is applicable, but the default
--                            keeps it off;
--                         2b no_reason — there is no reason at all: the
--                            event lives only as a journal row (claim,
--                            close, archive, dependencies, manual unclaim)
--                            or is unknown to the registry. The old fan-out
--                            created such rows; the new one creates none.
--                            ⚠ SC-014 calls the three-class list exhaustive
--                            and speaks in the language of REASONS; these
--                            rows have no reason, so the spec did not name
--                            them directly. Leaving them visible is not
--                            allowed — that is exactly "behaviour that new
--                            events no longer produce". So they are counted
--                            as a separate metric inside class 2, not
--                            hidden in the overall number. On the production
--                            database 2026-08-30 this subgroup is empty:
--                            `notifications` contains exactly 11 event
--                            types, all with a delivery path.
--
-- Priority 1 → 3 → 2 makes the classes disjoint and resolves the only real
-- overlap meaningfully: a row of an enabled reason, HIDDEN by previous
-- prefs, goes to class 1. SC-021 promises inviolability of enabled-reason
-- history, INV-12 forbids resurfacing of the hidden; there is no
-- contradiction, because such a row was already invisible, and deleting it
-- changes nobody's feed.
--
-- THE EVENT JOURNAL IS NOT TOUCHED. Only the DELIVERY table `notifications`
-- is deleted from; `events` is not written and not changed. The action
-- history remains complete.

-- ---------------------------------------------------------------------------
-- 1. SQL projection of the classifier
-- ---------------------------------------------------------------------------
--
-- Returns the pair (outcome, reason) — the same four outcomes as the runtime
-- classifier:
--   reason                   — the pair mapped to exactly one reason (in the
--                              second field);
--   audit_only               — the event lives only as a journal row, and
--                              that is not a failure;
--   no_reason_for_recipient  — the event is delivered, but not to THIS
--                              recipient;
--   unmapped                 — the event type is unknown to the registry
--                              (configuration error).
-- The migration needs only the reason; the outcome exists for the shared
-- regression with the runtime.

CREATE TYPE notif_classification AS (outcome text, reason text);

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

-- ---------------------------------------------------------------------------
-- 2. Classify history, count classes, delete the invisible
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    -- Reasons whose feed channel is applicable and ON under the new default
    -- (SC-021). The list is a constant, not computed from
    -- user_notification_reason_prefs: at this migration all accounts have
    -- just received the same narrow default (000039), nobody has their own
    -- prefs yet, and the spec states the class through the DEFAULT. Match
    -- with the code registry is checked by a test.
    kept_reasons     CONSTANT text[] := ARRAY[
        'assignment', 'accepted', 'delegated_status', 'mention'];
    -- Reasons whose feed channel is applicable but the default keeps it off.
    default_off      CONSTANT text[] := ARRAY[
        'issue_created', 'comment', 'attachment', 'reparent', 'auto_unclaim', 'other_edits'];

    v_total          bigint;
    v_class1         bigint;
    v_class2_off     bigint;
    v_class2_none    bigint;
    v_class3         bigint;
    v_kept           bigint;
    v_deleted        bigint;
    v_leftover       bigint;
BEGIN
    -- 2.1 Classification. Only HISTORICAL rows are processed — those that
    -- still have no reason: new delivery stamps reason at insert time, so
    -- "reason IS NULL" is the exact definition of accumulated history.
    CREATE TEMPORARY TABLE _notif_history ON COMMIT DROP AS
    SELECT
        n.id,
        n.user_id,
        (p.notify_event_types IS NOT NULL
             AND NOT (n.type = ANY (p.notify_event_types)))          AS legacy_hidden,
        (notif_classify(
            e.type,
            CASE WHEN jsonb_typeof(e.payload -> 'changes') = 'array'
                 THEN ARRAY(SELECT c ->> 'field'
                              FROM jsonb_array_elements(e.payload -> 'changes') c)
                 ELSE '{}'::text[] END,
            e.type = 'issue.accepted',                               -- explicit_accept
            e.type = 'issue.unclaimed',                              -- stale_lock_release
            NOT (e.type IN ('comment.mention', 'issue.mention', 'reminder.rang')),
            na.id IS NOT NULL AND na.id = n.user_id,                 -- is_new_assignee
            i.assignee IS NOT NULL AND ca.id = n.user_id,            -- is_assignee
            i.created_by = n.user_id,                                -- is_creator
            e.actor_user_id = n.user_id,                             -- is_actor
            e.type IN ('comment.mention', 'issue.mention'),          -- is_mentioned
            e.type = 'reminder.rang'                                 -- is_reminder_owner
        )).reason                                                    AS reason
      FROM notifications n
      JOIN events e ON e.id = n.event_id
      LEFT JOIN user_notification_prefs p ON p.user_id = n.user_id
      LEFT JOIN issues i ON i.id = CASE WHEN e.entity_type = 'issue' THEN e.entity_id END
      LEFT JOIN LATERAL (
          SELECT u.id FROM users u
           WHERE i.assignee IS NOT NULL AND i.assignee <> ''
             AND (LOWER(u.username) = LOWER(i.assignee)
               OR LOWER(u.email)    = LOWER(i.assignee)
               OR u.id::text        = LOWER(i.assignee))
           LIMIT 1) ca ON true
      LEFT JOIN LATERAL (
          SELECT h.handle FROM (
              SELECT CASE e.type
                         WHEN 'issue.created' THEN e.new_data ->> 'assignee'
                         WHEN 'issue.updated' THEN (
                             SELECT c ->> 'newValue'
                               FROM jsonb_array_elements(
                                        CASE WHEN jsonb_typeof(e.payload -> 'changes') = 'array'
                                             THEN e.payload -> 'changes'
                                             ELSE '[]'::jsonb END) c
                              WHERE c ->> 'field' = 'assignee'
                              LIMIT 1)
                     END AS handle) h) nh ON true
      LEFT JOIN LATERAL (
          SELECT u.id FROM users u
           WHERE nh.handle IS NOT NULL AND nh.handle <> ''
             AND (LOWER(u.username) = LOWER(nh.handle)
               OR LOWER(u.email)    = LOWER(nh.handle)
               OR u.id::text        = LOWER(nh.handle))
           LIMIT 1) na ON true
     WHERE n.reason IS NULL;

    CREATE INDEX ON _notif_history (id);

    UPDATE notifications n
       SET reason = h.reason
      FROM _notif_history h
     WHERE n.id = h.id
       AND h.reason IS NOT NULL;

    -- 2.2 Counters. Class priority 1 → 3 → 2 is made mutually exclusive by
    -- FILTERs, so the sum of the four deleted numbers plus the kept ones
    -- equals the total row count — that CHECKS disjointness, it does not
    -- promise it.
    SELECT
        count(*),
        count(*) FILTER (WHERE legacy_hidden),
        count(*) FILTER (WHERE NOT legacy_hidden AND reason = ANY (default_off)),
        count(*) FILTER (WHERE NOT legacy_hidden AND reason IS NULL),
        count(*) FILTER (WHERE NOT legacy_hidden AND reason = 'reminder'),
        count(*) FILTER (WHERE NOT legacy_hidden AND reason = ANY (kept_reasons))
      INTO v_total, v_class1, v_class2_off, v_class2_none, v_class3, v_kept
      FROM _notif_history;

    v_deleted := v_class1 + v_class2_off + v_class2_none + v_class3;

    IF v_deleted + v_kept <> v_total THEN
        RAISE EXCEPTION
            'migration 000041: classes do not partition the history (% deleted + % kept <> % total); an unclassified reason leaked in',
            v_deleted, v_kept, v_total;
    END IF;

    -- 2.3 Deletion. One DELETE against the same classification snapshot that
    -- produced the counters — so "how many we counted" and "how many we
    -- deleted" cannot diverge.
    DELETE FROM notifications n
     USING _notif_history h
     WHERE n.id = h.id
       AND (h.legacy_hidden
            OR h.reason IS NULL
            OR h.reason = 'reminder'
            OR h.reason = ANY (default_off));

    -- 2.4 Post-condition: not a single row of the invisible classes remains.
    SELECT count(*) INTO v_leftover
      FROM _notif_history h
      JOIN notifications n ON n.id = h.id
     WHERE h.legacy_hidden
        OR h.reason IS NULL
        OR h.reason = 'reminder'
        OR h.reason = ANY (default_off);
    IF v_leftover > 0 THEN
        RAISE EXCEPTION
            'migration 000041: % notification row(s) of the invisible classes survived the prune',
            v_leftover;
    END IF;

    INSERT INTO migration_counters (migration, metric, value) VALUES
        ('000041_classify_prune_notification_history', 'history_rows_classified',       v_total),
        ('000041_classify_prune_notification_history', 'deleted_legacy_hidden',         v_class1),
        ('000041_classify_prune_notification_history', 'deleted_default_off_reason',    v_class2_off),
        ('000041_classify_prune_notification_history', 'deleted_no_reason',             v_class2_none),
        ('000041_classify_prune_notification_history', 'deleted_feed_inapplicable',     v_class3),
        ('000041_classify_prune_notification_history', 'deleted_total',                 v_deleted),
        ('000041_classify_prune_notification_history', 'kept_enabled_reason',           v_kept);

    RAISE LOG 'migration 000041: history_rows_classified=% deleted_legacy_hidden=% deleted_default_off_reason=% deleted_no_reason=% deleted_feed_inapplicable=% deleted_total=% kept_enabled_reason=%',
        v_total, v_class1, v_class2_off, v_class2_none, v_class3, v_deleted, v_kept;
END
$$;
