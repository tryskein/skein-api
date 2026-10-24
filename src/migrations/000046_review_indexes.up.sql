-- 000046_review_indexes — sk-303.14, tasks T012 and T014.
--
-- ONE partial index under the UNREVIEWED SET, and only under it.
--
-- WHY THIS INDEX HERE, NOT IN 000044. Migration 000044 introduced the
-- mechanic's columns and deliberately introduced no partial index: an
-- index without its query is untestable. The set's semantic contract
-- states MEMBERSHIP, but not join shape, column order, or selectivity —
-- all of that is derived from the query, and a guessed-ahead index then
-- forces a correct query to be distorted so the planner will pick it.
-- Unreviewed queries (filter on two list paths and the second-number
-- counter) are written by THIS task, so here the index shape is
-- derivable and fixable.
--
-- WHY THERE IS NO INDEX FOR THE UNACCEPTED SET. The same rule, applied
-- the same way: the hot unaccepted query — with a join on rate-limit
-- state and a due-time predicate — appears only at the reminder worker
-- (T017, migration 000047). The unaccepted predicate already exists
-- (internal/review/set.go), but a predicate is not a query: an index
-- under a bare fragment may not fit the real select.
--
-- WHY ONE INDEX, NOT TWO — DECIDED BY MEASUREMENT, NOT REASONING. The
-- delegated surface has two paths, and their leading columns differ: the
-- cross-project one (/me/created-by, /me/assigned-by) — created_by, the
-- project one (/projects/{id}/issues?createdBy=me) — project_id. The
-- first revision of this migration introduced one index per path. Plan
-- checks on a representative volume (T014, matrix row 61, test
-- review_unreviewed_index_plan_test.go) showed the second index is not
-- needed: on BOTH paths and in the counter the planner takes the index
-- below, and takes it as an ordered Index Scan.
--
-- ⚠ CAVEAT (sk-303.19, added AFTER apply — the migration SQL is
-- untouched, only this comment is edited; applied migrations are not
-- rewritten per the repo rules, but the file is not the only instance of
-- the text, and keeping a known-false claim in it is worse than noting
-- the divergence). The line above — "without sorting the result" — was
-- not true at every `users` volume: measurement showed a NON-MONOTONE
-- dependence (a sort is present at 100 `users` rows, and absent at 2,
-- 20, 500, 5000). The cause is not this index and not the `issues` read
-- — the sort is added by the JOIN with `users`, which the index has no
-- relation to. A second index on issues would not have closed that pit
-- either, so the "one index" decision still stands. Full breakdown with
-- numbers and why the gate flapped on the shared test database — in the
-- header of internal/handler/review_unreviewed_index_plan_test.go.
--
-- This claim has exactly the weight the gate holds, and no more.
-- assertUsesUnreviewedIndex asks of two LIST queries three things: the
-- outer issues read node is Index Scan / Index Only Scan (bitmap access
-- is rejected: it reads the index but loses order), the index name sits
-- on the node itself, and among its ancestors there is no Sort /
-- Incremental Sort. Of the COUNT satellites and of the second-number
-- counter only index choice is asked — they do not need order.
--
-- The reason is the nature of the set: a person has a handful of
-- unreviewed items. After the created_by filter inside the partial index
-- a few records remain, and the extra project_id filter is cheaper than
-- a separate index. The reverse substitution does not work: the
-- cross-project path has no project_id equality at all, so an index with
-- leading project_id would give it neither an entry nor order. One index
-- covers both paths; two double the write cost without a single won
-- plan.
--
-- ⚠ Diagnosis miss: measuring this on a fixture where a noticeable
-- fraction of the table falls into the set is useless. At about a
-- quarter the planner becomes indifferent and walks off to the unrelated
-- idx_issues_project_status — observed on this task. The fixture
-- proportion is part of the check, not its decoration.
--
-- WHAT IS SERVED. The index is partial on the immutable part of the set
-- contract:
--
--     status = 'closed' AND review_required AND reviewed_at IS NULL
--
-- Three conditions are chosen into the WHERE body, not the key, because
-- they are the same for ALL queries of the set and cut the table down to
-- fractions of a percent: closed issues with the flag on and no mark are
-- orders of magnitude fewer than issues in general. The rest of the
-- contract (authorship, assignee ≠ author, no dismiss, project archive,
-- mechanic kill switch, visibility horizon) is checked on rows fetched
-- by the index.
--
-- Conditions "not in the body" — for a reason, not taste:
--   * created_by — in the KEY, not the body: each reader has their own
--     value;
--   * dismissed_by / archived_at / prefs — computed and external, they
--     have no place in a partial index (IMMUTABLE requirement);
--   * assignee <> author — almost always true, filtering on it is
--     useless.
--
-- COLUMN ORDER. Both list queries are ordered `updated_at DESC, id DESC`
-- (keyset pagination of every issue list — keyset on this pair), so the
-- key continues with exactly this pair: at a fixed created_by the index
-- already yields rows in the needed order, and a sort with LIMIT does
-- not become "collect everything → sort → discard". The second-number
-- counter is served by the first key column.
--
-- The justification rests on the absence of a sort, so it is the absence
-- of a sort that is checked — otherwise column order could be chosen
-- arbitrarily. Sort is searched among ANCESTORS of the outer issues
-- read, not across the whole plan: both lists' projection holds a
-- correlated subquery of the current parent with its own
-- `ORDER BY d.created_at LIMIT 1`, and its Sort is a neighbouring node,
-- not one stacked on the list.
--
-- COST. A partial index on a narrow predicate occupies a negligible
-- fraction of a full one: rows outside WHERE do not enter the index at
-- all. Writes get more expensive only on transitions into closed and
-- back and on flag/mark changes — operations orders of magnitude fewer
-- than reads of these lists.
--
-- CONCURRENTLY IS NOT USED: golang-migrate runs the migration in a
-- transaction, and CREATE INDEX CONCURRENTLY is forbidden in a
-- transaction. The same choice as every previous migration in this
-- repository.
--
-- GENERATION. sqlc.yaml reads `schema: migrations`, so the
-- internal/storage/generated/ directory is rebuilt and committed in THE
-- SAME commit. Indexes do not move the model, but cleanliness of the
-- directory after a generator run is a gate of EVERY task with a
-- migration, not only the first (sqlc-generated job, sk-339).

CREATE INDEX idx_issues_unreviewed_creator
    ON issues (created_by, updated_at DESC, id DESC)
 WHERE status = 'closed'
   AND review_required
   AND reviewed_at IS NULL;

COMMENT ON INDEX idx_issues_unreviewed_creator IS
    'sk-303.14 T012: the unreviewed set. Serves BOTH paths of the delegated '
    'surface — /me/created-by and /projects/{id}/issues?createdBy=me with '
    'unreviewed=true — and the second-number counter '
    '(delegatedUnreviewedCount). The key continues with the pagination pair, '
    'so at a fixed created_by the list is yielded without a separate sort. A '
    'separate index with leading project_id was checked by plans and is not '
    'needed: see the migration header.';

