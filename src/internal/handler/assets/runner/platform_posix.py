# --- platform layer: POSIX (Linux, macOS) ----------------------------------
# Private directories, reading a file only after checking what was opened,
# atomic writes, the one-runner lock, process facts (P-8) and the start of a
# detached process (P-6). Every check is made on the opened object (fstat of
# the descriptor that is then read), never on a path that could name another
# file by the time it is read, and a symbolic link is never followed.

import errno
import os
import select
import stat
import subprocess
import sys
import threading
import time

try:
    import fcntl
except ImportError:  # not POSIX (Python on Windows): no OS file locks
    fcntl = None

# Permission bits of group and other.
_GROUP_OTHER = 0o077
# Tries on a lock file renewed under us before the wait decides.
_LOCK_RENEWED_TRIES = 3
# Step of the wait for a lock held by another process.
_LOCK_WAIT_STEP_SEC = 0.05


class RunnerLock(object):
    """A held one-runner lock. The lock file is never deleted on release: the
    next holder locks the same file."""

    def __init__(self, path, fd):
        self.path = path
        self._fd = fd

    def release(self):
        fd, self._fd = self._fd, None
        if fd is None:
            return
        try:
            fcntl.flock(fd, fcntl.LOCK_UN)
        finally:
            os.close(fd)


