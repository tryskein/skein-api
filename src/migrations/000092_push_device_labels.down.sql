-- 000092_push_device_labels (down) — drops the push_device_revocations table
-- with every device revocation, and the push_subscriptions columns
-- device_label and version with their index. Lossy: revocations and labels
-- are lost; subscriptions deleted by the up migration (owners deactivated or
-- deleted) are not restored. Remaining subscriptions are kept.

DROP TABLE push_device_revocations;
DROP INDEX idx_push_subscriptions_user_label;
ALTER TABLE push_subscriptions
    DROP COLUMN version,
    DROP COLUMN device_label;
