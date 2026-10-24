-- 000080_projects_space_not_null (down). Only NOT NULL is dropped; projects
-- moved into the common space stay there (the move is data, not
-- schema), as do the members of the common space.

ALTER TABLE projects ALTER COLUMN space_id DROP NOT NULL;
