-- 000087_source_api - the origin value api for issues and comments.
--
-- A record written with an API key through the public REST API gets
-- issues.source = 'api' / comments.author_kind = 'api' unless the request names
-- an explicit kind. Rules that depend on the origin treat api as
-- agent_directed; the value itself keeps the record apart in filters and
-- labels. Only the CHECK constraints change; no row is rewritten.

ALTER TABLE issues DROP CONSTRAINT issues_source_check;
ALTER TABLE issues ADD CONSTRAINT issues_source_check
    CHECK (source IN ('human', 'agent_directed', 'agent_autonomous', 'api'));

ALTER TABLE comments DROP CONSTRAINT comments_author_kind_check;
ALTER TABLE comments ADD CONSTRAINT comments_author_kind_check
    CHECK (author_kind IN ('human', 'agent_directed', 'agent_autonomous', 'api'));
