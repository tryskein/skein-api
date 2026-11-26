"""A fake delivery channel for the runner tests (stdlib only, Python 3.6+).

FakeProgram is a program on PATH with a scripted answer per call (exit code,
stdout, a delay, a hang) that leaves a marker of every call: its argv, its
stdin, the content of the message files its argv names, selected variables
of its environment and the time. two_step() gives the two programs of an
"insert, then Enter" channel sharing one call log. FakeHTTPChannel is the
HTTP side of a channel with scripted answers (status, JSON body, delay,
hang) that records every request.
"""

import http.server
import json
import os
import socketserver
import stat
import sys
import threading
import time

_PROGRAM = r'''#!%(python)s
import json, os, sys, time
HERE = %(here)r
NAME = %(name)r
LOG = %(log)r
WATCH = %(watch)r
data = b""
try:
    if not sys.stdin.isatty():
        data = sys.stdin.buffer.read()
except (OSError, ValueError):
    pass
files = {}
for arg in sys.argv[1:]:
    if os.path.basename(arg).startswith("msg-") and os.path.isfile(arg):
        st = os.stat(arg)
        with open(arg, "rb") as fh:
            files[arg] = {"content": fh.read().decode("utf-8", "replace"), "mode": st.st_mode & 0o777}
script_path = os.path.join(HERE, NAME + ".script.json")
with open(script_path) as fh:
    script = json.load(fh)
count_path = os.path.join(HERE, NAME + ".count")
try:
    with open(count_path) as fh:
        n = int(fh.read() or "0")
except OSError:
    n = 0
with open(count_path, "w") as fh:
    fh.write(str(n + 1))
answer = script[min(n, len(script) - 1)] if script else {"exit": 0}
mark = {"program": NAME, "argv": sys.argv[1:], "stdin": data.decode("utf-8", "replace"), "files": files,
        "env": dict((k, os.environ.get(k)) for k in WATCH), "time": time.time(), "pid": os.getpid()}
with open(LOG, "a") as fh:
    fh.write(json.dumps(mark) + "\n")
if answer.get("kill"):
    import signal
    try:
        os.kill(answer["kill"], signal.SIGKILL)
    except OSError:
        pass
if answer.get("hang"):
    time.sleep(3600)
if answer.get("delay"):
    time.sleep(answer["delay"])
out = answer.get("stdout")
if out is not None:
    sys.stdout.write(out if isinstance(out, str) else json.dumps(out))
    sys.stdout.flush()
sys.exit(answer.get("exit", 0))
'''


class FakeProgram(object):
    """A program named name in directory bindir. respond() scripts its
    answers: the n-th call gets the n-th answer, later calls the last one.
    An answer is {"exit": int, "stdout": str or a JSON value, "delay": sec,
    "hang": bool, "kill": pid (killed during the call)}."""

    def __init__(self, bindir, name, log=None, watch=()):
        self.dir = bindir
        self.name = name
        self.log = log or os.path.join(bindir, name + ".calls.jsonl")
        self.path = os.path.join(bindir, name)
        with open(self.path, "w") as fh:
            fh.write(_PROGRAM % {"python": sys.executable, "here": bindir, "name": name, "log": self.log,
                                 "watch": list(watch)})
        os.chmod(self.path, stat.S_IRWXU)
        self.respond([{"exit": 0}])

    def respond(self, answers):
        with open(os.path.join(self.dir, self.name + ".script.json"), "w") as fh:
            json.dump(list(answers), fh)

    def calls(self):
        """The markers of this program's calls, in order."""
        return [c for c in read_calls(self.log) if c["program"] == self.name]

    def remove(self):
        """Take the program off PATH (it disappeared)."""
        os.remove(self.path)


def read_calls(log):
    try:
        with open(log) as fh:
            return [json.loads(line) for line in fh if line.strip()]
    except OSError:
        return []


def with_path(env, bindir):
    """env with bindir first on PATH."""
    env = dict(env)
    env["PATH"] = bindir + os.pathsep + env.get("PATH", os.defpath)
    return env


def two_step(bindir, insert="insert", enter="enter", watch=()):
    """The two programs of an "insert, then Enter" channel with one call
    log, so the order of their calls is seen."""
    log = os.path.join(bindir, "two-step.calls.jsonl")
    return FakeProgram(bindir, insert, log, watch), FakeProgram(bindir, enter, log, watch)


class _Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True


class FakeHTTPChannel(object):
    """The HTTP side of a channel on 127.0.0.1. respond() scripts the
    answers like FakeProgram: {"status": int, "json": value, "headers":
    {..}, "delay": sec, "hang": bool}. requests() returns what came in."""

    def __init__(self):
        self._lock = threading.Lock()
        self._answers = [{"status": 200, "json": {}}]
        self._requests = []
        owner = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def _serve(self):
                length = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(length) if length else b""
                with owner._lock:
                    n = len(owner._requests)
                    owner._requests.append({"method": self.command, "path": self.path,
                                            "headers": dict(self.headers.items()),
                                            "body": body.decode("utf-8", "replace"), "time": time.time()})
                    answer = owner._answers[min(n, len(owner._answers) - 1)]
                if answer.get("hang"):
                    time.sleep(3600)
                if answer.get("delay"):
                    time.sleep(answer["delay"])
                data = json.dumps(answer.get("json", {})).encode("utf-8")
                self.send_response(answer.get("status", 200))
                for k, v in (answer.get("headers") or {}).items():
                    self.send_header(k, v)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            do_POST = do_PUT = do_GET = _serve

        self._server = _Server(("127.0.0.1", 0), Handler)
        self.url = "http://127.0.0.1:%d" % self._server.server_address[1]
        self._thread = threading.Thread(target=self._server.serve_forever)
        self._thread.daemon = True
        self._thread.start()

    def respond(self, answers):
        with self._lock:
            self._answers = list(answers)

    def requests(self):
        with self._lock:
            return list(self._requests)

    def close(self):
        self._server.shutdown()
        self._server.server_close()
