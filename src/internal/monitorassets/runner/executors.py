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
# An http executor sends one request of the record: its method, its URL (a
# template of literals and {env:NAME}: a placeholder that starts it is the
# base, taken as is; every later one is escaped as one element of the path)
# and a JSON object body whose field bodyField carries the text. The scheme
# is https, or http for localhost and 127.0.0.1 only. The key (the first
# variable of keyEnv that is set) goes as "Authorization: Bearer"; it is a
# secret of the monitor like the token: redacted from the text and taken
# out of the environment of every channel program. Redirects are not
# followed, so the key is never sent to another address: a 3xx is the
# answer, and a table without its row makes it "unknown". A variable of the
# URL or the key that is not set, a URL of another scheme, or a connection
# that is refused or whose host does not resolve hands nothing over (a
# refusal); a timeout or a failure after the connection is made is
# "unknown". The answer is the status and the JSON object of the body.
#
# Probes check the boundary of a channel without delivering (P-7): argv (the
# command without the text of an event: no stdin, no message file),
# dir-writable (create and remove a probe file), socket-connect (connect to a
# unix socket or a named pipe and close it without sending a byte), tcp-reach
# (open a TCP connection and close it). A probe confirms that the boundary
# is reachable from the sandbox, not that a message would be accepted.

import base64
import http.client
import json
import os
import re
import select
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse

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

    def __init__(self, exit_code, stdout, stderr, timed_out, truncated=False, tree_failure=None,
                 stderr_truncated=False, still_running=False):
        self.exit_code = exit_code
        # A detached call whose program had not exited when its time ran out
        # (registry.go#Executor.Detach): no exit code, no output yet.
        self.still_running = still_running
        self.stdout = stdout
        self.stderr = stderr
        # stderr was cut at the cap: a prefix of it is not its JSON, its
        # emptiness or its phrases, so it gives no sign (sign_stderr).
        self.stderr_truncated = stderr_truncated
        self.timed_out = timed_out
        self.truncated = truncated
        # Why the processes of the call could not all be killed (Windows: a
        # job object call failed), or None: such a call is unknown whatever
        # it answered, its descendants may still run.
        self.tree_failure = tree_failure
        self.json = None if truncated else parse_json_object(stdout)

    @property
    def sign_stderr(self):
        """stderr as an answer sign reads it: None when it was cut."""
        return None if self.stderr_truncated else self.stderr


def parse_json_object(raw):
    """The JSON object raw (bytes) holds, or None."""
    try:
        doc = json.loads(raw.decode("utf-8").strip())
    except (UnicodeDecodeError, ValueError):
        return None
    return doc if isinstance(doc, dict) else None


# An element of a dotted path that indexes a list: ASCII digits only (str
# isdigit takes "²", which int() refuses).
_ASCII_INDEX = re.compile(r"\A[0-9]+\Z")


def json_path_lookup(doc, path):
    """(True, value) at the dotted path of a JSON document, else (False,
    None). An element of ASCII digits indexes a list ("stages.1": its second
    element) and only a list: an object with the key "0" is no list, so an
    element read as an index never finds an object's key."""
    for part in path.split("."):
        if isinstance(doc, dict) and part in doc and not _ASCII_INDEX.match(part):
            doc = doc[part]
        elif isinstance(doc, list) and _ASCII_INDEX.match(part) and int(part) < len(doc):
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


def capture_value(doc, path):
    """The value at path of a JSON document as an addressee capture keeps
    it, each kind apart: a string as itself, any other value (null, a
    number, a boolean, an object, a list) as {"json": its canonical JSON},
    None only for a path the document does not hold."""
    found, value = json_path_lookup(doc, path)
    if not found:
        return None
    if isinstance(value, str):
        return value
    return {"json": json.dumps(value, sort_keys=True, separators=(",", ":"))}


def _wanted(want, env):
    """The value a JSON criterion compares with: a literal as is, or for
    "{env:NAME}" the value of that variable of the agent process (env); an
    unset one, or no env, is a value no answer has (None)."""
    if isinstance(want, str) and want.startswith("{env:") and want.endswith("}"):
        value = (env or {}).get(want[len("{env:"):-1], "")
        return value or None
    return want


def _json_type_is(value, kind):
    """Whether a JSON value is of the type a jsonType criterion names."""
    if kind == "object":
        return isinstance(value, dict)
    if kind == "list":
        return isinstance(value, list)
    if kind == "string":
        return isinstance(value, str)
    if kind == "boolean":
        return isinstance(value, bool)
    if kind == "number":
        return isinstance(value, (int, float)) and not isinstance(value, bool)
    return False


