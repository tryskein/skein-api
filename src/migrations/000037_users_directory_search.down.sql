-- 000037_users_directory_search (down) — drop the directory search/sort indexes.

DROP INDEX IF EXISTS idx_users_name_trgm;
DROP INDEX IF EXISTS idx_users_username_trgm;
DROP INDEX IF EXISTS idx_users_active_username;
