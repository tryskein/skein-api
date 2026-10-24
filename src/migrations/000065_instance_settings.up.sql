-- 000065_instance_settings — sk-674.18, T011 (K-2, D-2, INV-20).
--
-- WHY. The "instance admin sees everything" bypass will be a branch inside the SQL access
-- function and must apply only when SKEIN_INSTANCE_ADMIN_ENABLED=true.
-- A function cannot read an environment variable, and receiving the flag as a
-- parameter from the caller is not allowed (the flag would become caller-controlled).
-- The source is this table: trusted configuration written by api itself.
--
-- MECHANISM.
--   * Exactly one row: id = 1 (CHECK).
--   * on every start after migrations api writes the value of the
--     variable into it and rereads the row (reconciliation) —
--     internal/config/startup_checks.go#SyncInstanceSettings. A divergence is a
--     start error.
--   * The row is seeded here with the value true — what is in effect today
--     (default K-30). A database api has not connected to yet (a test schema
--     applied by migrations alone) sees the former semantics, not emptiness.

CREATE TABLE instance_settings (
    id            smallint    PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    admin_enabled boolean     NOT NULL,
    updated_at    timestamptz NOT NULL DEFAULT now()
);

INSERT INTO instance_settings (id, admin_enabled) VALUES (1, true);
