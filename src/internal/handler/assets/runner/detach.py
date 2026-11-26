# --- the working start in client mode (SKEIN_MONITOR_MODE=run) ---------------
# The working command starts the monitor the preflight chose (R-2, R-3, P-6).
#
# The synchronous process (starter -> loader -> this body) checks early that
# the saved preflight result is the one the command names and that the
# overrides are the ones it ran with: a mismatch, no result, an unusable
# journal directory or an old Python is config_invalid before anything else.
# That check is only an early refusal; the authoritative one is made again by
# whoever ends up holding the one-runner lock.
#
# detached and journal: the synchronous process starts a detached child
# (current_platform().spawn_detached: a new session on POSIX), hands it this
# very body on its stdin and waits for its start receipt, the first line of
# the child's stdout. The child takes the locks in the one order of every
# start (P-6):
#   one-runner lock (locks/<key>.lock, P-5) -> open the token, check that it
#   is private, read it, close the descriptor -> state.lock -> check the last
#   preflight.json and the overrides again -> release state.lock -> ready ->
#   first poll.
# Its receipt is the monitor_started line when it is ready, or the one stop
# line of its failed start. The synchronous process prints exactly that line
# and returns: monitor_started is printed if and only if the detached process
# is alive and holds the one-runner lock with the current result. A child
# that gives no receipt in time is killed, and the start is internal_error.
#
# stream and exit-on-event run in the foreground with the same lock order.
#
# monitor_started (C1, INV-17): the full line - preflightId, method, channel,
# reason, warnings, journal - on the first start by a preflight result
# (preflight.json gets the mark "announced" under state.lock once that line
# is printed); a restart by the same working command prints only {event,
# preflightId, method, channel}.
# A new result, a takeover included, is a new preflight.json without the
# mark, so its first start is full again.

import json
import os
import signal
import stat
import sys
import time
import traceback

# How long the synchronous process waits for the start receipt of the
# detached child.
START_RECEIPT_TIMEOUT_SEC = 30
# A file of locks/ older than this and locked by no one is removed by the
# start of any monitor (P-5).
STALE_LOCK_AGE_SEC = 24 * 3600
# The name under which the loader keeps the body it runs, in the namespace it
# runs the body in; the synchronous process hands these bytes to the child.
RUNNER_BODY_NAME = "_skein_loader_body"
# Set in the namespace of the detached child by DETACHED_CHILD_SRC.
DETACHED_CHILD_NAME = "_SKEIN_DETACHED_CHILD"
DETACHED_CHILD = globals().get(DETACHED_CHILD_NAME) is True
# The detached child: reads the body from stdin (its length in bytes on a
# line, then the body; stdin stays open, see _send_receipt) and runs it as a
# client runner that knows it is the child.
DETACHED_CHILD_SRC = (
    "import sys\n"
    "r = sys.stdin.buffer\n"
    "b = r.read(int(r.readline())).decode('utf-8')\n"
    "g = {'__name__': '__main__', '_SKEIN_LOADER_MODE': 'client', '%s': True, '%s': b}\n"
    "exec(compile(b, '<skein-monitor-runner>', 'exec'), g)\n" % (DETACHED_CHILD_NAME, RUNNER_BODY_NAME))
# The status of an event that went only to the journal.
JOURNAL_ONLY_STATUS = "journal"
# The ways a preflight result runs.
_METHODS = (METHOD_STREAM, METHOD_EXIT_ON_EVENT, METHOD_DETACHED, METHOD_JOURNAL)
_START_SIGNALS = (signal.SIGTERM, signal.SIGINT)

try:
    import fcntl as _detach_fcntl
except ImportError:  # not POSIX
    _detach_fcntl = None


def runner_body():
    """The text of this body as the loader ran it, or None."""
    body = globals().get(RUNNER_BODY_NAME)
    return body if isinstance(body, str) and body else None


