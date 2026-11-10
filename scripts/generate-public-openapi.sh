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
# The operations answer with the domain types of other packages of this module
# (projects.ProjectDTO, …), so swag parses the models of the dependencies
# (--parseDependencyLevel 1, models only) including internal packages.
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
  --parseInternal --parseDependencyLevel 1 \
  --instanceName public --outputTypes json -o "$tmp_dir"

# swag v2 cannot tell which members a response always carries; requiredgen
# declares as required every member its Go struct writes without omitempty
# (see its doc comment). The step below rewrites the file in its final form.
go run ./internal/publicapi/openapi/requiredgen "$tmp_dir/public_swagger.json"

# swag v2 always writes "externalDocs": {"description": "", "url": ""} when no
# @externalDocs is declared. OpenAPI 3.1 requires externalDocs.url to be a
# URI, so that empty object makes the document invalid; it is dropped here.
# A declared (non-empty) externalDocs is kept as generated.
#
# swag v2 cannot say that a field may be null in OpenAPI 3.1 (a type array
# with "null"). A field the server answers as null (a Go pointer without
# omitempty) is tagged extensions:"x-nullable" instead; here each such
# property becomes its 3.1 form: "type": [<type>, "null"], or, for a
# reference, "anyOf": [<the reference>, {"type": "null"}]; an enum of the
# property gets null as well. x-nullable with a value other than true, or on
# any other shape, stops the script: the tag would otherwise be silently lost.
python3 -I - "$tmp_dir/public_swagger.json" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as f:
    doc = json.load(f)
if doc.get("externalDocs") == {"description": "", "url": ""}:
    del doc["externalDocs"]


def nullable(prop, where):
    if "enum" in prop and None not in prop["enum"]:
        prop["enum"].append(None)
    if isinstance(prop.get("type"), str):
        prop["type"] = [prop["type"], "null"]
        return prop
    if "$ref" in prop or "allOf" in prop:
        target = {k: prop.pop(k) for k in ("$ref", "allOf") if k in prop}
        prop["anyOf"] = [target, {"type": "null"}]
        return prop
    sys.exit(f"x-nullable on {where}: neither a type nor a reference")


def walk(node, where):
    if isinstance(node, dict):
        if "x-nullable" in node:
            mark = node.pop("x-nullable")
            if mark is not True:
                sys.exit(f"x-nullable on {where} is {mark!r}: the tag is extensions:\"x-nullable\", without a value")
            nullable(node, where)
        for k, v in node.items():
            walk(v, f"{where}/{k}")
    elif isinstance(node, list):
        for i, v in enumerate(node):
            walk(v, f"{where}/{i}")


walk(doc, "#")
with open(path, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=4, ensure_ascii=False)
PY

mkdir -p "$out_dir"
mv "$tmp_dir/public_swagger.json" "$out_dir/openapi.json"
