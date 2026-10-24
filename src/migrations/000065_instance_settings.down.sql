-- Rollback of 000065_instance_settings.
--
-- Fully reversible: the table stores only a copy of the environment variable,
-- which api will overwrite on the next start of a version with this migration.

DROP TABLE IF EXISTS instance_settings;
