-- Down: drop the scope column. The CHECK constraint is dropped with it.
ALTER TABLE issue_watchers DROP COLUMN scope;
