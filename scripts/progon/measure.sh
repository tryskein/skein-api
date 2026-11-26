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
# response measurement needs, and python3, which runs `prepare` and the
# runner's working command. Any script failure, a tools/list descriptor group
# other than exactly the one tool, or a missing named row fails the run. The
# named rows of SC-009 and SC-012 (the early slice of R-10):
#   MEASURE_RESP no_client, client_general_posix, client_fixture3_posix,
#                client_general_real_posix (the general record on the
#                registry the server embeds)
#   MEASURE_PART starter (source the bootstrap carries), prepare_ready (the
#                ready line of `prepare`), working_command (`command` of the
#                preflight line), client_posix_max (the largest response
#                with `client` on POSIX over every record of the real
#                registry and the general record)
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tool="skein_get_monitor_script"

list="$("$root/scripts/measure-tools-list.sh" "$tool")"
listed="$(printf '%s\n' "$list" | awk -v tool="$tool" '
	/^tools\/list total:/ { print "MEASURE_TOOLS_LIST_TOTAL", $3, $6 }
	$1 == tool            { print "MEASURE_TOOL_DESCRIPTOR", $1, $2, $5; if ($5 + 0 == 1) found = 1 }
	END                   { exit found ? 0 : 1 }
')" || {
	printf '%s\n' "$list" >&2
	echo "measure: tools/list has no descriptor of exactly one tool named $tool" >&2
	exit 1
}

resp="$("$root/scripts/measure-tool-responses.sh")"
rows="$(printf '%s\n' "$resp" | grep -E '^MEASURE_(RESP|PART) ' || true)"
# Every named point of SC-009 and SC-012 must be a row of its own.
for point in RESP:no_client RESP:client_general_posix RESP:client_fixture3_posix \
	RESP:client_general_real_posix PART:starter PART:prepare_ready PART:working_command \
	PART:client_posix_max; do
	kind="MEASURE_${point%%:*}"
	label="$tool.${point#*:}"
	if ! printf '%s\n' "$rows" | awk -v k="$kind" -v l="$label" '$1 == k && $NF == l { found = 1 } END { exit found ? 0 : 1 }'; then
		printf '%s\n' "$resp" >&2
		echo "measure: the row $kind $label was not measured" >&2
		exit 1
	fi
done

printf '%s\n%s\n' "$listed" "$rows" | awk 'NF { n++; $1 = $1; print "ok " n " - " $0 } END { print "1.." n }'
