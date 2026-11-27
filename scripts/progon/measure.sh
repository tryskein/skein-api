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
#                registry and the general record), first_start_monitor_started
#                and restart_monitor_started (the monitor_started lines of
#                the first start by a preflight result and of a restart by
#                the same working command, on the behavioural fixture),
#                delivered (the stop line of an exit-on-event cycle on that
#                fixture), windows_gitbash_delta_max,
#                windows_pwsh_delta_max and windows_powershell_delta_max (the
#                largest windows - posix growth of one record's response with
#                client, over every record whose commands are written for
#                that shell, the general record for powershell; no record of
#                the registry names pwsh, so its row is measured on a copy of
#                the registry with one record that does)
# Rows of SC-009 and SC-012 their owner tasks have not filled yet are listed
# as skipped TAP points "awaiting <task>" (see `pending` below): the run
# names them, and a green run does not read as covering them. A task that
# fills a row moves it from `pending` to the required rows.
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
	PART:client_posix_max PART:first_start_monitor_started PART:restart_monitor_started \
	PART:delivered PART:windows_gitbash_delta_max PART:windows_pwsh_delta_max \
	PART:windows_powershell_delta_max; do
	kind="MEASURE_${point%%:*}"
	label="$tool.${point#*:}"
	if ! printf '%s\n' "$rows" | awk -v k="$kind" -v l="$label" '$1 == k && $NF == l { found = 1 } END { exit found ? 0 : 1 }'; then
		printf '%s\n' "$resp" >&2
		echo "measure: the row $kind $label was not measured" >&2
		exit 1
	fi
done

# Named rows not measured yet, with the task that fills each (none now).
pending=""
skipped=""
for point in $pending; do
	kind="MEASURE_${point%%:*}"
	rest="${point#*:}"
	label="$tool.${rest%%:*}"
	owner="${rest#*:}"
	if printf '%s\n' "$rows" | awk -v k="$kind" -v l="$label" '$1 == k && $NF == l { found = 1 } END { exit found ? 0 : 1 }'; then
		echo "measure: $kind $label is measured: move it from pending to the required rows" >&2
		exit 1
	fi
	skipped="$skipped$kind $label # SKIP awaiting $owner
"
done

printf '%s\n%s\n%s' "$listed" "$rows" "$skipped" | awk 'NF { n++; $1 = $1; print "ok " n " - " $0 } END { print "1.." n }'
