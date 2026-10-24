-- 000005_dependencies — R1.4
--
-- Dependencies graph (7 types) between issues. Canonical DDL: the database schema document
-- §2.13 / §3 "000005_dependencies".
--
-- Direction (canonical, see internal/dependencies/types.go for full table):
--   - blocks         — from = blocked, to = blocker
--   - parent-child   — from = child,   to = parent
--   - waits-for      — from = waiter,  to = awaited
--   - supersedes     — from = new,     to = old
--   - duplicates     — from = dup,     to = original
--   - related        — directional storage; semantics symmetric
--   - discovered-from — directional, no cycle check
--
-- PK is composite (from_id, to_id, type) so a single (a,b) pair can hold
-- distinct typed edges. CASCADE on both ends matches vision §3.Dependency
-- "deletes follow the issues they connect".

CREATE TABLE dependencies (
    from_id         varchar(1000) NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    to_id           varchar(1000) NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    type            text NOT NULL
                    CHECK (type IN ('blocks','related','parent-child','discovered-from',
                                    'supersedes','duplicates','waits-for')),
    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb,
    version         int NOT NULL DEFAULT 1,
    created_by      uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (from_id, to_id, type),
    CONSTRAINT dep_no_self CHECK (from_id <> to_id)
);

-- Outgoing/incoming list per issue + per type filter (Ready uses type='blocks').
CREATE INDEX idx_deps_from_type ON dependencies (from_id, type);
CREATE INDEX idx_deps_to_type   ON dependencies (to_id,   type);
-- Ready algorithm hot path — partial index keeps the candidate scan tiny on
-- projects where most deps are non-blocking ('related', 'parent-child', etc).
CREATE INDEX idx_deps_blockers  ON dependencies (from_id) WHERE type = 'blocks';
-- FK index on RESTRICT FK so user-delete scans don't seq-scan dependencies.
CREATE INDEX idx_dependencies_created_by ON dependencies (created_by);

-- Backfill: R1.3 wrote issues.parent_id but no dep row. We mirror every
-- existing parent_id as a parent-child edge so R1.4 graph queries are
-- consistent from day one. Idempotent — re-running the migration is a no-op
-- thanks to the composite-PK conflict.
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
SELECT i.id, i.parent_id, 'parent-child', '{}'::jsonb, 1, i.created_by, i.created_at
  FROM issues i
 WHERE i.parent_id IS NOT NULL
ON CONFLICT (from_id, to_id, type) DO NOTHING;
