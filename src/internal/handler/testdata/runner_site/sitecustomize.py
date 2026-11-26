# Test-only interpreter hooks for the agent monitor runner (sk-579 test plan,
# "Python techniques"). Put this directory on PYTHONPATH and set
# SKEIN_TEST_SITE=1: Python imports it at start-up, in the runner process and
# in every child it spawns (the request executor). Nothing here touches the
# product files; every hook is opt-in through its own variable.
#
#   SKEIN_TEST_VCLOCK=1            virtual clock: time.monotonic/time.time read
#                                  it, time.sleep advances it. A sleep of
#                                  0.1 s or more is a runner wait (backoff,
#                                  Retry-After) — appended to
#                                  SKEIN_TEST_SLEEP_LOG, no real waiting. A
#                                  shorter sleep is the executor wait step: it
#                                  advances the clock by its own length after
#                                  a tiny real pause, so a real request in
#                                  flight can still finish.
#   SKEIN_TEST_VCLOCK_HOLD_UNTIL=<path>
#                                  with the clock: a sleep shorter than 0.1 s
#                                  does not advance it until <path> exists, so
#                                  a wait bounded in virtual time cannot run
#                                  out before a real event (another process
#                                  creating <path>) has happened.
#   SKEIN_TEST_VCLOCK_HOLD_STEPS=1 with the clock: a sleep shorter than 0.1 s
#                                  never advances it, so the executor wait
#                                  costs no virtual time however slowly the
#                                  executor process starts. Only for runs
#                                  whose executor answers at once.
#   SKEIN_TEST_BUDGET_SEC=<s>      with the clock: once virtual time since start
#                                  reaches <s>, write the probe (below) and
#                                  _exit(0).
#   SKEIN_TEST_PROBE_FILE=<path>   probe: "alive" or "dead" for the FIRST pid in
#                                  SKEIN_TEST_DNS_LOG at the budget moment.
#   SKEIN_TEST_RANDOM=<float>      random.random() returns <float>;
#   SKEIN_TEST_RANDOM=raise        random.random() raises RuntimeError.
#   SKEIN_TEST_DNS_LOG=<path>      socket.getaddrinfo appends "<pid> <thread>"
#                                  and then blocks forever (until this process
#                                  is killed or orphaned).
#   SKEIN_TEST_URLOPEN_TIMEOUT=1   every urllib.request opener (urlopen and
#                                  build_opener alike) raises TimeoutError.
#   SKEIN_TEST_SIGNAL_ON_IMPORT=<module>
#                                  the first `import <module>` sends this
#                                  process a real SIGTERM, before the import
#                                  runs (a signal at an import boundary).
#   SKEIN_TEST_SIGNAL_AFTER_HANDLER=<function name>
#                                  right after signal.signal(SIGINT, f) with
#                                  f.__name__ == <function name> returns, this
#                                  process gets a real SIGTERM (a signal in the
#                                  instant after a handler is installed).
#   SKEIN_TEST_FLOCK_ERRNO=<errno> fcntl.flock raises OSError(<errno>).
#   SKEIN_TEST_NO_FCNTL=1          `import fcntl` fails (a platform without OS
#                                  file locks).
#   SKEIN_TEST_CONNECT_LOG=<path>  socket.create_connection opens the real
#                                  connection, appends "timeout <value>" and
#                                  returns a socket whose recv_into raises
#                                  socket.timeout at once when that timeout is
#                                  finite and positive — the limit "expires"
#                                  without real waiting — and otherwise appends
#                                  "violation" and raises RuntimeError.
#   SKEIN_TEST_FOREIGN_UID=1       os.getuid() reports another uid, so every
#                                  file this process opens looks like another
#                                  user's.
#   SKEIN_TEST_SWAP_TOKEN_FILE=1   the first os.fstat of the file at
#                                  SKEIN_TOKEN_FILE returns its real stat, then
#                                  renames that file to <path>.opened and
#                                  writes a new file "swapped\n" at <path>: the
#                                  path names another inode right after fstat.
#   SKEIN_TEST_IO_LOG=<path>       file I/O counter: every os.open and built-in
#                                  open of a path under SKEIN_TEST_IO_WATCH
#                                  appends "open <path> <fd>" (or
#                                  "open-failed <path> <errno name>"), and the
#                                  os.fstat, os.read and os.close of a
#                                  descriptor so opened append
#                                  "fstat|read|close <fd>". <path> is absolute,
#                                  an open relative to dir_fd resolved through
#                                  /proc/self/fd. Lines are appended in call
#                                  order, so a reader sees what was opened, by
#                                  which path, and whether each read of a
#                                  descriptor came after its fstat.
#   SKEIN_TEST_IO_WATCH=<dir>      with the counter: the directory whose
#                                  paths are counted (absolute).
#   SKEIN_TEST_FOREIGN_INODE=<path>
#                                  os.fstat, os.lstat and os.stat of the file
#                                  or directory at <path> (the inode it names
#                                  at start-up) report another user as owner;
#                                  everything else keeps its real owner.
import os

