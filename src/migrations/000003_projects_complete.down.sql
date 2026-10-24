-- 000003_projects_complete — R1.2 (down)
--
-- Reverses the up migration in dependency order: drop tables that reference
-- projects/users first, then the indexes added on projects, then the columns.

DROP TABLE components;
DROP TABLE project_members;
DROP TABLE project_aliases;

DROP INDEX idx_projects_archived;
DROP INDEX idx_projects_name_trgm;

ALTER TABLE projects
    DROP COLUMN archived_at,
    DROP COLUMN gitlab_group,
    DROP COLUMN git_repos;
