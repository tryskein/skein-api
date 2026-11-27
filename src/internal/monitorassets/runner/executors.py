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
import re
import select
import shutil
import socket
import subprocess
import sys
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


def json_path_lookup(doc, path):
    """(True, value) at the dotted path of a JSON document, else (False,
    None). An element that is a decimal number indexes a list ("stages.1":
    its second element); in an object it is a key like any other."""
    for part in path.split("."):
        if isinstance(doc, dict) and part in doc:
            doc = doc[part]
        elif isinstance(doc, list) and part.isdigit() and int(part) < len(doc):
            doc = doc[int(part)]
        else:
            return False, None
    return True, doc


def json_path_value(doc, path):
    """The value at the dotted path of a JSON document as a string the
    outcome tables compare with (a string as is, a number, a boolean or null
    in its JSON form), or None when the path is absent or holds an object or
    a list."""
    found, doc = json_path_lookup(doc, path)
    if not found:
        return None
    if isinstance(doc, str):
        return doc
    if isinstance(doc, (dict, list)):
        return None
    return json.dumps(doc)


def outcome_sign_matches(match, exit_code=None, doc=None, http_status=None, stderr=None):
    """Whether an answer carries the sign of a table row: every criterion
    the row states holds. A row without a criterion matches nothing. stderr
    is what an argv program wrote there (bytes; None: no program ran)."""
    stated = False
    if match.get("stderrEmpty") is not None:
        stated = True
        if stderr is None or (len(stderr.strip()) == 0) != match["stderrEmpty"]:
            return False
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
    if match.get("stderrJson"):
        # The JSON document of stderr (a CLI that reports its errors there).
        stated = True
        err_doc = parse_json_object(stderr) if stderr is not None else None
        for path, want in match["stderrJson"].items():
            if err_doc is None or json_path_value(err_doc, path) != want:
                return False
    for path in match.get("jsonAbsent") or []:
        # Absent from a JSON answer: an answer that is no JSON document
        # carries no sign of its fields.
        stated = True
        if doc is None or json_path_lookup(doc, path)[0]:
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
    """A probe path: a literal, "~/" and literal elements (the home
    directory of the agent process), or "{env:NAME}" optionally followed by
    "/" and literal elements. ExecutorRefusal when the variable it needs is
    not set."""
    if value.startswith("~/"):
        home = env.get("HOME", "")
        if not home:
            raise ExecutorRefusal("variable HOME is not set")
        return os.path.join(home, value[2:])
    head, sep, rest = value.partition("/")
    if head.startswith("{env:"):
        base = env_value(head, env)
        return os.path.join(base, rest) if sep and rest else base
    return value


def probe_path(probe, env):
    """The path a dir-writable or socket-connect probe checks: its path, or
    the first of its order of paths whose variable is set. ExecutorRefusal
    when none resolves."""
    paths = probe.get("paths")
    if not paths:
        return expand_path_value(probe.get("path", ""), env)
    for value in paths:
        try:
            return expand_path_value(value, env)
        except ExecutorRefusal:
            continue
    raise ExecutorRefusal("no path of the probe resolves: %s" % ", ".join(paths))


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


# --- batch programs of Windows (US-11, INV-5) ---------------------------------
# cmd.exe parses the command line of a .cmd or .bat program by rules of its
# own (%, ^, &, |, <, >, !, quotes), which no quoting of Python makes safe.
# Such a program gets no byte of untrusted text in its arguments: every
# argument after the program is of a narrow character set, a message file
# goes by a path of ASCII letters, digits and \ : . _ - only, in the first
# private directory whose path is of that kind, and a text in an argument
# is the id-only form (text.py#ids_only_batch).

BATCH_EXTENSIONS = (".cmd", ".bat")
BATCH_SAFE_ARG = re.compile(r"\A[A-Za-z0-9 ._:\\/=,+-]*\Z")
BATCH_SAFE_PATH = re.compile(r"\A[A-Za-z0-9\\:._-]+\Z")
# The private subdirectory of the user directory for message files of
# batch programs, when the token directory has no ASCII path.
BATCH_MESSAGE_DIR_NAME = "messages"


