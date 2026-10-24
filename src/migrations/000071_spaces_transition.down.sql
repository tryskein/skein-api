-- 000071_spaces_transition (down) - rollback of the transition to the 000070 schema without
-- spaces in the data.
--
-- Restores theme selections to the originals and deletes the copies, drops the author-scoped
-- theme name uniqueness, restores the original names of themes that the transition
-- separated by case (spaces_transition_theme_renames; their suffix could have
-- matched another author's theme name), and restores global
-- uniqueness (fails if same-named themes by different authors appeared after the
-- transition - resolve manually),
-- takes projects out of spaces and deletes space data: members,
-- invitations with their tokens and emails, counters, the spaces themselves.
-- Project membership rows stay as "manual". email_confirmed_at is not
-- touched (the column is dropped by down 000069).

UPDATE users u SET custom_theme_id = c.original_id
  FROM spaces_transition_theme_copies c
 WHERE u.custom_theme_id = c.copy_id
   AND EXISTS (SELECT 1 FROM themes t WHERE t.id = c.original_id);
DELETE FROM themes WHERE id IN (SELECT copy_id FROM spaces_transition_theme_copies);
DROP TABLE spaces_transition_theme_copies;

-- Names - after dropping the author index: the original names of one author
-- differ only by case and are incompatible under it.
DROP INDEX IF EXISTS idx_themes_author_name_lower;
UPDATE themes t SET name = r.original_name
  FROM spaces_transition_theme_renames r
 WHERE t.id = r.theme_id;
DROP TABLE spaces_transition_theme_renames;
UPDATE themes SET author_id = NULL;
ALTER TABLE themes ADD CONSTRAINT themes_name_key UNIQUE (name);

UPDATE projects SET space_id = NULL, visibility = 'members';
UPDATE project_members SET via_manual = true, via_space = false
 WHERE NOT via_manual OR via_space;

UPDATE space_invitations SET current_token_id = NULL;
DELETE FROM auth_tokens WHERE invitation_id IS NOT NULL;
DELETE FROM outgoing_emails WHERE invitation_id IS NOT NULL;
DELETE FROM space_invitations;
DELETE FROM space_storage_usage;
DELETE FROM space_members;
DELETE FROM spaces;
