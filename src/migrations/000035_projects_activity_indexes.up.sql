-- 000035_projects_activity_indexes — sk-183.1 review follow-up
--
-- The new GET /dashboard/projects endpoint (internal/dashboard/projects.go)
-- runs, on every 30s HOT ZONE poll, windowed range scans over:
--
--   issues WHERE created_at >= $since             (issuesOpened metric)
--   issue_commits WHERE linked_at >= $since        (commits metric)
--
-- Neither column had a supporting index — issues only indexes created_by
-- (idx_issues_created_by, FK scan) and issue_commits only indexes hash /
-- linked_by (idx_issue_commits_hash, idx_issue_commits_linked_by). Both
-- queries degraded to a full table scan for every window narrower than
-- "all". Flagged independently by two reviewers on
-- the sk-183.1 review.

CREATE INDEX idx_issues_created_at ON issues (created_at);
CREATE INDEX idx_issue_commits_linked_at ON issue_commits (linked_at);
