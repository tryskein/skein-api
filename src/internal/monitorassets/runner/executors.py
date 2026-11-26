# --- executors: the transport of a channel and its probes (P-7) --------------
# An argv executor runs the program of a channel record with an argument
# list, never through a shell (INV-5, R-8). The text of an event reaches the
# program by the record's feed only:
#   stdin  written to the program's stdin;
#   file   a private file msg-<random> (0600) in the token directory whose
#          path is the {textFile} element; removed after the call, and the
#          ones a dead monitor left are removed when a monitor of the token
#          starts;
#   arg    the {text} element, one element of the argument list.
# An element of the argument list is a literal or exactly one placeholder:
# {text}, {textFile} or {env:NAME} (a variable of the agent process). The
# text never becomes another element, a flag or a path: a text that would
# read as an option (it starts with "-") is passed only behind a "--"
# element, otherwise the call is refused before it runs.
#
# No secret of the monitor (the token, INV-6) goes to a channel by any way:
# the program gets the environment of the agent process (the runner's own,
# inherited from the agent's shell) without any variable whose name or
# value holds one; {env:NAME} is taken from that environment, so a variable
# holding a secret is not there; the text of an event (stdin, the message
# file, {text}) has every secret replaced as the journal replaces it; and an
# argument or stdin that still carries one is refused before the program
# runs. A call is bounded by its timeout,
# which the caller keeps within the idle window of the token; on timeout the
# program and the processes of its session are killed. On POSIX one loop
# waits on the program's pipes without blocking reads, so neither a program
# that writes much or reads nothing nor a descendant that keeps a pipe open
# holds the runner past the timeout and a short grace after the program
# ends; the processes left in the program's group then are killed, and so
# are all of them when the call ends by an exception (a stop signal). At
# most OUTPUT_CAP_BYTES of stdout and of stderr are kept. A stdout that is
# one JSON object is parsed for the outcome table (Match.json).
#
# Probes check the boundary of a channel without delivering (P-7): argv (the
# command without the text of an event: no stdin, no message file),
# dir-writable (create and remove a probe file), socket-connect (connect to a
# unix socket or a named pipe and close it without sending a byte), tcp-reach
# (open a TCP connection and close it). A probe confirms that the boundary
# is reachable from the sandbox, not that a message would be accepted.

import json
import os
import select
import signal
import socket
import subprocess
import threading
import time

# The prefix of a message file in the token directory.
MESSAGE_FILE_PREFIX = "msg-"
# How much of stdout and of stderr a call keeps.
OUTPUT_CAP_BYTES = 64 * 1024
# The longest a probe of the preflight may take: the preflight is
# synchronous, an agent waits for its line.
PROBE_TIMEOUT_SEC = 10
# How long the pipes are read after the program ended or was killed: a
# descendant that keeps a pipe open does not hold the runner longer.
_READER_GRACE_SEC = 1.0
# The step of the POSIX wait loop (the program's end is seen within it).
_WAIT_STEP_SEC = 0.05


class ExecutorRefusal(Exception):
    """A call that is not made: a variable the argument list needs is not
    set, the text would read as an option, the message file cannot be
    written, or the program cannot be started. Nothing was handed to the
    channel."""

    def __init__(self, reason):
        Exception.__init__(self, reason)
        self.reason = reason


class CallResult(object):
    """What a call of a channel program gave: its exit code (None when it
    was killed on timeout), stdout and stderr as bytes (capped), the JSON
    object stdout held (None when it held none, or when stdout was cut at
    the cap: a prefix of a longer answer is not that answer), whether the
    timeout ended it and whether stdout was cut."""

    def __init__(self, exit_code, stdout, stderr, timed_out, truncated=False):
        self.exit_code = exit_code
        self.stdout = stdout
        self.stderr = stderr
        self.timed_out = timed_out
        self.truncated = truncated
        self.json = None if truncated else parse_json_object(stdout)


def parse_json_object(raw):
    """The JSON object raw (bytes) holds, or None."""
    try:
        doc = json.loads(raw.decode("utf-8").strip())
    except (UnicodeDecodeError, ValueError):
        return None
    return doc if isinstance(doc, dict) else None