def is_batch_program(path):
    """Whether the program at path is run by cmd.exe (Windows only)."""
    return (os.name == "nt" and isinstance(path, str)
            and os.path.splitext(path)[1].lower() in BATCH_EXTENSIONS)


def resolve_program(name, env, programs=None):
    """The path a channel program runs from: the path the preflight result
    names (programs), an absolute name, else the PATH of env; None when it
    is not found."""
    if programs and name in programs:
        return programs[name]
    if os.path.isabs(name):
        return name
    return shutil.which(name, path=env.get("PATH", os.defpath))


def batch_message_dir(token_file, env):
    """The directory of message files for a batch program: the first of the
    token directory and the private skein-monitor/messages whose absolute
    path is of BATCH_SAFE_PATH, or None when neither is."""
    first = message_dir(token_file)
    if BATCH_SAFE_PATH.match(first):
        return first
    try:
        second = os.path.join(user_dir(env), BATCH_MESSAGE_DIR_NAME)
    except PlatformRefusal:
        return None
    if not BATCH_SAFE_PATH.match(second):
        return None
    try:
        current_platform().ensure_private_dir(second)
    except PlatformRefusal:
        return None
    return second


def channel_takes_ids_only(channel, programs, env):
    """Whether the text of a channel goes as the id-only form: its program
    is a batch program and the text is an argument."""
    executor = channel.get("executor") or {}
    argv = executor.get("argv") or []
    if executor.get("feed") != "arg" or not argv:
        return False
    return is_batch_program(resolve_program(argv[0], env, programs if isinstance(programs, dict) else None))


def write_message_file(token_file, text, directory=None):
    """Write text into a new private file msg-<random> of the token
    directory (or of directory) and return its path. The file is created
    exclusively with mode 0600 and never follows a link."""
    directory = directory or message_dir(token_file)
    for _ in range(8):
        path = os.path.join(directory, MESSAGE_FILE_PREFIX + os.urandom(8).hex())
        try:
            fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
                         | getattr(os, "O_BINARY", 0), 0o600)
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
    if sys.version_info >= (3, 12):
        # Pipes of Windows can be set non-blocking from Python 3.12 on.
        return _run_polled(argv, env, stdin_data, timeout_sec)
    return _run_threads(argv, env, stdin_data, timeout_sec)


def _ended_unreaped(proc):
    """Whether the program has ended, leaving it unreaped where the system
    allows it (waitid with WNOWAIT): its pid, the id of its group, cannot
    be reused before the group is killed in the clean-up."""
    if hasattr(os, "waitid") and hasattr(os, "WNOWAIT"):
        try:
            return os.waitid(os.P_PID, proc.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT) is not None
        except ChildProcessError:
            pass
    return proc.poll() is not None


def _run_posix(argv, env, stdin_data, timeout_sec):
    proc = None
    group = None
    timed_out = False
    try:
        # The clean-up is armed before the program starts: a stop signal
        # right after it started, or during the set-up of its pipes, still
        # kills its group.
        try:
            proc = subprocess.Popen(argv, stdin=subprocess.PIPE if stdin_data is not None else subprocess.DEVNULL,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, close_fds=True,
                                    start_new_session=True)
        except OSError as exc:
            raise ExecutorRefusal("the program cannot be started (%s)" % (exc.strerror or exc))
        # The program leads its own session and group: the group is named by
        # its pid while any member is left.
        group = proc.pid
        out_fd, err_fd = proc.stdout.fileno(), proc.stderr.fileno()
        chunks = {out_fd: [], err_fd: []}
        sizes = {out_fd: 0, err_fd: 0}
        cut = {out_fd: False, err_fd: False}
        reading = set([out_fd, err_fd])
        in_fd = proc.stdin.fileno() if stdin_data is not None else None
        pending = memoryview(stdin_data) if stdin_data is not None else None
        ended_at = None
        for fd in list(reading) + ([in_fd] if in_fd is not None else []):
            os.set_blocking(fd, False)
        deadline = time.monotonic() + max(timeout_sec, 0)
        while True:
            now = time.monotonic()
            if ended_at is None and _ended_unreaped(proc):
                ended_at = now
            if ended_at is None and now >= deadline:
                timed_out = True
                current_platform().kill_group(group)
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
        if group is not None:
            current_platform().kill_group(group)
        raise
    finally:
        # Nothing of the call outlives it: the descendants of the program go
        # with its group, whether or not they still hold its pipes.
        if group is not None:
            current_platform().kill_group(group)
        for f in ((proc.stdin, proc.stdout, proc.stderr) if proc is not None else ()):
            if f is not None:
                try:
                    f.close()
                except OSError:
                    pass
        if proc is not None and proc.returncode is None:
            proc.wait()
    return CallResult(None if timed_out else proc.returncode, b"".join(chunks[out_fd]), b"".join(chunks[err_fd]),
                      timed_out, cut[out_fd])


