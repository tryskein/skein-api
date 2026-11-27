# --- platform layer: Windows -------------------------------------------------
# The Windows implementation of the platform layer, on the security part
# winsec.py (P-9). Privacy is criterion (a) of P-9, the term "private Windows
# directory": a directory is checked on a handle of it, a file is
# checked on the very handle that is then read (its own DACL), and a
# reparse point is never followed. Private directories are created in one
# call with their protected DACL. The one-runner lock is msvcrt.locking on
# the stable file locks/<key>.lock (P-5); the process tree comes from a
# Toolhelp32 snapshot and GetProcessTimes (P-8); the detached start uses
# DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW (P-6).
#
# Standard library only (ctypes, msvcrt). On another system the class is
# never chosen (current_platform), and its module-level code touches no
# Windows API.

import ctypes
import os
import subprocess
import threading
import time

try:
    import msvcrt
except ImportError:  # not Windows
    msvcrt = None

# Windows error codes the layer tells apart.
_ERROR_FILE_NOT_FOUND = 2
_ERROR_PATH_NOT_FOUND = 3
# Creation flags of the detached start (P-6).
_DETACHED_FLAGS = 0x00000008 | 0x00000200 | 0x08000000
# Step of the wait for a lock held by another process.
_WIN_LOCK_WAIT_STEP_SEC = 0.05

_WIN_SECURITY = []


def _win_security():
    """The system security source, created on first use (it loads the
    Windows libraries)."""
    if not _WIN_SECURITY:
        source = WinSecurity()
        _WIN_SECURITY.append((source, source.me()))
    return _WIN_SECURITY[0]


def _win_open(path, data=False):
    """A handle of path, a reparse point opened itself. Raises
    PlatformRefusal."""
    source, _ = _win_security()
    try:
        return source.open(path, data)
    except OSError as exc:
        if exc.errno in (_ERROR_FILE_NOT_FOUND, _ERROR_PATH_NOT_FOUND):
            raise PlatformRefusal("not_found", path)
        raise PlatformRefusal("unreadable", path)


def _win_view(handle, path):
    source, _ = _win_security()
    try:
        return source.view(handle)
    except OSError:
        raise PlatformRefusal("unreadable", path)


def _win_check(view, path, directory):
    """Refuse a view that fails criterion (a) or is of the wrong kind."""
    _, me = _win_security()
    if view.reparse:
        raise PlatformRefusal("symlink", path)
    is_dir = bool(view.attributes & FILE_ATTRIBUTE_DIRECTORY)
    if directory and not is_dir:
        raise PlatformRefusal("not_directory", path)
    if not directory and is_dir:
        raise PlatformRefusal("not_regular", path)
    reason = private_refusal(view, me, directory)
    if reason is not None:
        raise PlatformRefusal(reason, path)


def _win_check_dir(path):
    """Check the directory at path on a handle of it, closed on return."""
    source, _ = _win_security()
    handle = _win_open(path)
    try:
        _win_check(_win_view(handle, path), path, True)
    finally:
        source.close(handle)


def _win_read_checked(path):
    """Check the directory of path, open the file once (no reparse point
    followed), check that handle by criterion (a) and read that same handle;
    the handle is closed before return, so removing the file later does not
    run into it (P-12)."""
    _win_check_dir(os.path.dirname(os.path.abspath(path)))
    source, _ = _win_security()
    handle = _win_open(path, data=True)
    fd = None
    try:
        _win_check(_win_view(handle, path), path, False)
        fd = msvcrt.open_osfhandle(handle, os.O_RDONLY | os.O_BINARY)
        handle = None  # the descriptor owns it now
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
        if fd is not None:
            os.close(fd)
        elif handle is not None:
            source.close(handle)


