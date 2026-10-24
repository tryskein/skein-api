-- 000081_space_storage_usage_backfill - sk-674.18, T026 (K-8, INV-15).
--
-- The space_storage_usage counter is maintained since this commit in the transactions
-- that confirm and delete an attachment (attachments/store.go). Attachments
-- confirmed before it are not counted: we fill it with the sum of
-- confirmed (status = 'ready') attachments of all projects of each
-- space. Unconfirmed (pending) ones take no quota.

INSERT INTO space_storage_usage (space_id, bytes, updated_at)
SELECT p.space_id, sum(a.size_bytes), now()
  FROM attachments a
  JOIN issues i ON i.id = a.issue_id
  JOIN projects p ON p.id = i.project_id
 WHERE a.status = 'ready'
 GROUP BY p.space_id
ON CONFLICT (space_id) DO UPDATE SET bytes = EXCLUDED.bytes, updated_at = now();