def json_path_value(doc, path):
    """The value at the dotted path of a JSON document as a string the
    outcome tables compare with (a string as is, a number, a boolean or null
    in its JSON form), or None when the path is absent."""
    for part in path.split("."):
        if not isinstance(doc, dict) or part not in doc:
            return None
        doc = doc[part]
    if isinstance(doc, str):
        return doc
    if isinstance(doc, (dict, list)):
        return None
    return json.dumps(doc)


def outcome_sign_matches(match, exit_code=None, doc=None, http_status=None):
    """Whether an answer carries the sign of a table row: every criterion
    the row states holds. A row without a criterion matches nothing."""
    stated = False
    if match.get("exitCode") is not None:
        stated = True
        if exit_code is None or exit_code != match["exitCode"]:
            return False
    if match.get("httpStatus") is not None:
        stated = True
        if http_status is None or http_status != match["httpStatus"]:
            return False
    for path, want in (match.get("json") or {}).items():
        stated = True
        if doc is None or json_path_value(doc, path) != want:
            return False
    return stated


def env_value(element, env):
    """The value of a "{env:NAME}" element; ExecutorRefusal when NAME is not
    set. A literal is returned as is."""
    if element.startswith("{env:") and element.endswith("}"):
        name = element[len("{env:"):-1]
        value = env.get(name, "")
        if not value:
            raise ExecutorRefusal("variable %s is not set" % name)
        return value
    return element


def expand_path_value(value, env):
    """A probe path: a literal, or "{env:NAME}" optionally followed by "/"
    and literal elements."""
    head, sep, rest = value.partition("/")
    if head.startswith("{env:"):
        base = env_value(head, env)
        return os.path.join(base, rest) if sep and rest else base
    return value


def expand_argv(argv, env, text=None, text_file=None):
    """The argument list with its placeholders replaced. {text} becomes one
    element, the text itself; it must follow a "--" element when the text
    would read as an option."""
    out = []
    for element in argv:
        if element == "{text}":
            if text is None:
                raise ExecutorRefusal("the argument list takes no text here")
            if text.startswith("-") and (not out or out[-1] != "--"):
                raise ExecutorRefusal("the text would read as an option")
            out.append(text)
        elif element == "{textFile}":
            if text_file is None:
                raise ExecutorRefusal("the argument list takes no text file here")
            out.append(text_file)
        else:
            out.append(env_value(element, env))
    return out


def child_env(env, secrets):
    """The environment of the agent process without any variable whose name
    or value holds a secret of the monitor."""
    secrets = [s for s in secrets if s]
    return dict((k, v) for k, v in env.items()
                if not any(s in v or s in k for s in secrets))


def message_dir(token_file):
    return os.path.dirname(os.path.abspath(token_file))


def write_message_file(token_file, text):
    """Write text into a new private file msg-<random> of the token
    directory and return its path. The file is created exclusively with mode
    0600 and never follows a link."""
    directory = message_dir(token_file)
    for _ in range(8):
        path = os.path.join(directory, MESSAGE_FILE_PREFIX + os.urandom(8).hex())
        try:
            fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
        except FileExistsError:
            continue
        except OSError as exc:
            raise ExecutorRefusal("the message file cannot be written (%s)" % exc.strerror)
        try:
            try:
                view = memoryview(text.encode("utf-8"))
                while view:
                    view = view[os.write(fd, view):]
                os.fsync(fd)
            finally:
                os.close(fd)
        except OSError as exc:
            remove_quietly(path)
            raise ExecutorRefusal("the message file cannot be written (%s)" % exc.strerror)
        return path
    raise ExecutorRefusal("the message file cannot be written (no free name)")


def remove_quietly(path):
    try:
        os.remove(path)
    except OSError:
        pass


def sweep_message_files(token_file):
    """Remove the message files a monitor of this token left behind (a crash
    between writing and removing one). Called by a start that holds the
    one-runner lock of the token, so no live call of the token uses them."""
    directory = message_dir(token_file)
    try:
        names = os.listdir(directory)
    except OSError:
        return
    for name in names:
        if name.startswith(MESSAGE_FILE_PREFIX):
            remove_quietly(os.path.join(directory, name))


