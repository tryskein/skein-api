-- 000089_api_idempotency - stored outcomes of public REST requests sent with
-- an Idempotency-Key header.
--
-- One row per (API key, Idempotency-Key), kept for 24 hours (expires_at). The
-- identity of the request - operation_id, target, body_fingerprint, prefer,
-- force - is compared on a repeat: the same key with a different identity is
-- refused, the same identity gets the stored outcome.
--
-- Life of a row:
--   1. state in_progress with the executor's own marker_token, written in a
--      short transaction; in_progress_until is the write timeout plus 30 s.
--   2. The transaction that performs the operation writes response_status,
--      response_id, response_version and response_refs, compare-and-swap on
--      marker_token, and sets state completed. A mismatch rolls the
--      operation back.
--   3. response_body (and its content type) is written after the commit by a
--      separate UPDATE on the same token, so it may be NULL on a completed
--      row. A repeat that finds no body answers from the stored status, id
--      and version and reads the other fields through response_refs.
--   An outcome without a writing transaction (for example a stored 422) is
--   written in one short transaction together with its body.

CREATE TABLE api_idempotency (
    api_key_id            uuid        NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    idempotency_key       text        NOT NULL,
    operation_id          text        NOT NULL,
    target                text        NOT NULL,
    body_fingerprint      bytea       NOT NULL,
    prefer                text,
    force                 boolean     NOT NULL,
    state                 text        NOT NULL CHECK (state IN ('in_progress', 'completed')),
    marker_token          uuid        NOT NULL,
    in_progress_until     timestamptz NOT NULL,
    response_status       integer,
    response_id           text,
    response_version      bigint,
    response_refs         jsonb,
    response_content_type text,
    response_body         bytea,
    created_at            timestamptz NOT NULL DEFAULT now(),
    expires_at            timestamptz NOT NULL,
    PRIMARY KEY (api_key_id, idempotency_key),
    CONSTRAINT api_idempotency_completed_has_status
        CHECK (state <> 'completed' OR response_status IS NOT NULL),
    CONSTRAINT api_idempotency_body_has_type
        CHECK (response_body IS NULL OR response_content_type IS NOT NULL)
);

CREATE INDEX idx_api_idempotency_expires ON api_idempotency (expires_at);

COMMENT ON TABLE api_idempotency IS
    'Outcomes of public REST requests with Idempotency-Key, per API key, kept until expires_at (24 h).';
COMMENT ON COLUMN api_idempotency.target IS
    'Target object from the request path (empty when the operation has none).';
COMMENT ON COLUMN api_idempotency.body_fingerprint IS
    'Digest of the request body; a repeat with a different body is refused.';
COMMENT ON COLUMN api_idempotency.prefer IS
    'Prefer header of the request; NULL when absent.';
COMMENT ON COLUMN api_idempotency.force IS
    'The force mode of the request; part of the request identity.';
COMMENT ON COLUMN api_idempotency.marker_token IS
    'Token of the executor that owns the row; every write after the marker is a compare-and-swap on it.';
COMMENT ON COLUMN api_idempotency.in_progress_until IS
    'End of the in-progress marker: the write timeout plus 30 s.';
COMMENT ON COLUMN api_idempotency.response_refs IS
    'References to the objects of the response, read again when the body was not stored.';
COMMENT ON COLUMN api_idempotency.response_body IS
    'Response body, written after the commit; NULL until then.';