def outcome_sign_matches(match, exit_code=None, doc=None, http_status=None, stderr=None, env=None,
                         still_running=False):
    """Whether an answer carries the sign of a table row: every criterion
    the row states holds. A row without a criterion matches nothing. stderr
    is what an argv program wrote there (bytes; None: no program ran). A
    JSON value "{env:NAME}" is the value of that variable in env: the
    answer of a check names the very id the delivery addresses.
    still_running: a detached call whose program has not exited, which
    only the row stillRunning matches."""
    if bool(match.get("stillRunning")) != bool(still_running):
        return False
    stated = bool(still_running)
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
        want = _wanted(want, env)
        if doc is None or want is None or json_path_value(doc, path) != want:
            return False
    for path, unwanted in (match.get("jsonNot") or {}).items():
        # The path holds a value compared as text, and another one (a check
        # names another addressee).
        stated = True
        unwanted = _wanted(unwanted, env)
        value = json_path_value(doc, path) if doc is not None else None
        if value is None or unwanted is None or value == unwanted:
            return False
    if match.get("stderrContains"):
        # A literal phrase of a CLI that reports its errors as text.
        stated = True
        if stderr is None or match["stderrContains"] not in stderr.decode("utf-8", "replace"):
            return False
    if match.get("stderrJson"):
        # The JSON document of stderr (a CLI that reports its errors there).
        stated = True
        err_doc = parse_json_object(stderr) if stderr is not None else None
        for path, want in match["stderrJson"].items():
            if err_doc is None or json_path_value(err_doc, path) != want:
                return False
    for path, kind in (match.get("jsonType") or {}).items():
        # The path holds a value of that JSON type (a null is none of them).
        stated = True
        found, value = json_path_lookup(doc, path) if doc is not None else (False, None)
        if not found or not _json_type_is(value, kind):
            return False
    for path in match.get("jsonAbsent") or []:
        # Absent from a JSON answer: an answer that is no JSON document
        # carries no sign of its fields.
        stated = True
        if doc is None or json_path_lookup(doc, path)[0]:
            return False
    return stated


# A literal prefix and one variable in one element (registry.go, argument
# list placeholders): "id:{env:KITTY_WINDOW_ID}".
_PREFIXED_ENV = re.compile(r"\A([A-Za-z0-9_.:=-]+)\{env:([A-Za-z_][A-Za-z0-9_]*)\}\Z")


def env_value(element, env):
    """The value of a "{env:NAME}" element, or of a literal prefix and one
    such variable; ExecutorRefusal when NAME is not set. A literal is
    returned as is."""
    prefixed = _PREFIXED_ENV.match(element)
    if prefixed:
        return prefixed.group(1) + env_value("{env:%s}" % prefixed.group(2), env)
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
        # The home of the agent process as the programs take it: HOME, or
        # USERPROFILE on Windows when HOME is unset (preflight.py#home_path).
        path = home_path(value, env)
        if path is None:
            raise ExecutorRefusal("variable HOME is not set")
        return path
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


def child_env(env, secrets, drop=()):
    """The environment of the agent process without any variable whose name
    or value holds a secret of the monitor, and without the variables named
    in drop (the key variables of the http channels)."""
    secrets = [s for s in secrets if s]
    return dict((k, v) for k, v in env.items()
                if k not in drop and not any(s in v or s in k for s in secrets))


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


# A program a detached call left running past its window ends on its own
# within one grace after it, or its tree is killed then: no detached call
# lives longer than its window plus this.
DETACHED_GRACE_SEC = 30.0


class DetachedCalls(object):
    """The programs of detached calls that outlived their window
    (registry.go#Executor.Detach), by channel: at most one lives per
    channel. Each is reaped as it ends, killed with its tree at its hard
    deadline (the window plus DETACHED_GRACE_SEC) and at the end of the
    monitor (end_all). clock gives the monotonic time the deadlines are
    kept by."""

    def __init__(self, clock=time.monotonic):
        self.clock = clock
        self._lock = threading.Lock()
        # {channel key: (proc, tree, deadline)}
        self._calls = {}

    def admit(self, key):
        """Raise ExecutorRefusal when a call of this channel still runs: a
        refusal before send, retried by the delivery's budget."""
        self.reap()
        with self._lock:
            if key in self._calls:
                raise ExecutorRefusal("a detached call of this channel is still running")

    def leave(self, key, proc, tree, deadline):
        """Leave proc running until deadline; a watcher reaps it as it ends
        and kills it at its deadline."""
        with self._lock:
            self._calls[key] = (proc, tree, deadline)
        watcher = threading.Thread(target=self._watch, args=(key, proc))
        watcher.daemon = True
        watcher.start()

    def _watch(self, key, proc):
        # It only looks: the program is reaped by reap(), after its group
        # is killed, so the id of the group is never reused before.
        while True:
            with self._lock:
                if self._calls.get(key, (None,))[0] is not proc:
                    return
            time.sleep(_DETACHED_WATCH_STEP_SEC)
            self.reap()

    def running(self):
        """The channel keys whose program still runs."""
        with self._lock:
            return sorted(self._calls)

    def reap(self):
        """Reap the programs that ended and kill the trees of those past
        their deadline; the others run on."""
        now = self.clock()
        with self._lock:
            ended = [(k, c) for k, c in self._calls.items()
                     if _ended_unreaped(c[0]) or now >= c[2]]
            for key, _ in ended:
                del self._calls[key]
        for _, (proc, tree, _) in ended:
            _end_tree(proc, tree)

    def end_all(self):
        """Kill and reap every program left running: the monitor stops."""
        with self._lock:
            calls, self._calls = list(self._calls.values()), {}
        for proc, tree, _ in calls:
            _end_tree(proc, tree)


# How often a watcher of a detached call looks at its program and its
# deadline: a program is killed at most this long after its deadline.
_DETACHED_WATCH_STEP_SEC = 1.0