class _Reader(threading.Thread):
    """Reads a pipe to its end, keeping at most OUTPUT_CAP_BYTES."""

    def __init__(self, pipe):
        threading.Thread.__init__(self)
        self.daemon = True
        self.pipe = pipe
        self.chunks = []
        self.size = 0
        self.cut = False

    def run(self):
        try:
            while True:
                chunk = self.pipe.read1(65536) if hasattr(self.pipe, "read1") else self.pipe.read(65536)
                if not chunk:
                    return
                room = OUTPUT_CAP_BYTES - self.size
                if len(chunk) > room:
                    self.cut = True
                if room > 0:
                    self.chunks.append(chunk[:room])
                    self.size += min(len(chunk), room)
        except (OSError, ValueError):
            return

    def data(self):
        return b"".join(self.chunks)


class _Writer(threading.Thread):
    """Writes data to a pipe and closes it; a program that does not read is
    not waited on past the call."""

    def __init__(self, pipe, data):
        threading.Thread.__init__(self)
        self.daemon = True
        self.pipe = pipe
        self.data = data

    def run(self):
        try:
            try:
                self.pipe.write(self.data)
            finally:
                self.pipe.close()
        except (OSError, ValueError):
            return


def redact_text(text, secrets):
    """text with every secret replaced, as the journal does."""
    return _redact(text, [s for s in secrets if s])


def run_program(argv, env, stdin_data, timeout_sec, secrets=()):
    """Run argv (no shell) with env, stdin_data (bytes, or None for no
    stdin) and a timeout. Returns a CallResult; raises ExecutorRefusal when
    the program cannot be started, or when an argument or stdin carries one
    of secrets (env is the caller's child_env). Returns within timeout_sec
    plus _READER_GRACE_SEC on POSIX whatever the program's descendants do."""
    for s in secrets:
        if s and (any(s in a for a in argv) or (stdin_data is not None and s.encode("utf-8") in stdin_data)):
            raise ExecutorRefusal("the call would carry a secret of the monitor")
    if os.name == "posix":
        return _run_posix(argv, env, stdin_data, timeout_sec)
    return _run_threads(argv, env, stdin_data, timeout_sec)


def _kill_group(pgid):
    try:
        os.killpg(pgid, signal.SIGKILL)
    except OSError:
        pass


def _run_posix(argv, env, stdin_data, timeout_sec):
    try:
        proc = subprocess.Popen(argv, stdin=subprocess.PIPE if stdin_data is not None else subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, close_fds=True,
                                start_new_session=True)
    except OSError as exc:
        raise ExecutorRefusal("the program cannot be started (%s)" % (exc.strerror or exc))
    # The program leads its own session and group: the group is named by
    # its pid while any member is left. The clean-up is armed from here: the
    # set-up of the pipes below is inside it.
    group = proc.pid
    out_fd, err_fd = proc.stdout.fileno(), proc.stderr.fileno()
    chunks = {out_fd: [], err_fd: []}
    sizes = {out_fd: 0, err_fd: 0}
    cut = {out_fd: False, err_fd: False}
    reading = set([out_fd, err_fd])
    in_fd = proc.stdin.fileno() if stdin_data is not None else None
    pending = memoryview(stdin_data) if stdin_data is not None else None
    ended_at = None
    timed_out = False
    try:
        for fd in list(reading) + ([in_fd] if in_fd is not None else []):
            os.set_blocking(fd, False)
        deadline = time.monotonic() + max(timeout_sec, 0)
        while True:
            now = time.monotonic()
            if ended_at is None and proc.poll() is not None:
                ended_at = now
            if ended_at is None and now >= deadline:
                timed_out = True
                _kill_group(group)
                proc.wait()
                ended_at = time.monotonic()
            if ended_at is not None and (not reading or time.monotonic() >= ended_at + _READER_GRACE_SEC):
                break
            writing = [in_fd] if in_fd is not None and ended_at is None else []
            try:
                ready_r, ready_w, _ = select.select(list(reading), writing, [], _WAIT_STEP_SEC)
            except InterruptedError:
                continue
            for fd in ready_r:
                try:
                    chunk = os.read(fd, 65536)
                except BlockingIOError:
                    continue
                except OSError:
                    chunk = b""
                if not chunk:
                    reading.discard(fd)
                    continue
                room = OUTPUT_CAP_BYTES - sizes[fd]
                if len(chunk) > room:
                    cut[fd] = True
                if room > 0:
                    chunks[fd].append(chunk[:room])
                    sizes[fd] += min(len(chunk), room)
            if ready_w:
                try:
                    pending = pending[os.write(in_fd, pending):]
                except BlockingIOError:
                    pass
                except OSError:
                    pending = memoryview(b"")
                if not pending:
                    proc.stdin.close()
                    in_fd = None
            if in_fd is not None and ended_at is not None:
                proc.stdin.close()
                in_fd = None
    except BaseException:
        # A stop signal (or any error) during the call: the program and its
        # group do not outlive it.
        _kill_group(group)
        raise
    finally:
        # Nothing of the call outlives it: the descendants of the program go
        # with its group, whether or not they still hold its pipes.
        _kill_group(group)
        for f in (proc.stdin, proc.stdout, proc.stderr):
            if f is not None:
                try:
                    f.close()
                except OSError:
                    pass
        if proc.returncode is None:
            proc.wait()
    return CallResult(None if timed_out else proc.returncode, b"".join(chunks[out_fd]), b"".join(chunks[err_fd]),
                      timed_out, cut[out_fd])


