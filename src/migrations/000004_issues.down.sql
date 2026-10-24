-- 000004_issues — R1.3 (down)
--
-- Reverse order: leaf tables first, then issues, then project_counters last.
-- Postgres drops indexes implicitly with the table, so no DROP INDEX clauses.

DROP TABLE issue_labels;
DROP TABLE comments;
DROP TABLE issue_commits;
DROP TABLE issue_components;
DROP TABLE child_counters;
DROP TABLE issues;
DROP TABLE project_counters;
