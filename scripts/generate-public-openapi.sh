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
# Needs go (the swag tool comes from go.mod) and python3 (stdlib only).
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

# swag v2 always writes "externalDocs": {"description": "", "url": ""} when no
# @externalDocs is declared. OpenAPI 3.1 requires externalDocs.url to be a
# URI, so that empty object makes the document invalid; it is dropped here.
# A declared (non-empty) externalDocs is kept as generated.
python3 -I - "$tmp_dir/public_swagger.json" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as f:
    doc = json.load(f)
if doc.get("externalDocs") == {"description": "", "url": ""}:
    del doc["externalDocs"]
with open(path, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=4, ensure_ascii=False)
PY

mkdir -p "$out_dir"
mv "$tmp_dir/public_swagger.json" "$out_dir/openapi.json"
