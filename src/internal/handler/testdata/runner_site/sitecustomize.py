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
#   SKEIN_TEST_URLOPEN_TIMEOUT=1   urllib.request.urlopen raises TimeoutError.
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
        urllib.request.urlopen = _timed_out

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