def check_result(token_file, env, settings):
    """The saved preflight result this start may run, checked against the
    working command: SKEIN_MONITOR_PREFLIGHT names the last result and the
    overrides are the ones it ran with. Raises ConfigInvalid."""
    raw = env.get("SKEIN_MONITOR_PREFLIGHT", "")
    if not raw.isdigit() or raw.startswith("0") or len(raw) > 18:
        raise ConfigInvalid("SKEIN_MONITOR_PREFLIGHT", "not set to the number of a preflight result")
    doc = load_preflight(token_file)
    if doc is None:
        raise ConfigInvalid("SKEIN_MONITOR_PREFLIGHT", "there is no preflight result for this token")
    if doc["preflightId"] != int(raw):
        raise ConfigInvalid("SKEIN_MONITOR_PREFLIGHT",
                            "the last preflight result is %d, not %s" % (doc["preflightId"], raw))
    overrides = doc.get("overrides") if isinstance(doc.get("overrides"), dict) else {}
    if overrides.get("SKEIN_MONITOR_CHANNEL") != settings.channel:
        raise ConfigInvalid("SKEIN_MONITOR_CHANNEL", "differs from the value the preflight ran with")
    if overrides.get("SKEIN_MONITOR_BLIND") != (BLIND_OFF if settings.blind_off else None):
        raise ConfigInvalid("SKEIN_MONITOR_BLIND", "differs from the value the preflight ran with")
    if doc.get("method") not in _METHODS:
        raise ConfigInvalid("SKEIN_MONITOR_PREFLIGHT", "the preflight result names no method this runner runs")
    return doc


def check_start(token_file, env, settings):
    """The early checks of the synchronous process: the Python version, the
    preflight result and the journal directory. Returns the result."""
    if sys.version_info < MIN_PYTHON:
        raise ConfigInvalid("python3", "Python %d.%d or newer is needed" % MIN_PYTHON)
    doc = check_result(token_file, env, settings)
    try:
        probe_journal_dir(env)
    except JournalUnavailable as exc:
        raise ConfigInvalid(exc.path, "the journal directory is unavailable (%s)" % exc.reason)
    except PlatformRefusal as exc:
        raise ConfigInvalid(journal_dir_name(env), "the journal directory is unavailable (%s)" % exc.reason)
    return doc


def monitor_started_line(doc, issue, full):
    """The monitor_started line of a client start (C1)."""
    line = {"event": "monitor_started", "preflightId": doc["preflightId"],
            "method": doc.get("method"), "channel": doc.get("channel")}
    if full:
        line.update({"issue": issue, "pollIntervalSec": POLL_INTERVAL_SEC, "reason": doc.get("reason"),
                     "warnings": doc.get("warnings") or [], "journal": doc.get("journal")})
    return line


class WorkingStart(object):
    """A working start that holds the one-runner lock: taken by begin(),
    released by release() as the last step of the process."""

    def __init__(self):
        self.lock = None
        self.token = None
        self.doc = None
        self.line = None
        self.full = False

    def begin(self, token_file, env, settings, issue):
        """Take the locks in the order of P-6 and check the result again as
        the holder. Raises Stop or ConfigInvalid; the lock, once taken, stays
        held for the stop that follows."""
        try:
            self.lock = take_runner_lock(token_file, env)
        except LockBusy:
            raise Stop("already_running")
        except PlatformRefusal as exc:
            raise ConfigInvalid(runner_lock_path(token_file, env),
                                "the runner lock cannot be taken (%s)" % exc.reason)
        sweep_stale_locks(env, self.lock.path)
        sweep_message_files(token_file)
        self.token = read_token_once(token_file)
        try:
            with StateLock(token_file):
                doc = check_result(token_file, env, settings)
        except (PlatformRefusal, OSError) as exc:
            raise ConfigInvalid(os.path.dirname(os.path.abspath(token_file)),
                                "the preflight result cannot be read (%s)" % getattr(exc, "reason", exc))
        ledger_tails_at_start(env)
        self.doc = doc
        # The mark is set by whoever prints the full line, after printing it.
        self.full = doc.get("announced") is not True
        self.line = monitor_started_line(doc, issue, self.full)

    def release(self):
        lock, self.lock = self.lock, None
        if lock is not None:
            lock.release()


def mark_announced(token_file, preflight_id):
    """Mark preflight result preflight_id announced in preflight.json, under
    state.lock: its full monitor_started has been printed. A mark that
    cannot be written costs a second full line, not the start."""
    try:
        with StateLock(token_file):
            doc = load_preflight(token_file)
            if doc is not None and doc["preflightId"] == preflight_id and doc.get("announced") is not True:
                save_preflight(token_file, dict(doc, announced=True))
    except (PlatformRefusal, OSError) as exc:
        log_stderr("monitor runner: cannot mark the preflight result announced (%s)" % getattr(exc, "reason", exc))


