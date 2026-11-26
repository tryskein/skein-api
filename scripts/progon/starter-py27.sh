#!/bin/sh
# The monitor starter under Python 2.7 (R-9): its text must parse there, so an
# old interpreter gets the stop line `config_invalid`, not a SyntaxError. Runs
# both modes of the starter (the call without `client` and with it) and
# prints TAP: one point per mode. A point is ok only when stdout is exactly
# one JSON line {event: "closed", reason: "config_invalid", action: "Loader
# download: ..."} (checked by Python 2.7's own JSON parser), stderr is empty
# and the exit code is 1. Any point not ok makes the script exit 1. Run from
# the repository root by the progon kind `py27`.
set -u
src=src/internal/mcp/tools/assets/monitor_starter.py
tmp=$(mktemp -d) || { echo "starter-py27: mktemp failed" >&2; exit 1; }
[ -n "$tmp" ] && [ -d "$tmp" ] || { echo "starter-py27: no temporary directory" >&2; exit 1; }
trap 'rm -rf "$tmp"' EXIT
check='import json, sys
lines = sys.stdin.read().splitlines()
assert len(lines) == 1, lines
d = json.loads(lines[0])
assert sorted(d) == ["action", "event", "reason"], d
assert d["event"] == "closed" and d["reason"] == "config_invalid", d
assert d["action"].startswith("Loader download: "), d'
version=$(python2.7 -V 2>&1)
echo "1..2"
failed=0
n=0
for mode in 0 1; do
  n=$((n + 1))
  name="starter (client=$mode) under $version prints config_invalid"
  if ! sed "s/__SKEIN_STARTER_CLIENT__/$mode/" "$src" > "$tmp/starter.py"; then
    echo "not ok $n - $name"
    echo "# the starter could not be written"
    failed=1
    continue
  fi
  python2.7 "$tmp/starter.py" > "$tmp/out" 2> "$tmp/err"
  rc=$?
  why=""
  [ "$rc" -eq 1 ] || why="exit $rc, want 1"
  [ -s "$tmp/err" ] && why="$why; stderr is not empty"
  python2.7 -c "$check" < "$tmp/out" 2> "$tmp/check" || why="$why; stdout is not the config_invalid line"
  if [ -z "$why" ]; then
    echo "ok $n - $name"
  else
    echo "not ok $n - $name"
    echo "# $why"
    sed 's/^/# stdout: /' "$tmp/out"
    sed 's/^/# stderr: /' "$tmp/err"
    sed 's/^/# check: /' "$tmp/check"
    failed=1
  fi
done
exit "$failed"
