-- Rollback of 000041_classify_prune_notification_history.
--
-- ⚠ DELETED NOTIFICATIONS ARE NOT RESTORED. That is a spec decision, not an
-- omission: the delete is named irreversible verbatim, and a full-database
-- backup before the migration is required in exchange. So there is no, and
-- must not be, a shadow copy of deleted rows, a soft-delete flag in place of
-- DELETE, or any other mechanism that would make a data rollback possible:
-- all of that would be an undeclared change of construction and would keep
-- exactly the data the wave is getting rid of.
--
-- The only way to get the deleted notifications back is to restore the
-- full-database backup taken by the release's backup gate BEFORE the up
-- migration was applied — as a separate restore with the service stopped,
-- after which a compatible image is returned. Rolling the image back on its
-- own restores nothing, which is why that gate precedes the new image starting
-- and why the artifact's retention bounds the rollback window.
--
-- What rollback ACTUALLY does — drop the schema objects the up migration
-- created, and remove its trail in the counters journal so a re-apply does
-- not double the rows:
--
--   * drops the SQL projection of the classifier and its composite type;
--   * deletes this migration's counter rows.
--
-- What rollback does NOT do, and why: it does not null out `notifications.reason`.
-- The column was added by migration 000039 and is dropped by that migration's
-- own rollback; and reasons stamped by the historical classification are, after
-- this rollback, indistinguishable from reasons that new delivery writes at
-- insert time. Blanking them all would wipe both — data corruption for
-- cosmetics, not a schema return.
--
-- The up/down/up smoke checks CHAIN INTEGRITY, not data return.

DROP FUNCTION IF EXISTS notif_classify(
    text, text[], boolean, boolean, boolean, boolean, boolean, boolean, boolean, boolean, boolean);
DROP TYPE IF EXISTS notif_classification;

DELETE FROM migration_counters WHERE migration = '000041_classify_prune_notification_history';
