-- 000076_events_space_entity — sk-674.18, T018 (SC-017, K-7).
--
-- A bulk membership change (join, leave, visibility change) writes one
-- space-level event, not one event per project (SC-017):
-- entity_type 'space', entity_id is the space id, project_id is empty, the list of
-- projects is in the payload. The event is history; live updates go through the personal
-- signal `spaces` and access_changed (contract signals/contract.go), so
-- the primitives write it already published (published_at = now()) and
-- the outbox dispatcher does not pick it up.

ALTER TABLE events DROP CONSTRAINT events_entity_type_check;
ALTER TABLE events ADD CONSTRAINT events_entity_type_check
    CHECK (entity_type IN ('issue','project','dep','comment','attachment','component','user','space'));
