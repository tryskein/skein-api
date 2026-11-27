# --- stops: the stop protocol and the token file (sk-579 US-3) --------------
# The one stop line of every controlled stop, its table of reasons, the
# token-file checks and the fate of the token file. No part of it depends on
# the values the serving handler templates, so the client branch of the
# loader runs it too (monitorassets/loader_client): a loader that cannot
# download the runner ends the monitor by the same rules.

import json
import os
import random
import signal
import stat
import sys

def emit(obj):
    """Write one JSON object as a single flushed stdout line (the Monitor contract)."""
    text = json.dumps(obj, separators=(",", ":"))
    sys.stdout.write(text + "\n")
    sys.stdout.flush()


def log_stderr(msg):
    """Diagnostics go to stderr so they never pollute the stdout line protocol."""
    sys.stderr.write(msg + "\n")
    sys.stderr.flush()


def jitter(base):
    """Full-jitter: a random point in [base/2, base] to de-sync a fleet of runners."""
    if base <= 0:
        return 0.0
    return base / 2.0 + random.random() * (base / 2.0)


def open_token(path):
    """Open the token file, take the one-runner lock, read the token.

    Order is open → check → lock → read (sk-579 INV-13): a missing file is
    reported as missing, never as somebody else's lock, and a file others can
    read is refused before its token is read. Raises Stop for every outcome
    that ends the runner; returns (fd, token) otherwise. The fd stays open for
    the life of the process — it holds the lock, which the kernel releases on
    any death of the process, SIGKILL included.
    """
    fd = open_token_checked(path)
    # The legacy runner keeps its behaviour off POSIX (US-11, INV-7): no
    # lock on the token file there.
    if os.name != "posix" or not current_platform().has_file_locks():
        log_stderr("monitor runner: this platform has no OS file locks; running without the "
                   "one-runner lock, so a second runner on this token file is not prevented")
    else:
        try:
            current_platform().lock_fd(fd, blocking=False)
        except BlockingIOError:
            os.close(fd)
            raise Stop("already_running")
        except OSError as exc:
            # The lock guards against a duplicate; it is not a condition of
            # running (same decision as a platform without fcntl): say so
            # once and run without it.
            log_stderr("monitor runner: cannot lock the token file (%s); running without the "
                       "one-runner lock, so a second runner on this token file is not prevented" % exc)
    try:
        chunks = []
        while True:
            chunk = os.read(fd, 65536)
            if not chunk:
                break
            chunks.append(chunk)
        token = b"".join(chunks).decode("utf-8").strip()
    except (OSError, UnicodeDecodeError):
        os.close(fd)
        raise Stop("token_file_missing")
    if not token:
        os.close(fd)
        raise Stop("token_file_missing", delete_token=True)
    return fd, token


def open_token_checked(path):
    """Open the token file and check what was opened: a regular file no one
    else can read. Returns the descriptor, nothing read yet; raises Stop."""
    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    except FileNotFoundError:
        # The token is gone: its state and its empty directory go too.
        raise Stop("token_file_missing", delete_token=True)
    except OSError:
        # Unreadable (a symlink, no permission): not ours to touch.
        raise Stop("token_file_missing")
    try:
        st = os.fstat(fd)
    except OSError:
        os.close(fd)
        raise Stop("token_file_missing")
    if not stat.S_ISREG(st.st_mode):
        # A directory or a device: not ours to touch.
        os.close(fd)
        raise Stop("token_file_missing")
    if not token_file_private(path, st):
        os.close(fd)
        # The token is burnt: delete our readable copy so it does not
        # outlive the refusal on disk. Another user's file is not ours to touch.
        if os.name == "posix" and current_platform().owns(st):
            remove_token_file(path, opened=st)
        raise Stop("token_file_insecure")
    return fd


def token_file_private(path, st):
    """True when no other user can read the token file of stat st: it is
    ours and either has no group/other bits or sits in a directory of ours
    that has none. Platforms without POSIX owners (Windows) are not checked."""
    if os.name != "posix":
        return True
    owns = current_platform().owns
    if not owns(st):
        return False
    if not st.st_mode & 0o077:
        return True
    try:
        parent = os.lstat(os.path.dirname(os.path.abspath(path)))
    except OSError:
        return False
    return stat.S_ISDIR(parent.st_mode) and owns(parent) and not parent.st_mode & 0o077


