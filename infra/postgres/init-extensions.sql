-- Postgres extensions for Skein.
--
-- Baked into the local Postgres image (see Dockerfile next to this file) at
-- /docker-entrypoint-initdb.d/00-extensions.sql so they are activated by the
-- postgres superuser on first volume init. The application user used by
-- golang-migrate does not have CREATE EXTENSION privilege (matches
-- CloudNativePG behaviour in production), so extensions must be created at
-- cluster bootstrap, not inside migrations.

CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS vector;