def _end_tree(proc, tree):
    """Kill what is left of a call's tree (its descendants too) and reap
    its program."""
    tree.kill()
    try:
        proc.wait(timeout=_READER_GRACE_SEC)
    except subprocess.TimeoutExpired:
        pass


# The detached calls of this process.
DETACHED_CALLS = DetachedCalls()


def _read_capped(fh):
    fh.seek(0)
    data = fh.read(OUTPUT_CAP_BYTES + 1)
    return data[:OUTPUT_CAP_BYTES], len(data) > OUTPUT_CAP_BYTES


def run_detached(argv, env, stdin_data, wait_sec, secrets=(), key=None, calls=None):
    """Run argv (no shell) as run_program does, as one tree of processes
    (_ProcessTree: its own session on POSIX, a job object on Windows, with
    the options of every call of a channel program), but wait at most
    wait_sec for its exit (registry.go#Executor.Detach). An exit in that
    time gives its CallResult and the tree goes. A program still running
    is left to the detached calls (calls, DETACHED_CALLS by default) under
    key, the channel: it is killed with its tree at wait_sec plus
    DETACHED_GRACE_SEC from its start at the latest, and at the end of the
    monitor; it gives a CallResult with still_running. A call of a channel
    whose earlier program still runs is refused before send. Raises
    ExecutorRefusal as run_program does."""
    for s in secrets:
        if s and (any(s in a for a in argv) or (stdin_data is not None and s.encode("utf-8") in stdin_data)):
            raise ExecutorRefusal("the call would carry a secret of the monitor")
    calls = DETACHED_CALLS if calls is None else calls
    key = tuple(argv) if key is None else key
    calls.admit(key)
    out, err = tempfile.TemporaryFile(), tempfile.TemporaryFile()
    proc = None
    tree = None
    left = False
    try:
        try:
            proc = subprocess.Popen(argv, stdin=subprocess.PIPE if stdin_data is not None else subprocess.DEVNULL,
                                    stdout=out, stderr=err, env=env, close_fds=True,
                                    **current_platform().tree_popen_options())
        except OSError as exc:
            raise ExecutorRefusal("the program cannot be started (%s)" % (exc.strerror or exc))
        started = calls.clock()
        tree = _ProcessTree(proc)
        tree.start()
        if stdin_data is not None:
            # A writer of its own: a program that reads its stdin late never
            # holds the call past its time.
            _Writer(proc.stdin, stdin_data).start()
        try:
            exit_code = proc.wait(timeout=max(wait_sec, 0))
        except subprocess.TimeoutExpired:
            calls.leave(key, proc, tree, started + max(wait_sec, 0) + DETACHED_GRACE_SEC)
            left = True
            return CallResult(None, b"", b"", False, still_running=True)
        stdout, cut = _read_capped(out)
        stderr, err_cut = _read_capped(err)
        return CallResult(exit_code, stdout, stderr, False, truncated=cut, stderr_truncated=err_cut)
    finally:
        if not left and proc is not None:
            # An exit within the window, an error or a stop signal: nothing
            # of the call outlives it.
            if tree is not None:
                tree.kill()
            else:
                _kill_quietly(proc)
            if proc.returncode is None:
                try:
                    proc.wait(timeout=_READER_GRACE_SEC)
                except subprocess.TimeoutExpired:
                    pass
        out.close()
        err.close()


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
                      timed_out, cut[out_fd], stderr_truncated=cut[err_fd])


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
                try:
                    # One grace: a program the kill did not end does not hold
                    # the call longer (its outcome is unknown anyway).
                    proc.wait(timeout=_READER_GRACE_SEC)
                except subprocess.TimeoutExpired:
                    pass
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
                      timed_out, cut[out_fd], tree_failure=tree.failure if tree is not None else None,
                      stderr_truncated=cut[err_fd])


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
                      readers[0].cut, tree_failure=tree.failure if tree is not None else None,
                      stderr_truncated=readers[1].cut)


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
        # Why a kill could not end the tree (the first such cause), or None.
        self.failure = None

    def start(self):
        """Let the program run (Windows: resume it inside its job)."""
        current_platform().tree_start(self.proc, self.job)

    def kill(self):
        job, self.job = self.job, None
        failure = current_platform().tree_kill(self.proc, job)
        if failure and self.failure is None:
            self.failure = failure


class ArgvExecutor(object):
    """Delivers a text through the argv executor of a channel record."""

    def __init__(self, token_file, env, secrets, programs=None, drop=()):
        self.token_file = token_file
        self.env = env
        self.secrets = list(secrets)
        # Variables no channel program gets (the http keys, by name).
        self.drop = tuple(drop)
        # {name: absolute path}: the programs of the preflight result
        # (preflight.json "programs"). Given (the working runner), it is the
        # only way a program runs: a name it does not hold is refused, never
        # run bare. None (a test of the executor alone) runs names by PATH.
        self.strict = programs is not None
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
        env = child_env(self.env, self.secrets, self.drop)
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
            elif self.strict:
                raise ExecutorRefusal("program %s is not one the preflight found" % (argv[0] if argv else ""))
            if batch:
                if text_file is not None and not BATCH_SAFE_PATH.match(text_file):
                    raise ExecutorRefusal("the message path of a .cmd program is not ASCII")
                if not all(BATCH_SAFE_ARG.match(a) for a in argv[1:]):
                    raise ExecutorRefusal("an argument of a .cmd program holds characters cmd.exe interprets")
            if executor.get("detach"):
                return run_detached(argv, env, stdin_data, timeout_sec, self.secrets,
                                    key=tuple(executor.get("argv") or ()))
            return run_program(argv, env, stdin_data, timeout_sec, self.secrets)
        finally:
            if text_file is not None:
                remove_quietly(text_file)


