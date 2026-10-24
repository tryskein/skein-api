-- 000039_notification_reason_defaults — sk-303.13 / T005, migration 1 of three.
--
-- What it does and why this way:
--
-- 1. Reason × channel prefs (user_notification_reason_prefs).
--    Previous prefs are NOT migrated (R-6, Q5): every account gets the
--    narrow default. Previous columns (notify_event_types, push_disabled_
--    categories) this migration neither touches nor drops — the wave's
--    read-switch removes them together with the code that still reads them.
--
-- 2. Flag that a row belongs to the feed channel (notifications.feed_channel).
--    Today the mere existence of a row IS the fact of the feed, so dropping
--    the read filter alone is not enough: a row created for push would leak
--    into every feed (R-3, US-2, the third state of the four-state table).
--    Historical rows get feed_channel = true — they were feed rows by
--    construction; narrowing history visibility is migration 3 (T020) after
--    the backup, not this one.
--
-- 3. DB-backed push lifecycle (push_state / outcome / attempts / lease /
--    deadline). Push stops being a "side effect of publishing to NATS" and
--    gets its own state that survives a restart.
--    Historical rows stay in state 'none': none of them become pending
--    without classification — otherwise the deploy would fan the accumulated
--    history out to devices.
--
-- 4. Migration counters journal (migration_counters). The spec requires the
--    migration to leave in the journal the number of affected accounts and
--    watches: without that, "did it apply" is checked only by sampling
--    data. RAISE LOG below puts the numbers in the server log, and the
--    table makes them queryable by SQL and later checkable against
--    preflight T019. Migrations 2 and 3 (T018/T020) write their counters
--    into this same table.

-- ---------------------------------------------------------------------------
-- 4. Counters journal (created first — the steps below use it)
-- ---------------------------------------------------------------------------
CREATE TABLE migration_counters (
    id          bigserial PRIMARY KEY,
    migration   text        NOT NULL,
    metric      text        NOT NULL,
    value       bigint      NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_migration_counters_migration ON migration_counters (migration, metric);

COMMENT ON TABLE migration_counters IS
    'Named counters of affected rows left by the sk-303.13 wave migrations. '
    'Read by migration tests and checked against T019 preflight counters.';

-- ---------------------------------------------------------------------------
-- 1. Reason × channel prefs
-- ---------------------------------------------------------------------------
--
-- The closed list of reasons is duplicated in the CHECK on purpose. A
-- migration cannot call application code, so the registry exists here as a
-- second instance; equivalence of the two instances is checked by tests
-- (shared corpus T002 and migration class T006), not by hoping someone
-- notices.
CREATE TABLE user_notification_reason_prefs (
    user_id      uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    reason       text        NOT NULL CHECK (reason IN (
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
                 )),
    feed_enabled boolean     NOT NULL,
    push_enabled boolean     NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, reason)
);

COMMENT ON TABLE user_notification_reason_prefs IS
    'State of two channels (feed, push) per reason per user. '
    'Absence of a row means the registry narrow default, not "everything on".';

-- Backfill: the narrow default for ALL accounts, with no transfer of
-- previous prefs. Exactly seven reasons are on in at least one channel;
-- two of the seven are reserved for waves 2-3 and get no toggle in this
-- wave, so the form will show five enabled.
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled)
SELECT u.id, d.reason, d.feed_enabled, d.push_enabled
  FROM users u
 CROSS JOIN (VALUES
     -- reason                feed    push
     ('assignment',           true,   true),
     ('accepted',             true,   true),
     ('delegated_status',     true,   true),
     ('reminder',             false,  true),   -- feed channel inapplicable
     ('mention',              true,   true),
     ('issue_created',        false,  false),  -- push inapplicable
     ('comment',              false,  false),  -- push inapplicable
     ('attachment',           false,  false),  -- push inapplicable
     ('reparent',             false,  false),  -- push inapplicable
     ('auto_unclaim',         false,  false),  -- push inapplicable
     ('other_edits',          false,  false),  -- push inapplicable
     ('unaccepted_reminder',  false,  true),   -- reserved for a later release
     ('review_reminder',      false,  true)    -- reserved for a later release
 ) AS d(reason, feed_enabled, push_enabled)
ON CONFLICT (user_id, reason) DO NOTHING;

-- ---------------------------------------------------------------------------
-- 2-3. Feed-channel flag and push lifecycle on the notification row
-- ---------------------------------------------------------------------------
ALTER TABLE notifications
    ADD COLUMN reason              text,
    ADD COLUMN feed_channel        boolean     NOT NULL DEFAULT false,
    ADD COLUMN push_state          text        NOT NULL DEFAULT 'none',
    ADD COLUMN push_outcome        text,
    ADD COLUMN push_attempts       integer     NOT NULL DEFAULT 0,
    ADD COLUMN push_leased_until   timestamptz,
    ADD COLUMN push_next_attempt_at timestamptz,
    ADD COLUMN push_deadline_at    timestamptz,
    ADD COLUMN push_finalized_at   timestamptz;