def _run_polled(argv, env, stdin_data, timeout_sec):
    """The wait of a platform without select on pipes whose pipes can be set
    non-blocking (Windows, Python 3.12+): every _WAIT_STEP_SEC the pipes
    are read and written without blocking. The processes of the call are
    one tree (_ProcessTree, a job object on Windows): it is killed on the
    timeout, at the end of the call and on any error or stop signal. The
    output is read for one grace after the program ended, so a call lasts
    at most its timeout plus one grace."""
    tree = None
    proc = None
    timed_out = False
    try:
        try:
            proc = subprocess.Popen(argv, stdin=subprocess.PIPE if stdin_data is not None else subprocess.DEVNULL,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, close_fds=True,
                                    **current_platform().tree_popen_options())
        except OSError as exc:
            raise ExecutorRefusal("the program cannot be started (%s)" % (exc.strerror or exc))
        tree = _ProcessTree(proc)
        out_fd, err_fd = proc.stdout.fileno(), proc.stderr.fileno()
        chunks = {out_fd: [], err_fd: []}
        sizes = {out_fd: 0, err_fd: 0}
        cut = {out_fd: False, err_fd: False}
        reading = set([out_fd, err_fd])
        in_fd = proc.stdin.fileno() if stdin_data is not None else None
        pending = memoryview(stdin_data) if stdin_data is not None else None
        for fd in list(reading) + ([in_fd] if in_fd is not None else []):
            os.set_blocking(fd, False)
        tree.start()
        ended_at = None
        deadline = time.monotonic() + max(timeout_sec, 0)
        while True:
            now = time.monotonic()
            if ended_at is None and proc.poll() is not None:
                ended_at = now
            if ended_at is None and now >= deadline:
                timed_out = True
                tree.kill()
                proc.wait()
                ended_at = time.monotonic()
            if ended_at is not None and (not reading or time.monotonic() >= ended_at + _READER_GRACE_SEC):
                break
            progress = False
            for fd in list(reading):
                try:
                    chunk = os.read(fd, 65536)
                except BlockingIOError:
                    continue
                except OSError:
                    chunk = b""
                if not chunk:
                    reading.discard(fd)
                    continue
                progress = True
                room = OUTPUT_CAP_BYTES - sizes[fd]
                if len(chunk) > room:
                    cut[fd] = True
                if room > 0:
                    chunks[fd].append(chunk[:room])
                    sizes[fd] += min(len(chunk), room)
            if in_fd is not None and ended_at is None:
                try:
                    # A non-blocking pipe of Windows may take part of a
                    # write, or none of it.
                    written = os.write(in_fd, pending[:4096])
                    pending = pending[written:]
                    progress = progress or written > 0
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
            if not progress:
                time.sleep(_WAIT_STEP_SEC)
    except BaseException:
        # A stop signal (or any error) during the call: the program and its
        # descendants do not outlive it.
        if tree is not None:
            tree.kill()
        elif proc is not None:
            _kill_quietly(proc)
        raise
    finally:
        if tree is not None:
            tree.kill()
        for f in ((proc.stdin, proc.stdout, proc.stderr) if proc is not None else ()):
            if f is not None:
                try:
                    f.close()
                except OSError:
                    pass
        if proc is not None and proc.returncode is None:
            try:
                proc.wait(timeout=_READER_GRACE_SEC)
            except subprocess.TimeoutExpired:
                pass
    return CallResult(None if timed_out else proc.returncode, b"".join(chunks[out_fd]), b"".join(chunks[err_fd]),
                      timed_out, cut[out_fd])


