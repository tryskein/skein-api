-- sk-303.13 / T019 — PREFLIGHT COUNTERS before irreversible migration 000041.
--
-- WHAT THIS IS. A read-only query: it counts HOW MANY `notifications` rows
-- wave migration 3 (000041_classify_prune_notification_history) will
-- delete and how many it will keep. It changes nothing — no DDL, no DML,
-- no temp tables. It can be run on a live database at any moment; in a
-- release it is run in the stop window, while the database is quiescent and
-- BEFORE the backup is taken — so the numbers describe exactly the state the
-- copy freezes — and its output is kept as release evidence.
--
-- WHEN IT RUNS AND ON WHICH SCHEMA. The wave migrations (000039 / 000040 /
-- 000041) are applied by the first pod of the new image, which starts only
-- AFTER the backup gate. So this query executes on the schema
-- BEFORE the wave: `notifications` still has neither `reason` nor
-- `feed_channel`, and tables `user_notification_reason_prefs` and
-- `migration_counters` do not yet exist. None of the listed objects is
-- mentioned here — on purpose.
--
-- WHY. The spec requires measuring and naming the deletion volume BEFORE
-- it is performed: "The migration volume must be measured and named
-- before it is applied: at the time of writing the spec the row count is
-- unknown" (wave spec, section "Data, entities, migrations"; Q11). These
-- same numbers are later checked against the per-class totals that
-- migration 000041 puts in `migration_counters`.
--
-- HOW TO RUN.
--   kubectl -n <namespace> exec -i <primary-pod> -c postgres -- \
--     psql -U postgres -d skein -f - \
--       < src/migrations/preflight/sk-303.13-preflight-notification-history.sql
--   (inside the Postgres pod, peer authentication of the OS user postgres
--    works; `psql -U skein` there exits with code 2 — verified 2026-08-30.
--    From outside, over TCP with the password from the DB-credentials
--    secret, the skein role is fine:
--    psql -h <host> -U skein -d skein -f src/migrations/preflight/…sql)
--
-- WHERE IT LIVES AND WHY RIGHT HERE. The `migrations/preflight/` directory
-- is not an accident:
--   * golang-migrate reads only files of the form NNNNNN_name.{up,down}.sql
--     in the directory root, it ignores a subdirectory, so the query does
--     not enter the migration chain;
--   * the Dockerfile copies the whole `migrations` into the image, so in
--     the deploy window the query is available from the image itself
--     (`/app/migrations/preflight/…`), without checking out the
--     repository;
--   * the integration-test harness mounts only `src`, and the matrix-row-26
--     regression reads THIS VERY FILE, not a copy of its text — otherwise
--     "preflight matched the migration" would prove a copy matching a
--     copy.
-- Keep the output in full — that is evidence E-3.
--
-- ---------------------------------------------------------------------------
-- HOW THIS CLASSIFIES HISTORY
-- ---------------------------------------------------------------------------
--
-- The rule is the same as new delivery: "event × RECIPIENT → reason", not
-- "event type → reason". One historical issue-created event for the new
-- assignee means "Assignment" (reason on — the row stays), and for a
-- watcher it means "New issue" (reason off — the row is deleted). Both
-- rows point at ONE event, so classifying by event type would either wipe
-- assignment history or leave a turned-off reason in the feed.
--
-- Recipient facts are reconstructed from what is actually stored:
--
--   is_mentioned       — event type 'comment.mention' / 'issue.mention'.
--                        Rows of these types are written by the point
--                        mention path (WriteMentionNotification), the
--                        addressee is one and is the mentioned person.
--   is_reminder_owner  — event type 'reminder.rang'. The same point path:
--                        the ring row is created only for the reminder
--                        owner.
--   is_subscriber      — everything else. The role delivery path (push to
--                        the new assignee and the creator) appeared ONLY
--                        in this wave, so every historical row arrived
--                        through the watcher UNION. For the two point
--                        types above the classifier does not read the
--                        watch at all.
--   is_new_assignee    — the assignee THIS event set:
--                        for 'issue.created'  — snapshot events.new_data->>'assignee';
--                        for 'issue.updated'  — newValue of field 'assignee' in
--                                               events.payload->'changes'.
--                        The row is matched against users the same way the
--                        runtime does (ResolveAssigneeUserID): by username,
--                        email or text id, case-insensitive.
--   is_creator         — the recipient created the issue (issues.created_by).
--   is_assignee        — the recipient is the CURRENT assignee of the issue.
--                        There is no historical value at event time in the
--                        database; this does not affect the result: the
--                        classifier reads this fact only in the push-audience
--                        predicate, not when choosing a reason.
--   is_actor           — the recipient performed the action
--                        (events.actor_user_id).
--   explicit_accept    — always TRUE for 'issue.accepted': the accept event
--                        is written by exactly one code path — the explicit
--                        accept tool, and only it fanned a notification.
--                        Automatic accept (claim, self-assign) produced no
--                        fan-out, so there is no historical row from it.
--   stale_lock_release — always TRUE for 'issue.unclaimed': lock-release
--                        fan-out was done only by the stale-lock sweeper;
--                        a manual unclaim created no notification.
--
-- ---------------------------------------------------------------------------
-- THREE INVISIBILITY CLASSES (SC-014) AND THEIR MUTUAL DISJOINTNESS
-- ---------------------------------------------------------------------------
--
--   1. legacy_hidden      — the row is hidden by the user's previous prefs:
--      `user_notification_prefs.notify_event_types` is NOT NULL and does not
--      contain the row's type. Such rows exist because the filter
--      historically applied ON READ; the wave lifts the filter from the
--      read, so without deletion they would RESURFACE (INV-12).
--
--   2. new_default_off    — the row belongs to no reason that the new
--      default shows in the feed. Inside the class two subgroups, counted
--      separately:
--        2a default_off_reason — a reason is determined, its feed channel
--           is applicable, but the new default keeps it off: "New issue",
--           "Comment", "Attachment", "Reparent", "Auto-unclaim", "Other
--           edits".
--        2b no_reason — there is no reason at all: the event lives only as
--           a journal row (claim, close, archive, dependencies, manual
--           unclaim) or is unknown to the registry. The old fan-out created
--           such rows; the new one creates none, so under the rule "the
--           feed shows exactly what it would have shown had the new prefs
--           been in force from the start" they cannot remain visible.
--           ⚠ SC-014 lists three classes and calls the list exhaustive;
--           subgroup 2b is the part of class 2 the spec did not name
--           directly, because it speaks in the language of reasons, and
--           these rows have no reason. It is counted as a separate metric
--           precisely for that visibility.
--
--   3. feed_inapplicable  — the "Reminder" reason: it STAYS on, but its
--      feed channel became inapplicable, so old rings stop being visible
--      on a par with turned-off ones (SC-022).
--
-- Class priority is stated explicitly and makes them disjoint: first 1,
-- then 3, then 2. Class 3 is checked BEFORE class 2, because "channel
-- inapplicable" and "channel off by default" are different claims, and
-- "Reminder" must be counted as its own class, not dissolve into the
-- turned-off ones.
--
--   kept_enabled_reason — the explicit exception: reasons the default
--   turns on in the feed (Assignment, Accepted, Delegated status,
--   Mention). The migration does not touch these rows at all — neither
--   visibility, nor read state, nor archive (SC-021). Their number is
--   printed beside the others so "how many remain" is visible in the same
--   output as "how many deleted".

