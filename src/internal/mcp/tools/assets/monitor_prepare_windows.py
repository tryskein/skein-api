# Skein monitor `prepare` for a call with `client` on Windows (R-10, US-11).
# Run as `py -3 -c` in Git Bash and on the standard input of `py -3 -` in
# PowerShell. It checks the Python version first (before anything is
# created), then runs the starter in prepare mode: the starter downloads the
# loader and checks its SHA-256 as on every start, and the loader creates
# the token directory, checks the whole path before the token is written
# ("До записи токена"), writes starter.py byte for byte from the text this
# script hands it and prints the ready line. The code of those checks lives
# in the loader, not in the response (SC-009). A refusal of the starter here
# is a prepare_failed line with its reason. The mode and the starter text go
# to the loader in the environment of this process only. Its text parses
# under Python 2.7, so an old interpreter gets the refusal line.
import json
import os
import sys

_STARTER = __SKEIN_STARTER__

if sys.version_info < (3, 12, 4):
    sys.stdout.write(json.dumps({"event": "prepare_failed", "reason": "config_invalid",
                                 "action": "Python 3.12.4 or newer is needed. Install it, then run `prepare` "
                                           "again."}, separators=(",", ":")) + "\n")
    sys.exit(1)
os.environ["SKEIN_MONITOR_MODE"] = "prepare"
os.environ["SKEIN_PREPARE_STARTER"] = _STARTER
exec(compile(_STARTER, "starter.py", "exec"), {"__name__": "__main__", "E": "prepare_failed"})