def ledger_tails_at_start(env):
    """The start of every monitor appends the deferred journal tails of the
    user's monitors (P-12). The start does not depend on them: a ledger or a
    journal that refuses leaves the tails for a later try."""
    try:
        append_tails(env)
    except LedgerUnavailable:
        pass


def sweep_stale_locks(env, keep):
    """Remove the files of locks/ older than STALE_LOCK_AGE_SEC that no
    process holds (P-5), keep (the caller's own lock) aside. A file is
    removed only while this process holds its lock and the path still names
    the file it locked, the identity rule of acquire_runner_lock: a process
    that opened it meanwhile finds the path renewed and starts over."""
    if _detach_fcntl is None:
        return
    try:
        locks = os.path.join(user_dir(env), LOCKS_DIR_NAME)
        names = os.listdir(locks)
    except (PlatformRefusal, OSError):
        return
    now = time.time()
    for name in names:
        path = os.path.join(locks, name)
        if not name.endswith(".lock") or path == keep:
            continue
        try:
            fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0))
        except OSError:
            continue
        try:
            st = os.fstat(fd)
            if (not stat.S_ISREG(st.st_mode) or st.st_uid != os.getuid()
                    or now - st.st_mtime < STALE_LOCK_AGE_SEC):
                continue
            try:
                _detach_fcntl.flock(fd, _detach_fcntl.LOCK_EX | _detach_fcntl.LOCK_NB)
            except OSError:
                continue
            try:
                cur = os.lstat(path)
            except OSError:
                continue
            if (cur.st_dev, cur.st_ino) == (st.st_dev, st.st_ino):
                try:
                    os.unlink(path)
                except OSError:
                    pass
        finally:
            os.close(fd)


def read_token_once(path):
    """Open the token file, check it, read it and close the descriptor (the
    one-runner lock of client mode is locks/<key>.lock, not the token file).
    Raises Stop like open_token."""
    fd = open_token_checked(path)
    try:
        chunks = []
        while True:
            chunk = os.read(fd, 65536)
            if not chunk:
                break
            chunks.append(chunk)
        token = b"".join(chunks).decode("utf-8").strip()
    except (OSError, UnicodeDecodeError):
        raise Stop("token_file_missing")
    finally:
        os.close(fd)
    if not token:
        raise Stop("token_file_missing", delete_token=True)
    return token


class _HeldSignals(object):
    """SIGTERM and SIGINT held back while the detached child is started and
    its receipt read: a signal then cannot leave a started child unknown to
    the agent. A held signal arrives on exit, when the start is decided."""

    def __enter__(self):
        if hasattr(signal, "pthread_sigmask"):
            signal.pthread_sigmask(signal.SIG_BLOCK, _START_SIGNALS)
        return self

    def __exit__(self, *exc):
        if hasattr(signal, "pthread_sigmask"):
            signal.pthread_sigmask(signal.SIG_UNBLOCK, _START_SIGNALS)
        return False


def start_detached(token_file, issue):
    """Start the detached child and print its receipt; returns the exit code.
    Raises Stop or ConfigInvalid for a start that did not reach the child.

    The child waits for the end of its stdin before its first poll, and this
    process closes it only after the receipt is printed: monitor_started is
    printed while the child is alive, holds the lock and has not polled. A
    receipt that is not a line of the protocol, or one this process cannot
    print, kills the child."""
    body = runner_body()
    if body is None:
        log_stderr("monitor runner: the loader did not keep the runner body (%s); "
                   "the detached child cannot be started" % RUNNER_BODY_NAME)
        raise Stop("internal_error")
    data = body.encode("utf-8")
    printed = []

    def on_receipt(receipt):
        global _stopping
        line = _receipt_line(receipt)
        if line is None:
            # Silent or gone: it must not run unknown to the agent.
            log_stderr("monitor runner: the detached child gave no start receipt")
            return False
        # The start is decided: a signal held until now does not undo it.
        _stopping = True
        emit(line)
        printed.append(line)
        if line["event"] == "monitor_started" and "reason" in line:
            # The full line is out; the child waits until this returns.
            mark_announced(token_file, line["preflightId"])
        return True

    with _HeldSignals():
        try:
            current_platform().spawn_detached(
                [sys.executable, "-c", DETACHED_CHILD_SRC], b"%d\n" % len(data) + data,
                dict(os.environ), START_RECEIPT_TIMEOUT_SEC, on_receipt)
        except PlatformRefusal as exc:
            raise ConfigInvalid("SKEIN_PLATFORM", "a detached start is not available here (%s)" % exc.reason)
    if not printed:
        raise Stop("internal_error")
    line = printed[0]
    if line["event"] == "monitor_started":
        return 0
    return STOPS[line["reason"]][1]


