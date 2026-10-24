-- Down: drop the monitor_credentials table. The UNIQUE(token_digest) index
-- and FK constraints are dropped with it.
DROP TABLE monitor_credentials;
