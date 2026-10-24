#!/usr/bin/env bash
#
# measure-tools-list.sh — weight of the MCP tools/list payload.
#
# Why this lives in the repo instead of in a wave's scratch directory:
# sk-255.3 folds ~34 tools into 8 across four waves, and each wave has to
# state what it cost. Waves 1 and 2 each wrote their own throwaway script
# and reported 108 012 and 108 072 chars for the SAME starting point —
# numbers that cannot be put in one series. One committed script is the
# fix: every wave and the final report run this, so the deltas add up.
#
# Metric: sum of len(json.Marshal(tool)) over every tool in the tools/list
# response, read back through an in-process MCP client against the
# production registration closure (cmd/api.mcpRegisterToolsFn). That is
# the descriptor a real client receives — name, description, inputSchema,
# annotations, _meta.
#
# Usage:
#   scripts/measure-tools-list.sh                     # total + tool count
#   scripts/measure-tools-list.sh skein_theme         # + weight of one group
#   scripts/measure-tools-list.sh skein_user skein_theme
#
# Each extra argument is matched against tool names as an exact name OR a
# `<arg>_` prefix, so `skein_user` sums skein_user today and would have
# summed skein_user_create / _list / _update before the fold — which is
# what makes a before/after comparison of one group possible from one
# script.
set -euo pipefail

src_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../src" && pwd)"

out="$(cd "$src_dir" && SKEIN_MEASURE_TOOLS=1 go test ./cmd/api \
	-run '^TestMeasureToolsList$' -count=1 -v 2>&1)" || {
	printf '%s\n' "$out" >&2
	echo "measure-tools-list: go test failed" >&2
	exit 1
}

rows="$(printf '%s\n' "$out" | grep '^MEASURE_TOOL ' || true)"
totals="$(printf '%s\n' "$out" | grep '^MEASURE_TOTAL ' || true)"
if [ -z "$totals" ]; then
	printf '%s\n' "$out" >&2
	echo "measure-tools-list: no measurement in output (test skipped?)" >&2
	exit 1
fi

total_chars="$(printf '%s\n' "$totals" | awk '{print $2}')"
tool_count="$(printf '%s\n' "$totals" | awk '{print $3}')"
echo "tools/list total: $total_chars chars over $tool_count tools"

for prefix in "$@"; do
	printf '%s\n' "$rows" | awk -v p="$prefix" '
		$3 == p || index($3, p "_") == 1 { sum += $2; n += 1 }
		END { printf "  %-24s %8d chars over %d tools\n", p, sum, n }
	'
done