def remove_token_file(path, opened=None):
    """Delete the token file with its state file, and its directory when it
    is the mint's own skein-mon-* directory and nothing else is left in it.
    With opened (the fstat of the opened file), only that same file is
    deleted: a path that now names another inode is left alone, and so is
    the state next to it. A token file already gone still takes its state."""
    try:
        if opened is not None:
            cur = os.lstat(path)
            if (cur.st_ino, cur.st_dev) != (opened.st_ino, opened.st_dev):
                return
        os.remove(path)
    except FileNotFoundError:
        pass
    except OSError:
        return
    remove_state_file(path)
    parent = os.path.dirname(os.path.abspath(path))
    if os.path.basename(parent).startswith("skein-mon-"):
        try:
            os.rmdir(parent)
        except OSError:
            pass


# --- stop protocol (sk-579 US-3) ---------------------------------------------
# Every controlled stop prints exactly ONE last stdout line
# {"event", "reason", "action"[, "status"]}; "action" is a finished instruction
# to the agent with the root issue id filled in. The table below is the single
# place a reason's event, exit code and token-file fate are decided (the
# columns of the US-3 table). Diagnostics, tracebacks included, go to stderr.
#
# reason              event     exit  token file
# token_expired       closed    0     deleted
# visibility_lost     closed    0     deleted
# token_file_missing  closed    1     missing stays missing (its state is
#                                     deleted); empty → deleted;
#                                     unreadable → not touched
# token_file_insecure closed    1     deleted when ours; another user's not touched
# misconfigured       closed    1     deleted when its path is known
# stopped             paused    0     kept
# internal_error      paused    1     kept
# root_closed         closed    0     deleted
# already_running     duplicate 0     not touched
# config_invalid      closed    1     kept (client mode: a setting, directory
#                                     or the Python version to fix)
# delivered           paused    0     kept (client mode, exit-on-event: the
#                                     batch is out; run the working command
#                                     of the preflight again)
STOPS = {
    "token_expired": ("closed", 0),
    "visibility_lost": ("closed", 0),
    "token_file_missing": ("closed", 1),
    "token_file_insecure": ("closed", 1),
    "misconfigured": ("closed", 1),
    "stopped": ("paused", 0),
    "internal_error": ("paused", 1),
    "root_closed": ("closed", 0),
    "already_running": ("duplicate", 0),
    "config_invalid": ("closed", 1),
    "delivered": ("paused", 0),
    "addressee_gone": ("closed", 0),
}


def mint_call(issue, env=None):
    """The call that mints a new monitor for issue with the parameters of
    this one (US-12): in client mode subtree and eventTypes when the start
    variables carry them (SKEIN_SUBTREE, SKEIN_EVENT_TYPES: set by the
    command, empty for the mint's default), client and platform, and tokenDir on Windows (the
    directory the token directory was made in). Without `client` the start
    variables are not read (INV-7): the issue id alone."""
    args = ["id=%s" % json.dumps(issue)]
    if LOADER_MODE != "client":
        return "skein_get_monitor_script(%s)" % ", ".join(args)
    env = os.environ if env is None else env
    if env.get("SKEIN_SUBTREE", "") == "1":
        args.append("subtree=true")
    types = [t for t in env.get("SKEIN_EVENT_TYPES", "").split(",") if t]
    if types:
        args.append("eventTypes=%s" % json.dumps(types))
    for name, key in (("client", "SKEIN_CLIENT"), ("platform", "SKEIN_PLATFORM")):
        if env.get(key, ""):
            args.append("%s=%s" % (name, json.dumps(env[key])))
    token_file = env.get("SKEIN_TOKEN_FILE", "")
    if env.get("SKEIN_PLATFORM", "") == "windows" and token_file:
        token_dir = os.path.dirname(os.path.dirname(os.path.abspath(token_file)))
        args.append("tokenDir=%s" % json.dumps(token_dir))
    return "skein_get_monitor_script(%s)" % ", ".join(args)


