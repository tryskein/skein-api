-- 000070_auth_tokens_emails (down) — reverse order of up.

ALTER TABLE space_invitations DROP COLUMN IF EXISTS current_token_id;
DROP TABLE IF EXISTS auth_tokens;
DROP TABLE IF EXISTS outgoing_emails;
