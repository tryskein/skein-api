-- 000024 — Add comments.author_kind enum-as-text-with-CHECK alongside the
-- existing comments.author_type.
--
-- Two orthogonal axes:
--   author_type  — transport (human / agent / system) — UNCHANGED.
--   author_kind  — intent (human / agent_directed / agent_autonomous) — NEW.
--
-- sk-16: the user can tell the agent «leave a comment» and the agent's
-- write lands as author_type='agent' even though semantically a human
-- initiated it. The "comments by humans" filter loses such rows. Mirrors
-- the same split already in issues.source.
--
-- Backfill (conservative — we lose nothing, new comments arrive with the
-- correct kind):
--   - author_type='agent'           → author_kind='agent_autonomous'
--     (we don't know if old agent comments were directed; mark as
--      autonomous so a future audit can spot the ambiguity. Future comments
--      land with the explicit kind set by the MCP/REST caller.)
--   - author_type='human'           → author_kind='human'
--   - author_type='system'          → author_kind='human'
--     (system comments are seeded by migrations and don't surface in the
--      Humans/Agents UI filter; folding them into 'human' keeps the
--      'agent_autonomous' label pure for actual autonomous agent writes.)
--
-- Order: add the column NULLable so the backfill UPDATE can populate it
-- before the NOT NULL + CHECK constraints land in the same migration.
ALTER TABLE comments ADD COLUMN author_kind text;

UPDATE comments SET author_kind = 'agent_autonomous' WHERE author_type = 'agent';
UPDATE comments SET author_kind = 'human'            WHERE author_type IN ('human', 'system');

ALTER TABLE comments ALTER COLUMN author_kind SET NOT NULL;
ALTER TABLE comments ALTER COLUMN author_kind SET DEFAULT 'human';
ALTER TABLE comments ADD CONSTRAINT comments_author_kind_check
    CHECK (author_kind IN ('human', 'agent_directed', 'agent_autonomous'));

-- Index supports the UI filter `origin=human-driven|agent-autonomous` on
-- the comments list (ListComments WHERE clause). Composite on (issue_id,
-- author_kind) so the filtered list is index-scanned without re-reading.
CREATE INDEX idx_comments_issue_author_kind
    ON comments (issue_id, author_kind);