def _receipt_line(receipt):
    """The receipt as a line to print: monitor_started or a stop line of the
    stop table; None for anything else."""
    if receipt is None:
        return None
    try:
        line = json.loads(receipt)
    except ValueError:
        return None
    if not isinstance(line, dict):
        return None
    if line.get("event") == "monitor_started":
        return line
    if line.get("reason") in STOPS and STOPS[line["reason"]][0] == line.get("event"):
        return line
    return None


def _send_receipt(line):
    """Hand the start receipt to the synchronous process and wait until it
    has printed it (it closes this process's stdin then, or ends), then
    point stdout and stdin at the null device: nothing reads or writes them
    any more."""
    try:
        emit(line)
        failed = False
    except OSError:
        failed = True
    if not failed:
        try:
            while sys.stdin.buffer.read(65536):
                pass
        except (OSError, ValueError):
            pass
    devnull = os.open(os.devnull, os.O_RDWR)
    try:
        os.dup2(devnull, sys.stdout.fileno())
        os.dup2(devnull, sys.stdin.fileno())
    finally:
        os.close(devnull)
    if failed:
        # The synchronous process is gone: no one was told of this start.
        raise StopSignal()


class JournalSink(object):
    """Where a detached or journal monitor puts its lines: the journal."""

    def __init__(self, journal, method, channel):
        self.journal = journal
        self.method = method
        self.channel = channel

    def lifecycle(self, line):
        fields = dict(line)
        event = fields.pop("event")
        try:
            self.journal.lifecycle(event, **fields)
        except JournalUnavailable:
            pass

    def notification(self, n):
        # Raises JournalUnavailable: the page is not done, the cursor stays.
        self.journal.delivery(n, JOURNAL_ONLY_STATUS, method=self.method, channel=self.channel)


def delivery_sink(journal, token_file, doc, env, base, issue, token, executor):
    """The DeliverySink of a detached monitor: the ledger of the server,
    the addressee of the preflight, this monitor keyed by its journal, and
    the poll of the access boundary on the runner's request executor."""
    monitor = os.path.splitext(os.path.basename(journal.path))[0]
    ledger = Ledger(base, monitor, identity_from_doc(doc.get("identity")), env=env)
    clock = SystemClock()
    cancel = PollCancel(executor, base, token, env.get("SKEIN_EXCLUDE_SESSION", ""), clock)
    return DeliverySink(journal, token_file, doc, load_registry(), env, ledger,
                        ArgvExecutor(token_file, env, [token]), issue, cancel=cancel, clock=clock)


def run_detached_child():
    """The detached child: start in the lock order, hand the receipt, then
    poll into the journal until a stop. Returns the exit code."""
    global _stopping
    if hasattr(signal, "pthread_sigmask"):
        signal.pthread_sigmask(signal.SIG_UNBLOCK, _START_SIGNALS)
    issue = os.environ.get("SKEIN_ISSUE", "")
    token_file = os.environ.get("SKEIN_TOKEN_FILE", "")
    held = WorkingStart()
    executor = Executor()
    sink = None
    outcome = None
    try:
        try:
            base = os.environ.get("SKEIN_PUBLIC_URL", "")
            if not token_file or not base:
                raise Stop("misconfigured", delete_token=bool(token_file))
            env = dict(os.environ)
            settings = read_settings(env, load_registry())
            held.begin(token_file, env, settings, issue)
            journal = Journal(token_file, issue, secrets=[held.token], env=env)
            # Ready means the journal took the monitor_started line: a
            # journal that refuses it is a refused start, before the receipt.
            fields = dict(held.line)
            try:
                journal.lifecycle(fields.pop("event"), **fields)
            except JournalUnavailable as exc:
                raise ConfigInvalid(exc.path, "the journal cannot be written (%s)" % exc.reason)
            if held.doc.get("method") == METHOD_DETACHED:
                sink = delivery_sink(journal, token_file, held.doc, env, base, issue, held.token, executor)
            else:
                sink = JournalSink(journal, held.doc.get("method"), held.doc.get("channel"))
            _send_receipt(held.line)
            work(executor, base, held.token, issue, os.environ.get("SKEIN_EXCLUDE_SESSION", ""),
                 token_file, sink=sink)
        except ConfigInvalid as exc:
            raise Stop("config_invalid", detail="%s (%s)" % (exc.variable, exc.detail))
    except Stop as stop:
        outcome = stop
    except StopSignal:
        outcome = Stop("stopped")
    except Exception:
        _stopping = True
        log_stderr("monitor runner: unexpected error\n" + traceback.format_exc())
        outcome = Stop("internal_error")
    finally:
        executor.stop()
    try:
        # Before the receipt this is the receipt; after it, stdout is null.
        if held.lock is None:
            return finish(outcome, issue, token_file)
        # P-12 under the one-runner lock, released last.
        return finish_client(outcome, issue, token_file, sink=sink,
                             preflight=(held.doc or {}).get("preflightId"))
    finally:
        held.release()


