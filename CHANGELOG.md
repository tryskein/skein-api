# Changelog

All notable changes to **skein-api** (Go REST + MCP backend).

## [Unreleased]

- Fix search pagination after a result pool expires or another replica handles
  the next page: continuations retain their offset and ranking mode, with
  deterministic ordering when relevance scores and update times are equal.
  During a rolling upgrade from the previous cursor format, replicas on
  different versions may reject each other's cursors; restart the query without
  a cursor if this happens.
- MCP `skein_update` and `skein_update_many` reject non-string values for
  `description`, `design`, `acceptanceCriteria`, `notes`, `externalRef`, and
  `sourceContext` instead of silently clearing them. Batch validation rejects
  only the affected row and continues processing valid rows. Explicit null or
  empty strings still clear a field; omitted fields remain unchanged.

## [1.0.0] - 2026-10-23

- First public release.