class PosixPlatform(object):
    """The POSIX implementation of the platform layer."""

    @staticmethod
    def make_private_dir(path):
        """Create path as a new directory of ours with mode 0700. An existing
        entry — directory, file or link — is refused (exists), never adopted."""
        try:
            os.mkdir(path, 0o700)
        except FileExistsError:
            raise PlatformRefusal("exists", path)
        except OSError:
            raise PlatformRefusal("unreadable", path)
        fd = _open_dir(path)
        try:
            # The umask may have dropped owner bits; the directory is new and
            # ours, so its mode is set outright.
            os.fchmod(fd, 0o700)
            _check_private_dir_fd(fd, path)
        finally:
            os.close(fd)

    @staticmethod
    def ensure_private_dir(path):
        """A directory of ours without group/other bits at path, created (with
        its missing parents, 0700) when absent. An existing one that is not a
        directory, a link, another user's or open to others is refused, not
        repaired."""
        try:
            os.makedirs(path, 0o700)
        except FileExistsError:
            pass
        except OSError:
            raise PlatformRefusal("unreadable", path)
        fd = _open_dir(path)
        try:
            _check_private_dir_fd(fd, path)
        finally:
            os.close(fd)

    @staticmethod
    def read_token(path):
        """The content of a token file, read only after the opened file passed
        the POSIX token predicate (P-5): a regular file of ours that either
        has no group/other bits or lies in a directory of ours without them —
        so a token written 0644 into the 0700 directory of `prepare` is
        accepted. The file is opened once, inside the directory whose
        descriptor is checked, read through that same descriptor and closed
        before return. Raises PlatformRefusal."""
        return _read_checked(path, _token_private)

    @staticmethod
    def read_own_file(path):
        """The content of a file the monitor writes itself (state, ledger,
        journal, messages): a regular file of ours without group/other bits in
        a directory of ours without them. Same discipline as read_token.
        Raises PlatformRefusal."""
        return _read_checked(path, _own_private)

    @staticmethod
    def atomic_write(path, data, dir_check=None):
        """Replace the file at path with data (bytes) atomically: a new 0600
        temporary file in the same directory, written and fsynced, then
        os.replace. Everything happens inside the opened directory, which
        must not be a link and, with dir_check, must pass dir_check(its
        fstat) (else PlatformRefusal). A failure at any point before the
        replace leaves the old file as it was and no temporary file behind."""
        directory, name = os.path.split(os.path.abspath(path))
        dir_fd = _open_dir(directory)
        try:
            if dir_check is not None and not dir_check(os.fstat(dir_fd)):
                raise PlatformRefusal("open_to_others", directory)
            tmp = ".%s.tmp-%d-%s" % (name, os.getpid(), os.urandom(6).hex())
            fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=dir_fd)
            try:
                try:
                    view = memoryview(data)
                    while view:
                        view = view[os.write(fd, view):]
                    os.fsync(fd)
                finally:
                    os.close(fd)
                os.replace(tmp, name, src_dir_fd=dir_fd, dst_dir_fd=dir_fd)
            except BaseException:
                try:
                    os.unlink(tmp, dir_fd=dir_fd)
                except OSError:
                    pass
                raise
            try:
                os.fsync(dir_fd)
            except OSError as exc:
                # A file system that cannot fsync a directory has nothing
                # more to give.
                if exc.errno != errno.EINVAL:
                    raise
        finally:
            os.close(dir_fd)

    @staticmethod
    def acquire_runner_lock(token_path, wait_sec=0, env=None):
        """Take the one-runner lock of a token file (P-5) on its stable file
        skein-monitor/locks/<key>.lock. wait_sec=0 does not wait; otherwise
        the lock held by another process is awaited up to wait_sec seconds.
        Returns a RunnerLock; raises LockBusy when still held, PlatformRefusal
        when the directories or the lock file are not private."""
        path = runner_lock_path(token_path, env)
        locks = os.path.dirname(path)
        PosixPlatform.ensure_private_dir(os.path.dirname(locks))
        PosixPlatform.ensure_private_dir(locks)
        if fcntl is None:
            raise PlatformRefusal("unsupported", path)
        deadline = time.monotonic() + max(wait_sec, 0)
        renewed = 0
        while True:
            try:
                fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | _O_NONBLOCK, 0o600)
            except OSError as exc:
                raise PlatformRefusal(_open_refusal(exc), path)
            try:
                st = os.fstat(fd)
                if not stat.S_ISREG(st.st_mode):
                    raise PlatformRefusal("not_regular", path)
                if st.st_uid != os.getuid():
                    raise PlatformRefusal("foreign_owner", path)
                if st.st_mode & _GROUP_OTHER:
                    raise PlatformRefusal("open_to_others", path)
                try:
                    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                except BlockingIOError:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        raise LockBusy(path)
                    os.close(fd)
                    fd = None
                    time.sleep(min(_LOCK_WAIT_STEP_SEC, remaining))
                    continue
                # The file may have been unlinked and recreated between our
                # open and the lock: a lock on a file the path no longer
                # names locks nothing, so start over.
                try:
                    cur = os.lstat(path)
                except FileNotFoundError:
                    cur = None
                if cur is None or (cur.st_dev, cur.st_ino) != (st.st_dev, st.st_ino):
                    os.close(fd)
                    fd = None
                    # A path that keeps being renewed is not waited on past
                    # the wait (and past a few tries when there is none).
                    renewed += 1
                    if renewed > _LOCK_RENEWED_TRIES and time.monotonic() >= deadline:
                        raise LockBusy(path)
                    continue
                lock, fd = RunnerLock(path, fd), None
                return lock
            finally:
                if fd is not None:
                    os.close(fd)

    @staticmethod
    def process_info(pid):
        """ProcessInfo of a live process; None when it is unknown: no such
        process, a zombie, or facts that cannot be read or parsed. Linux
        reads /proc/<pid>/stat and the boot id; macOS asks ps. Raises
        PlatformRefusal (unsupported) on any other system."""
        if sys.platform.startswith("linux"):
            return _linux_process_info(pid)
        if sys.platform == "darwin":
            return _darwin_process_info(pid)
        raise PlatformRefusal("unsupported", "process_info")

    @staticmethod
    def spawn_detached(argv, stdin_data, env, receipt_timeout_sec):
        """Start argv in a new session (P-6), hand it stdin_data (bytes) on
        its stdin and wait up to receipt_timeout_sec for its first stdout line,
        the start receipt. Returns (pid, receipt): receipt is the line without
        its newline, or None when the process closed stdout or was silent for
        the whole wait. The pipes of this process are closed on return; the
        child outlives it, and a daemon thread of this process reaps it when
        it ends first, so it never lingers as a zombie."""
        proc = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, env=env, close_fds=True,
                                start_new_session=True)
        threading.Thread(target=proc.wait, daemon=True).start()
        try:
            try:
                proc.stdin.write(stdin_data)
                proc.stdin.close()
            except BrokenPipeError:
                pass
            return proc.pid, _read_line(proc.stdout.fileno(), receipt_timeout_sec)
        finally:
            proc.stdout.close()


_O_NONBLOCK = getattr(os, "O_NONBLOCK", 0)


def _open_refusal(exc):
    """The refusal reason of a failed open with O_NOFOLLOW."""
    if exc.errno == errno.ENOENT:
        return "not_found"
    # Linux and macOS answer ELOOP for a link opened with O_NOFOLLOW, the BSDs
    # EMLINK.
    if exc.errno in (errno.ELOOP, errno.EMLINK):
        return "symlink"
    if exc.errno == errno.ENOTDIR:
        return "not_directory"
    return "unreadable"


def _open_dir(path):
    try:
        return os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    except OSError as exc:
        raise PlatformRefusal(_open_refusal(exc), path)