def _run_threads(argv, env, stdin_data, timeout_sec):
    """The wait of a platform without select on pipes: reader threads. The
    processes of the call are one tree (_ProcessTree): it is killed at the
    end of the call, on its timeout and on any error or stop signal, so no
    descendant outlives it. A pipe a reader still blocks on is never closed
    (closing waits for the reader): the call returns within timeout_sec plus
    the grace of the readers whatever the descendants do."""
    extra = current_platform().tree_popen_options()
    tree = None
    proc = None
    timed_out = False
    readers = []
    try:
        try:
            # On Windows the program starts suspended: it runs only once it
            # is in its job object, so no descendant is born outside it.
            proc = subprocess.Popen(argv, stdin=subprocess.PIPE if stdin_data is not None else subprocess.DEVNULL,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, close_fds=True,
                                    **extra)
        except OSError as exc:
            raise ExecutorRefusal("the program cannot be started (%s)" % (exc.strerror or exc))
        tree = _ProcessTree(proc)
        readers = [_Reader(proc.stdout), _Reader(proc.stderr)]
        for r in readers:
            r.start()
        if stdin_data is not None:
            _Writer(proc.stdin, stdin_data).start()
        tree.start()
        try:
            proc.wait(timeout=max(timeout_sec, 0))
        except subprocess.TimeoutExpired:
            timed_out = True
    finally:
        # The tree goes first, then the readers get one grace together: a
        # call lasts at most its timeout plus one grace.
        if tree is not None:
            tree.kill()
        elif proc is not None:
            _kill_quietly(proc)
        if proc is not None and proc.returncode is None:
            try:
                proc.wait(timeout=_READER_GRACE_SEC)
            except subprocess.TimeoutExpired:
                pass
    deadline = time.monotonic() + _READER_GRACE_SEC
    for r in readers:
        r.join(max(deadline - time.monotonic(), 0))
    for r, pipe in zip(readers, (proc.stdout, proc.stderr)):
        if not r.is_alive():
            try:
                pipe.close()
            except OSError:
                pass
    return CallResult(None if timed_out else proc.returncode, readers[0].data(), readers[1].data(), timed_out,
                      readers[0].cut)


def _kill_quietly(proc):
    try:
        proc.kill()
    except OSError:
        pass


class _ProcessTree(object):
    """The processes of one call, by the platform layer (tree_*): on POSIX
    the session the program leads, on Windows a job object the program is
    assigned to while it is still suspended, so every descendant is born
    in it; kill() ends them all. On Windows a job that cannot be made, or a
    program that cannot be resumed in it, is a refusal before send
    (ExecutorRefusal)."""

    def __init__(self, proc):
        self.proc = proc
        self.job = current_platform().tree_make(proc)

    def start(self):
        """Let the program run (Windows: resume it inside its job)."""
        current_platform().tree_start(self.proc, self.job)

    def kill(self):
        job, self.job = self.job, None
        current_platform().tree_kill(self.proc, job)


