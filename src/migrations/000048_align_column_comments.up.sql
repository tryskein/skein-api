-- sk-303.51 follow-up: bring the column comments in the DATABASE into line
-- with the wording already committed in the generated model.
--
-- The i18n wave polished the English of these fourteen comments directly in
-- src/internal/storage/generated/models.go. That file is generated FROM the
-- COMMENT ON statements of the earlier migrations, so the polish had no
-- source: `sqlc generate` reproduced the old wording and the sqlc-generated
-- guard (sk-339) went red on the first pipeline that saw those commits.
--
-- The fix belongs at the source, not in the generated file. Applied
-- migrations are never edited, so the new text is written here, and the
-- generated model is regenerated from it in the same commit — after which
-- source and artifact agree and the guard is green for the right reason.
--
-- Comments only: no table, column, index, constraint or data is touched.

COMMENT ON COLUMN issues.review_required IS
    'Whether the author must review the result. The author''s private bookkeeping: read and changed only by them (INV-2, INV-4). For issues created before the review workflow shipped, the value is false — "not required" (SC-015).';

COMMENT ON COLUMN issues.reviewed_at IS
    'Instant of the reviewed mark. NULL — the result has not been reviewed. The mark does not change the issue status and gates nothing (INV-1).';

COMMENT ON COLUMN issues.reviewed_by IS
    'Author of the reviewed mark. Always the issue author (INV-4). NULL after a hard account delete — the mark''s timestamp is kept.';

COMMENT ON COLUMN issues.assignee_set_at IS
    'Burst-countdown base (R-5): the instant the issue was assigned to the current assignee. Stamped by the issues_stamp_assignee_set_at trigger on every assignee-change path. NULL — the assignment instant was not observed (the issue was assigned before the review workflow shipped) or there is no assignee; such a row has no countdown base.';

COMMENT ON COLUMN notifications.reason IS
    'Reason this row belongs to. NULL on historical rows until migration 3 (T020) classified them.';

COMMENT ON COLUMN notifications.feed_channel IS
    'Whether the row belongs to the feed channel. Reading surfaces select rows by this flag; a push-only row (feed_channel = false) is visible in no feed, no counters, no live stream, and no agent response. The wave plan calls this flag feed_enabled — it is named feed_channel here on purpose, so it does not collide with user_notification_reason_prefs.feed_enabled: there it is a channel SETTING, here it is the row''s BELONGING to the channel. When the plan says "filter by feed_enabled on the notification row", substitute this column.';

COMMENT ON COLUMN notifications.push_state IS
    'DB-backed push lifecycle: none (no push) | pending (waiting to send) | leased (claimed by the runner) | final (terminal outcome in push_outcome).';

COMMENT ON COLUMN reminder_rate_limit.last_formed_at IS
    'Instant of the last FORMED reminder of this category (INV-10). A window is consumed by a created notification, not by a successful delivery.';

COMMENT ON COLUMN reminder_rate_limit.next_due_at IS
    'Frozen instant of the next reminder of this category, computed from the settings that were in force at the OPENING of the current window. Stored, not derived from last_formed_at: a derivation would take the current settings and thereby recompute an already-started window — it would shorten it when the interval shrinks and stretch it when it lengthens, and the spec requires a setting change to take effect only from the next window. The "is it due" check reads this instant; a new value is written with the CURRENT settings at the same moment the window is consumed.';

COMMENT ON COLUMN user_notification_prefs.review_mechanic_enabled IS
    'Master switch of the review workflow. false — the unreviewed set, its display, and its reminders disappear for the user (INV-6). The data is kept and returns when the switch is turned back on.';

COMMENT ON COLUMN user_notification_prefs.review_required_default IS
    'Personal default of the "review required" flag, applied on EVERY issue-create path where the flag is not passed explicitly (US-3). Built-in default is true.';

COMMENT ON COLUMN user_notification_prefs.reminder_burst_count IS
    'Number of reminders in the first hour for unaccepted issues, 0..6. NULL — empty, the factory (3) is taken. 0 — no burst. Bounds from internal/review/schedule.go.';

COMMENT ON COLUMN user_notification_prefs.unaccepted_interval_minutes IS
    'Background interval for unaccepted issues, minutes, 15..527040 (366 days). NULL — empty, the factory (180) is taken. Bounds from internal/review/schedule.go; the upper bound is the representability limit of the mark.';

COMMENT ON COLUMN user_notification_prefs.unreviewed_interval_minutes IS
    'Background interval for unreviewed results, minutes, 15..527040 (366 days). NULL — empty, the factory (720) is taken. Bounds from internal/review/schedule.go; the upper bound is the representability limit of the mark.';

COMMENT ON TABLE migration_counters IS
    'Named counts of affected rows left by the sk-303.13 wave migrations. Read by migration checks and compared with T019 preflight counters.';

COMMENT ON TABLE reminder_rate_limit IS
    'Rate-limit state of repeat reminders on a "user and category" pair (INV-8). Not a setting, a delivery state: written by the nudge runner, not edited by a person.';

COMMENT ON TABLE user_notification_reason_prefs IS
    'State of the two channels (feed, push) for each reason for each user. Absence of a row means the registry''s narrow default, not "everything is on".';