class WindowsRunnerLock(object):
    """A held one-runner lock (one byte of the lock file locked through
    msvcrt). The lock file is never deleted on release."""

    def __init__(self, path, fd):
        self.path = path
        self._fd = fd

    def release(self):
        fd, self._fd = self._fd, None
        if fd is None:
            return
        try:
            os.lseek(fd, 0, os.SEEK_SET)
            msvcrt.locking(fd, msvcrt.LK_UNLCK, 1)
        finally:
            os.close(fd)


class WindowsPlatform(object):
    """The Windows implementation of the platform layer."""

    @staticmethod
    def has_file_locks():
        """Whether this system has file locks (msvcrt)."""
        return msvcrt is not None

    @staticmethod
    def lock_fd(fd, blocking=True):
        """Take the lock of the first byte of an open file (msvcrt.locking
        locks from the current position, so it is set to 0 first). Raises
        BlockingIOError when another process holds it and blocking is
        false. A blocking wait polls, since LK_LOCK gives up after ten
        seconds."""
        while True:
            os.lseek(fd, 0, os.SEEK_SET)
            try:
                msvcrt.locking(fd, msvcrt.LK_NBLCK, 1)
                return
            except OSError:
                if not blocking:
                    raise BlockingIOError("the file is locked by another process")
            time.sleep(_WIN_LOCK_WAIT_STEP_SEC)

    @staticmethod
    def owns(st):
        """Ownership is not the privacy criterion on Windows (criterion (a)
        of P-9 reads the DACL where privacy is checked); stat has no owner
        here."""
        return True

    @staticmethod
    def read_checked(path, private):
        """The content of path, read only after its directory and the opened
        file passed criterion (a); private, the POSIX predicate of a caller,
        has no meaning here and is not read. Raises PlatformRefusal."""
        return _win_read_checked(path)

    @staticmethod
    def open_file_refusal(fd, path):
        """Why the open file fd may not be used as a file of the monitor's
        own (criterion (a) on its handle), or None."""
        try:
            _win_check(_win_view(msvcrt.get_osfhandle(fd), path), path, False)
        except PlatformRefusal as exc:
            return exc.reason
        return None

    @staticmethod
    def remove_stale_lock(path, max_age_sec):
        """Remove the lock file path when it is older than max_age_sec. A
        file another process has open cannot be deleted (the layer opens
        lock files without sharing deletion), so a held lock stays."""
        try:
            if time.time() - os.lstat(path).st_mtime < max_age_sec:
                return
            os.remove(path)
        except OSError:
            pass

    # The processes of one call of a channel program: a job object the
    # program is assigned to while it is still suspended, so every
    # descendant is born in it.

    @staticmethod
    def tree_popen_options():
        # A detached runner has no console: its programs get none either
        # (no window opens for a call).
        return {"creationflags": _CREATE_SUSPENDED | _CREATE_NO_WINDOW}

    @staticmethod
    def child_popen_options():
        """Options of a helper process of the runner (the request
        executor): no console window."""
        return {"creationflags": _CREATE_NO_WINDOW}

    @staticmethod
    def tree_make(proc):
        """The job of the suspended proc. A job that cannot be made is a
        refusal before send: the suspended program is killed and nothing
        ran (ExecutorRefusal names why)."""
        try:
            return _WINDOWS_JOBS.make(proc)
        except ExecutorRefusal:
            _win_kill_quietly(proc)
            raise

    @staticmethod
    def tree_start(proc, job):
        """Resume the program inside its job (ExecutorRefusal when it
        cannot be resumed)."""
        _WINDOWS_JOBS.resume(job, proc)

    @staticmethod
    def tree_kill(proc, job):
        """Kill the tree of proc. Returns None, or why the tree may outlive
        the call (the job was not ended): the root is killed all the same."""
        failure = _WINDOWS_JOBS.close(job) if job is not None else None
        _win_kill_quietly(proc)
        return failure

    @staticmethod
    def make_private_dir(path):
        """Create path as a new private directory in one call (a protected
        DACL for the user, SYSTEM and the administrators). An existing entry
        is refused (exists), never adopted."""
        source, _ = _win_security()
        try:
            source.make_private_dir(path)
        except FileExistsError:
            raise PlatformRefusal("exists", path)
        except OSError:
            raise PlatformRefusal("unreadable", path)
        _win_check_dir(path)

    @staticmethod
    def ensure_private_dir(path):
        """A private directory at path, created with its missing parents,
        each new one private, when absent. An existing one that fails
        criterion (a) is refused, not repaired."""
        source, _ = _win_security()
        missing = []
        current = os.path.abspath(path)
        while not os.path.lexists(current):
            missing.append(current)
            parent = os.path.dirname(current)
            if parent == current:
                break
            current = parent
        for directory in reversed(missing):
            try:
                source.make_private_dir(directory)
            except FileExistsError:
                pass  # created meanwhile: checked below like any existing one
            except OSError:
                raise PlatformRefusal("unreadable", directory)
        _win_check_dir(path)

    @staticmethod
    def read_token(path):
        """The content of a token file, read only after its directory and the
        opened file passed criterion (a). Raises PlatformRefusal."""
        return _win_read_checked(path)

    @staticmethod
    def read_own_file(path):
        """The content of a file the monitor writes itself, by the same
        criterion. Raises PlatformRefusal."""
        return _win_read_checked(path)

    @staticmethod
    def atomic_write(path, data, dir_check=None):
        """Replace the file at path with data (bytes) atomically: a new
        temporary file in the same directory (it takes the inheritable DACL
        of the private directory), written and flushed to disk, then
        os.replace. The directory must pass criterion (a); dir_check, the
        POSIX predicate of a caller, has no meaning here and is not read. A
        failure before the replace leaves the old file and no temporary
        file."""
        directory, name = os.path.split(os.path.abspath(path))
        _win_check_dir(directory)
        tmp = os.path.join(directory, ".%s.tmp-%d-%s" % (name, os.getpid(), os.urandom(6).hex()))
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_BINARY, 0o600)
        try:
            try:
                view = memoryview(data)
                while view:
                    view = view[os.write(fd, view):]
                os.fsync(fd)
            finally:
                os.close(fd)
            os.replace(tmp, path)
        except BaseException:
            try:
                os.remove(tmp)
            except OSError:
                pass
            raise

    @staticmethod
    def acquire_runner_lock(token_path, wait_sec=0, env=None):
        """Take the one-runner lock of a token file (P-5) on its stable file
        skein-monitor/locks/<key>.lock with msvcrt.locking. wait_sec=0 does
        not wait; otherwise the lock held by another process is awaited up
        to wait_sec seconds. The file is opened without sharing deletion, so
        it cannot be renewed under the holder. Returns a WindowsRunnerLock;
        raises LockBusy when still held, PlatformRefusal when the
        directories or the lock file are not private."""
        path = runner_lock_path(token_path, env)
        locks = os.path.dirname(path)
        WindowsPlatform.ensure_private_dir(os.path.dirname(locks))
        WindowsPlatform.ensure_private_dir(locks)
        if msvcrt is None:
            raise PlatformRefusal("unsupported", path)
        deadline = time.monotonic() + max(wait_sec, 0)
        while True:
            try:
                fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_BINARY, 0o600)
            except OSError:
                raise PlatformRefusal("unreadable", path)
            try:
                _win_check(_win_view(msvcrt.get_osfhandle(fd), path), path, False)
                try:
                    msvcrt.locking(fd, msvcrt.LK_NBLCK, 1)
                except OSError:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        raise LockBusy(path)
                    os.close(fd)
                    fd = None
                    time.sleep(min(_WIN_LOCK_WAIT_STEP_SEC, remaining))
                    continue
                lock, fd = WindowsRunnerLock(path, fd), None
                return lock
            finally:
                if fd is not None:
                    os.close(fd)

    @staticmethod
    def process_info(pid):
        """ProcessInfo of a live process; None when it is unknown or its
        facts cannot be read. The parent and the executable name come from
        a Toolhelp32 snapshot (the name without ".exe", as the registry
        names executables), the start from the creation time of
        GetProcessTimes. Windows has no process groups or terminals of the
        POSIX kind: pgid is the pid and tpgid -1, so no process is in a
        foreground group."""
        entry = _win_snapshot_entry(pid)
        if entry is None:
            return None
        ppid, exe = entry
        start = _win_creation_time(pid)
        if start is None:
            return None
        name = exe[:-4] if exe.lower().endswith(".exe") else exe
        return ProcessInfo(pid=pid, ppid=ppid, start=start, pgid=pid, tpgid=-1, name=name)

    @staticmethod
    def spawn_detached(argv, stdin_data, env, receipt_timeout_sec, on_receipt=None):
        """Start argv detached (P-6: no console, a process group of its own),
        hand it stdin_data on its stdin and wait up to receipt_timeout_sec
        for its first stdout line, the start receipt, read on a thread (a
        pipe of Windows has no select). Returns (pid, receipt) as on POSIX;
        with on_receipt the stdin stays open until it returns, and its
        refusal kills the child, which is not reaped yet."""
        proc = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, env=env, close_fds=True,
                                creationflags=_DETACHED_FLAGS)
        threading.Thread(target=proc.wait, daemon=True).start()
        try:
            try:
                proc.stdin.write(stdin_data)
                proc.stdin.flush()
                if on_receipt is None:
                    proc.stdin.close()
            except (BrokenPipeError, OSError):
                pass
            receipt = _win_read_line(proc.stdout, receipt_timeout_sec)
            if on_receipt is not None:
                keep = False
                try:
                    keep = on_receipt(receipt)
                finally:
                    if not keep and proc.poll() is None:
                        try:
                            proc.kill()
                        except OSError:
                            pass
            return proc.pid, receipt
        finally:
            try:
                proc.stdin.close()
            except (BrokenPipeError, OSError):
                pass
            proc.stdout.close()


