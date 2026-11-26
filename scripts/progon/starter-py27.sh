#!/bin/sh
# The monitor starter under Python 2.7 (R-9): its text must parse there, so an
# old interpreter gets the stop line `config_invalid`, not a SyntaxError. Runs
# both modes of the starter (the call without `client` and with it) and
# prints TAP: one point per mode. Run from the repository root by the progon
# kind `py27`.
set -u
src=src/internal/mcp/tools/assets/monitor_starter.py
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "1..2"
n=0
for mode in 0 1; do
  n=$((n + 1))
  name="starter (client=$mode) under $(python2.7 -V 2>&1) prints config_invalid"
  sed "s/__SKEIN_STARTER_CLIENT__/$mode/" "$src" > "$tmp/starter.py"
  out=$(python2.7 "$tmp/starter.py" 2>&1)
  rc=$?
  lines=$(printf '%s\n' "$out" | wc -l)
  case "$out" in
    *Traceback*|*Error*) ok=no ;;
    '{'*'"reason":"config_invalid"'*) ok=yes ;;
    *) ok=no ;;
  esac
  case "$out" in
    *'"event":"closed"'*) ;;
    *) ok=no ;;
  esac
  if [ "$ok" = yes ] && [ "$rc" -eq 1 ] && [ "$lines" -eq 1 ]; then
    echo "ok $n - $name"
  else
    echo "not ok $n - $name"
    echo "# exit $rc, $lines line(s):"
    printf '%s\n' "$out" | sed 's/^/# /'
  fi
done