class ArgvExecutor(object):
    """Delivers a text through the argv executor of a channel record."""

    def __init__(self, token_file, env, secrets, programs=None):
        self.token_file = token_file
        self.env = env
        self.secrets = list(secrets)
        # {name: absolute path}: programs of the preflight result that run
        # from a path of the agent process (preflight.json "programs").
        self.programs = {}
        if isinstance(programs, dict):
            for name, path in programs.items():
                if isinstance(name, str) and isinstance(path, str) and os.path.isabs(path):
                    self.programs[name] = path

    def call(self, executor, text, timeout_sec):
        """Hand text to the channel program by the record's feed. Returns a
        CallResult; raises ExecutorRefusal when nothing was handed over."""
        feed = executor.get("feed")
        text_file = None
        env = child_env(self.env, self.secrets)
        text = redact_text(text, self.secrets) if text is not None else None
        program = resolve_program((executor.get("argv") or [""])[0], env, self.programs)
        batch = is_batch_program(program)
        try:
            if feed == "file":
                directory = None
                if batch:
                    directory = batch_message_dir(self.token_file, self.env)
                    if directory is None:
                        raise ExecutorRefusal("no private message directory of an ASCII path for a .cmd program")
                text_file = write_message_file(self.token_file, text, directory)
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
            if argv and argv[0] in self.programs:
                argv[0] = self.programs[argv[0]]
            if batch:
                if text_file is not None and not BATCH_SAFE_PATH.match(text_file):
                    raise ExecutorRefusal("the message path of a .cmd program is not ASCII")
                if not all(BATCH_SAFE_ARG.match(a) for a in argv[1:]):
                    raise ExecutorRefusal("an argument of a .cmd program holds characters cmd.exe interprets")
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
                path = probe_path(probe, env)
                return PROBE_AVAILABLE if os.path.isdir(path) and dir_writable(path) else PROBE_UNAVAILABLE
            if kind == "socket-connect":
                return self._socket(probe_path(probe, env))
            if kind == "tcp-reach":
                return self._tcp(env_value(probe.get("address", ""), env))
        except ExecutorRefusal as exc:
            raise ProbeError(exc.reason)
        raise ProbeError("unknown probe type %s" % kind)

    def _argv(self, channel, probe, env):
        timeout = min((channel.get("executor") or {}).get("timeoutSec") or PROBE_TIMEOUT_SEC, PROBE_TIMEOUT_SEC)
        clean = child_env(env, self.secrets)
        argv = expand_argv(probe.get("argv") or [], clean)
        # The program of the channel by the path it is found at (a path under
        # the home directory among them), as the working runner runs it.
        programs = channel_programs(channel, env, None)
        if argv and argv[0] in programs:
            argv[0] = programs[argv[0]]
        result = run_program(argv, clean, None, timeout, self.secrets)
        if result.timed_out:
            raise ProbeError("timed out after %d s" % timeout)
        for row in probe.get("results") or []:
            if outcome_sign_matches(row.get("match") or {}, exit_code=result.exit_code, doc=result.json,
                                    stderr=result.stderr):
                return row.get("result")
        return PROBE_UNAVAILABLE

    def capture(self, channel, env):
        """The values the addressee command of the channel gives at its
        capture paths, [[path, value], ...], None for a path its answer does
        not hold (it must stay absent): the command answers "same" with a
        JSON document, else ProbeError."""
        command = (channel.get("addressee") or {}).get("command") or {}
        timeout = min((channel.get("executor") or {}).get("timeoutSec") or PROBE_TIMEOUT_SEC, PROBE_TIMEOUT_SEC)
        clean = child_env(env, self.secrets)
        try:
            argv = expand_argv(command.get("argv") or [], clean)
        except ExecutorRefusal as exc:
            raise ProbeError(exc.reason)
        programs = channel_programs(channel, env, None)
        if argv and argv[0] in programs:
            argv[0] = programs[argv[0]]
        result = run_program(argv, clean, None, timeout, self.secrets)
        if result.timed_out:
            raise ProbeError("timed out after %d s" % timeout)
        verdict = None
        for row in command.get("results") or []:
            if outcome_sign_matches(row.get("match") or {}, exit_code=result.exit_code, doc=result.json,
                                    stderr=result.stderr):
                verdict = row.get("result")
                break
        if verdict != "same":
            raise ProbeError("the addressee check does not answer same")
        if result.json is None:
            raise ProbeError("the addressee check gives no JSON answer")
        return [[path, json_path_value(result.json, path)] for path in command.get("capture") or []]

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
