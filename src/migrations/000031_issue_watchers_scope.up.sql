-- 000031_issue_watchers_scope — sk-142.8 (T8)
--
-- Subtree watch scope for the agent task-monitor (inter-agent comms). A
-- watcher on sk-42 with scope='subtree' is subscribed to sk-42 AND every
-- descendant by hierarchical id-prefix (sk-42.1, sk-42.1.3, ...). New
-- subtasks fall under the subscription automatically..
--
-- The scope lives on the SAME issue_watchers row — the PK (issue_id, user_id)
-- is unchanged, so a user still has at most one watcher row per issue. scope
-- is a third axis alongside reasons[] and the sticky-unwatch sentinel.
--
-- Semantics (enforced in fanout.AddIssueWatcherScoped, NOT the schema):
--   * subtree DOMINATES issue — calling watch with subtree on an existing
--     'issue' row upgrades it to 'subtree'.
--   * monotonic — a plain (scope='issue') watch / auto-add never downgrades an
--     existing 'subtree' row.
--   * sticky-unwatch is unchanged — a non-manual auto-add on an unwatched row
--     stays a full no-op (scope is not touched either).
--
-- scope is INTERNAL for v1: it is not surfaced on watcher DTOs / /me/watchers.
-- Those readers select explicit columns, so the new column is invisible to
-- them and a subtree subscription continues to render as a watch on the root
-- issue.
--
-- DEFAULT 'issue' backfills every existing row to the pre-feature behaviour.

ALTER TABLE issue_watchers
    ADD COLUMN scope text NOT NULL DEFAULT 'issue'
        CHECK (scope IN ('issue', 'subtree'));
