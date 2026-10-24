#!/usr/bin/env bash
#
# measure-tool-responses.sh — size of the SERIALIZED TOOL RESPONSES on the
# objects sk-282's success criteria are stated on (SC-001, SC-002, SC-006,
# SC-007, SC-008, SC-009, SC-015).
#
# Sibling of measure-tools-list.sh, and it exists for the same reason: the
# waves of sk-282 each have to state what they cost, and two waves that
# each roll their own throwaway script produce numbers that cannot be put
# in one series. One committed command → the deltas add up.
#
# The two scripts measure different things and are not interchangeable:
#   measure-tools-list.sh      — what the client is TOLD about the tools
#                                (the tools/list descriptors).
#   measure-tool-responses.sh  — what the client is SENT when it calls
#                                them (the response payloads).
#
# Metric: len of the tool result's text content — the JSON a real client
# receives — reported in bytes and in runes. MEASURE_RESP rows are whole
# responses; MEASURE_PART rows are the sub-objects an SC threshold is
# stated on (one list row, one dependency edge).
#
# PRECONDITION — a migrated Postgres, addressed by TEST_DATABASE_URL. The
# measurement calls real tools against real rows, so unlike
# measure-tools-list.sh it cannot run without a database:
#
#   export TEST_DATABASE_URL='postgres://skein:qwerty12@…/skein?sslmode=disable'
#   scripts/measure-tool-responses.sh
#
# Under the throwaway harness (scripts/integration-test.sh in the
# workspace root) the database is created for you but the env gate below
# is not forwarded into the runner container; drive compose directly in
# that case:
#
#   C=(docker compose -f docker-compose.test.yml -p skein-itest)
#   "${C[@]}" up -d postgres
#   "${C[@]}" run --rm -T migrate
#   cid="$("${C[@]}" run -d -e SKEIN_MEASURE_RESPONSES=1 test-runner \
#     sh -c 'go test -v -count=1 -run TestMeasureToolResponses ./cmd/api/...' | tail -1)"
#   docker logs -f "$cid"; docker wait "$cid"; docker rm -f "$cid"
#   "${C[@]}" down -v
#
# `run -d` + `docker logs`, not `run --rm -T`: compose drops the container's
# stdout whenever the command outlives the attach, so the obvious form runs
# the measurement and prints NOTHING while exiting 0 — the same "plausible
# nothing" this script's own exit codes exist to prevent. Same workaround,
# same reason, as scripts/integration-test.sh (see its comment).
#
# Usage:
#   scripts/measure-tool-responses.sh              # every measured row
#   scripts/measure-tool-responses.sh skein_show   # only rows for one tool
#   scripts/measure-tool-responses.sh -h           # this usage
#
# Each extra argument filters the printed rows by label prefix. A row is
# printed once even when several prefixes match it.
#
# Exit codes:
#   0  rows were measured and printed
#   1  the measurement produced nothing, or a filter matched nothing —
#      see «Why the failures are loud» below
#   2  usage error, or a precondition is missing (binary, TEST_DATABASE_URL)
#
# Why the failures are loud
# -------------------------
# The numbers this prints are the acceptance baseline for SC-001…SC-015:
# T030 compares "after" against them. A measuring instrument that prints a
# plausible number without having measured anything is worse than one that
# does not run, because the plausible number goes into the comparison and
# nobody re-checks it. So: zero measured rows is a failure, and a filter
# that matches nothing is a failure that names the filter — never a silent
# fallback to the unfiltered total, which reads exactly like a successful
# narrow measurement.
set -euo pipefail

usage() {
	sed -n '2,/^set -euo pipefail$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; $d'
}

# Arguments first, and `-h` is not a filter prefix. It used to be: the
# script had no option handling at all, so `-h` fell through as a label
# prefix, matched nothing, and printed the global total with exit 0.
filters=()
for arg in "$@"; do
	case "$arg" in
	-h | --help)
		usage
		exit 0
		;;
	-*)
		echo "measure-tool-responses: unknown option: $arg" >&2
		usage >&2
		exit 2
		;;
	*) filters+=("$arg") ;;
	esac