def _win_read_line(stream, timeout_sec):
    """One line of stream within timeout_sec, without its newline; None on
    EOF or when the time runs out. A reader thread does the blocking read;
    a line that comes after the wait is dropped with the thread."""
    box = []

    def read():
        box.append(stream.readline())

    reader = threading.Thread(target=read, daemon=True)
    reader.start()
    reader.join(max(timeout_sec, 0))
    if not box or not box[0].endswith(b"\n"):
        return None
    return box[0][:-1].decode("utf-8", "replace")


class _ProcessEntry32W(ctypes.Structure):
    _fields_ = [("size", ctypes.c_uint32), ("usage", ctypes.c_uint32), ("pid", ctypes.c_uint32),
                ("heap", ctypes.c_size_t), ("module", ctypes.c_uint32), ("threads", ctypes.c_uint32),
                ("ppid", ctypes.c_uint32), ("priority", ctypes.c_long), ("flags", ctypes.c_uint32),
                ("exe", ctypes.c_wchar * 260)]


_WIN_KERNEL32 = []


def _win_kernel32():
    if _WIN_KERNEL32:
        return _WIN_KERNEL32[0]
    k = ctypes.WinDLL("kernel32", use_last_error=True)
    H = ctypes.c_void_p
    for fn, args, res in (
            (k.CreateToolhelp32Snapshot, (ctypes.c_uint32, ctypes.c_uint32), H),
            (k.Process32FirstW, (H, ctypes.POINTER(_ProcessEntry32W)), ctypes.c_int),
            (k.Process32NextW, (H, ctypes.POINTER(_ProcessEntry32W)), ctypes.c_int),
            (k.OpenProcess, (ctypes.c_uint32, ctypes.c_int, ctypes.c_uint32), H),
            (k.GetProcessTimes, (H,) + (ctypes.POINTER(ctypes.c_uint64),) * 4, ctypes.c_int),
            (k.CloseHandle, (H,), ctypes.c_int)):
        fn.argtypes, fn.restype = args, res
    _WIN_KERNEL32.append(k)
    return k