def _run_threads(argv, env, stdin_data, timeout_sec):
    """The wait of a platform without select on pipes: reader threads. The
    processes of the call are one tree (_ProcessTree): it is killed at the
    end of the call, on its timeout and on any error or stop signal, so no
    descendant outlives it. A pipe a reader still blocks on is never closed
    (closing waits for the reader): the call returns within timeout_sec plus
    the grace of the readers whatever the descendants do."""
    try:
        proc = subprocess.Popen(argv, stdin=subprocess.PIPE if stdin_data is not None else subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, close_fds=True,
                                start_new_session=(os.name == "posix"))
    except OSError as exc:
        raise ExecutorRefusal("the program cannot be started (%s)" % (exc.strerror or exc))
    tree = _ProcessTree(proc)
    timed_out = False
    readers = []
    try:
        readers = [_Reader(proc.stdout), _Reader(proc.stderr)]
        for r in readers:
            r.start()
        if stdin_data is not None:
            _Writer(proc.stdin, stdin_data).start()
        try:
            proc.wait(timeout=max(timeout_sec, 0))
        except subprocess.TimeoutExpired:
            timed_out = True
        for r in readers:
            r.join(_READER_GRACE_SEC)
    finally:
        tree.kill()
        if proc.returncode is None:
            proc.wait()
    for r in readers:
        # The tree is gone: its pipes are at their end.
        r.join(_READER_GRACE_SEC)
    for r, pipe in zip(readers, (proc.stdout, proc.stderr)):
        if not r.is_alive():
            try:
                pipe.close()
            except OSError:
                pass
    return CallResult(None if timed_out else proc.returncode, readers[0].data(), readers[1].data(), timed_out,
                      readers[0].cut)


class _ProcessTree(object):
    """The processes of one call: on POSIX the session the program leads,
    on Windows a job object the program is assigned to as it starts (its
    descendants join it); kill() ends them all."""

    def __init__(self, proc):
        self.proc = proc
        self.job = None
        if os.name != "posix":
            self.job = _windows_job(proc)

    def kill(self):
        if os.name == "posix":
            _kill_group(self.proc.pid)
            return
        if self.job is not None:
            import ctypes
            ctypes.windll.kernel32.TerminateJobObject(self.job, 1)
            ctypes.windll.kernel32.CloseHandle(self.job)
            self.job = None
        try:
            self.proc.kill()
        except OSError:
            pass


