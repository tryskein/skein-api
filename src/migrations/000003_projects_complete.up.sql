-- 000003_projects_complete — R1.2
--
-- Completes the projects schema:
--   * adds git_repos (jsonb), gitlab_group, archived_at to projects
--   * adds GIN trigram index on name + partial index on archived_at
--   * creates project_aliases (global lowercase alias → project mapping)
--   * creates project_members (per-project ACL + role)
--   * creates components (project-scoped flat micro-modules)
--
-- See the database schema document for the canonical
-- DDL. project_counters is intentionally NOT created here — it ships in
-- 000004_issues alongside the issues table that consumes it.

ALTER TABLE projects
    ADD COLUMN git_repos    jsonb       NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN gitlab_group text,
    ADD COLUMN archived_at  timestamptz;

-- Trigram index on name powers fuzzy resolve (§4.2). The "name % $q" operator
-- in queries uses this index; "similarity(name, $q) > 0.5" alone wouldn't.
CREATE INDEX idx_projects_name_trgm ON projects USING gin (name gin_trgm_ops);

-- Partial index — most projects are active; the index only stores rows where
-- archived_at IS NOT NULL, so it stays tiny.
CREATE INDEX idx_projects_archived ON projects (archived_at)
    WHERE archived_at IS NOT NULL;

-- ---------- project_aliases ----------
-- Denormalized flat lookup. PRIMARY KEY (alias) — globally unique
-- lowercase strings. CASCADE on project deletion: aliases follow.
CREATE TABLE project_aliases (
    alias         text PRIMARY KEY,
    project_id    uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    added_by      uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    added_by_kind text NOT NULL CHECK (added_by_kind IN ('human','agent','system')),
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_project_aliases_project ON project_aliases (project_id);
CREATE INDEX idx_project_aliases_trgm    ON project_aliases USING gin (alias gin_trgm_ops);
CREATE INDEX idx_project_aliases_added_by ON project_aliases (added_by);

-- ---------- project_members ----------
-- Per-project ACL. Composite PK (project_id, user_id) — one membership per
-- (user, project). admin role on users skips this table entirely.
CREATE TABLE project_members (
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       text NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'member')),
    added_at   timestamptz NOT NULL DEFAULT now(),
    added_by   uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    PRIMARY KEY (project_id, user_id)
);
CREATE INDEX idx_project_members_user     ON project_members (user_id);
CREATE INDEX idx_project_members_added_by ON project_members (added_by);

-- ---------- components ----------
-- Project-scoped micro-modules. UNIQUE(project_id, name) prevents duplicates
-- inside one project. paths jsonb stores [{repo_id, glob}] tuples; repo_id
-- referential integrity to projects.git_repos[].id is validated in the app
-- layer (jsonb FKs are not supported by Postgres).
CREATE TABLE components (
    id          uuid PRIMARY KEY,
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    paths       jsonb NOT NULL DEFAULT '[]'::jsonb,
    archived_at timestamptz,
    created_by  uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);
CREATE INDEX idx_components_project    ON components (project_id);
CREATE INDEX idx_components_active     ON components (project_id) WHERE archived_at IS NULL;
CREATE INDEX idx_components_created_by ON components (created_by);
