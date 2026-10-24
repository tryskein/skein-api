-- 000076_events_space_entity (down): space events are deleted, the CHECK is the
-- previous set.

DELETE FROM events WHERE entity_type = 'space';
ALTER TABLE events DROP CONSTRAINT events_entity_type_check;
ALTER TABLE events ADD CONSTRAINT events_entity_type_check
    CHECK (entity_type IN ('issue','project','dep','comment','attachment','component','user'));
