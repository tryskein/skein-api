-- 000052_quiet_hours — sk-340.
--
-- WHY. A push goes out at any time of day. A night push for a non-urgent issue
-- makes a person turn off the whole category, and the notification
-- stops working altogether. A quiet window is a way not to wake someone without losing the
-- notification itself: the feed row is created as usual and waits for the person in the
-- morning in the UI; ONLY the push is silent.
--
-- OWNER DECISION (sk-340, 2026-09-20): SILENCE WINS. A push that falls into the
-- window is NOT SENT AT ALL — no shift to the end of the window, no catch-up after it.
-- The price is named explicitly and accepted: an urgent night notification will never arrive.
--
-- ⚠ WHY NOT A SHIFT, although the first revision of the spec asked for exactly that. Two
-- independent reasons, both measured from the code, not assumed:
--
--   * a shift is literally unreachable. `notifications.PushDeliveryDeadline` equals
--     ONE HOUR and is stamped into push_deadline_at when the row is created, and
--     `retireExpiredDeadlines` retires the expired WITHOUT SENDING. A night window
--     lasts hours, so "shift to the end of the window" in this system means
--     "lose, but not right away" — exactly what the shift was meant to avoid;
--   * a shift is forbidden by a recorded invariant. Migration 000044 says in so many
--     words: "NO CATCH-UP... computing the next deadline by shifting from the previous one is
--     FORBIDDEN", and the rationale is there too — a salvo after downtime is exactly the
--     hammering the reminders table was created against.
--
-- WHAT IT DOES, step by step:
--   1. adds the user's time zone (users.timezone) and a flag that the
--      value was set BY HAND (users.timezone_set_manually);
--   2. adds the per-user quiet window bounds
--      (user_notification_prefs.quiet_hours_start / _end), off by
--      default;
--   3. introduces the eighth terminal delivery outcome `quiet_hours`.

-- ---------------------------------------------------------------------------
-- 1. The user's time zone
-- ---------------------------------------------------------------------------
--
-- ⚠ WHY THE ZONE IS STORED AND NOT TAKEN FROM SERVER TIME. Quiet "by server
-- time" works only while the whole team is in one zone, and breaks SILENTLY,
-- not with an error: for everyone else it simply falls on the wrong hours, and
-- this cannot be told apart from correct operation by any observable sign.
--
-- ⚠ WHY AN IANA ZONE NAME AND NOT AN OFFSET IN MINUTES. An offset is right exactly until the
-- next clock change: stored in summer, in winter it shifts the window by
-- an hour, and a person who set quiet hours from 22:00 gets a push at 22:30. A zone name
-- ("Europe/Moscow") accounts for the changes, because the time zone database
-- does, not our arithmetic.
--
-- ⚠ WHY NULL IS ALLOWED. Agents and MCP sessions do not go through a browser, and for
-- a user who has never come in through the web there is nowhere to get a zone from.
-- NULL here is an honest "we do not know", not "UTC": substituting UTC would pass
-- ignorance off as knowledge and would enable the quiet window at the wrong hours. A window without a zone
-- cannot be interpreted, so setting one with a NULL zone is rejected
-- by the application layer (422), and the pair "window set, zone unknown" is unreachable.
ALTER TABLE users
    ADD COLUMN timezone text,
    ADD COLUMN timezone_set_manually boolean NOT NULL DEFAULT false;

-- The zone name is checked for non-emptiness and for no spaces at the edges. A full
-- check "such a zone exists" is impossible here and is NOT FAKED: the list of
-- zones lives in the time zone database, not in the schema, and a list in the CHECK would go stale
-- by the first tzdata update. The existence of the zone is checked by the application
-- layer (time.LoadLocation) on write.
ALTER TABLE users
    ADD CONSTRAINT users_timezone_chk
        CHECK (timezone IS NULL OR (timezone <> '' AND timezone = btrim(timezone)));

COMMENT ON COLUMN users.timezone IS
    'The user''s time zone as an IANA name ("Europe/Moscow"). NULL — the zone '
    'is unknown: the person has never come in through a browser. A quiet window is not '
    'computed or set when it is NULL.';
COMMENT ON COLUMN users.timezone_set_manually IS
    'true — the zone was set by a person by hand. Detection from the browser '
    'does NOT overwrite such a value: otherwise a correction made in settings would live until '
    'the next sign-in and be silently reverted.';

-- ---------------------------------------------------------------------------
-- 2. Quiet window bounds
-- ---------------------------------------------------------------------------
--
-- ⚠ WHY `time`, NOT `timestamptz`. A window bound is a WALL-CLOCK mark
-- ("22:00") repeating every day, not a moment on the time axis.
-- A moment would have to be recomputed every day and would silently drift at a
-- clock change; a wall-clock mark survives the changes, because it is
-- interpreted in the recipient's zone at check time.
--
-- ⚠ WHY BOTH ARE NULL BY DEFAULT. "The window is off by default" is an acceptance
-- requirement. An empty pair is the off window; adding a separate "enabled"
-- flag would mean adding a second source of truth that could
-- diverge from the bounds.
ALTER TABLE user_notification_prefs
    ADD COLUMN quiet_hours_start time,
    ADD COLUMN quiet_hours_end   time;

-- Two checks, and both are about the fact that a window cannot be set half-way or
-- degenerately.
--
--   * pairing: one bound without the other does not define a window, and the question "what is the
--     other one" is answered by nobody;
--   * non-degeneracy: start = end is EITHER "never quiet" OR "always
--     quiet", and choosing one of the two for the person would mean making
--     a product decision silently. The setting is rejected, not interpreted.
ALTER TABLE user_notification_prefs
    ADD CONSTRAINT user_prefs_quiet_hours_pair_chk
        CHECK ((quiet_hours_start IS NULL) = (quiet_hours_end IS NULL)),
    ADD CONSTRAINT user_prefs_quiet_hours_nondegenerate_chk
        CHECK (quiet_hours_start IS NULL OR quiet_hours_start <> quiet_hours_end);

COMMENT ON COLUMN user_notification_prefs.quiet_hours_start IS
    'Start of the quiet window in the recipient''s wall-clock time, inclusive. NULL '
    'together with _end — the window is off (factory state).';
COMMENT ON COLUMN user_notification_prefs.quiet_hours_end IS
    'End of the quiet window in the recipient''s wall-clock time, NOT inclusive. Less than '
    'the start — the window crosses midnight (22:00–07:00), and that is a normal case, '
    'not an input error.';

-- ---------------------------------------------------------------------------
-- 3. The eighth terminal delivery outcome — silence
-- ---------------------------------------------------------------------------
--
-- ⚠ WHY A VALUE OF ITS OWN, NOT A SUBREASON OF configuration_no_send.
-- The sk-340 acceptance requires DISTINGUISHING "push not sent because of quiet hours" from
-- "push not sent because the path broke". Both look like the absence of
-- a push, so the discriminator must be in the row STATE, not only in the
-- measurement label: a subreason inside an existing outcome would leave silence
-- indistinguishable from a configuration failure when reading the row itself.
--
-- Exactly this decision was made by migration 000044 for the same reason, introducing
-- `superseded`. The set of outcomes is closed by a schema constraint (000039), so
-- a new value is introduced by a migration, not by a constant in code.
--
-- Recreating the CHECK, not ADD: the closed set exists as exactly one
-- constraint, and a second one next to it would mean two sources of truth.
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
            'superseded',
            'quiet_hours'
        ));