def _win_snapshot_entry(pid):
    """(parent pid, executable name) of pid from a Toolhelp32 snapshot of
    the processes (TH32CS_SNAPPROCESS), or None when it is not in it."""
    k = _win_kernel32()
    snap = k.CreateToolhelp32Snapshot(0x2, 0)
    if snap is None or snap == ctypes.c_void_p(-1).value:
        return None
    try:
        entry = _ProcessEntry32W()
        entry.size = ctypes.sizeof(_ProcessEntry32W)
        ok = k.Process32FirstW(snap, ctypes.byref(entry))
        while ok:
            if entry.pid == pid:
                return entry.ppid, entry.exe
            ok = k.Process32NextW(snap, ctypes.byref(entry))
        return None
    finally:
        k.CloseHandle(snap)


def _win_creation_time(pid):
    """The creation time of a live process (GetProcessTimes, 100 ns since
    1601) as an opaque start value, or None when it cannot be opened
    (PROCESS_QUERY_LIMITED_INFORMATION) or has exited."""
    k = _win_kernel32()
    handle = k.OpenProcess(0x1000, 0, pid)
    if not handle:
        return None
    try:
        created, exited, kernel, user = (ctypes.c_uint64() for _ in range(4))
        if not k.GetProcessTimes(handle, ctypes.byref(created), ctypes.byref(exited),
                                 ctypes.byref(kernel), ctypes.byref(user)):
            return None
        if exited.value:
            return None
        return "win:%d" % created.value
    finally:
        k.CloseHandle(handle)


