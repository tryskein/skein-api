-- 000004_issues — R1.3
--
-- Issues + counters + comments + labels + components-junction + commit-refs.
-- Canonical DDL: the database schema document.
--
-- Order matters:
--   1. project_counters BEFORE issues — atomic claim ID must work on the
--      first INSERT (counter mode). Backfill row for every existing project.
--   2. issues — main entity. locked_by/locked_at columns are seeded now
--      (R1.3) but only used from R2.3 (agent claim flow). CHECK constraints
--      on consistency are in place from day one.
--   3. child_counters — references issues(id) so it lives after issues.
--   4. issue_components — M-to-M (issues ↔ components, both pre-exist).
--   5. issue_commits — normalised commit-refs (replaces jsonb on issues).
--      Created here so the table is empty when issues table starts taking
--      writes; later commit-link flow populates it.
--   6. comments + issue_labels — leaf tables, FK on issues only.
--
-- Indexes per the database schema document, "Issue list filters" + FK indexes
-- on every RESTRICT FK to keep the cleanup scan O(log N).

-- ---------- project_counters ----------

CREATE TABLE project_counters (
    project_id uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    counter    int  NOT NULL DEFAULT 0
);

-- Backfill rows for every existing project so the first INSERT INTO issues
-- in counter mode can RETURNING-claim without an extra INSERT branch.
-- ON CONFLICT DO NOTHING makes the migration idempotent.
INSERT INTO project_counters (project_id, counter)
SELECT id, 0 FROM projects
ON CONFLICT (project_id) DO NOTHING;

-- ---------- issues ----------

CREATE TABLE issues (
    id                  varchar(1000) PRIMARY KEY,
    project_id          uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    parent_id           varchar(1000) REFERENCES issues(id) ON DELETE RESTRICT,
    version             int  NOT NULL DEFAULT 1,
    title               text NOT NULL,
    description         text NOT NULL DEFAULT '',
    type                text NOT NULL
                        CHECK (type IN ('bug','feature','task','epic','chore','decision','idea','note')),
    priority            int  NOT NULL DEFAULT 2 CHECK (priority BETWEEN 0 AND 4),
    status              text NOT NULL DEFAULT 'open'
                        CHECK (status IN ('open','in_progress','blocked','deferred','closed')),
    pinned              boolean NOT NULL DEFAULT false,
    source              text NOT NULL CHECK (source IN ('human','agent')),
    source_context      text,
    assignee            text,
    locked_by           text,
    locked_at           timestamptz,
    design              text NOT NULL DEFAULT '',
    acceptance_criteria text NOT NULL DEFAULT '',
    notes               text NOT NULL DEFAULT '',
    estimated_minutes   int  CHECK (estimated_minutes IS NULL OR estimated_minutes >= 0),
    due_at              timestamptz,
    defer_until         timestamptz,
    started_at          timestamptz,
    closed_at           timestamptz,
    closed_reason       text,
    external_ref        text,
    created_by          uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    metadata            jsonb NOT NULL DEFAULT '{}'::jsonb,

    CONSTRAINT issues_locked_consistency CHECK (
        (locked_by IS NULL AND locked_at IS NULL) OR
        (locked_by IS NOT NULL AND locked_at IS NOT NULL)
    ),
    CONSTRAINT issues_closed_consistency CHECK (
        (status = 'closed' AND closed_at IS NOT NULL) OR
        (status <> 'closed' AND closed_at IS NULL)
    )
);

CREATE INDEX idx_issues_project_status ON issues (project_id, status);
CREATE INDEX idx_issues_status_priority ON issues (status, priority) WHERE status = 'open';
CREATE INDEX idx_issues_assignee       ON issues (assignee) WHERE assignee IS NOT NULL;
CREATE INDEX idx_issues_parent         ON issues (parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX idx_issues_updated        ON issues (updated_at);
CREATE INDEX idx_issues_title_trgm     ON issues USING gin (title gin_trgm_ops);
CREATE INDEX idx_issues_external_ref   ON issues (external_ref) WHERE external_ref IS NOT NULL;
CREATE INDEX idx_issues_created_by     ON issues (created_by);

-- ---------- child_counters ----------

CREATE TABLE child_counters (
    parent_id  varchar(1000) PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
    last_child int  NOT NULL DEFAULT 0
);

-- ---------- issue_components ----------

CREATE TABLE issue_components (
    issue_id     varchar(1000) NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    component_id uuid          NOT NULL REFERENCES components(id) ON DELETE CASCADE,
    PRIMARY KEY (issue_id, component_id)
);
CREATE INDEX idx_issue_components_component ON issue_components (component_id);

-- ---------- issue_commits ----------

CREATE TABLE issue_commits (
    issue_id       varchar(1000) NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    repo_id        uuid          NOT NULL,
    hash           text          NOT NULL,
    url            text,
    message        text,
    author         text,
    committed_at   timestamptz,
    linked_at      timestamptz   NOT NULL DEFAULT now(),
    linked_by      uuid          REFERENCES users(id) ON DELETE RESTRICT,
    linked_by_kind text          NOT NULL CHECK (linked_by_kind IN ('human','agent','system')),
    PRIMARY KEY (issue_id, repo_id, hash)
);
CREATE INDEX idx_issue_commits_hash      ON issue_commits (hash);
CREATE INDEX idx_issue_commits_linked_by ON issue_commits (linked_by) WHERE linked_by IS NOT NULL;

-- ---------- comments ----------

CREATE TABLE comments (
    id             uuid PRIMARY KEY,
    issue_id       varchar(1000) NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    author         text NOT NULL,
    author_user_id uuid REFERENCES users(id) ON DELETE RESTRICT,
    author_type    text NOT NULL CHECK (author_type IN ('human','agent','system')),
    body           text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_comments_issue       ON comments (issue_id, created_at);
CREATE INDEX idx_comments_author_user ON comments (author_user_id) WHERE author_user_id IS NOT NULL;

-- ---------- issue_labels ----------

CREATE TABLE issue_labels (
    issue_id varchar(1000) NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    label    text          NOT NULL,
    PRIMARY KEY (issue_id, label)
);
CREATE INDEX idx_issue_labels_label ON issue_labels (label);
