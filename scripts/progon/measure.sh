#!/usr/bin/env bash
#
# measure.sh — the progon kind `measure` (.progon/api.toml): runs the two
# committed measurement scripts on a test VM and prints their figures as
# MEASURE_* lines, each as one TAP point (`ok N - MEASURE_…`), so a green run
# lists the figures as the names of its tests (`progon show <run>`).
#
#   MEASURE_TOOLS_LIST_TOTAL <chars> <tools>       whole tools/list payload
#   MEASURE_TOOL_DESCRIPTOR <tool> <chars> <tools> descriptor of one tool
#   MEASURE_RESP / MEASURE_PART <bytes> bytes <runes> runes <label>
#                                                  tool responses, as
#                                                  scripts/measure-tool-responses.sh
#                                                  prints them
#
# The kind provides a migrated Postgres in TEST_DATABASE_URL, which the
# response measurement needs. Any script failure, or a measurement without
# a skein_get_monitor_script row, fails the run.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tool="skein_get_monitor_script"

list="$("$root/scripts/measure-tools-list.sh" "$tool")"
listed="$(printf '%s\n' "$list" | awk -v tool="$tool" '
	/^tools\/list total:/ { print "MEASURE_TOOLS_LIST_TOTAL", $3, $6 }
	$1 == tool            { print "MEASURE_TOOL_DESCRIPTOR", $1, $2, $5; if ($5 + 0 > 0) found = 1 }
	END                   { exit found ? 0 : 1 }
')" || {
	printf '%s\n' "$list" >&2
	echo "measure: tools/list has no $tool descriptor" >&2
	exit 1
}

resp="$("$root/scripts/measure-tool-responses.sh")"
rows="$(printf '%s\n' "$resp" | grep -E '^MEASURE_(RESP|PART) ' || true)"
if ! printf '%s\n' "$rows" | awk -v p="$tool." '{ if (index($NF, p) == 1) found = 1 } END { exit found ? 0 : 1 }'; then
	printf '%s\n' "$resp" >&2
	echo "measure: no $tool response row was measured" >&2
	exit 1
fi

printf '%s\n%s\n' "$listed" "$rows" | awk 'NF { n++; $1 = $1; print "ok " n " - " $0 } END { print "1.." n }'
