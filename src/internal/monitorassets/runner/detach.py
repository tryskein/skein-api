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
# detached child, beyond the wait of a takeover for the lock.
START_RECEIPT_TIMEOUT_SEC = 30 + TAKEOVER_WAIT_SEC
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
        # A host message is a key of the preflight line only ("Runner
        # stdout lines"); here it is one of the warnings (INV-17).
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
        # The takeover request this start made, until its monitor_started is
        # out or the start fails (takeover.py).
        self.takeover = None
        self.token_file = None
        self.issue = None
        self.env = None

    def begin(self, token_file, env, settings, issue):
        """Take the locks in the order of P-6 and check the result again as
        the holder. A lock held by a runner whose addressee changed is taken
        over (takeover.py). Raises Stop or ConfigInvalid; the lock, once
        taken, stays held for the stop that follows."""
        self.token_file, self.issue, self.env = token_file, issue, env
        try:
            self.lock = take_runner_lock(token_file, env)
        except LockBusy:
            self.lock, self.takeover = take_over(token_file, env, settings, held=self)
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
        if addressee_changed(token_file):
            # This runner starts with the addressee of its own result.
            try:
                mark_addressee_changed(token_file, None, changed=False)
            except (OSError, PlatformRefusal) as exc:
                raise ConfigInvalid(state_file_path(token_file),
                                    "the monitor state cannot be written (%s)" % getattr(exc, "reason", exc))
        ledger_tails_at_start(env)
        self.doc = doc
        # The mark is set by whoever prints the full line, after printing it.
        self.full = doc.get("announced") is not True
        self.line = monitor_started_line(doc, issue, self.full)

    def started(self, journal):
        """monitor_started is out: a takeover is done - the journal says so
        and the request is removed."""
        if self.takeover is None:
            return
        try:
            journal.lifecycle("taken_over", preflightId=self.doc["preflightId"])
        except JournalUnavailable:
            pass
        # The old runner ends once it sees the mark (it removes the request).
        confirm_takeover(self.token_file, self.takeover)
        self.takeover = None

    def failed(self, outcome, journal=None):
        """The start ended (outcome) before its monitor_started: a takeover it
        began is not completed - the token and the state with the saved
        cursor stay, the journal says so. An old runner that ended on its
        own (its token is gone) leaves only its own stop line."""
        if self.takeover is None:
            return
        if outcome is None or outcome.reason != "token_file_missing":
            try:
                if journal is None:
                    journal = Journal(self.token_file, self.issue,
                                      secrets=monitor_secrets(self.token, self.env, load_registry(),
                                                             (self.doc or {}).get("record")), env=self.env)
                journal.lifecycle("takeover_incomplete", reason=outcome.reason if outcome else None)
            except (JournalUnavailable, OSError, PlatformRefusal):
                pass
        withdraw_takeover(self.token_file, self.takeover)
        self.takeover = None

    def retake(self, env):
        """Take the one-runner lock back after a takeover that did not come;
        True when taken."""
        try:
            self.lock = current_platform().acquire_runner_lock(self.token_file, 0, env)
        except (LockBusy, PlatformRefusal):
            return False
        # The successor cleared the mark of the changed addressee when it
        # took the lock; this runner still writes only the journal, so a
        # later start may take it over again.
        try:
            mark_addressee_changed(self.token_file, (self.doc or {}).get("preflightId"))
        except (OSError, PlatformRefusal) as exc:
            log_stderr("monitor runner: cannot restore the mark of the changed addressee (%s)"
                       % getattr(exc, "reason", exc))
        return True

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
    process holds (P-5), keep (the caller's own lock) aside. The platform
    layer removes a file only when no process holds it
    (remove_stale_lock)."""
    platform = current_platform()
    if not platform.has_file_locks():
        return
    try:
        locks = os.path.join(user_dir(env), LOCKS_DIR_NAME)
        names = os.listdir(locks)
    except (PlatformRefusal, OSError):
        return
    for name in names:
        path = os.path.join(locks, name)
        if not name.endswith(".lock") or path == keep:
            continue
        platform.remove_stale_lock(path, STALE_LOCK_AGE_SEC)


def _read_token_windows(path):
    try:
        raw = current_platform().read_token(path)
    except PlatformRefusal as exc:
        if exc.reason == "not_found":
            raise Stop("token_file_missing", delete_token=True)
        if exc.reason in ("open_to_others", "symlink", "foreign_owner"):
            raise Stop("token_file_insecure")
        raise Stop("token_file_missing")
    try:
        token = raw.decode("utf-8").strip()
    except UnicodeDecodeError:
        raise Stop("token_file_missing")
    if not token:
        raise Stop("token_file_missing", delete_token=True)
    return token


def read_token_once(path):
    """Open the token file, check it, read it and close the descriptor (the
    one-runner lock of client mode is locks/<key>.lock, not the token file).
    Raises Stop like open_token. On Windows the platform layer reads it
    through the one handle it checked by criterion (a) (P-9, US-11): the
    loader's check of an earlier open does not cover this one."""
    if os.name != "posix":
        return _read_token_windows(path)
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

    def __init__(self, journal, method, channel, env=None, exclusion=None):
        self.journal = journal
        self.method = method
        self.channel = channel
        # The exclusion of one's own echo in force (echo.py), or None.
        self.exclusion = exclusion
        # The environment of the deferred tails (a stop with the journal down).
        self.env = env
        self.tails = TailTimer(env) if env is not None else None

    def maybe_tails(self):
        """The periodic append of the user's deferred tails."""
        if self.tails is not None:
            self.tails.maybe()

    def lifecycle(self, line):
        fields = dict(line)
        event = fields.pop("event")
        try:
            self.journal.lifecycle(event, **fields)
        except JournalUnavailable:
            pass

    def notification(self, n):
        # Raises JournalUnavailable: the page is not done, the cursor stays.
        if self.exclusion is not None and self.exclusion.own(n):
            self.journal.delivery(n, OUTCOME_OWN_ECHO, method=self.method)
            return
        self.journal.delivery(n, JOURNAL_ONLY_STATUS, method=self.method, channel=self.channel)


def delivery_sink(journal, token_file, doc, env, base, issue, token, executor, exclusion):
    """The DeliverySink of a detached monitor: the ledger of the server,
    the addressee of the preflight, this monitor keyed by its journal, the
    poll of the access boundary on the runner's request executor, and the
    exclusion of one's own echo that every poll reads again."""
    monitor = os.path.splitext(os.path.basename(journal.path))[0]
    ledger = Ledger(base, monitor, identity_from_doc(doc.get("identity")), env=env)
    clock = SystemClock()
    cancel = PollCancel(executor, base, token, exclusion, clock)
    registry = load_registry()
    return DeliverySink(journal, token_file, doc, registry, env, ledger,
                        ChannelExecutor(token_file, env, [token], doc.get("programs") or {}, registry,
                                        doc.get("record")), issue,
                        cancel=cancel, clock=clock, exclusion=exclusion)


def run_detached_child():
    """The detached child: start in the lock order, hand the receipt, then
    poll into the journal until a stop. Returns the exit code."""
    if hasattr(signal, "pthread_sigmask"):
        signal.pthread_sigmask(signal.SIG_UNBLOCK, _START_SIGNALS)
    issue = os.environ.get("SKEIN_ISSUE", "")
    token_file = os.environ.get("SKEIN_TOKEN_FILE", "")
    held = WorkingStart()
    try:
        return _run_detached_child(held, issue, token_file)
    finally:
        # Last on every way out (a handover, a stop, an error past the
        # handlers), after every receipt: the programs of detached calls do
        # not outlive the monitor, and the lock goes after them.
        DETACHED_CALLS.close()
        held.release()


def _run_detached_child(held, issue, token_file):
    global _stopping
    executor = Executor()
    sink = None
    journal = None
    outcome = None
    try:
        try:
            base = os.environ.get("SKEIN_PUBLIC_URL", "")
            if not token_file or not base:
                held.lock = unowned_lock(token_file)
                raise Stop("misconfigured", delete_token=held.lock is not None)
            env = agent_env(os.environ, load_registry())
            settings = read_settings(env, load_registry())
            held.begin(token_file, env, settings, issue)
            journal = Journal(token_file, issue, secrets=monitor_secrets(held.token, env, load_registry(),
                                                                         held.doc.get("record")), env=env)
            # Ready means the journal took the monitor_started line: a
            # journal that refuses it is a refused start, before the receipt.
            fields = dict(held.line)
            try:
                journal.lifecycle(fields.pop("event"), **fields)
            except JournalUnavailable as exc:
                raise ConfigInvalid(exc.path, "the journal cannot be written (%s)" % exc.reason)
            exclusion = Exclusion(token_file, env.get("SKEIN_EXCLUDE_SESSION", ""))
            if held.doc.get("method") == METHOD_DETACHED:
                sink = delivery_sink(journal, token_file, held.doc, env, base, issue, held.token, executor,
                                     exclusion)
            else:
                sink = JournalSink(journal, held.doc.get("method"), held.doc.get("channel"), env,
                                   exclusion=exclusion)
            _send_receipt(held.line)
            held.started(journal)
            while True:
                try:
                    work(executor, base, held.token, issue, exclusion, token_file, sink=sink,
                         root=RootClosed(token_file))
                except TakenOver:
                    # A successor waits for the lock: every outcome, the
                    # cursor and the journal lines are written. Release the
                    # lock and end once the successor's monitor is out; go on
                    # if it did not come (P-12, takeover.py).
                    if hand_over(token_file, env, held.release, lambda: held.retake(env)):
                        executor.stop()
                        return 0
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
        # The stop has begun: no detached call is left running from now on
        # (a receipt that needs one is kept to its window).
        DETACHED_CALLS.closing = True
    held.failed(outcome, journal)
    # Before the receipt this is the receipt; after it, stdout is null.
    if held.lock is None:
        return finish(outcome, issue, token_file)
    # P-12 under the one-runner lock, released last.
    return finish_client(outcome, issue, token_file, sink=sink,
                         preflight=(held.doc or {}).get("preflightId"))


def run_client():
    """The working start of client mode; returns the exit code."""
    global _stopping
    issue = os.environ.get("SKEIN_ISSUE", "")
    token_file = os.environ.get("SKEIN_TOKEN_FILE", "")
    # The one-runner lock a misconfigured start took to delete the files.
    lock = None
    try:
        try:
            if not token_file or not os.environ.get("SKEIN_PUBLIC_URL", ""):
                lock = unowned_lock(token_file)
                raise Stop("misconfigured", delete_token=lock is not None)
            env = agent_env(os.environ, load_registry())
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
    try:
        return finish(outcome, issue, token_file,
                      write=limited_write(load_preflight(token_file) if token_file else None))
    finally:
        if lock is not None:
            lock.release()


def unowned_lock(token_file):
    """The one-runner lock of token_file when no runner holds it (taken
    without waiting), else None. A misconfigured start deletes the files of
    the token only under it: a live runner's files are not touched (P-5,
    P-12)."""
    if not token_file:
        return None
    try:
        return take_runner_lock(token_file, dict(os.environ))
    except (LockBusy, PlatformRefusal, OSError):
        return None


def run_foreground(token_file, issue, env, settings, doc=None):
    """stream and exit-on-event: the same lock order in this process, then
    the poll loop on stdout within the limits of the record's method (the
    modes_stream part). Returns the exit code."""
    held = WorkingStart()
    try:
        return _run_foreground(held, token_file, issue, env, settings, doc)
    finally:
        # Last on every way out, after every line of the stop: the programs
        # of detached calls do not outlive the monitor, and the lock goes
        # after them.
        DETACHED_CALLS.close()
        held.release()


def _run_foreground(held, token_file, issue, env, settings, doc):
    global _stopping
    executor = Executor()
    outcome = None
    output = None
    journal = None
    try:
        try:
            held.begin(token_file, env, settings, issue)
            limits = StreamLimits(held.doc.get("limits"))
            secrets = monitor_secrets(held.token, env, load_registry(), held.doc.get("record"))
            journal = Journal(token_file, issue, secrets=secrets, env=env)
            output = StreamOutput(limits, journal, held.doc.get("method"), issue=issue, secrets=secrets)
            # exit-on-event: monitor_started is part of the cycle's output,
            # which keeps room for its stop line from the start.
            output.keep_room(cycle_stop_lines(issue, held.doc["preflightId"]))
            if output.lifecycle(held.line) and held.full:
                mark_announced(token_file, held.doc["preflightId"])
            held.started(journal)
            run_stream_loop(executor, os.environ.get("SKEIN_PUBLIC_URL", ""), held.token, issue,
                            Exclusion(token_file, env.get("SKEIN_EXCLUDE_SESSION", "")), token_file,
                            held.doc, output, env=env)
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
        # The stop has begun: no detached call is left running from now on.
        DETACHED_CALLS.closing = True
    held.failed(outcome, journal)
    # Before its output exists (a refused begin) the stop line keeps to
    # the line limit of the result the synchronous check read.
    write = output.stop_line if output is not None else limited_write(held.doc or doc)
    if held.lock is None:
        return finish(outcome, issue, token_file, write=write)
    # P-12 under the one-runner lock, released last.
    return finish_client(outcome, issue, token_file, write=write,
                         preflight=(held.doc or {}).get("preflightId"))
