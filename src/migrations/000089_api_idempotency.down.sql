-- 000089_api_idempotency (down) - drops the table api_idempotency with every
-- stored outcome. Repeats sent after the rollback are executed again.

DROP TABLE IF EXISTS api_idempotency;
