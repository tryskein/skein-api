#!/usr/bin/env bash
#
# generate-public-openapi.sh — regenerate the public OpenAPI 3.1 document.
#
# The public API (package internal/publicapi) is documented with swag v2
# annotations; the internal API keeps swag v1 (see AGENTS.md, "API docs").
# swag v2 names its output <instance>_swagger.json and has no option for the
# file name, so the document is generated into a temporary directory and
# moved to its committed path, src/internal/publicapi/openapi/openapi.json.
# CI runs this same script and fails when the committed file differs.
#
# Usage: scripts/generate-public-openapi.sh   (from any directory)
set -euo pipefail

src_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../src" && pwd)"
out_dir="$src_dir/internal/publicapi/openapi"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

cd "$src_dir"
go tool github.com/swaggo/swag/v2/cmd/swag init --v3.1 --quiet \
  -g doc.go -d internal/publicapi \
  --instanceName public --outputTypes json -o "$tmp_dir"

mkdir -p "$out_dir"
mv "$tmp_dir/public_swagger.json" "$out_dir/openapi.json"