\pset footer off

WITH facts AS (
    SELECT
        n.id                                    AS notification_id,
        n.user_id,
        e.type                                  AS event_type,
        -- Edited fields: changes[*].field. jsonb_typeof guards against
        -- events whose payload does not carry a changes array.
        CASE
            WHEN jsonb_typeof(e.payload -> 'changes') = 'array' THEN EXISTS (
                SELECT 1 FROM jsonb_array_elements(e.payload -> 'changes') c
                 WHERE c ->> 'field' = 'assignee')
            ELSE false
        END                                     AS changed_assignee,
        CASE
            WHEN jsonb_typeof(e.payload -> 'changes') = 'array' THEN EXISTS (
                SELECT 1 FROM jsonb_array_elements(e.payload -> 'changes') c
                 WHERE c ->> 'field' = 'priority')
            ELSE false
        END                                     AS changed_priority,
        -- Assignee this event set (text handle).
        CASE e.type
            WHEN 'issue.created' THEN e.new_data ->> 'assignee'
            WHEN 'issue.updated' THEN (
                SELECT c ->> 'newValue'
                  FROM jsonb_array_elements(
                           CASE WHEN jsonb_typeof(e.payload -> 'changes') = 'array'
                                THEN e.payload -> 'changes'
                                ELSE '[]'::jsonb END) c
                 WHERE c ->> 'field' = 'assignee'
                 LIMIT 1)
        END                                     AS new_assignee_handle,
        e.actor_user_id,
        CASE WHEN e.entity_type = 'issue' THEN e.entity_id END AS issue_id,
        (p.notify_event_types IS NOT NULL
             AND NOT (n.type = ANY (p.notify_event_types)))    AS legacy_hidden
      FROM notifications n
      JOIN events e ON e.id = n.event_id
      LEFT JOIN user_notification_prefs p ON p.user_id = n.user_id
),
relations AS (
    SELECT
        f.*,
        (f.event_type IN ('comment.mention', 'issue.mention'))  AS is_mentioned,
        (f.event_type = 'reminder.rang')                        AS is_reminder_owner,
        NOT (f.event_type IN ('comment.mention', 'issue.mention', 'reminder.rang'))
                                                                AS is_subscriber,
        (na.id IS NOT NULL AND na.id = f.user_id)               AS is_new_assignee,
        (i.created_by = f.user_id)                              AS is_creator,
        (f.actor_user_id = f.user_id)                           AS is_actor
      FROM facts f
      LEFT JOIN issues i ON i.id = f.issue_id
      LEFT JOIN LATERAL (
          SELECT u.id
            FROM users u
           WHERE f.new_assignee_handle IS NOT NULL
             AND f.new_assignee_handle <> ''
             AND (LOWER(u.username) = LOWER(f.new_assignee_handle)
               OR LOWER(u.email)    = LOWER(f.new_assignee_handle)
               OR u.id::text        = LOWER(f.new_assignee_handle))
           LIMIT 1) na ON true
),
classified AS (
    SELECT
        r.*,
        CASE r.event_type
            WHEN 'issue.created' THEN
                CASE WHEN r.is_new_assignee     THEN 'assignment'
                     WHEN r.is_subscriber       THEN 'issue_created' END
            WHEN 'issue.updated' THEN
                CASE WHEN r.changed_assignee AND (r.is_new_assignee OR r.is_subscriber)
                                                THEN 'assignment'
                     WHEN r.changed_priority AND r.is_subscriber
                                                THEN 'other_edits' END
            WHEN 'issue.status_changed' THEN
                CASE WHEN r.is_subscriber OR r.is_creator THEN 'delegated_status' END
            WHEN 'issue.accepted' THEN
                CASE WHEN r.is_subscriber OR r.is_creator THEN 'accepted' END
            WHEN 'issue.unclaimed' THEN
                CASE WHEN r.is_subscriber THEN 'auto_unclaim' END
            WHEN 'issue.reparented' THEN
                CASE WHEN r.is_subscriber THEN 'reparent' END
            WHEN 'comment.created' THEN
                CASE WHEN r.is_subscriber THEN 'comment' END
            WHEN 'attachment.added' THEN
                CASE WHEN r.is_subscriber THEN 'attachment' END
            WHEN 'comment.mention' THEN
                CASE WHEN r.is_mentioned THEN 'mention' END
            WHEN 'issue.mention' THEN
                CASE WHEN r.is_mentioned THEN 'mention' END
            WHEN 'reminder.rang' THEN
                CASE WHEN r.is_reminder_owner THEN 'reminder' END
        END AS reason
      FROM relations r
),
bucketed AS (
    SELECT
        c.*,
        CASE
            WHEN c.legacy_hidden                     THEN '1 legacy_hidden'
            WHEN c.reason = 'reminder'               THEN '3 feed_inapplicable'
            WHEN c.reason IS NULL                    THEN '2 new_default_off / no_reason'
            WHEN c.reason IN ('issue_created', 'comment', 'attachment',
                              'reparent', 'auto_unclaim', 'other_edits')
                                                     THEN '2 new_default_off / default_off_reason'
            ELSE                                          '0 kept_enabled_reason'
        END AS class
      FROM classified c
)
SELECT
    COALESCE(class, 'TOTAL')                AS "class",
    count(*)                                AS "records",
    count(DISTINCT user_id)                 AS "accounts",
    count(*) FILTER (WHERE class <> '0 kept_enabled_reason') AS "to_delete"
  FROM bucketed
 GROUP BY ROLLUP (class)
 ORDER BY class NULLS LAST;

