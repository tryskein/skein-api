-- 000091_api_key_created_email (down) — drops the API key notice emails:
-- every outgoing_emails row of kind api_key_created (queued, in flight or
-- final) is deleted, then the api_key_id column and the widened kind CHECK
-- go. Lossy: undelivered notices are not sent.

DELETE FROM outgoing_emails WHERE kind = 'api_key_created';
DROP INDEX idx_outgoing_emails_api_key;
ALTER TABLE outgoing_emails DROP CONSTRAINT outgoing_emails_api_key_id_check;
ALTER TABLE outgoing_emails DROP COLUMN api_key_id;
ALTER TABLE outgoing_emails DROP CONSTRAINT outgoing_emails_kind_check;
ALTER TABLE outgoing_emails ADD CONSTRAINT outgoing_emails_kind_check
    CHECK (kind IN ('email_confirm', 'password_reset', 'space_invite'));