if os.environ.get("SKEIN_TEST_SITE") == "1":
    import random
    import socket
    import threading
    import time

    _real_sleep = time.sleep
    _parent = os.getppid()

    if os.environ.get("SKEIN_TEST_VCLOCK") == "1":
        _state = {"now": 1000.0}
        _start = _state["now"]
        _budget = float(os.environ.get("SKEIN_TEST_BUDGET_SEC", "0") or "0")
        _sleep_log = os.environ.get("SKEIN_TEST_SLEEP_LOG", "")
        _probe = os.environ.get("SKEIN_TEST_PROBE_FILE", "")
        _dns_log = os.environ.get("SKEIN_TEST_DNS_LOG", "")
        _hold_until = os.environ.get("SKEIN_TEST_VCLOCK_HOLD_UNTIL", "")
        _hold_steps = os.environ.get("SKEIN_TEST_VCLOCK_HOLD_STEPS") == "1"

        def _first_executor_alive():
            try:
                with open(_dns_log) as fh:
                    first = fh.readline().split()
            except OSError:
                return False
            if not first:
                return False
            try:
                os.kill(int(first[0]), 0)
            except OSError:
                return False
            return True

        def _check_budget():
            if _budget and _state["now"] - _start >= _budget:
                if _probe:
                    with open(_probe, "w") as fh:
                        fh.write("alive" if _first_executor_alive() else "dead")
                try:
                    import sys
                    sys.stdout.flush()
                except Exception:
                    pass
                os._exit(0)

        def _sleep(seconds):
            seconds = float(seconds)
            if seconds < 0.1:
                _real_sleep(0.002)
                if _hold_steps or (_hold_until and not os.path.exists(_hold_until)):
                    return
            elif _sleep_log:
                with open(_sleep_log, "a") as fh:
                    fh.write(repr(seconds) + "\n")
            _state["now"] += max(seconds, 0.0)
            _check_budget()

        time.sleep = _sleep
        time.monotonic = lambda: _state["now"]
        time.time = lambda: _state["now"]

    _rnd = os.environ.get("SKEIN_TEST_RANDOM", "")
    if _rnd == "raise":
        def _raise():
            raise RuntimeError("sitecustomize: random.random is broken on purpose")
        random.random = _raise
    elif _rnd:
        _fixed = float(_rnd)
        random.random = lambda: _fixed

    _dns = os.environ.get("SKEIN_TEST_DNS_LOG", "")
    if _dns:
        def _blocked_getaddrinfo(*args, **kwargs):
            with open(_dns, "a") as fh:
                fh.write("%d %d\n" % (os.getpid(), threading.get_ident()))
            while os.getppid() == _parent:
                _real_sleep(0.05)
            os._exit(0)
        socket.getaddrinfo = _blocked_getaddrinfo

    if os.environ.get("SKEIN_TEST_URLOPEN_TIMEOUT") == "1":
        import urllib.request

        def _timed_out(*args, **kwargs):
            raise TimeoutError("sitecustomize: urlopen timed out on purpose")
        urllib.request.OpenerDirector.open = _timed_out

    if os.environ.get("SKEIN_TEST_NO_FCNTL") == "1":
        import sys
        sys.modules["fcntl"] = None

    _connect_log = os.environ.get("SKEIN_TEST_CONNECT_LOG", "")
    if _connect_log:
        _real_create_connection = socket.create_connection

        class _LimitProbeSocket(socket.socket):
            def recv_into(self, *args, **kwargs):
                t = self.gettimeout()
                if t is not None and t > 0:
                    raise socket.timeout("sitecustomize: read limit reached")
                with open(_connect_log, "a") as fh:
                    fh.write("violation\n")
                raise RuntimeError("sitecustomize: read without a finite positive timeout")

        def _probing_create_connection(address, timeout=socket._GLOBAL_DEFAULT_TIMEOUT, *args, **kwargs):
            real = _real_create_connection(address, timeout, *args, **kwargs)
            with open(_connect_log, "a") as fh:
                fh.write("timeout %r\n" % (real.gettimeout(),))
            probe = _LimitProbeSocket(real.family, real.type, real.proto, fileno=real.detach())
            probe.settimeout(None if timeout is socket._GLOBAL_DEFAULT_TIMEOUT else timeout)
            return probe

        socket.create_connection = _probing_create_connection

    _on_import = os.environ.get("SKEIN_TEST_SIGNAL_ON_IMPORT", "")
    if _on_import:
        import builtins
        import signal as _signal
        _real_import = builtins.__import__
        _import_fired = [False]

        def _signalling_import(name, *args, **kwargs):
            if name == _on_import and not _import_fired[0]:
                _import_fired[0] = True
                os.kill(os.getpid(), _signal.SIGTERM)
            return _real_import(name, *args, **kwargs)
        builtins.__import__ = _signalling_import

    _after_handler = os.environ.get("SKEIN_TEST_SIGNAL_AFTER_HANDLER", "")
    if _after_handler:
        import signal as _signal
        _real_signal = _signal.signal
        _handler_fired = [False]

        def _signalling_signal(signum, handler):
            previous = _real_signal(signum, handler)
            if (signum == _signal.SIGINT and not _handler_fired[0]
                    and getattr(handler, "__name__", "") == _after_handler):
                _handler_fired[0] = True
                os.kill(os.getpid(), _signal.SIGTERM)
            return previous
        _signal.signal = _signalling_signal

    _flock_errno = os.environ.get("SKEIN_TEST_FLOCK_ERRNO", "")
    if _flock_errno:
        import fcntl as _fcntl

        def _failing_flock(fd, op):
            raise OSError(int(_flock_errno), os.strerror(int(_flock_errno)))
        _fcntl.flock = _failing_flock

    if os.environ.get("SKEIN_TEST_FOREIGN_UID") == "1":
        _foreign_uid = os.getuid() + 1
        os.getuid = lambda: _foreign_uid

    if os.environ.get("SKEIN_TEST_SWAP_TOKEN_FILE") == "1":
        _swap_path = os.environ.get("SKEIN_TOKEN_FILE", "")
        _real_fstat = os.fstat
        _swapped = [False]

        def _swapping_fstat(fd):
            st = _real_fstat(fd)
            if not _swapped[0] and _swap_path:
                try:
                    cur = os.lstat(_swap_path)
                except OSError:
                    cur = None
                if cur is not None and (cur.st_dev, cur.st_ino) == (st.st_dev, st.st_ino):
                    _swapped[0] = True
                    os.rename(_swap_path, _swap_path + ".opened")
                    with open(_swap_path, "w") as fh:
                        fh.write("swapped\n")
            return st
        os.fstat = _swapping_fstat

    _io_log = os.environ.get("SKEIN_TEST_IO_LOG", "")
    if _io_log:
        import builtins
        import errno as _errno
        _io_watch = os.path.join(os.path.abspath(os.environ.get("SKEIN_TEST_IO_WATCH", "/")), "")
        _io_real = {"open": os.open, "fstat": os.fstat, "read": os.read, "close": os.close,
                    "write": os.write, "builtin_open": builtins.open}
        _io_fds = set()

        def _io_note(line):
            fd = _io_real["open"](_io_log, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
            try:
                _io_real["write"](fd, (line + "\n").encode("utf-8", "surrogateescape"))
            finally:
                _io_real["close"](fd)

        def _io_resolve(path, dir_fd):
            path = os.fsdecode(path)
            if dir_fd is not None and not os.path.isabs(path):
                try:
                    return os.path.join(os.readlink("/proc/self/fd/%d" % dir_fd), path)
                except OSError:
                    return path
            return os.path.abspath(path)

        def _io_watched(path):
            return path.startswith(_io_watch)

        def _io_open(path, flags, mode=0o777, *, dir_fd=None):
            full = _io_resolve(path, dir_fd)
            try:
                fd = _io_real["open"](path, flags, mode, dir_fd=dir_fd)
            except OSError as exc:
                if _io_watched(full):
                    _io_note("open-failed %s %s" % (full, _errno.errorcode.get(exc.errno, exc.errno)))
                raise
            if _io_watched(full):
                _io_fds.add(fd)
                _io_note("open %s %d" % (full, fd))
            return fd

        def _io_builtin_open(file, *args, **kwargs):
            fh = _io_real["builtin_open"](file, *args, **kwargs)
            if isinstance(file, (str, bytes)) and _io_watched(_io_resolve(file, None)):
                _io_note("open %s %d" % (_io_resolve(file, None), fh.fileno()))
            return fh

        def _io_fstat(fd, *args, **kwargs):
            if fd in _io_fds:
                _io_note("fstat %d" % fd)
            return _io_real["fstat"](fd, *args, **kwargs)

        def _io_read(fd, n):
            if fd in _io_fds:
                _io_note("read %d" % fd)
            return _io_real["read"](fd, n)

        def _io_close(fd):
            if fd in _io_fds:
                _io_fds.discard(fd)
                _io_note("close %d" % fd)
            return _io_real["close"](fd)

        os.open = _io_open
        os.fstat = _io_fstat
        os.read = _io_read
        os.close = _io_close
        builtins.open = _io_builtin_open

    _foreign_inode = os.environ.get("SKEIN_TEST_FOREIGN_INODE", "")
    if _foreign_inode:
        _fi_st = os.lstat(_foreign_inode)
        _fi_key = (_fi_st.st_dev, _fi_st.st_ino)
        _fi_uid = os.getuid() + 1

        def _fi_wrap(real):
            def _stat(*args, **kwargs):
                st = real(*args, **kwargs)
                if (st.st_dev, st.st_ino) != _fi_key:
                    return st
                fields = list(st)
                fields[4] = _fi_uid  # st_uid
                return os.stat_result(fields)
            return _stat

        os.fstat = _fi_wrap(os.fstat)
        os.lstat = _fi_wrap(os.lstat)
        os.stat = _fi_wrap(os.stat)
