# Changelog

All notable changes to **skein-api** (Go REST + MCP backend).

## [Unreleased]

- Search pagination preserves its ranking mode and offset when a result pool
  expires. During rolling upgrades, replicas on different versions may reject
  each other's cursors; restart the query without a cursor if this happens.

## [1.0.0] - 2026-10-23

- First public release.
