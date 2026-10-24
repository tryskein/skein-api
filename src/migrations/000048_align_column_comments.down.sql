-- Restores the wording that was in force before the sk-303.51 follow-up,
-- i.e. the text written by migrations 000044 and the notifications wave.

COMMENT ON COLUMN issues.review_required IS
    'Whether the creator must review the result. Private bookkeeping of the author: read and changed only by them (INV-2, INV-4). Issues created before the review workflow release have false — "not required" (SC-015).';

COMMENT ON COLUMN issues.reviewed_at IS
    'Moment of the review mark. NULL — the result has not been reviewed. The mark does not change issue status and gates nothing (INV-1).';

COMMENT ON COLUMN issues.reviewed_by IS
    'Author of the review mark. Always the issue author (INV-4). NULL after hard-delete of an account — the mark time is preserved.';

COMMENT ON COLUMN issues.assignee_set_at IS
    'Burst schedule base (R-5): the moment the issue was assigned to the current assignee. Stamped by trigger issues_stamp_assignee_set_at on every assignee-change path. NULL — the assignment moment was not observed (issue assigned before the review workflow release) or there is no assignee; such a row has no schedule base.';

COMMENT ON COLUMN notifications.reason IS
    'Reason the row belongs to. NULL on historical rows until migration 3 (T020) classifies them.';

COMMENT ON COLUMN notifications.feed_channel IS
    'Whether the row belongs to the feed channel. Reading surfaces select rows by this flag; a push-only row (feed_channel = false) is visible in no feed, no counters, no live stream, and no agent response. The wave plan names this flag feed_enabled — here it is named feed_channel on purpose, so it does not collide with user_notification_reason_prefs.feed_enabled: there it is a channel SETTING, here it is the row BELONGING to the channel. When the plan says "filter by feed_enabled on the notification row", substitute this column.';

COMMENT ON COLUMN notifications.push_state IS
    'DB-backed push lifecycle: none (no push) | pending (waiting to send) | leased (taken by the worker) | final (terminal outcome in push_outcome).';

COMMENT ON COLUMN reminder_rate_limit.last_formed_at IS
    'Moment of the last FORMED reminder of this category (INV-10). The window is consumed by a created notification, not by a successful delivery.';

COMMENT ON COLUMN reminder_rate_limit.next_due_at IS
    'Frozen moment of the next reminder of this category, computed from the prefs that were in force at OPEN of the current window. Stored, not derived from last_formed_at: derivation would take current prefs and thereby recompute an already-started window — cut it on interval shortening and stretch it on lengthening, and the spec requires a prefs change to take effect only from the next window. The "is it due" check reads this moment; a new value is written from CURRENT prefs at the same moment the window is consumed.';

COMMENT ON COLUMN user_notification_prefs.review_mechanic_enabled IS
    'Master kill switch of the review workflow. false — the unreviewed set, its display and reminders on it disappear for the user (INV-6). Data is preserved and returns when the switch is turned back on.';

COMMENT ON COLUMN user_notification_prefs.review_required_default IS
    'Personal default of the "review required" flag, applied on EVERY issue create path where the flag is not passed explicitly (US-3). Factory is true.';

COMMENT ON COLUMN user_notification_prefs.reminder_burst_count IS
    'Number of reminders in the first hour for unaccepted issues, 0..6. NULL — empty, factory is taken (3). 0 — no burst. Bounds from internal/review/schedule.go.';

COMMENT ON COLUMN user_notification_prefs.unaccepted_interval_minutes IS
    'Background interval for unaccepted issues, minutes, 15..527040 (366 days). NULL — empty, factory is taken (180). Bounds from internal/review/schedule.go; the upper is the representability limit of a mark.';

COMMENT ON COLUMN user_notification_prefs.unreviewed_interval_minutes IS
    'Background interval for unreviewed results, minutes, 15..527040 (366 days). NULL — empty, factory is taken (720). Bounds from internal/review/schedule.go; the upper is the representability limit of a mark.';

COMMENT ON TABLE migration_counters IS
    'Named counters of affected rows left by the sk-303.13 wave migrations. Read by migration tests and checked against T019 preflight counters.';

COMMENT ON TABLE reminder_rate_limit IS
    'Rate-limit state of repeating reminders on the pair "user and category" (INV-8). Not a preference, but delivery state: written by the reminder worker, not edited by a human.';

COMMENT ON TABLE user_notification_reason_prefs IS
    'State of two channels (feed, push) per reason per user. Absence of a row means the registry narrow default, not "everything on".';

