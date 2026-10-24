-- Down migration for 000001_init.
--
-- Drop in reverse order of creation. events references users (via
-- actor_user_id), projects references users (via created_by) — drop them
-- before users.
--
-- Extensions are NOT dropped here: they live at the cluster level and may be
-- used by other databases or migrations.

DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS projects;
DROP TABLE IF EXISTS users;