def run_client():
    """The working start of client mode; returns the exit code."""
    global _stopping
    issue = os.environ.get("SKEIN_ISSUE", "")
    token_file = os.environ.get("SKEIN_TOKEN_FILE", "")
    try:
        try:
            if not token_file or not os.environ.get("SKEIN_PUBLIC_URL", ""):
                raise Stop("misconfigured", delete_token=bool(token_file))
            env = dict(os.environ)
            settings = read_settings(env, load_registry())
            doc = check_start(token_file, env, settings)
            if doc["method"] in (METHOD_DETACHED, METHOD_JOURNAL):
                return start_detached(token_file, issue)
            return run_foreground(token_file, issue, env, settings, doc)
        except ConfigInvalid as exc:
            raise Stop("config_invalid", detail="%s (%s)" % (exc.variable, exc.detail))
    except Stop as stop:
        outcome = stop
    except StopSignal:
        outcome = Stop("stopped")
    except Exception:
        _stopping = True
        log_stderr("monitor runner: unexpected error\n" + traceback.format_exc())
        outcome = Stop("internal_error")
    # A stream or exit-on-event result keeps its stop line within its line
    # limit even when the start failed before the result was checked.
    return finish(outcome, issue, token_file, write=limited_write(load_preflight(token_file) if token_file else None))


def run_foreground(token_file, issue, env, settings, doc=None):
    """stream and exit-on-event: the same lock order in this process, then
    the poll loop on stdout within the limits of the record's method (the
    modes_stream part). Returns the exit code."""
    global _stopping
    held = WorkingStart()
    executor = Executor()
    outcome = None
    output = None
    try:
        try:
            held.begin(token_file, env, settings, issue)
            limits = StreamLimits(held.doc.get("limits"))
            journal = Journal(token_file, issue, secrets=[held.token], env=env)
            output = StreamOutput(limits, journal, held.doc.get("method"), held.doc.get("journal"), issue=issue)
            if output.lifecycle(held.line) and held.full:
                mark_announced(token_file, held.doc["preflightId"])
            run_stream_loop(executor, os.environ.get("SKEIN_PUBLIC_URL", ""), held.token, issue,
                            os.environ.get("SKEIN_EXCLUDE_SESSION", ""), token_file, held.doc, output)
        except ConfigInvalid as exc:
            raise Stop("config_invalid", detail="%s (%s)" % (exc.variable, exc.detail))
    except Stop as stop:
        outcome = stop
    except StopSignal:
        outcome = Stop("stopped")
    except Exception:
        _stopping = True
        log_stderr("monitor runner: unexpected error\n" + traceback.format_exc())
        outcome = Stop("internal_error")
    finally:
        executor.stop()
    try:
        # Before its output exists (a refused begin) the stop line keeps to
        # the line limit of the result the synchronous check read.
        write = output.stop_line if output is not None else limited_write(held.doc or doc)
        if held.lock is None:
            return finish(outcome, issue, token_file, write=write)
        # P-12 under the one-runner lock, released last.
        return finish_client(outcome, issue, token_file, write=write,
                             preflight=(held.doc or {}).get("preflightId"))
    finally:
        held.release()