# A placeholder of an http URL template.
_URL_PLACEHOLDER = re.compile(r"(\{env:[A-Za-z_][A-Za-z0-9_]*\})")
# The hosts an http URL may name without TLS (a server of this machine).
_LOCAL_HTTP_HOSTS = ("localhost", "127.0.0.1")


class HTTPResult(object):
    """What a request of an http executor gave: its status (None when no
    answer came), the JSON object of its body (None when it held none or
    was cut at the cap), whether the timeout ended it, and error, the name
    of the failure that left no status (None otherwise). exit_code and
    stderr are None: no program ran."""

    exit_code = None
    stderr = None

    def __init__(self, status, body, timed_out, error=None, truncated=False):
        self.http_status = status
        self.stdout = body
        self.timed_out = timed_out
        self.error = error
        self.truncated = truncated
        self.json = None if truncated or body is None else parse_json_object(body)


def expand_url(template, env, endpoint=None):
    """The URL of an http executor. A leading placeholder is the base
    (scheme, host, maybe a path, taken as is): {env:NAME}, or {endpoint},
    the base read from the executor's endpoint file; every later one is one
    element of the path, escaped. ExecutorRefusal when a variable is not
    set, the URL cannot be parsed, the base carries a query or a fragment,
    the authority has user information or a port that is no number, or the
    URL is neither https of a host nor http of a local host."""
    out = []
    base = None
    if template.startswith(ENDPOINT_PLACEHOLDER):
        if not endpoint:
            raise ExecutorRefusal("the URL needs the base of an endpoint file")
        base = endpoint.rstrip("/")
        out.append(base)
        template = template[len(ENDPOINT_PLACEHOLDER):]
    for i, part in enumerate(_URL_PLACEHOLDER.split(template)):
        if i % 2 == 0:
            out.append(part)
        elif i == 1 and out == [""] and base is None:
            base = env_value(part, env)
            out.append(base)
        else:
            out.append(urllib.parse.quote(env_value(part, env), safe=""))
    url = "".join(out)
    try:
        if base is not None:
            given = urllib.parse.urlsplit(base)
            if given.query or given.fragment or "?" in base or "#" in base:
                raise ExecutorRefusal("the base of the URL carries a query or a fragment")
        parts = urllib.parse.urlsplit(url)
        port = parts.port
    except ValueError as exc:
        raise ExecutorRefusal("the URL of the channel cannot be parsed (%s)" % exc)
    if parts.username is not None or parts.password is not None or "@" in parts.netloc:
        raise ExecutorRefusal("the URL of the channel carries user information")
    if port is not None and not 0 < port < 65536:
        raise ExecutorRefusal("the URL of the channel has no valid port")
    if parts.scheme == "https" and parts.hostname:
        return url
    if parts.scheme == "http" and parts.hostname in _LOCAL_HTTP_HOSTS:
        return url
    raise ExecutorRefusal("the URL of the channel is neither https nor http of localhost")


# The placeholder of the base an endpoint file gives
# (registry.go#PlaceholderEndpoint).
ENDPOINT_PLACEHOLDER = "{endpoint}"


def _endpoint_private(file_st, dir_st):
    """An endpoint file is read only when no one else may read it (0600):
    it holds the password of the server."""
    return not file_st.st_mode & 0o077


def read_endpoint(http, env):
    """(base URL, Basic user, password) from the endpoint file of an http
    request (registry.go#EndpointFile): the first path whose variable is
    set, read only as a regular file of this user that no one else may read
    and no link. ExecutorRefusal names why the file gives no endpoint."""
    spec = http.get("endpointFile") or {}
    path = None
    for candidate in spec.get("paths") or []:
        path = expand_path(candidate, env)
        if path is not None:
            break
    if path is None:
        raise ExecutorRefusal("no path of the endpoint file resolves")
    try:
        raw = current_platform().read_checked(path, _endpoint_private)
    except PlatformRefusal as exc:
        raise ExecutorRefusal("the endpoint file is refused (%s)" % exc.reason)
    doc = parse_json_object(raw)
    if doc is None:
        raise ExecutorRefusal("the endpoint file is no JSON object")
    base = doc.get(spec.get("urlField") or "")
    if not isinstance(base, str) or not base:
        raise ExecutorRefusal("the endpoint file names no URL")
    password = None
    if spec.get("passwordField"):
        password = doc.get(spec["passwordField"])
        if password is not None and (not isinstance(password, str) or "\r" in password or "\n" in password):
            raise ExecutorRefusal("the password of the endpoint file is no single line")
    # The base passes the checks of a template's base.
    expand_url(ENDPOINT_PLACEHOLDER, env, endpoint=base)
    return base, spec.get("user") or None, password or None


