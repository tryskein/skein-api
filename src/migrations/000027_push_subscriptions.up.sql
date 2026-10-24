-- 000027_push_subscriptions — Web Push foundation.
--
-- A push subscription is the browser-supplied endpoint + keypair that the
-- application server signs (VAPID) and POSTs payloads to. One row per
-- browser/device push registration. The endpoint URL is the natural unique
-- key: a given browser push channel maps to exactly one endpoint, and the
-- browser re-sends the same endpoint when it re-subscribes, so we upsert on
-- it (ON CONFLICT (endpoint) DO UPDATE) — re-subscribing the same device
-- after it was reassigned to another user simply re-homes the row.
--
-- user_id ON DELETE CASCADE: a deactivated/removed user's subscriptions go
-- with them (a subscription is a personal delivery channel, no audit value
-- once the user is gone). The Send path also prunes dead endpoints itself:
-- a 404/410 from the push service means the browser unsubscribed, and the
-- row is hard-deleted.
--
-- p256dh / auth are the browser-provided ECDH public key and auth secret
-- used to encrypt the payload (RFC 8291). ua is the optional User-Agent
-- captured at subscribe time so a user can recognise which device a row
-- belongs to when managing their registrations.

CREATE TABLE push_subscriptions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    endpoint     text NOT NULL,
    p256dh       text NOT NULL,
    auth         text NOT NULL,
    ua           text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now()
);

-- endpoint is the upsert conflict target (one row per browser push channel).
CREATE UNIQUE INDEX idx_push_subscriptions_endpoint ON push_subscriptions (endpoint);

-- Per-user lens ("my devices") + the fan-out send query (every endpoint for
-- a user). Also serves the users ON DELETE CASCADE so it doesn't seq-scan.
CREATE INDEX idx_push_subscriptions_user ON push_subscriptions (user_id);
