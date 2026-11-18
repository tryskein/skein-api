-- 000091_api_key_created_email — a notice email when an API key is created
-- or reissued (sk-977.7.49).
--
-- WHY. A key created behind the owner's back (a stolen browser session)
-- keeps working after the password is changed; the owner learns about every
-- new key from an email naming it and the time it was created.
--
-- WHAT.
--   * outgoing_emails.kind accepts api_key_created. Its subject is the
--     key's owner (user_id, the existing CHECK of user subjects); it carries
--     no link token, so auth_tokens.purpose is unchanged.
--   * outgoing_emails.api_key_id — the key the notice is about. The worker
--     reads the key's label and creation time from it at delivery, so no
--     user text is stored in the queue. ON DELETE SET NULL: a notice whose
--     key row is gone is cancelled by the worker instead of blocking the
--     deletion. Only api_key_created rows carry it.

ALTER TABLE outgoing_emails DROP CONSTRAINT outgoing_emails_kind_check;
ALTER TABLE outgoing_emails ADD CONSTRAINT outgoing_emails_kind_check
    CHECK (kind IN ('email_confirm', 'password_reset', 'space_invite', 'api_key_created'));

ALTER TABLE outgoing_emails
    ADD COLUMN api_key_id uuid REFERENCES api_keys(id) ON DELETE SET NULL;
ALTER TABLE outgoing_emails ADD CONSTRAINT outgoing_emails_api_key_id_check
    CHECK (api_key_id IS NULL OR kind = 'api_key_created');
-- The SET NULL of a deleted key finds its notices by this index.
CREATE INDEX idx_outgoing_emails_api_key ON outgoing_emails (api_key_id)
    WHERE api_key_id IS NOT NULL;
