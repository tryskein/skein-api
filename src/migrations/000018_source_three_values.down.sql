-- 000018 down — collapse {human, agent_directed, agent_autonomous}
-- back to {human, agent}.
--
-- Both new values fold to 'agent' on the way down — this is lossy (the
-- directed/autonomous distinction is not recoverable on re-up) but
-- matches the only sensible reverse mapping given the original schema
-- had no place to record the difference.
ALTER TABLE issues DROP CONSTRAINT issues_source_check;
UPDATE issues SET source = 'agent'
 WHERE source IN ('agent_directed', 'agent_autonomous');
ALTER TABLE issues ADD CONSTRAINT issues_source_check
    CHECK (source IN ('human', 'agent'));
