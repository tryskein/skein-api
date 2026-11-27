# Skein monitor `prepare` for a call with `client`. Run as `python3 -c` on
# POSIX (in Git Bash `py -3 -c`, in PowerShell on the standard input);
# SKEIN_TOKEN_DIR names the token directory. It checks the Python version
# first (before anything is created), creates the directory
# new with mode 0700 (an existing one is refused, so nobody else's directory
# is adopted), checks that it is owned by the caller and private, writes the
# starter into it atomically (a temporary file renamed to starter.py) and
# prints one JSON line: the short ready line on success, a refusal with its
# reason and action otherwise. The token is written only after the ready
# line. Its text parses under Python 2.7, so an old interpreter gets the
# refusal line, not a SyntaxError. No line repeats a path of the response.
import json
import os
import stat
import sys

_STARTER = __SKEIN_STARTER__
_MINT = " Mint a new monitor with skein_get_monitor_script and the same parameters."
_FIX = " Fix the cause, then mint a new monitor with skein_get_monitor_script and the same parameters."


def _say(obj):
    sys.stdout.write(json.dumps(obj, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def _refuse(reason, action):
    _say({"event": "prepare_failed", "reason": reason, "action": action})
    sys.exit(1)


# The lowest Python of a monitor with `client` (R-9): 3.12.4 on Windows,
# 3.6 elsewhere.
_MIN = (3, 12, 4) if sys.platform == "win32" else (3, 6)


def main():
    if sys.version_info < _MIN:
        _refuse("config_invalid", "Python %s or newer is needed. Install it, then run `prepare` again."
                % ".".join(map(str, _MIN)))
    token_dir = os.environ.get("SKEIN_TOKEN_DIR", "")
    if not os.path.isabs(token_dir):
        _refuse("misconfigured", "The prepare command is broken (no absolute token directory)." + _MINT)
    try:
        os.mkdir(token_dir, 0o700)
    except FileExistsError:
        _refuse("token_dir_exists", "The token directory already exists, so it is not used." + _MINT)
    except OSError as exc:
        _refuse("token_dir_failed", "The token directory could not be created (%s)." % exc.strerror + _FIX)
    try:
        st = os.lstat(token_dir)
    except OSError as exc:
        _refuse("token_dir_failed", "The token directory could not be checked (%s)." % exc.strerror + _FIX)
    if (not stat.S_ISDIR(st.st_mode) or st.st_uid != os.getuid()
            or stat.S_IMODE(st.st_mode) & 0o077):
        _refuse("token_dir_insecure", "The token directory is not a private directory of yours." + _MINT)
    tmp = os.path.join(token_dir, ".starter.py.tmp")
    try:
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w") as fh:
            fh.write(_STARTER)
            fh.flush()
            os.fsync(fh.fileno())
        os.rename(tmp, os.path.join(token_dir, "starter.py"))
    except OSError as exc:
        try:
            os.remove(tmp)
        except OSError:
            pass
        _refuse("starter_write_failed",
                "The starter could not be written into the token directory (%s)." % exc.strerror + _FIX)
    _say({"event": "prepare_ready"})


main()
