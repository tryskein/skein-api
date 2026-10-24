-- 000001_init — R0.3 Bootstrap
--
-- Minimal schema required to run skein-api: users, projects, events.
-- Subsequent migrations (000002+) extend these tables with auth, components,
-- aliases, members, issues, dependencies, etc..
--
-- Extensions (pg_trgm, vector) are NOT created here. They live at the cluster
-- level: locally via infra/postgres/init-extensions.sql executed by the
-- postgres entrypoint as superuser; in production via CloudNativePG
-- postInitSQL. The application user used by golang-migrate does not have
-- CREATE EXTENSION privilege.

-- ---------- users ----------
-- Minimum columns for R0.3. password_hash, keycloak_sub, last_login_at are
-- added in 000002_auth_standalone (R1.1).
CREATE TABLE users (
    id              uuid PRIMARY KEY,
    email           text NOT NULL,
    username        text NOT NULL CHECK (username ~ '^[a-z0-9][a-z0-9_-]{2,29}$'),
    name            text NOT NULL,
    role            text NOT NULL CHECK (role IN ('admin', 'user')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE UNIQUE INDEX idx_users_email_lower ON users (lower(email));
CREATE UNIQUE INDEX idx_users_username_lower ON users (lower(username));

-- ---------- projects ----------
-- Minimum columns for R0.3. archived_at, git_repos, gitlab_group are added
-- in 000003_projects_complete (R1.2).
CREATE TABLE projects (
    id              uuid PRIMARY KEY,
    name            text NOT NULL,
    id_prefix       varchar(50) NOT NULL,
    default_id_mode text NOT NULL DEFAULT 'counter'
                    CHECK (default_id_mode IN ('counter','hash','custom')),
    description     text NOT NULL DEFAULT '',
    created_by      uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE UNIQUE INDEX idx_projects_name_lower ON projects (lower(name));
CREATE UNIQUE INDEX idx_projects_id_prefix_lower ON projects (lower(id_prefix));
CREATE INDEX idx_projects_created_by ON projects (created_by);

-- ---------- events ----------
-- Final structure from day one — CHECK on entity_type is locked down here so
-- R0.3 cannot insert garbage values that would persist forever once the
-- constraint is later added.
CREATE TABLE events (
    id                  bigserial PRIMARY KEY,
    occurred_at         timestamptz NOT NULL DEFAULT now(),
    type                text NOT NULL,
    actor               text NOT NULL,
    actor_user_id       uuid REFERENCES users(id) ON DELETE RESTRICT,
    actor_session_id    text,
    actor_type          text NOT NULL CHECK (actor_type IN ('human','agent','system')),
    entity_type         text NOT NULL CHECK (entity_type IN ('issue','project','dep','comment','attachment','component','user')),
    entity_id           text NOT NULL,
    project_id          uuid,
    field               text,
    old_data            jsonb,
    new_data            jsonb,
    reason              text,
    payload             jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX idx_events_entity ON events (entity_type, entity_id, id);
CREATE INDEX idx_events_project ON events (project_id, id) WHERE project_id IS NOT NULL;
CREATE INDEX idx_events_occurred ON events (occurred_at);
CREATE INDEX idx_events_actor_user ON events (actor_user_id) WHERE actor_user_id IS NOT NULL;
