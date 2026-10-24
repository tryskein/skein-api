-- Reverse 000028_push_delivery.

ALTER TABLE user_notification_prefs DROP COLUMN push_disabled_categories;

ALTER TABLE notifications DROP COLUMN push_category;
