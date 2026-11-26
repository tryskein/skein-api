"""A fake process source for the monitor runner tests: synthetic processes
with a parent, a start, process groups, a name and an argument list, answered
the way the runner's system source answers (proctree.SystemProcesses): info()
gives a ProcessInfo-like object or None, argv() the argument list or None.

Standard library of Python 3.6 only.
"""


class FakeInfo(object):
    """The facts of one fake process, with the fields of the runner's
    ProcessInfo."""

    def __init__(self, pid, ppid, start, pgid, tpgid, name):
        self.pid = pid
        self.ppid = ppid
        self.start = start
        self.pgid = pgid
        self.tpgid = tpgid
        self.name = name

    def foreground(self):
        return self.tpgid > 0 and self.tpgid == self.pgid


class FakeProcesses(object):
    """A table of fake processes. A pid not in the table, or ended, is not
    known."""

    def __init__(self):
        self.procs = {}
        self.argvs = {}

    def add(self, pid, ppid, name, argv=None, start=None, pgid=None, tpgid=-1):
        """Add a process. argv defaults to [name]; start to "s<pid>"."""
        self.procs[pid] = FakeInfo(pid, ppid, start if start is not None else "s%d" % pid,
                                   pgid if pgid is not None else pid, tpgid, name)
        self.argvs[pid] = list(argv) if argv is not None else [name]
        return pid

    def end(self, pid):
        """The process ends: it is no longer known."""
        self.procs.pop(pid, None)
        self.argvs.pop(pid, None)

    def info(self, pid):
        return self.procs.get(pid)

    def argv(self, pid):
        argv = self.argvs.get(pid)
        return None if argv is None else list(argv)


def linear(*procs, **kwargs):
    """A chain of processes, the runner first: each element is a name or a
    (name, argv) pair; the first is the runner, every next one the parent of
    the one before. Pids count down from first_pid (default 1000). Returns
    (source, pids) with pids in the order given."""
    source = FakeProcesses()
    pid = kwargs.get("first_pid", 1000)
    pids = [pid - i for i in range(len(procs))]
    for i, proc in enumerate(procs):
        name, argv = (proc, None) if isinstance(proc, str) else proc
        ppid = pids[i + 1] if i + 1 < len(pids) else 1
        source.add(pids[i], ppid, name, argv)
    return source, pids