def endpoint_secrets(registry, env):
    """The passwords the endpoint files of the http channels hold now:
    secrets of the monitor whatever their length (a password is no word of
    a text)."""
    out = []
    for channel in (registry.channels.values() if registry is not None else []):
        http = (channel.get("executor") or {}).get("http") or {}
        if http.get("endpointFile"):
            try:
                password = read_endpoint(http, env or {})[2]
            except ExecutorRefusal:
                continue
            if password and password not in out:
                out.append(password)
    return out


def http_key(http, env):
    """The key of an http request: the value of the first variable of its
    keyEnv that is set, or None."""
    for name in http.get("keyEnv") or []:
        value = env.get(name, "")
        if value:
            return value
    return None


# A key shorter than this is not redacted as a substring of texts, lines and
# environments: a short value would wipe ordinary words. Its variable still
# leaves every program environment by name (http_key_names).
MIN_SECRET_CHARS = 16


def http_key_secrets(registry, env):
    """Secrets of the monitor from the http channels of the registry: the
    key each would send in this environment (the first variable of its
    keyEnv that is set) whatever its length - it goes into Authorization;
    and the value of every other set key variable when it is at least
    MIN_SECRET_CHARS long (a short value would wipe ordinary words of a
    text; its variable leaves every program environment by name)."""
    keys = []
    env = env or {}
    for channel in (registry.channels.values() if registry is not None else []):
        spec = channel.get("executor") or {}
        if spec.get("type") != "http":
            continue
        http = spec.get("http") or {}
        selected = http_key(http, env)
        if selected and selected not in keys:
            keys.append(selected)
        for name in http.get("keyEnv") or []:
            value = env.get(name, "")
            if value and value != selected and len(value) >= MIN_SECRET_CHARS and value not in keys:
                keys.append(value)
    return keys


def http_key_names(registry):
    """The key variables of every http channel of the registry: no channel
    program, probe or check gets them in its environment."""
    names = []
    for channel in (registry.channels.values() if registry is not None else []):
        spec = channel.get("executor") or {}
        if spec.get("type") == "http":
            for name in (spec.get("http") or {}).get("keyEnv") or []:
                if name not in names:
                    names.append(name)
    return names


def monitor_secrets(token, env, registry):
    """The secrets of the monitor, the one list every mode redacts its
    journal and its stdout by (INV-6): the token and the keys of the http
    channels (http_key_secrets)."""
    return [s for s in [token] + http_key_secrets(registry, env) + endpoint_secrets(registry, env) if s]


# The name of the thread of one http request, and of the name lookup it
# waits for.
HTTP_WORKER_NAME = "skein-http-request"
HTTP_RESOLVER_NAME = "skein-http-resolve"


def _resolve(host, port):
    """The addresses of host:port (the system resolver; it may hang)."""
    return socket.getaddrinfo(host, port, 0, socket.SOCK_STREAM)


class _NotReached(Exception):
    """Nothing of the request went to the server."""

    def __init__(self, reason):
        Exception.__init__(self, reason)
        self.reason = reason


class _Transfer(object):
    """One request under one deadline, shared by the caller and the thread
    that sends it. The thread holds its socket here and asks before the
    first byte of the request; the caller cancels at the deadline, which
    shuts the socket so no wait of the thread outlives it, and learns
    whether a byte may have gone."""

    def __init__(self, deadline):
        self.deadline = deadline
        self.lock = threading.Lock()
        self.cancelled = False
        self.sent = False
        self.sock = None
        self.answer = {}

    def left(self):
        return self.deadline - time.monotonic()

    def hold(self, sock):
        """Keep sock as the socket of the request; False (sock closed) when
        the caller has cancelled."""
        with self.lock:
            if not self.cancelled:
                self.sock = sock
                return True
        _close_quietly(sock)
        return False

    def begin_send(self):
        """Whether the request may start now (in its time, not cancelled);
        from here on a byte may reach the server."""
        with self.lock:
            if self.cancelled or self.left() <= 0:
                return False
            self.sent = True
            return True

    def cancel(self):
        """Stop the thread's waits; whether a byte of the request may have
        gone out."""
        with self.lock:
            self.cancelled = True
            sock, sent = self.sock, self.sent
        if sock is not None:
            _close_quietly(sock)
        return sent


def _close_quietly(sock):
    try:
        sock.shutdown(socket.SHUT_RDWR)
    except OSError:
        pass
    try:
        sock.close()
    except OSError:
        pass


def _wait_left(transfer, what):
    left = transfer.left()
    if left <= 0:
        raise _NotReached("%s outlasted the timeout" % what)
    return left


def _resolve_within(transfer, host, port):
    """The addresses of host:port within the deadline: the lookup runs in a
    thread of its own (no timeout bounds the system resolver), and a lookup
    still running at the deadline is left behind - it sends nothing."""
    box = {}
    done = threading.Event()

    def lookup():
        try:
            box["addresses"] = _resolve(host, port)
        except Exception as exc:  # any failure of the lookup: not reached
            box["error"] = type(exc).__name__
        done.set()
    resolver = threading.Thread(target=lookup, name=HTTP_RESOLVER_NAME)
    resolver.daemon = True
    resolver.start()
    if not done.wait(_wait_left(transfer, "the name lookup")):
        raise _NotReached("the name lookup outlasted the timeout")
    if "error" in box:
        raise _NotReached(box["error"])
    return box["addresses"]