def _check_private_dir_fd(fd, path):
    st = os.fstat(fd)
    if not stat.S_ISDIR(st.st_mode):
        raise PlatformRefusal("not_directory", path)
    if st.st_uid != os.getuid():
        raise PlatformRefusal("foreign_owner", path)
    if st.st_mode & _GROUP_OTHER:
        raise PlatformRefusal("open_to_others", path)


def _token_private(file_st, dir_st):
    """The POSIX token predicate (P-5), one for the client loader and the
    runner: the file has no group/other bits, or its directory is ours and
    has none."""
    if not file_st.st_mode & _GROUP_OTHER:
        return True
    return dir_st.st_uid == os.getuid() and not dir_st.st_mode & _GROUP_OTHER


def _own_private(file_st, dir_st):
    """A file the monitor creates itself: 0600 in a 0700 directory of ours."""
    return (not file_st.st_mode & _GROUP_OTHER and dir_st.st_uid == os.getuid()
            and not dir_st.st_mode & _GROUP_OTHER)


def _read_checked(path, private):
    """Open path once (no link followed) inside its directory, check the
    opened file and the opened directory, then read that same descriptor."""
    directory, name = os.path.split(os.path.abspath(path))
    dir_fd = _open_dir(directory)
    try:
        try:
            fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | _O_NONBLOCK, dir_fd=dir_fd)
        except OSError as exc:
            raise PlatformRefusal(_open_refusal(exc), path)
        try:
            st = os.fstat(fd)
            if not stat.S_ISREG(st.st_mode):
                raise PlatformRefusal("not_regular", path)
            if st.st_uid != os.getuid():
                raise PlatformRefusal("foreign_owner", path)
            if not private(st, os.fstat(dir_fd)):
                raise PlatformRefusal("open_to_others", path)
            chunks = []
            try:
                while True:
                    chunk = os.read(fd, 65536)
                    if not chunk:
                        break
                    chunks.append(chunk)
            except OSError:
                raise PlatformRefusal("unreadable", path)
            return b"".join(chunks)
        finally:
            os.close(fd)
    finally:
        os.close(dir_fd)


def _read_line(fd, timeout_sec):
    """One line from fd within timeout_sec, without its newline; None on EOF
    or when the time runs out."""
    deadline = time.monotonic() + timeout_sec
    buf = b""
    while b"\n" not in buf:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            return None
        ready, _, _ = select.select([fd], [], [], remaining)
        if not ready:
            return None
        chunk = os.read(fd, 4096)
        if not chunk:
            return None
        buf += chunk
    return buf.split(b"\n", 1)[0].decode("utf-8", "replace")


def _linux_process_info(pid):
    try:
        with open("/proc/%d/stat" % pid, "rb") as fh:
            raw = fh.read().decode("utf-8", "replace")
        with open("/proc/sys/kernel/random/boot_id", "r") as fh:
            boot = fh.read().strip()
    except OSError:
        # Gone, or hidden from us (hidepid): not known.
        return None
    try:
        return _parse_linux_stat(pid, raw, boot)
    except (IndexError, ValueError):
        return None


def _parse_linux_stat(pid, raw, boot):
    # "pid (comm) state ppid pgrp session tty_nr tpgid ... starttime ...":
    # comm may hold spaces and parentheses, so it ends at the LAST ")".
    head, _, rest = raw.rpartition(")")
    name = head.partition("(")[2]
    fields = rest.split()
    if fields[0] == "Z":
        return None  # a zombie: it has ended
    # fields[0] is field 3 of proc(5) (state): ppid is field 4, pgrp 5,
    # tpgid 8, starttime 22.
    return ProcessInfo(pid=pid, ppid=int(fields[1]), start="%s:%s" % (boot, fields[19]),
                       pgid=int(fields[2]), tpgid=int(fields[5]), name=name)


def _darwin_process_info(pid):
    env = dict(os.environ, LC_ALL="C")
    try:
        proc = subprocess.run(["ps", "-o", "ppid=,pgid=,tpgid=,lstart=,comm=", "-p", str(pid)],
                              stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=env)
    except OSError:
        return None
    fields = proc.stdout.decode("utf-8", "replace").split(None, 8)
    if proc.returncode != 0 or len(fields) < 9:
        return None
    try:
        int(fields[0]), int(fields[1]), int(fields[2])
    except ValueError:
        return None
    # lstart is five words in the C locale: "Wed Nov 26 11:00:00 2026".
    return ProcessInfo(pid=pid, ppid=int(fields[0]), start=" ".join(fields[3:8]),
                       pgid=int(fields[1]), tpgid=int(fields[2]),
                       name=os.path.basename(fields[8].strip()))