# CreateProcess flags: a program started suspended; a console program
# without a console window.
_CREATE_SUSPENDED = 0x00000004
_CREATE_NO_WINDOW = 0x08000000


def _win_kill_quietly(proc):
    try:
        proc.kill()
    except OSError:
        pass


class _WindowsJobs(object):
    """The job object calls of kernel32 and ntdll through ctypes, each with
    its restype and argtypes, and every BOOL, HANDLE and NTSTATUS checked."""

    def __init__(self):
        self._k32 = None
        self._nt = None

    def _api(self):
        if self._k32 is None:
            from ctypes import wintypes
            k32 = ctypes.WinDLL("kernel32", use_last_error=True)
            k32.CreateJobObjectW.restype = wintypes.HANDLE
            k32.CreateJobObjectW.argtypes = (wintypes.LPVOID, wintypes.LPCWSTR)
            k32.AssignProcessToJobObject.restype = wintypes.BOOL
            k32.AssignProcessToJobObject.argtypes = (wintypes.HANDLE, wintypes.HANDLE)
            k32.TerminateJobObject.restype = wintypes.BOOL
            k32.TerminateJobObject.argtypes = (wintypes.HANDLE, wintypes.UINT)
            k32.CloseHandle.restype = wintypes.BOOL
            k32.CloseHandle.argtypes = (wintypes.HANDLE,)
            k32.SetInformationJobObject.restype = wintypes.BOOL
            k32.SetInformationJobObject.argtypes = (wintypes.HANDLE, ctypes.c_int, wintypes.LPVOID, wintypes.DWORD)
            nt = ctypes.WinDLL("ntdll")
            nt.NtResumeProcess.restype = wintypes.LONG
            nt.NtResumeProcess.argtypes = (wintypes.HANDLE,)
            self._k32, self._nt = k32, nt
        return self._k32, self._nt

    def make(self, proc):
        """A job object holding the suspended proc."""
        try:
            k32, _ = self._api()
            job = k32.CreateJobObjectW(None, None)
            if not job:
                raise ExecutorRefusal("no job object for the call (error %d)" % ctypes.get_last_error())
            # Closing the last handle of the job kills every process in it:
            # the tree dies with the runner too (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE).
            info = _JobExtendedLimits()
            info.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
            if not k32.SetInformationJobObject(job, JOB_OBJECT_EXTENDED_LIMIT_INFORMATION, ctypes.byref(info),
                                               ctypes.sizeof(info)):
                err = ctypes.get_last_error()
                k32.CloseHandle(job)
                raise ExecutorRefusal("the job object cannot kill its processes on close (error %d)" % err)
            if not k32.AssignProcessToJobObject(job, int(proc._handle)):
                err = ctypes.get_last_error()
                k32.CloseHandle(job)
                raise ExecutorRefusal("the program cannot join its job object (error %d)" % err)
            return job
        except ExecutorRefusal:
            raise
        except Exception as exc:  # ctypes.ArgumentError among them: the call is refused, not crashed
            raise ExecutorRefusal("no job object for the call (%s)" % exc)

    def resume(self, job, proc):
        try:
            _, nt = self._api()
            status = nt.NtResumeProcess(int(proc._handle))
        except Exception as exc:  # ctypes.ArgumentError among them
            raise ExecutorRefusal("the program cannot be resumed (%s)" % exc)
        if status != 0:
            raise ExecutorRefusal("the program cannot be resumed (status 0x%08x)" % (status & 0xFFFFFFFF))

    def close(self, job):
        """Terminate every process of the job and close its handle. Returns
        None, or why the tree may not be ended: a failed TerminateJobObject
        (the close still kills it by JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE when
        CloseHandle succeeds), a failed CloseHandle, or a ctypes error."""
        failures = []
        try:
            k32, _ = self._api()
        except Exception as exc:  # ctypes.ArgumentError among them
            return "the job object API is not available (%s)" % exc
        try:
            if not k32.TerminateJobObject(job, 1):
                failures.append("TerminateJobObject error %d" % ctypes.get_last_error())
        except Exception as exc:
            failures.append("TerminateJobObject: %s" % exc)
        try:
            if not k32.CloseHandle(job):
                failures.append("CloseHandle error %d" % ctypes.get_last_error())
                return "; ".join(failures)
        except Exception as exc:
            failures.append("CloseHandle: %s" % exc)
            return "; ".join(failures)
        # The handle is closed: kill-on-close ends whatever the termination
        # left, so the tree is ended either way.
        return None