def _connect_within(transfer, host, port):
    """A connected socket to host:port within the deadline, held by the
    transfer: each address in turn, each connect waiting what is left."""
    last = "no address"
    for family, kind, proto, _, address in _resolve_within(transfer, host, port):
        left = _wait_left(transfer, "the connection")
        sock = socket.socket(family, kind, proto)
        if not transfer.hold(sock):
            raise _NotReached("cancelled")
        try:
            sock.settimeout(left)
            sock.connect(address)
            return sock
        except OSError as exc:
            last = type(exc).__name__
            _close_quietly(sock)
    raise _NotReached(last)


def _read_head(transfer, sock):
    """The status line and headers of an answer on sock, up to the empty
    line, within the deadline."""
    raw = b""
    while b"\r\n\r\n" not in raw:
        if len(raw) > 65536:
            raise _NotReached("the proxy answer is too long")
        sock.settimeout(_wait_left(transfer, "the proxy answer"))
        chunk = sock.recv(4096)
        if not chunk:
            raise _NotReached("the proxy closed the connection")
        raw += chunk
    return raw.split(b"\r\n", 1)[0]


def _tunnel(transfer, sock, host, port, proxy):
    """A CONNECT through the proxy to host:port (the request itself goes
    inside the tunnel, so the proxy sees no key); its Basic credentials
    from the proxy URL."""
    target = "%s:%d" % ("[%s]" % host if ":" in host else host, port)
    lines = ["CONNECT %s HTTP/1.1" % target, "Host: %s" % target]
    if proxy.username is not None:
        user = urllib.parse.unquote(proxy.username)
        password = urllib.parse.unquote(proxy.password or "")
        token = base64.b64encode(("%s:%s" % (user, password)).encode("utf-8")).decode("ascii")
        lines.append("Proxy-Authorization: Basic " + token)
    sock.settimeout(_wait_left(transfer, "the proxy"))
    sock.sendall(("\r\n".join(lines) + "\r\n\r\n").encode("latin-1"))
    status = _read_head(transfer, sock).split(None, 2)
    if len(status) < 2 or status[1] != b"200":
        raise _NotReached("the proxy answered %s" % (status[1].decode("latin-1", "replace") if len(status) > 1
                                                     else "nothing"))


def _proxy_for(parts, env):
    """The proxy of an https request by the variables of the agent process
    (https_proxy, no_proxy), or None. Only an http proxy is used; another
    is refused."""
    proxies = dict((k.lower(), v) for k, v in env.items()
                   if k.lower() in ("https_proxy", "no_proxy") and v)
    url = proxies.get("https_proxy")
    if not url:
        return None
    if _bypassed(parts.hostname or "", parts.port or 443, proxies.get("no_proxy", "")):
        return None
    proxy = urllib.parse.urlsplit(url if "://" in url else "http://" + url)
    if proxy.scheme != "http" or not proxy.hostname:
        raise _NotReached("the proxy %s is not an http proxy" % proxy.scheme)
    return proxy


def _bypassed(host, port, no_proxy):
    """Whether no_proxy names host (its whole name or a domain of it, with
    or without the port, or *)."""
    host = host.lower()
    for entry in (e.strip().lower() for e in no_proxy.split(",")):
        if not entry:
            continue
        if entry == "*":
            return True
        name, _, entry_port = entry.partition(":") if entry.count(":") == 1 else (entry, "", "")
        if entry_port and entry_port != str(port):
            continue
        name = name.lstrip(".")
        if host == name or host.endswith("." + name):
            return True
    return False


