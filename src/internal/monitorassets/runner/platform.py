# --- platform layer: the part shared by every platform ----------------------
# The runner body is one module joined from the parts of parts.txt; the
# platform parts come first so every later part can use them. This part holds
# what does not depend on the operating system — the refusal type, the user
# directory skein-monitor/ and the key of the one-runner lock — and picks the
# implementation of the current platform (PosixPlatform, WindowsPlatform).
#
# Standard library of Python 3.6 only: the runner runs on the agent's machine
# with whatever python3 it has.

import hashlib
import os


class PlatformRefusal(Exception):
    """A file or directory the platform layer refuses to use, decided before
    any byte of its content is read. reason is one of:

      not_found       nothing at the path
      symlink         the path is a symbolic link (it is never followed)
      not_regular     a directory, device or pipe where a file is expected
      not_directory   a file where a directory is expected
      foreign_owner   owned by another user
      open_to_others  group/other permission bits where none are allowed
      exists          a directory that must be created new already exists
      unreadable      the system refused to open or read it
      lock_timeout    a lock another process held was not released in time
      no_user_dir     the user directory cannot be named (no HOME)
      unsupported     the platform layer is not implemented on this system
    """

    def __init__(self, reason, path):
        Exception.__init__(self, "%s: %s" % (reason, path))
        self.reason = reason
        self.path = path


class LockBusy(Exception):
    """The one-runner lock is held by another process (after the wait, if
    one was asked for)."""

    def __init__(self, path):
        Exception.__init__(self, "lock held by another process: %s" % path)
        self.path = path


class ProcessInfo(object):
    """What the platform knows of one process (P-8).

    start is an opaque value comparable for equality only: the same pid with
    another start is another process. tpgid is the foreground process group
    of the process's terminal (-1 without one); pgid is its own group.
    """

    __slots__ = ("pid", "ppid", "start", "pgid", "tpgid", "name")

    def __init__(self, pid, ppid, start, pgid, tpgid, name):
        self.pid = pid
        self.ppid = ppid
        self.start = start
        self.pgid = pgid
        self.tpgid = tpgid
        self.name = name

    def foreground(self):
        """True when the process's group is the foreground group of its
        terminal."""
        return self.tpgid > 0 and self.tpgid == self.pgid


# The name of the user directory of the monitor (P-5).
USER_DIR_NAME = "skein-monitor"
# The subdirectory of the one-runner lock files.
LOCKS_DIR_NAME = "locks"


def current_platform():
    """The implementation of the platform layer for this system."""
    if os.name == "posix":
        return PosixPlatform
    return WindowsPlatform


def user_dir(env=None):
    """The user directory skein-monitor/ (P-5): ${XDG_STATE_HOME:-~/.local/state}
    on POSIX, %LOCALAPPDATA% on Windows. A relative XDG_STATE_HOME is ignored,
    as the XDG base directory rules require. Raises PlatformRefusal
    (no_user_dir) when no base can be named; never falls back to a shared
    location."""
    env = os.environ if env is None else env
    if os.name == "posix":
        base = env.get("XDG_STATE_HOME", "")
        if not os.path.isabs(base):
            home = env.get("HOME", "")
            if not os.path.isabs(home):
                raise PlatformRefusal("no_user_dir", "HOME")
            base = os.path.join(home, ".local", "state")
    else:
        base = env.get("LOCALAPPDATA", "")
        if not os.path.isabs(base):
            raise PlatformRefusal("no_user_dir", "LOCALAPPDATA")
    return os.path.join(base, USER_DIR_NAME)


def runner_lock_path(token_path, env=None):
    """The stable file of the one-runner lock of a token file (P-5):
    skein-monitor/locks/<sha256 of the absolute token path>.lock. It lies
    outside the token directory, so removing that directory leaves the lock
    in place, and it is keyed by the token path, so two tokens never share
    one lock."""
    key = hashlib.sha256(os.path.abspath(token_path).encode("utf-8", "surrogateescape")).hexdigest()
    return os.path.join(user_dir(env), LOCKS_DIR_NAME, key + ".lock")
