-- 000081_space_storage_usage_backfill (down). Data only, the schema was not
-- changed: the filled counter stays - it is correct after the rollback too (it is
-- maintained by code since T026), and zeroing it would make it wrong.

SELECT 1;