class HTTPExecutor(object):
    """Delivers a text through the http executor of a channel record."""

    def __init__(self, env, secrets):
        self.env = env
        self.secrets = [s for s in secrets if s]

    def call(self, executor, text, timeout_sec):
        """Send the request. Returns an HTTPResult; raises ExecutorRefusal
        when nothing was handed over."""
        http = executor.get("http") or {}
        return self.request(http, http.get("url") or "", http.get("method") or "POST", text, timeout_sec)

    def get(self, http, url_template, timeout_sec):
        """A GET of url_template with the address and the authentication
        of http (an http-get probe, an http check): an HTTPResult;
        ExecutorRefusal when nothing was sent."""
        return self.request(http, url_template, "GET", None, timeout_sec)

    def request(self, http, url_template, method, text, timeout_sec):
        endpoint = user = password = None
        if http.get("endpointFile"):
            endpoint, user, password = read_endpoint(http, self.env)
        url = expand_url(url_template, self.env, endpoint=endpoint)
        key = http_key(http, self.env)
        if http.get("keyEnv") and key is None:
            raise ExecutorRefusal("no key variable is set: %s" % ", ".join(http["keyEnv"]))
        secrets = self.secrets + ([key] if key else []) + ([password] if password else [])
        if key and ("\r" in key or "\n" in key):
            raise ExecutorRefusal("the key is not one header line")
        headers = {"Accept": "application/json"}
        body = None
        if method != "GET":
            fields = dict(http.get("bodyFixed") or {})
            fields[http.get("bodyField") or "text"] = redact_text(text or "", secrets)
            body = json.dumps(fields).encode("utf-8")
            headers["Content-Type"] = "application/json"
        if key:
            headers["Authorization"] = "Bearer " + key
        elif password and user:
            token = base64.b64encode(("%s:%s" % (user, password)).encode("utf-8")).decode("ascii")
            headers["Authorization"] = "Basic " + token
        if http.get("userAgent"):
            # The client's own signature: a proxy in front of some APIs
            # rejects the default one of urllib.
            headers["User-Agent"] = http["userAgent"]
        transfer = _Transfer(time.monotonic() + timeout_sec)
        worker = threading.Thread(target=self._send, args=(transfer, url, method, body, headers),
                                  name=HTTP_WORKER_NAME)
        worker.daemon = True
        worker.start()
        worker.join(max(transfer.left(), 0))
        if worker.is_alive():
            if not transfer.cancel():
                raise ExecutorRefusal("the server was not reached in %s s" % timeout_sec)
            return HTTPResult(None, None, True)
        answer = transfer.answer
        if "refused" in answer:
            raise ExecutorRefusal("the server was not reached: %s" % answer["refused"])
        if "timeout" in answer:
            return HTTPResult(None, None, True)
        if "error" in answer:
            return HTTPResult(None, None, False, error=answer["error"])
        return HTTPResult(answer["status"], answer["body"], False, truncated=answer["truncated"])

    def _send(self, transfer, url, method, body, headers):
        """The request within the deadline of transfer: the name lookup,
        the connection, a proxy tunnel, the TLS handshake, the request and
        every read of the answer each wait at most what is left of it, and
        the caller shuts the socket at the deadline. Nothing goes out once
        the deadline has passed - also when this thread starts after it.
        Any failure before the first byte of the request is a refusal; a
        3xx is the answer (the request is never sent elsewhere)."""
        answer = transfer.answer
        sock = None
        try:
            try:
                parts = urllib.parse.urlsplit(url)
                host, secure = parts.hostname or "", parts.scheme == "https"
                port = parts.port or (443 if secure else 80)
                # A local server (http) goes direct: no proxy sees the key in
                # the clear.
                proxy = _proxy_for(parts, self.env) if secure else None
                if proxy is not None:
                    sock = _connect_within(transfer, proxy.hostname, proxy.port or 80)
                    _tunnel(transfer, sock, host, port, proxy)
                else:
                    sock = _connect_within(transfer, host, port)
                if secure:
                    tls = ssl.create_default_context().wrap_socket(sock, server_hostname=host,
                                                                   do_handshake_on_connect=False)
                    if not transfer.hold(tls):
                        raise _NotReached("cancelled")
                    sock = tls
                    sock.settimeout(_wait_left(transfer, "the TLS handshake"))
                    sock.do_handshake()
                conn = (http.client.HTTPSConnection if secure else http.client.HTTPConnection)(host, port)
                conn.sock = sock
                path = parts.path or "/"
                if parts.query:
                    path += "?" + parts.query
                if not transfer.begin_send():
                    raise _NotReached("the request did not start in its time")
            except (OSError, ValueError, http.client.HTTPException) as exc:
                raise _NotReached(type(exc).__name__)
            sock.settimeout(max(transfer.left(), 0.001))
            conn.request(method, path, body=body, headers=headers)
            # The answer is read on this socket, which only this thread
            # closes (the connection's own getresponse would close it on an
            # answer that ends the connection, before its body is read).
            resp = http.client.HTTPResponse(sock, method=method)
            resp.begin()
            raw = b""
            while len(raw) <= OUTPUT_CAP_BYTES:
                left = transfer.left()
                if left <= 0:
                    raise socket.timeout("the answer outlasted the timeout")
                sock.settimeout(left)
                chunk = resp.read(min(8192, OUTPUT_CAP_BYTES + 1 - len(raw)))
                if not chunk:
                    break
                raw += chunk
            answer["status"] = resp.status
            answer["body"] = raw[:OUTPUT_CAP_BYTES]
            answer["truncated"] = len(raw) > OUTPUT_CAP_BYTES
        except _NotReached as exc:
            answer["refused"] = exc.reason
        except socket.timeout:
            answer["timeout"] = True
        except Exception as exc:  # any failure after the request started: unknown
            if transfer.cancelled:
                answer["timeout"] = True
            else:
                answer["error"] = type(exc).__name__
        finally:
            if sock is not None:
                _close_quietly(sock)


class ChannelExecutor(object):
    """The executor of every channel type: argv or http by the record. Its
    secrets are the monitor's and the keys of the http channels, so no
    channel program sees a key."""

    def __init__(self, token_file, env, secrets, programs=None, registry=None):
        self.secrets = [s for s in list(secrets) + http_key_secrets(registry, env) + endpoint_secrets(registry, env)
                        if s]
        self.drop = tuple(http_key_names(registry))
        self.argv = ArgvExecutor(token_file, env, self.secrets, programs, self.drop)
        self.http = HTTPExecutor(env, self.secrets)
        self.programs = self.argv.programs
        self.strict = self.argv.strict

    def call(self, executor, text, timeout_sec):
        if executor.get("type") == "http":
            return self.http.call(executor, text, timeout_sec)
        return self.argv.call(executor, text, timeout_sec)


