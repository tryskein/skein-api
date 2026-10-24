-- 000024 down — drop comments.author_kind (column, CHECK, index).
DROP INDEX IF EXISTS idx_comments_issue_author_kind;
ALTER TABLE comments DROP CONSTRAINT IF EXISTS comments_author_kind_check;
ALTER TABLE comments DROP COLUMN IF EXISTS author_kind;