-- Breakdown by reason — a control select against the table above: the
-- sum over rows of "to-delete" reasons must match classes 2 and 3.
WITH facts AS (
    SELECT
        n.id AS notification_id, n.user_id, e.type AS event_type,
        CASE WHEN jsonb_typeof(e.payload -> 'changes') = 'array' THEN EXISTS (
                SELECT 1 FROM jsonb_array_elements(e.payload -> 'changes') c
                 WHERE c ->> 'field' = 'assignee') ELSE false END AS changed_assignee,
        CASE WHEN jsonb_typeof(e.payload -> 'changes') = 'array' THEN EXISTS (
                SELECT 1 FROM jsonb_array_elements(e.payload -> 'changes') c
                 WHERE c ->> 'field' = 'priority') ELSE false END AS changed_priority,
        CASE e.type
            WHEN 'issue.created' THEN e.new_data ->> 'assignee'
            WHEN 'issue.updated' THEN (
                SELECT c ->> 'newValue'
                  FROM jsonb_array_elements(
                           CASE WHEN jsonb_typeof(e.payload -> 'changes') = 'array'
                                THEN e.payload -> 'changes' ELSE '[]'::jsonb END) c
                 WHERE c ->> 'field' = 'assignee' LIMIT 1)
        END AS new_assignee_handle,
        e.actor_user_id,
        CASE WHEN e.entity_type = 'issue' THEN e.entity_id END AS issue_id,
        (p.notify_event_types IS NOT NULL
             AND NOT (n.type = ANY (p.notify_event_types))) AS legacy_hidden
      FROM notifications n
      JOIN events e ON e.id = n.event_id
      LEFT JOIN user_notification_prefs p ON p.user_id = n.user_id
),
relations AS (
    SELECT f.*,
        (f.event_type IN ('comment.mention', 'issue.mention')) AS is_mentioned,
        (f.event_type = 'reminder.rang')                       AS is_reminder_owner,
        NOT (f.event_type IN ('comment.mention', 'issue.mention', 'reminder.rang'))
                                                               AS is_subscriber,
        (na.id IS NOT NULL AND na.id = f.user_id)              AS is_new_assignee,
        (i.created_by = f.user_id)                             AS is_creator
      FROM facts f
      LEFT JOIN issues i ON i.id = f.issue_id
      LEFT JOIN LATERAL (
          SELECT u.id FROM users u
           WHERE f.new_assignee_handle IS NOT NULL AND f.new_assignee_handle <> ''
             AND (LOWER(u.username) = LOWER(f.new_assignee_handle)
               OR LOWER(u.email)    = LOWER(f.new_assignee_handle)
               OR u.id::text        = LOWER(f.new_assignee_handle))
           LIMIT 1) na ON true
),
classified AS (
    SELECT r.*,
        CASE r.event_type
            WHEN 'issue.created' THEN
                CASE WHEN r.is_new_assignee THEN 'assignment'
                     WHEN r.is_subscriber   THEN 'issue_created' END
            WHEN 'issue.updated' THEN
                CASE WHEN r.changed_assignee AND (r.is_new_assignee OR r.is_subscriber)
                                            THEN 'assignment'
                     WHEN r.changed_priority AND r.is_subscriber
                                            THEN 'other_edits' END
            WHEN 'issue.status_changed' THEN
                CASE WHEN r.is_subscriber OR r.is_creator THEN 'delegated_status' END
            WHEN 'issue.accepted' THEN
                CASE WHEN r.is_subscriber OR r.is_creator THEN 'accepted' END
            WHEN 'issue.unclaimed'  THEN CASE WHEN r.is_subscriber THEN 'auto_unclaim' END
            WHEN 'issue.reparented' THEN CASE WHEN r.is_subscriber THEN 'reparent' END
            WHEN 'comment.created'  THEN CASE WHEN r.is_subscriber THEN 'comment' END
            WHEN 'attachment.added' THEN CASE WHEN r.is_subscriber THEN 'attachment' END
            WHEN 'comment.mention'  THEN CASE WHEN r.is_mentioned THEN 'mention' END
            WHEN 'issue.mention'    THEN CASE WHEN r.is_mentioned THEN 'mention' END
            WHEN 'reminder.rang'    THEN CASE WHEN r.is_reminder_owner THEN 'reminder' END
        END AS reason
      FROM relations r
)
SELECT
    COALESCE(reason, '(no reason)') AS "reason",
    event_type                       AS "event_type",
    count(*)                         AS "records",
    count(*) FILTER (WHERE legacy_hidden) AS "hidden_by_legacy_prefs"
  FROM classified
 GROUP BY 1, 2
 ORDER BY 3 DESC, 1, 2;
