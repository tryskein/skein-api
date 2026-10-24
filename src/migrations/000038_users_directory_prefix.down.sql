-- Reverses 000038_users_directory_prefix.

DROP INDEX IF EXISTS idx_users_active_name_lower_pattern;
DROP INDEX IF EXISTS idx_users_active_username_pattern;
