-- Down: drop the attachments table. CASCADE on FKs handles the indexes.
-- S3 objects in MinIO are NOT touched on rollback — operators must clear
-- the bucket manually if they want a clean slate.
DROP TABLE attachments;
