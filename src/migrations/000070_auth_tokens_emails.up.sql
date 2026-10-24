-- 000070_auth_tokens_emails — sk-674.18, T021 (D-6, INV-16, INV-25; K-9, K-11).
--
-- WHY. Emails for email confirmation, password reset and invitations go out
-- through a queue with retries until a deadline (K-9), and their one-time links are
-- tokens stored only as a hash (D-6, "Constraints" of the spec). The raw
-- token is stored nowhere: the worker gets it into memory from
-- tokens.IssueForEmail at the start of each delivery attempt (K-11).
--
-- WHAT.
--   * outgoing_emails — an email queue modeled on the push columns of notifications
--     (000039): state none → pending → leased → final, outcome at final,
--     attempts — the attempt counter and the lease generation (grows on every
--     lease; the pair (id, attempts) names one lease), leased_until,
--     next_attempt_at, deadline_at (= created_at + SKEIN_EMAIL_DELIVERY_TTL,
--     passed at enqueue time). An email stores the kind, the addressee (email in
--     lower case), the language and the SUBJECT — a user (confirmation,
--     reset) or an invitation; neither a token nor rendered text.
--   * auth_tokens — the sha256 hash of the token, purpose, subject, the email the
--     token was issued for (email_id), deadline, used_at, superseded_at. A live
--     token: not used, not retired, deadline not passed.
--   * space_invitations.current_token_id — the live invitation token
--     (D-6: an invitation is a business record with a reference to the live token).
--
-- SUBJECT. The purpose space_invite has an invitation subject (invitation_id),
-- user_id is empty; email_confirm and password_reset have a user subject,
-- invitation_id is empty (a CHECK in both tables). The subject row is
-- the serialization point of the K-11 protocol (internal/tokens).
--
-- DELETION. Emails in the final state are deleted by cleanup (T022);
-- the token's reference to the email is then nulled (ON DELETE SET NULL) — the token
-- lives out its deadline without the email. The invitation's reference to the token is also SET
-- NULL. Other foreign keys have no cascade (K-22: cascades are explicit).

CREATE TABLE outgoing_emails (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    kind            text        NOT NULL CHECK (kind IN ('email_confirm', 'password_reset', 'space_invite')),
    recipient       text        NOT NULL CHECK (recipient = lower(btrim(recipient)) AND length(recipient) BETWEEN 3 AND 320),
    lang            text        NOT NULL CHECK (lang IN ('ru', 'en')),
    user_id         uuid        REFERENCES users(id) ON DELETE RESTRICT,
    invitation_id   uuid        REFERENCES space_invitations(id) ON DELETE RESTRICT,
    state           text        NOT NULL DEFAULT 'pending'
                                CHECK (state IN ('none', 'pending', 'leased', 'final')),
    outcome         text        CHECK (outcome IN ('success', 'rejected', 'expired', 'cancelled')),
    attempts        integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    leased_until    timestamptz,
    next_attempt_at timestamptz,
    deadline_at     timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    finalized_at    timestamptz,
    CHECK ((state = 'final') = (outcome IS NOT NULL)),
    CHECK ((state = 'final') = (finalized_at IS NOT NULL)),
    CHECK (CASE WHEN kind = 'space_invite'
                THEN invitation_id IS NOT NULL AND user_id IS NULL
                ELSE user_id IS NOT NULL AND invitation_id IS NULL END)
);

-- Lease by the worker (T022): ready for an attempt by time.
CREATE INDEX idx_outgoing_emails_pending ON outgoing_emails (next_attempt_at NULLS FIRST, created_at)
    WHERE state = 'pending';
-- Return of expired leases.
CREATE INDEX idx_outgoing_emails_leased ON outgoing_emails (leased_until) WHERE state = 'leased';
-- Cleanup of final rows by age.
CREATE INDEX idx_outgoing_emails_finalized ON outgoing_emails (finalized_at) WHERE state = 'final';
-- K-11 protocol: unfinished emails of the subject and purpose.
CREATE INDEX idx_outgoing_emails_user_open ON outgoing_emails (user_id, kind)
    WHERE user_id IS NOT NULL AND state <> 'final';
CREATE INDEX idx_outgoing_emails_invitation_open ON outgoing_emails (invitation_id)
    WHERE invitation_id IS NOT NULL AND state <> 'final';

CREATE TABLE auth_tokens (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    purpose       text        NOT NULL CHECK (purpose IN ('email_confirm', 'password_reset', 'space_invite')),
    token_hash    bytea       NOT NULL CHECK (length(token_hash) = 32),
    user_id       uuid        REFERENCES users(id) ON DELETE RESTRICT,
    invitation_id uuid        REFERENCES space_invitations(id) ON DELETE RESTRICT,
    email_id      uuid        REFERENCES outgoing_emails(id) ON DELETE SET NULL,
    expires_at    timestamptz NOT NULL,
    used_at       timestamptz,
    superseded_at timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (used_at IS NULL OR superseded_at IS NULL),
    CHECK (CASE WHEN purpose = 'space_invite'
                THEN invitation_id IS NOT NULL AND user_id IS NULL
                ELSE user_id IS NOT NULL AND invitation_id IS NULL END)
);

CREATE UNIQUE INDEX idx_auth_tokens_hash ON auth_tokens (token_hash);
-- K-11 protocol: unretired and unused tokens of the subject, emails.
CREATE INDEX idx_auth_tokens_user_open ON auth_tokens (user_id, purpose)
    WHERE user_id IS NOT NULL AND used_at IS NULL AND superseded_at IS NULL;
CREATE INDEX idx_auth_tokens_invitation_open ON auth_tokens (invitation_id)
    WHERE invitation_id IS NOT NULL AND used_at IS NULL AND superseded_at IS NULL;
CREATE INDEX idx_auth_tokens_email_open ON auth_tokens (email_id)
    WHERE email_id IS NOT NULL AND used_at IS NULL AND superseded_at IS NULL;

ALTER TABLE space_invitations
    ADD COLUMN current_token_id uuid REFERENCES auth_tokens(id) ON DELETE SET NULL;
