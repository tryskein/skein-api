-- 000069_spaces_schema — sk-674.18, T015 (D-3, D-4, D-6, INV-6, INV-16; K-20, K-23 step 1).
--
-- WHY. The first step of the K-23 transition: an additive spaces schema. Only
-- new tables and new columns with safe defaults —
-- code that does not yet know about spaces works on this schema as
-- before (inserts without the new columns get the defaults).
-- The transition data (common, "Personal", sources) and the tightening (NOT NULL,
-- uniqueness of names within a space and per author) are the next migrations (T016).
--
-- WHAT.
--   * spaces — a space: a short address id (K-20: 8 base32 characters,
--     lower case), kind personal / common / regular, owner, name
--     (not unique). "Personal" is one per owner, common is one per installation
--     (partial unique indexes).
--   * space_members — a space member with role owner / member;
--     there is one owner (INV-10, partial unique index).
--   * space_invitations — an invitation (INV-16): one pending per
--     (space, lower(email)) — a partial unique index; email is
--     stored normalized (CHECK). The reference to the live token
--     (current_token_id) will appear together with the tokens table (T021).
--   * projects.space_id (NULL until the transition; NOT NULL is the T016 tightening),
--     projects.visibility space / members (default members — the strict one).
--   * project_members.via_manual / via_space — the sources of a membership row
--     (INV-6). Existing rows and rows inserted by old code are "manual"
--     (project operations set only this source); a CHECK forbids a
--     row without sources.
--   * users: email_confirmed_at, terms_accepted_at, deleted_at, pseudonym
--     (INV-18: deleted-<8hex>, unique), session_epoch (K-16; contract
--     accounts/contract.go: the epoch of a new account is 0).
--   * reserved_identities — sha256(lower(value)) hashes of email/username
--     of deleted accounts (K-21, INV-18); reserved_prefixes — prefixes
--     of deleted projects (K-22, D-4), in lower case, as compared by
--     idx_projects_id_prefix_lower.
--   * space_storage_usage — a denormalized volume of confirmed
--     attachments of a space (K-8).
--   * themes.author_id — the theme owner (K-31); filled by the transition (T016).
--
-- CASCADES. Foreign keys of the new tables have no ON DELETE CASCADE: deleting a
-- space is an explicit cascade in one transaction (K-22, T019), not an FK.
--
-- INDEXES. space_members(user_id) — "my spaces" and the access relation
-- (T017); projects(space_id) — the projects of a space. project_members
-- (user_id) already exists (idx_project_members_user, 000003) — not duplicated.

CREATE TABLE spaces (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    short_id   text        NOT NULL CHECK (short_id ~ '^[a-z2-7]{8}$'),
    kind       text        NOT NULL CHECK (kind IN ('personal', 'common', 'regular')),
    owner_id   uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    name       text        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_spaces_short_id ON spaces (short_id);
-- INV-8: exactly one "Personal" per owner.
CREATE UNIQUE INDEX idx_spaces_personal_owner ON spaces (owner_id) WHERE kind = 'personal';
-- INV-22: the common space is one per installation.
CREATE UNIQUE INDEX idx_spaces_common_single ON spaces (kind) WHERE kind = 'common';
CREATE INDEX idx_spaces_owner ON spaces (owner_id);

CREATE TABLE space_members (
    space_id  uuid        NOT NULL REFERENCES spaces(id) ON DELETE RESTRICT,
    user_id   uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    role      text        NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'member')),
    joined_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (space_id, user_id)
);

CREATE INDEX idx_space_members_user ON space_members (user_id);
-- INV-10: a space has one owner.
CREATE UNIQUE INDEX idx_space_members_single_owner ON space_members (space_id) WHERE role = 'owner';

CREATE TABLE space_invitations (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id    uuid        NOT NULL REFERENCES spaces(id) ON DELETE RESTRICT,
    email       text        NOT NULL CHECK (email = lower(btrim(email)) AND length(email) BETWEEN 3 AND 320),
    invited_by  uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    status      text        NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending', 'accepted', 'declined', 'revoked', 'expired')),
    lang        text        NOT NULL CHECK (lang IN ('ru', 'en')),
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    CHECK ((status = 'pending') = (resolved_at IS NULL))
);

-- INV-16: one pending invitation per (space, email case-
-- insensitive). lower() is insurance on top of the normalization CHECK: uniqueness
-- is case-insensitive even if the CHECK is ever weakened.
CREATE UNIQUE INDEX idx_space_invitations_pending
    ON space_invitations (space_id, lower(email)) WHERE status = 'pending';
CREATE INDEX idx_space_invitations_space ON space_invitations (space_id);

ALTER TABLE projects
    ADD COLUMN space_id   uuid REFERENCES spaces(id) ON DELETE RESTRICT,
    ADD COLUMN visibility text NOT NULL DEFAULT 'members' CHECK (visibility IN ('space', 'members'));

CREATE INDEX idx_projects_space ON projects (space_id);

ALTER TABLE project_members
    ADD COLUMN via_manual boolean NOT NULL DEFAULT true,
    ADD COLUMN via_space  boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT project_members_has_source CHECK (via_manual OR via_space);

ALTER TABLE users
    ADD COLUMN email_confirmed_at timestamptz,
    ADD COLUMN terms_accepted_at  timestamptz,
    ADD COLUMN deleted_at         timestamptz,
    ADD COLUMN pseudonym          text CHECK (pseudonym ~ '^deleted-[0-9a-f]{8}$'),
    ADD COLUMN session_epoch      bigint NOT NULL DEFAULT 0 CHECK (session_epoch >= 0),
    ADD CONSTRAINT users_deleted_has_pseudonym CHECK (deleted_at IS NULL OR pseudonym IS NOT NULL);

CREATE UNIQUE INDEX idx_users_pseudonym ON users (pseudonym) WHERE pseudonym IS NOT NULL;

CREATE TABLE reserved_identities (
    kind        text        NOT NULL CHECK (kind IN ('email', 'username')),
    value_hash  bytea       NOT NULL CHECK (length(value_hash) = 32),
    reserved_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, value_hash)
);

CREATE TABLE reserved_prefixes (
    prefix      text        PRIMARY KEY CHECK (prefix = lower(prefix) AND length(prefix) BETWEEN 1 AND 50),
    reserved_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE space_storage_usage (
    space_id   uuid        PRIMARY KEY REFERENCES spaces(id) ON DELETE RESTRICT,
    bytes      bigint      NOT NULL DEFAULT 0 CHECK (bytes >= 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE themes
    ADD COLUMN author_id uuid REFERENCES users(id) ON DELETE RESTRICT;
