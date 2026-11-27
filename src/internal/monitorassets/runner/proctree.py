# --- the chain of ancestor processes (P-8) -----------------------------------
# The preflight finds the agent process among the ancestors of the runner.
# A process source answers three questions about a pid: what the platform
# knows of it (ProcessInfo: parent, start, groups, name), its argument list
# and the path of its executable. The system source asks the platform layer
# and the OS; the tests hand in a fake source with synthetic chains.

import os
import struct
import sys

# The longest chain followed; a deeper one is cut there (a loop of pids that
# reuses numbers cannot keep the walk going).
MAX_ANCESTRY = 256


class Proc(object):
    """One process of a chain: its ProcessInfo facts, its argument list and
    the absolute path of its executable (each None when it cannot be
    read)."""

    __slots__ = ("pid", "ppid", "start", "name", "argv", "executable", "info")

    def __init__(self, info, argv, executable=None):
        self.pid = info.pid
        self.ppid = info.ppid
        self.start = info.start
        self.name = info.name
        self.argv = argv
        self.executable = executable
        self.info = info

    def names(self):
        """The executable names of the process: its name and the base name
        of its first argument (a login shell's leading "-" removed)."""
        names = set()
        if self.name:
            names.add(self.name)
        if self.argv:
            first = self.argv[0].replace("\\", "/").rsplit("/", 1)[-1].lstrip("-")
            if first:
                names.add(first)
        return names


class SystemProcesses(object):
    """The processes of this system."""

    @staticmethod
    def info(pid):
        return current_platform().process_info(pid)

    @staticmethod
    def argv(pid):
        """The argument list of a live process, or None when it cannot be
        read."""
        if sys.platform.startswith("linux"):
            return _linux_argv(pid)
        if sys.platform == "darwin":
            procargs = _darwin_procargs(pid)
            return None if procargs is None else procargs[1]
        return None

    @staticmethod
    def executable(pid):
        """The absolute path of the executable of a live process, or None
        when it cannot be read (Windows: not read, no channel program there
        is taken from the agent process)."""
        if sys.platform.startswith("linux"):
            try:
                path = os.readlink("/proc/%d/exe" % pid)
            except OSError:
                return None
            # A replaced or deleted executable is not the program to run.
            return None if path.endswith(" (deleted)") or not os.path.isabs(path) else path
        if sys.platform == "darwin":
            procargs = _darwin_procargs(pid)
            return None if procargs is None else procargs[0]
        return None


def ancestry(source, pid):
    """The chain of pid and its ancestors, pid first. It ends at a process
    that is not known, at pid 0, at a pid already in the chain, or after
    MAX_ANCESTRY processes."""
    chain = []
    seen = set()
    while pid > 0 and pid not in seen and len(chain) < MAX_ANCESTRY:
        seen.add(pid)
        info = source.info(pid)
        if info is None:
            break
        chain.append(Proc(info, source.argv(pid), source.executable(pid)))
        pid = info.ppid
    return chain


def _linux_argv(pid):
    try:
        with open("/proc/%d/cmdline" % pid, "rb") as fh:
            raw = fh.read()
    except OSError:
        return None
    if not raw:
        return None  # a kernel thread or a zombie
    return [a.decode("utf-8", "replace") for a in raw.rstrip(b"\0").split(b"\0")]


# sysctl(3) of macOS: CTL_KERN, KERN_PROCARGS2.
_CTL_KERN = 1
_KERN_PROCARGS2 = 49


def _darwin_procargs(pid):
    """(executable path, argument list) of a live process, or None.
    KERN_PROCARGS2 holds argc (an int), the executable path, NUL padding
    and then argc NUL-terminated arguments."""
    try:
        import ctypes
        import ctypes.util
        libc = ctypes.CDLL(ctypes.util.find_library("c"), use_errno=True)
        mib = (ctypes.c_int * 3)(_CTL_KERN, _KERN_PROCARGS2, pid)
        size = ctypes.c_size_t(0)
        if libc.sysctl(mib, 3, None, ctypes.byref(size), None, ctypes.c_size_t(0)) != 0:
            return None
        buf = ctypes.create_string_buffer(size.value)
        if libc.sysctl(mib, 3, buf, ctypes.byref(size), None, ctypes.c_size_t(0)) != 0:
            return None
    except (OSError, AttributeError, TypeError):
        return None
    raw = buf.raw[:size.value]
    width = struct.calcsize("i")
    if len(raw) < width:
        return None
    argc = struct.unpack("i", raw[:width])[0]
    path, _, rest = raw[width:].partition(b"\0")
    args = rest.lstrip(b"\0").split(b"\0")
    if argc < 1 or len(args) < argc:
        return None
    executable = path.decode("utf-8", "replace")
    return (executable if os.path.isabs(executable) else None,
            [a.decode("utf-8", "replace") for a in args[:argc]])
