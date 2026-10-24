-- 000008_watchers_notifications — R1.6
--
-- R1.6 closes the R1 personal-tracker slice: manual watch on issues/projects,
-- materialized notifications inbox with REST polling, per-user notification
-- preferences. Outbox + NATS push are deferred (R3.2/R3.3).
--
-- Five tables in one migration so partial state never leaks: handlers
-- assume all of them exist as soon as the migration applies. Per
-- db-schema.md §2.18-2.22.
--
-- IMPORTANT: this slot was previously reserved for an outbox library
-- migration (see db-schema.md §3 "000008 reserved for outbox"). The
-- outbox decision moved to R3.2 with a new slot — we take 000008 for
-- watchers/notifications because it lands first.

-- Per-issue watchers. reasons[] accumulates: creator | assignee | commenter
-- | mention | manual. 'unwatched' is a sticky sentinel — when present,
-- auto-add paths must not re-add the row, only an explicit POST /watch
-- (manual) clears it.
CREATE TABLE issue_watchers (
    issue_id             varchar(1000) NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    user_id              uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    reasons              text[] NOT NULL DEFAULT '{}',
    since                timestamptz NOT NULL DEFAULT now(),
    override_event_types text[],
    PRIMARY KEY (issue_id, user_id)
);
CREATE INDEX idx_issue_watchers_user ON issue_watchers (user_id);

-- Project-level watchers. Default reason 'manual' because there's no
-- auto-add path for project subscribers in R1.6 — members and watchers are
-- separate axes (see db-schema.md §2.20).
CREATE TABLE project_watchers (
    project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    reasons         text[] NOT NULL DEFAULT '{manual}',
    since           timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, user_id)
);
CREATE INDEX idx_project_watchers_user ON project_watchers (user_id);

-- Filter-based custom watchers. Created in R1.6 as a dormant table — no
-- handler endpoints yet. R3.3 will add fan-out matching.
CREATE TABLE custom_watchers (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name            text NOT NULL,
    filter          jsonb NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, name)
);
CREATE INDEX idx_custom_watchers_user ON custom_watchers (user_id);

-- One row per user; insert-on-first-use. notify_event_types NULL = all
-- types pass; [] = none pass; explicit list = whitelist. Filter is applied
-- at read time on inbox queries (db-schema §2.22).
CREATE TABLE user_notification_prefs (
    user_id                 uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    notify_event_types      text[],
    default_inbox_scope     text NOT NULL DEFAULT 'all'
                            CHECK (default_inbox_scope IN ('all','watched_only','assigned_only')),
    updated_at              timestamptz NOT NULL DEFAULT now()
);

-- Materialized notifications. id is bigserial so the inbox can use cursor
-- pagination on a stable monotonic key. UNIQUE (user_id, event_id)
-- dedupes the fan-out — both issue and project watchers can match the
-- same event, so we collapse to one row per recipient.
CREATE TABLE notifications (
    id              bigserial PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    event_id        bigint NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    type            text NOT NULL,
    project_id      uuid,
    read_at         timestamptz,
    archived_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, event_id)
);

-- Indexes for the four primary inbox views (db-schema.md §2.18 / §4):
--   * unread inbox      — partial WHERE read_at IS NULL AND archived_at IS NULL
--   * type-filtered     — same predicate plus type
--   * full feed         — every notification, read or not
--   * project view      — narrow to one project
-- Plus an FK index on event_id so the CASCADE-delete path on events
-- doesn't seq-scan the table.
CREATE INDEX idx_notif_unread ON notifications (user_id, created_at DESC)
    WHERE read_at IS NULL AND archived_at IS NULL;
CREATE INDEX idx_notif_user_type ON notifications (user_id, type, id DESC)
    WHERE read_at IS NULL AND archived_at IS NULL;
CREATE INDEX idx_notif_user_id ON notifications (user_id, id DESC);
CREATE INDEX idx_notif_user_project ON notifications (user_id, project_id, id DESC)
    WHERE project_id IS NOT NULL;
CREATE INDEX idx_notifications_event ON notifications (event_id);