# SetInformationJobObject: the class of JOBOBJECT_EXTENDED_LIMIT_INFORMATION
# and its limit that kills every process of the job when its last handle is
# closed (winnt.h).
JOB_OBJECT_EXTENDED_LIMIT_INFORMATION = 9
JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE = 0x2000


class _JobBasicLimits(ctypes.Structure):
    _fields_ = [("PerProcessUserTimeLimit", ctypes.c_int64), ("PerJobUserTimeLimit", ctypes.c_int64),
                ("LimitFlags", ctypes.c_uint32), ("MinimumWorkingSetSize", ctypes.c_size_t),
                ("MaximumWorkingSetSize", ctypes.c_size_t), ("ActiveProcessLimit", ctypes.c_uint32),
                ("Affinity", ctypes.c_size_t), ("PriorityClass", ctypes.c_uint32),
                ("SchedulingClass", ctypes.c_uint32)]


class _IoCounters(ctypes.Structure):
    _fields_ = [(name, ctypes.c_uint64) for name in ("ReadOperationCount", "WriteOperationCount",
                                                     "OtherOperationCount", "ReadTransferCount",
                                                     "WriteTransferCount", "OtherTransferCount")]


class _JobExtendedLimits(ctypes.Structure):
    _fields_ = [("BasicLimitInformation", _JobBasicLimits), ("IoInfo", _IoCounters),
                ("ProcessMemoryLimit", ctypes.c_size_t), ("JobMemoryLimit", ctypes.c_size_t),
                ("PeakProcessMemoryUsed", ctypes.c_size_t), ("PeakJobMemoryUsed", ctypes.c_size_t)]


_WINDOWS_JOBS = _WindowsJobs()
