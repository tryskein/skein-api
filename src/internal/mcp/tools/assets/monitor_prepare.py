# Skein monitor `prepare` for a call with `client` on POSIX. Run as
# `python3 -c`; SKEIN_TOKEN_DIR names the token directory. It creates the
# directory new with mode 0700 (an existing one is refused, so nobody else's
# directory is adopted), checks that it is owned by the caller and private,
# writes the client-mode loader into it and prints one JSON line: the ready
# line on success, a refusal with its reason and action otherwise. The token
# is written only after the ready line.
import json
import os
import stat
import sys

_LOADER = __SKEIN_CLIENT_LOADER__


def _say(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


def _refuse(reason, action):
    _say({"event": "prepare_failed", "reason": reason, "action": action})
    sys.exit(1)


def main():
    token_dir = os.environ.get("SKEIN_TOKEN_DIR", "")
    if not os.path.isabs(token_dir):
        _refuse("misconfigured", "The prepare command is broken (no absolute token directory). "
                "Mint a new monitor with skein_get_monitor_script.")
    try:
        os.mkdir(token_dir, 0o700)
    except FileExistsError:
        _refuse("token_dir_exists", "The token directory %s already exists, so it is not used. "
                "Mint a new monitor with skein_get_monitor_script." % token_dir)
    except OSError as exc:
        _refuse("token_dir_failed", "The token directory %s could not be created (%s). Fix the "
                "cause, then mint a new monitor with skein_get_monitor_script." % (token_dir, exc.strerror))
    st = os.lstat(token_dir)
    if (not stat.S_ISDIR(st.st_mode) or st.st_uid != os.getuid()
            or stat.S_IMODE(st.st_mode) & 0o077):
        _refuse("token_dir_insecure", "The token directory %s is not a private directory of yours. "
                "Mint a new monitor with skein_get_monitor_script." % token_dir)
    loader_path = os.path.join(token_dir, "loader.py")
    try:
        fd = os.open(loader_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w") as fh:
            fh.write(_LOADER)
            fh.flush()
            os.fsync(fh.fileno())
    except OSError as exc:
        _refuse("loader_write_failed", "The loader could not be written into %s (%s). Fix the cause, "
                "then mint a new monitor with skein_get_monitor_script." % (token_dir, exc.strerror))
    _say({"event": "prepare_ready", "tokenDir": token_dir, "loader": loader_path})


main()