COMMENT ON COLUMN notifications.reason IS
    'Reason the row belongs to. NULL on historical rows until migration 3 '
    '(T020) classifies them.';
COMMENT ON COLUMN notifications.feed_channel IS
    'Whether the row belongs to the feed channel. Reading surfaces select '
    'rows by this flag; a push-only row (feed_channel = false) is visible in '
    'no feed, no counters, no live stream, and no agent response. '
    'The wave plan names this flag feed_enabled — here it is named '
    'feed_channel on purpose, so it does not collide with '
    'user_notification_reason_prefs.feed_enabled: there it is a channel '
    'SETTING, here it is the row BELONGING to the channel. When the plan '
    'says "filter by feed_enabled on the notification row", substitute this '
    'column.';
COMMENT ON COLUMN notifications.push_state IS
    'DB-backed push lifecycle: none (no push) | pending (waiting to send) | '
    'leased (taken by the worker) | final (terminal outcome in push_outcome).';

-- Closed sets of states and outcomes. A terminal outcome exists if and
-- only if the state is terminal: otherwise "sent" and "not yet sent"
-- would be indistinguishable after a restart.
ALTER TABLE notifications
    ADD CONSTRAINT notifications_push_state_chk
        CHECK (push_state IN ('none', 'pending', 'leased', 'final')),
    ADD CONSTRAINT notifications_push_outcome_chk
        CHECK (push_outcome IS NULL OR push_outcome IN (
            'success',
            'no_device',
            'access_revoked',
            'prefs_unavailable',
            'configuration_no_send',
            'transport_exhausted'
        )),
    ADD CONSTRAINT notifications_push_final_outcome_chk
        CHECK ((push_state = 'final') = (push_outcome IS NOT NULL)),
    ADD CONSTRAINT notifications_push_attempts_chk
        CHECK (push_attempts >= 0);

-- Backfill of historical rows: they were feed rows by construction, so the
-- feed flag is set on them, and push state stays 'none' (the default) —
-- no historical row enters the send queue.
UPDATE notifications SET feed_channel = true WHERE feed_channel = false;

-- Indexes of reading surfaces: the same slices as before, but filtered by
-- the feed-channel flag.
CREATE INDEX idx_notif_feed_unread ON notifications (user_id, created_at DESC)
    WHERE feed_channel AND read_at IS NULL AND archived_at IS NULL;
CREATE INDEX idx_notif_feed_user ON notifications (user_id, id DESC)
    WHERE feed_channel;

-- Indexes of the push delivery loop: pick ready-to-send, reclaim expired
-- leases, and sweep terminal invisible rows.
CREATE INDEX idx_notif_push_pending ON notifications (push_next_attempt_at, id)
    WHERE push_state = 'pending';
CREATE INDEX idx_notif_push_leased ON notifications (push_leased_until)
    WHERE push_state = 'leased';
CREATE INDEX idx_notif_push_final_invisible ON notifications (push_finalized_at)
    WHERE push_state = 'final' AND NOT feed_channel;

-- ---------------------------------------------------------------------------
-- Counters: number of affected accounts, pref rows, and notification rows
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    v_accounts bigint;
    v_prefs    bigint;
    v_feed     bigint;
    v_all_on   bigint;
BEGIN
    SELECT count(DISTINCT user_id) INTO v_accounts FROM user_notification_reason_prefs;
    SELECT count(*)                INTO v_prefs    FROM user_notification_reason_prefs;
    SELECT count(*)                INTO v_feed     FROM notifications WHERE feed_channel;
    -- Invariant SC-003: no one is left in the "everything on" state.
    SELECT count(*) INTO v_all_on FROM (
        SELECT user_id
          FROM user_notification_reason_prefs
         GROUP BY user_id
        HAVING bool_and(feed_enabled) AND bool_and(push_enabled)
    ) AS everything_on;

    IF v_all_on > 0 THEN
        RAISE EXCEPTION
            'migration 000039: % account(s) left in the all-enabled state; the narrow default did not apply',
            v_all_on;
    END IF;

    INSERT INTO migration_counters (migration, metric, value) VALUES
        ('000039_notification_reason_defaults', 'accounts_defaulted',          v_accounts),
        ('000039_notification_reason_defaults', 'reason_pref_rows_written',    v_prefs),
        ('000039_notification_reason_defaults', 'notifications_marked_feed',   v_feed),
        ('000039_notification_reason_defaults', 'accounts_all_enabled_left',   v_all_on);

    RAISE LOG 'migration 000039: accounts_defaulted=% reason_pref_rows_written=% notifications_marked_feed=% accounts_all_enabled_left=%',
        v_accounts, v_prefs, v_feed, v_all_on;
END
$$;
