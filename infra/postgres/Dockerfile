# Skein local Postgres image.
#
# Base: pgvector/pgvector:pg18 — official pgvector image with pg_trgm and
# vector extensions already compiled in. We bake init-extensions.sql directly
# into the image (instead of bind-mounting from the host) to avoid Docker
# Desktop / WSL2 quirks where single-file bind mounts get resolved as empty
# directories.
#
# In production the same effect is achieved by CloudNativePG operator running
# `bootstrap.initdb.postInitSQL: ["CREATE EXTENSION pg_trgm;", "CREATE EXTENSION vector;"]`
# against a pgvector-flavour Postgres image.

FROM pgvector/pgvector:pg18

COPY init-extensions.sql /docker-entrypoint-initdb.d/00-extensions.sql
