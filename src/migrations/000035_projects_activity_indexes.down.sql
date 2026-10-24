-- 000035_projects_activity_indexes (down) — drop the /dashboard/projects support indexes.

DROP INDEX IF EXISTS idx_issue_commits_linked_at;
DROP INDEX IF EXISTS idx_issues_created_at;
