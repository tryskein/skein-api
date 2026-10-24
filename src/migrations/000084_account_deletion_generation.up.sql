-- 000084_account_deletion_generation — sk-674.18, T041 (SC-011, INV-18).
--
-- WHY. The search result cache lives in the memory of each replica
-- (search/cache.go). Account deletion rewrites its email and name in
-- text that a cached page could hold in snippets;
-- "after the response the email appears in no response" cannot wait for the cache
-- TTL and cannot be signalled to other replicas. Therefore the fingerprint of the
-- search scope (search/store.go#visibilityFingerprint) carries the deletion
-- GENERATION: any deletion changes it, and from the next request no
-- replica serves a page filled before it.
--
-- HOW. One row (id = 1) with a monotonic counter. Anonymization
-- (users/anonymize.go#anonymizeInTx) increments it in its own transaction:
-- rolling back the deletion rolls back the counter too. Search reads it by primary
-- key - a constant cost independent of the number of deleted accounts (the previous
-- variant counted count(*) of deleted accounts on every search).
--
-- The row is seeded here; the application does not create it.

CREATE TABLE account_deletion_generation (
    id         smallint PRIMARY KEY CHECK (id = 1),
    generation bigint   NOT NULL DEFAULT 0 CHECK (generation >= 0)
);

-- The initial value is the number of already deleted accounts: the generation will not
-- match any under which cache pages could have been filled
-- before the migration (in practice the cache is empty after a restart).
INSERT INTO account_deletion_generation (id, generation)
SELECT 1, count(*) FROM users WHERE deleted_at IS NOT NULL;