def stop_action(reason, issue, detail=None, preflight=None):
    """The action of a stop line. preflight is the number of the result
    whose working command resumes a client-mode monitor: the action names
    it, never repeats the command (INV-17)."""
    mint = mint_call(issue)
    if LOADER_MODE == "client" and os.environ.get("SKEIN_MONITOR_MODE") == "preflight" and \
            reason in ("stopped", "internal_error"):
        # The preflight stopped before its line: it is what runs again (no
        # working command of it exists yet).
        return ("The preflight of the monitor for issue %s %s. Run the same preflight command again." % (
            issue, "was stopped by a signal" if reason == "stopped" else "hit an unexpected error"))
    if preflight is not None and reason in ("stopped", "internal_error"):
        return ("The monitor for issue %s %s. Run the working command of preflight %s again to resume "
                "it." % (issue, "was stopped by a signal" if reason == "stopped" else "hit an unexpected error",
                         preflight))
    if LOADER_MODE == "client" and reason in ("stopped", "internal_error"):
        # No result to name (its file is gone or unreadable): a call with
        # `client` has a working command, not a bootstrap.
        return ("The monitor for issue %s %s. Run the same working command again to resume it; if it "
                "fails again, mint a new monitor with %s." % (
                    issue, "was stopped by a signal" if reason == "stopped" else "hit an unexpected error",
                    mint))
    if reason == "addressee_gone":
        return ("The agent the monitor for issue %s delivered to has ended, so the monitor stopped. "
                "If the work goes on in another agent, mint a new monitor there with %s." % (issue, mint))
    if reason == "config_invalid":
        return ("The monitor for issue %s did not start: %s. Fix that, then run the preflight again and its "
                "working command; do not mint a new monitor: its token is kept." % (issue, detail or "a setting is invalid"))
    if reason == "delivered":
        # detail: the number of the preflight result whose working command
        # restarts the monitor; the command itself is not repeated (INV-17).
        return ("Handle the events above, then run the working command of preflight %s again." % detail)
    if reason == "token_expired":
        return ("The monitor for issue %s has ended: its token expired or was revoked. If you "
                "stopped it yourself, do nothing; otherwise mint a new monitor with %s." % (issue, mint))
    if reason == "visibility_lost":
        return ("The monitor for issue %s has ended because you no longer have access to its project. "
                "Do not mint it again until you regain access to that project; then mint a new "
                "monitor with %s." % (issue, mint))
    if reason == "token_file_missing":
        return ("The monitor for issue %s cannot run: its token file is missing, empty or unreadable. "
                "Mint a new monitor with %s." % (issue, mint))
    if reason == "token_file_insecure":
        return ("The token file of the monitor for issue %s can be read by other users, so the monitor "
                "did not start. Mint a new monitor with %s and run its `prepare` before writing the "
                "token." % (issue, mint))
    if reason == "misconfigured":
        return ("The bootstrap of the monitor for issue %s is broken (the server address or the token "
                "file path is not set). Mint a new monitor with %s." % (issue, mint))
    if reason == "root_closed" and isinstance(detail, dict):
        # A client-mode close: the summary of what was received since it.
        return ("Issue %s is %s. Process the delivered events and do not restart the monitor; if the "
                "issue is reopened and you still need to follow it, mint a new monitor with %s."
                % (issue, summary_text(detail), mint))
    if reason == "root_closed":
        return ("Issue %s is closed, so its monitor has finished after delivering everything it owed. "
                "Nothing to do; if the issue is reopened and you still need to follow it, mint a new "
                "monitor with %s." % (issue, mint))
    if reason == "already_running":
        return ("A runner is already monitoring issue %s with this token file, so this second one "
                "stopped. Do nothing: the monitor is already running." % issue)
    if reason == "stopped":
        return ("The monitor for issue %s was stopped by a signal. Run the same bootstrap again to "
                "resume from where it stopped; its token stays valid until the renewal window runs "
                "out." % issue)
    return ("The monitor for issue %s hit an unexpected error. Run the same bootstrap again; if it "
            "happens again, mint a new monitor with %s." % (issue, mint))


class Stop(Exception):
    """A controlled stop: reason (a STOPS key), optional HTTP status, and
    whether the token file is to be deleted."""

    def __init__(self, reason, status=None, delete_token=False, detail=None, preflight=None):
        global _stopping
        # Deciding a stop starts the final phase: from here on a signal no
        # longer raises, so it cannot replace this stop or print a second line.
        _stopping = True
        super().__init__(reason)
        self.reason = reason
        self.status = status
        self.delete_token = delete_token
        # What config_invalid names to fix (a variable, a directory).
        self.detail = detail
        # The number of the preflight result a client-mode action names.
        self.preflight = preflight


class StopSignal(BaseException):
    """Raised by the SIGTERM/SIGINT handler. A BaseException so no ordinary
    handler swallows it on the way out."""


# Set once the runner has started its final stop: a late signal must not
# start a second stop line. The handler sets it too, so it raises at most once.
_stopping = False


def _on_signal(signum, frame):
    global _stopping
    if not _stopping:
        _stopping = True
        raise StopSignal()


def finish(outcome, issue, token_file, write=None):
    """Apply the token-file fate of the stop, print its line (through write,
    when given: a stream monitor keeps it within its limits), return its
    code."""
    event, code = STOPS[outcome.reason]
    if outcome.delete_token and token_file:
        remove_token_file(token_file)
    line = {"event": event, "reason": outcome.reason,
            "action": stop_action(outcome.reason, issue, outcome.detail, outcome.preflight)}
    if outcome.status is not None:
        line["status"] = outcome.status
    (write or emit)(line)
    return code
