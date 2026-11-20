-- 000092_push_device_labels — device label, record version and device
-- revocations for browser push subscriptions.
--
-- push_subscriptions.device_label is the UUID the browser creates for one
-- device consent; it is NULL on every subscription registered before this
-- migration and on subscriptions registered by an older page.
-- push_subscriptions.version grows with every write of the record; a
-- conditional removal and the "endpoint is gone" removal match a record by
-- id and version, so a later registration of the same endpoint is never
-- removed by a stale decision. Existing rows start at 1.
--
-- push_device_revocations holds a durable revocation of a device label for a
-- user: a registration with a revoked label is refused at write time, and a
-- revocation is written even when no subscription exists. It goes with the
-- user only when the users row is physically deleted (ON DELETE CASCADE).
--
-- Subscriptions of users that are deactivated or deleted at deployment time
-- are deleted: such users must not keep a delivery channel.

ALTER TABLE push_subscriptions
    ADD COLUMN device_label uuid NULL,
    ADD COLUMN version      bigint NOT NULL DEFAULT 1;

-- Lookup of a user's subscriptions by label (revocation, removal by label).
CREATE INDEX idx_push_subscriptions_user_label
    ON push_subscriptions (user_id, device_label)
    WHERE device_label IS NOT NULL;

CREATE TABLE push_device_revocations (
    user_id      uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_label uuid        NOT NULL,
    revoked_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, device_label)
);

DELETE FROM push_subscriptions s
 USING users u
 WHERE u.id = s.user_id
   AND (u.deactivated_at IS NOT NULL OR u.deleted_at IS NOT NULL);