def _probe_program(channel, argv, env, agent=None, programs=None):
    """argv with its program as an absolute path: the channel's own program
    as the preflight finds it with the agent process (its executable, a
    path under the home directory, the PATH), never another program of that
    name; a program of another name (a check or a probe of its own) found on
    the PATH of the agent process. programs: the channel's programs the
    preflight already checked (preflight.py#checked_programs), taken as they
    are. ProbeError when none is found."""
    if programs is None:
        programs = channel_programs(channel, env, agent)
    name = argv[0] if argv else ""
    path = programs.get(name)
    if path is None:
        own = ((channel.get("executor") or {}).get("argv") or [""])[0]
        if name == own:
            raise ProbeError("program %s not found" % name)
        path = absolute_program(name, env)
    if path is None:
        raise ProbeError("program %s not found" % name)
    return [path] + list(argv[1:])


def absolute_program(name, env):
    """The absolute path name runs from: itself when absolute and present,
    else what the PATH of env finds, made absolute from the runner's
    directory; None when not found."""
    if not name:
        return None
    if os.path.isabs(name):
        return name if os.path.isfile(name) else None
    found = shutil.which(name, path=env.get("PATH", os.defpath))
    return os.path.abspath(found) if found else None


class SystemProbeRunner(ProbeRunner):
    """Runs the probes of a channel for the preflight (P-7)."""

    def __init__(self, secrets=(), drop=()):
        self.secrets = list(secrets)
        # Variables no probe or check gets (the http keys, by name).
        self.drop = tuple(drop)
        # The agent process of the preflight: a channel's own program is
        # the one the preflight found with it.
        self.agent = None
        # {channel id: {name: path}} the preflight checked (its Context's
        # programs), or None outside a preflight.
        self.programs = None

    def _checked(self, channel):
        """The programs the preflight checked for channel, or None."""
        if self.programs is None:
            return None
        return self.programs.get(channel.get("id"))

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
            if kind == "http-get":
                return self._http_get(channel, probe, env)
        except ExecutorRefusal as exc:
            raise ProbeError(exc.reason)
        raise ProbeError("unknown probe type %s" % kind)

    def _http_get(self, channel, probe, env):
        """A GET of the probe's URL with the address and authentication of
        the channel's http executor; its status by the probe's table."""
        executor = channel.get("executor") or {}
        timeout = min(executor.get("timeoutSec") or PROBE_TIMEOUT_SEC, PROBE_TIMEOUT_SEC)
        result = HTTPExecutor(env, self.secrets).get(executor.get("http") or {}, probe.get("url") or "", timeout)
        if result.timed_out:
            raise ProbeError("timed out after %d s" % timeout)
        for row in probe.get("results") or []:
            if outcome_sign_matches(row.get("match") or {}, doc=result.json, http_status=result.http_status, env=env):
                return row.get("result")
        return PROBE_UNAVAILABLE

    def _argv(self, channel, probe, env):
        timeout = min((channel.get("executor") or {}).get("timeoutSec") or PROBE_TIMEOUT_SEC, PROBE_TIMEOUT_SEC)
        clean = child_env(env, self.secrets, self.drop)
        argv = expand_argv(probe.get("argv") or [], clean)
        # The program of the channel by the path it is found at (a path under
        # the home directory among them), as the working runner runs it.
        argv = _probe_program(channel, argv, env, self.agent, self._checked(channel))
        result = run_program(argv, clean, None, timeout, self.secrets)
        if result.timed_out:
            raise ProbeError("timed out after %d s" % timeout)
        for row in probe.get("results") or []:
            if outcome_sign_matches(row.get("match") or {}, exit_code=result.exit_code, doc=result.json,
                                    stderr=result.sign_stderr, env=env):
                return row.get("result")
        return PROBE_UNAVAILABLE

    def capture(self, channel, env):
        """The values the addressee command of the channel gives at its
        capture paths, [[path, value], ...], None for a path its answer does
        not hold (it must stay absent): the command answers "same" with a
        JSON document, else ProbeError."""
        command = (channel.get("addressee") or {}).get("command") or {}
        timeout = min((channel.get("executor") or {}).get("timeoutSec") or PROBE_TIMEOUT_SEC, PROBE_TIMEOUT_SEC)
        clean = child_env(env, self.secrets, self.drop)
        try:
            argv = expand_argv(command.get("argv") or [], clean)
        except ExecutorRefusal as exc:
            raise ProbeError(exc.reason)
        argv = _probe_program(channel, argv, env, self.agent, self._checked(channel))
        result = run_program(argv, clean, None, timeout, self.secrets)
        if result.timed_out:
            raise ProbeError("timed out after %d s" % timeout)
        verdict = None
        for row in command.get("results") or []:
            if outcome_sign_matches(row.get("match") or {}, exit_code=result.exit_code, doc=result.json,
                                    stderr=result.sign_stderr, env=env):
                verdict = row.get("result")
                break
        if verdict != "same":
            raise ProbeError("the addressee check does not answer same")
        if result.json is None:
            raise ProbeError("the addressee check gives no JSON answer")
        return [[path, capture_value(result.json, path)] for path in command.get("capture") or []]

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
