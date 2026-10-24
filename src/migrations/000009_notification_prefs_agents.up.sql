-- 000009_notification_prefs_agents — R1.6.x
--
-- Adds per-user opt-out for "my agent did something" notifications.
-- When suppress_own_agent_actions = true, the fan-out arms in
-- notifications.FanOut skip a recipient row whenever the originating
-- event was emitted by an agent actor (events.actor_type = 'agent')
-- belonging to the same user. This lets a user run heavy agentic tasks
-- (vision §F24/F25) without flooding their own inbox.
--
-- Default false preserves existing behaviour: every recipient still gets
-- a notification regardless of who acted.
ALTER TABLE user_notification_prefs
    ADD COLUMN suppress_own_agent_actions boolean NOT NULL DEFAULT false;
