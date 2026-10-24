-- 000018 — Widen issues.source from {human, agent} to
-- {human, agent_directed, agent_autonomous}.
--
-- Motivation: distinguish "the user asked the agent to create this
-- issue" (directed) from "the agent created it mid-task as a discovered
-- side-quest" (autonomous). Inbox / activity feed surface autonomous
-- issues with a different visual cue so the user can spot self-spawned
-- work without reading every title.
--
-- Backfill: every existing 'agent' row is folded into 'agent_directed'.
-- No historical issue was ever an autonomous side-quest (the value did
-- not exist before this migration), so the assumption is provably safe.
--
-- Order: drop the old CHECK before the UPDATE so the rewrite is allowed
-- to land 'agent_directed' values, then re-add the widened CHECK.
ALTER TABLE issues DROP CONSTRAINT issues_source_check;
UPDATE issues SET source = 'agent_directed' WHERE source = 'agent';
ALTER TABLE issues ADD CONSTRAINT issues_source_check
    CHECK (source IN ('human', 'agent_directed', 'agent_autonomous'));
