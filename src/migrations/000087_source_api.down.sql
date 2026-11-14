-- 000087_source_api (down) - removes the value api from the CHECK constraints
-- issues_source_check and comments_author_kind_check. Lossy: rows that carry
-- api are rewritten to agent_directed, the value the origin rules already
-- treated them as; the api / agent_directed distinction is not recoverable on
-- re-up.

ALTER TABLE issues DROP CONSTRAINT issues_source_check;
UPDATE issues SET source = 'agent_directed' WHERE source = 'api';
ALTER TABLE issues ADD CONSTRAINT issues_source_check
    CHECK (source IN ('human', 'agent_directed', 'agent_autonomous'));

ALTER TABLE comments DROP CONSTRAINT comments_author_kind_check;
UPDATE comments SET author_kind = 'agent_directed' WHERE author_kind = 'api';
ALTER TABLE comments ADD CONSTRAINT comments_author_kind_check
    CHECK (author_kind IN ('human', 'agent_directed', 'agent_autonomous'));
