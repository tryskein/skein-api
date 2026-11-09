# Changelog

All notable changes to **skein-api** (Go REST + MCP backend).

## [1.1.0] - 2026-11-09

- Search (`GET /api/v1/search`) can now be scoped like the task list: new issue
  filters `assignee`, `createdBy` (`me` keeps the Delegated selection,
  `not-me`), `acceptedAt`, `isBlocked`, `componentId` and `origin` join the
  existing `status`, `priority`, `type` and `label`. Filters are applied before
  ranking and pagination, so a filtered search returns complete pages instead
  of a ranked page that is narrowed afterwards. Filters apply to issue results
  only; comments, projects and components keep their own selection (sk-346).
- Search pagination continues correctly after the cached result set expires or
  another replica serves the next page: a continuation keeps its offset and
  ranking mode, and results with equal relevance and update time keep a stable
  order (sk-194.6.53).
- A write that loses a database serialization race no longer reports a false
  `version_conflict`. When the issue's version has not changed, REST (issue
  update, status transitions, accept, bulk actions) and MCP (`skein_update`,
  `skein_update_many`, `skein_close`, `skein_close_many`, `skein_accept`)
  return `serialization_retry`: the aborted transaction was rolled back. A
  single-step write can be retried unchanged; after a multi-step update, re-read
  the issue first, because earlier steps may already be committed. A real
  version mismatch is still `version_conflict` (sk-648).
- MCP `skein_update` and `skein_update_many` reject non-string values for
  `description`, `design`, `acceptanceCriteria`, `notes`, `externalRef` and
  `sourceContext` (`validation_error`, code `text_field_not_string`) instead of
  silently clearing the field. In a batch only the affected row fails. Null or
  an empty string still clears a field; an omitted field stays unchanged (sk-626).
- MCP `skein_list` and `skein_ready` reject a non-string `assignee` filter
  (`validation_error`, code `assignee_not_string`) instead of ignoring it.
  Omitted, null or blank still means no filter (sk-827).
- Several skein-api replicas starting at once on an empty database no longer
  fail while racing to create the first administrator; the losing replicas
  retry and pick up the administrator that was created (sk-771).
- Dashboard: the scoring response adds `leaderboardUsers`, the per-person
  breakdown for everyone on a leaderboard, including people beyond the first
  50 rows of `matrixUsers` (sk-192).
- The MCP agent guide (`skein_get_instructions`) is much shorter and keeps only
  Skein-specific rules; several tool descriptions (create, close, dependencies,
  attachments, watchers, monitor script) are clarified (sk-908, sk-909).

### Upgrade notes

- No database migrations or configuration changes are required.
- If a search cursor is rejected during rollout or when a filtered search
  reaches another API replica, restart the search with the same query and
  filters. The web UI retries automatically once.
- Custom API clients should handle `serialization_retry`; re-read an issue
  before retrying a multi-step update.

## [1.0.0] - 2026-10-23

- First public release.
