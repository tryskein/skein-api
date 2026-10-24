-- 000078_instance_settings_common_space - sk-674.18, T020 (INV-22, K-2).
--
-- The SKEIN_COMMON_SPACE_ENABLED flag is needed by the account creation primitive in all
-- creation paths (admin REST, admin agent, SSO provisioning, bootstrap):
-- users are added to the common space automatically only when the flag is on.
-- The source is the same trusted configuration as the admin flag (000065): on
-- startup api writes the variable's value and re-reads the row
-- (config/startup_checks.go#SyncInstanceSettings). Seed - true, the K-30 default.

ALTER TABLE instance_settings
    ADD COLUMN common_space_enabled boolean NOT NULL DEFAULT true;