done

# Preflight EVERY binary the run needs, before the expensive part. `go` was
# checked by failing to run it; `grep` and `awk` were not checked at all,
# so a missing one surfaced as `command not found` AFTER the test had
# already paid for a seeded database.
for bin in go grep awk; do
	if ! command -v "$bin" >/dev/null 2>&1; then
		echo "measure-tool-responses: $bin is required but not on PATH" >&2
		exit 2
	fi
done

if [ -z "${TEST_DATABASE_URL:-}" ]; then
	echo "measure-tool-responses: TEST_DATABASE_URL is not set — the measurement calls real tools against real rows" >&2
	exit 2
fi

src_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../src" && pwd)"

out="$(cd "$src_dir" && SKEIN_MEASURE_RESPONSES=1 go test ./cmd/api \
	-run '^TestMeasureToolResponses$' -count=1 -v 2>&1)" || {
	printf '%s\n' "$out" >&2
	echo "measure-tool-responses: go test failed" >&2
	exit 1
}

rows="$(printf '%s\n' "$out" | grep -E '^MEASURE_(RESP|PART) ' || true)"
total="$(printf '%s\n' "$out" | grep '^MEASURE_RESP_TOTAL ' || true)"
if [ -z "$total" ]; then
	printf '%s\n' "$out" >&2
	echo "measure-tool-responses: no measurement in output (test skipped?)" >&2
	exit 1
fi

# A total with no rows behind it is the empty measurement dressed as a
# result: `total: 0 bytes over 0 measured rows` is a well-formed line that
# says nothing was measured, and it used to exit 0.
total_rows="$(printf '%s\n' "$total" | awk '{ print $3 + 0 }')"
if [ -z "$rows" ] || [ "$total_rows" -eq 0 ]; then
	printf '%s\n' "$out" >&2
	echo "measure-tool-responses: the test ran but measured 0 rows — there is no baseline here, only a well-formed empty one" >&2
	exit 1
fi

if [ "${#filters[@]}" -gt 0 ]; then
	# One pass over all prefixes, and `next` after the first hit: two
	# overlapping prefixes (`skein_list` and `skein_list.cross`) used to
	# print the same row twice and double it in any total taken off this
	# output.
	matched="$(printf '%s\n' "$rows" | awk -v plist="$(printf '%s\n' "${filters[@]}")" '
		BEGIN { n = split(plist, p, "\n") }
		{
			for (i = 1; i <= n; i++) {
				if (p[i] != "" && index($4, p[i]) == 1) { print; next }
			}
		}')"
	if [ -z "$matched" ]; then
		echo "measure-tool-responses: no measured row label starts with any of: ${filters[*]}" >&2
		echo "measure-tool-responses: measured labels are:" >&2
		printf '%s\n' "$rows" | awk '{ print "  " $4 }' >&2
		exit 1
	fi
	printf '%s\n' "$matched" | awk '{ printf "%-12s %8d bytes %8d runes  %s\n", $1, $2, $3, $4 }'
	# The total is of the MATCHED rows. Printing the global
	# MEASURE_RESP_TOTAL under a filter answers a question nobody asked and
	# reads like the filtered figure.
	printf '%s\n' "$matched" | awk -v f="${filters[*]}" '
		{ bytes += $2; n++ }
		END { printf "total: %d bytes over %d rows matching: %s\n", bytes, n, f }'
	exit 0
fi

printf '%s\n' "$rows" | awk '{ printf "%-12s %8d bytes %8d runes  %s\n", $1, $2, $3, $4 }'
printf '%s\n' "$total" | awk '{ printf "total: %d bytes over %d measured rows\n", $2, $3 }'