def _windows_job(proc):
    """A job object holding proc (and, from now on, its descendants), or
    None when it cannot be made (then only the program is killed)."""
    try:
        import ctypes
        kernel32 = ctypes.windll.kernel32
        job = kernel32.CreateJobObjectW(None, None)
        if not job:
            return None
        if not kernel32.AssignProcessToJobObject(job, int(proc._handle)):
            kernel32.CloseHandle(job)
            return None
        return job
    except (AttributeError, OSError, ValueError):
        return None


class ArgvExecutor(object):
    """Delivers a text through the argv executor of a channel record."""

    def __init__(self, token_file, env, secrets):
        self.token_file = token_file
        self.env = env
        self.secrets = list(secrets)

    def call(self, executor, text, timeout_sec):
        """Hand text to the channel program by the record's feed. Returns a
        CallResult; raises ExecutorRefusal when nothing was handed over."""
        feed = executor.get("feed")
        text_file = None
        env = child_env(self.env, self.secrets)
        text = redact_text(text, self.secrets)
        try:
            if feed == "file":
                text_file = write_message_file(self.token_file, text)
                argv = expand_argv(executor["argv"], env, text_file=text_file)
                stdin_data = None
            elif feed == "arg":
                argv = expand_argv(executor["argv"], env, text=text)
                stdin_data = None
            elif feed == "stdin":
                argv = expand_argv(executor["argv"], env)
                stdin_data = text.encode("utf-8")
            else:
                raise ExecutorRefusal("feed %r is not one of stdin, file, arg" % feed)
            return run_program(argv, env, stdin_data, timeout_sec, self.secrets)
        finally:
            if text_file is not None:
                remove_quietly(text_file)


class SystemProbeRunner(ProbeRunner):
    """Runs the probes of a channel for the preflight (P-7)."""

    def __init__(self, secrets=()):
        self.secrets = list(secrets)

    def run(self, channel, probe, env):
        kind = probe.get("type")
        try:
            if kind == "argv":
                return self._argv(channel, probe, env)
            if kind == "dir-writable":
                path = expand_path_value(probe.get("path", ""), env)
                return PROBE_AVAILABLE if os.path.isdir(path) and dir_writable(path) else PROBE_UNAVAILABLE
            if kind == "socket-connect":
                return self._socket(expand_path_value(probe.get("path", ""), env))
            if kind == "tcp-reach":
                return self._tcp(env_value(probe.get("address", ""), env))
        except ExecutorRefusal as exc:
            raise ProbeError(exc.reason)
        raise ProbeError("unknown probe type %s" % kind)

    def _argv(self, channel, probe, env):
        timeout = min((channel.get("executor") or {}).get("timeoutSec") or PROBE_TIMEOUT_SEC, PROBE_TIMEOUT_SEC)
        clean = child_env(env, self.secrets)
        argv = expand_argv(probe.get("argv") or [], clean)
        result = run_program(argv, clean, None, timeout, self.secrets)
        if result.timed_out:
            raise ProbeError("timed out after %d s" % timeout)
        for row in probe.get("results") or []:
            if outcome_sign_matches(row.get("match") or {}, exit_code=result.exit_code, doc=result.json):
                return row.get("result")
        return PROBE_UNAVAILABLE

    @staticmethod
    def _socket(path):
        if os.name == "posix":
            sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            try:
                sock.settimeout(PROBE_TIMEOUT_SEC)
                sock.connect(path)
            except OSError:
                return PROBE_UNAVAILABLE
            finally:
                sock.close()
            return PROBE_AVAILABLE
        try:
            # A named pipe: opening its client end connects, closing it
            # disconnects; nothing is written.
            with open(path, "r+b", buffering=0):
                pass
        except OSError:
            return PROBE_UNAVAILABLE
        return PROBE_AVAILABLE

    @staticmethod
    def _tcp(address):
        host, sep, port = address.rpartition(":")
        if not sep or not port.isdigit():
            raise ProbeError("address %r is not host:port" % address)
        host = host[1:-1] if host.startswith("[") and host.endswith("]") else host
        try:
            sock = socket.create_connection((host, int(port)), timeout=PROBE_TIMEOUT_SEC)
        except OSError:
            return PROBE_UNAVAILABLE
        sock.close()
        return PROBE_AVAILABLE
