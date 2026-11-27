"""Shared helpers of the monitor runner tests.

Builds the runner body the way the server does (agent_script.go): the parts
listed in internal/monitorassets/runner/parts.txt joined in order, every __SKEIN_*__ placeholder
substituted, the registry being the server's registry.json in base64. Also
holds the fake poll and loader servers the test files share.

Standard library of Python 3.6 only — no f-strings, no ThreadingHTTPServer
(3.7), nothing newer — so the Python 3.6 test kind can import it too.
"""

import base64
import json
import os
import socket
import socketserver
import struct
import threading
import types
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

HERE = os.path.dirname(os.path.abspath(__file__))
# The directory of the runner parts and of their manifest.
RUNNER_DIR = os.path.join(HERE, "..", "..", "..", "monitorassets", "runner")
PARTS_MANIFEST = os.path.join(RUNNER_DIR, "parts.txt")
# The module name a mutation tool derives from the path of the core part
# (src/internal/monitorassets/runner/core.py). The in-process runner is
# loaded under this name so the tool attributes the runner's functions to
# the file it mutated.
RUNNER_MODULE = "internal.monitorassets.runner.core"
# The module of each part as mutmut names it from its path: this prefix and
# the part's file name without .py.
PART_MODULE_PREFIX = "internal.monitorassets.runner."
# The registry the server embeds into the body.
REGISTRY_JSON = os.path.join(HERE, "..", "..", "..", "monitorregistry", "registry.json")
# Shared interpreter hooks for runner subprocesses (virtual clock, pinned
# random, blocked resolver) — see its header.
SITE_DIR = os.path.join(HERE, "..", "..", "testdata", "runner_site")
# The loader asset (one part of the served loader).
LOADER_SRC = os.path.join(HERE, "..", "..", "..", "mcp", "tools", "assets", "monitor_loader.py")
# The starter asset (monitor_loader.go#monitorStarter substitutes the mode).
STARTER_SRC = os.path.join(os.path.dirname(LOADER_SRC), "monitor_starter.py")
STARTER_CLIENT_PLACEHOLDER = "__SKEIN_STARTER_CLIENT__"
# The manifest of the loader GET /agent/v1/monitor-loader serves
# (monitor_loader.go#servedLoaderSource); paths relative to its directory.
LOADER_PARTS_MANIFEST = os.path.join(os.path.dirname(LOADER_SRC), "loader_parts.txt")
# The manifest of the client branch of the loader, joined before those parts.
LOADER_CLIENT_MANIFEST = os.path.join(HERE, "..", "..", "..", "monitorassets", "loader_client", "parts.txt")

# The token renewal window the server templates by default (6 h).
DEFAULT_TOKEN_IDLE_SEC = 6 * 3600

ROOT = "zz-77"
EMPTY = {"notifications": [], "cursor": {"ts": "2026-01-01T00:00:00Z", "id": 0}}

# Whether the tests run under mutmut. `mutmut run` (3.8.1) puts the variable
# MUTANT_UNDER_TEST into its own environment before it collects any test
# (`_run` calls `set_mutant_under_test("mutant_generation")` first, and
# `set_mutant_under_test` of mutation/trampoline.py writes the name to
# os.environ) and keeps it set in every later phase: the stats, the clean
# run (an empty name), the forced failure and each mutant. Read once, at
# import: a test may clear os.environ while it runs.
UNDER_MUTMUT = "MUTANT_UNDER_TEST" in os.environ


def subprocess_runner(test):
    """Skip a test method or class when the tests run under mutmut.

    For the tests that run the runner body, the loader, the starter, prepare
    or the working command in a child interpreter: under mutmut the child
    runs the mutated copy of the parts, mutmut credits no mutant killed
    there to the test, and its stats run fails on such tests. Without
    mutmut the test runs as usual."""
    return unittest.skipIf(UNDER_MUTMUT, "runs the runner in a child interpreter, which mutmut does not measure")(test)


def source_size(test):
    """Skip a test method or class when the tests run under mutmut.

    For the tests that measure the size of the text of the parts (the body,
    the served loader): mutmut's copy of a part holds every mutant of its
    functions, so the size measured there is not the size the server
    serves. Without mutmut the test runs as usual."""
    return unittest.skipIf(UNDER_MUTMUT, "measures the text of the parts, which mutmut's copy inflates")(test)


def _join_manifest(manifest):
    """The parts a manifest lists (paths relative to its directory), each
    ending with a newline, joined in order."""
    base = os.path.dirname(manifest)
    chunks = []
    with open(manifest, "r") as fh:
        for line in fh:
            name = line.strip()
            if name and not name.startswith("#"):
                with open(os.path.join(base, name), "r") as part:
                    text = part.read()
                if text and not text.endswith("\n"):
                    text += "\n"
                chunks.append(text)
    if not chunks:
        raise AssertionError("the manifest %s lists no part" % manifest)
    return "".join(chunks)


def served_loader():
    """The loader the route serves: the source of the client branch
    (LOADER_CLIENT_MANIFEST) as the string _skein_loader_client_src, then
    the parts of loader_parts.txt, joined in order the way
    monitor_loader.go assembles them. The starter runs these
    bytes; a test that runs the loader runs them too."""
    client = json.dumps(_join_manifest(LOADER_CLIENT_MANIFEST), ensure_ascii=False)
    return "_skein_loader_client_src = " + client + "\n" + _join_manifest(LOADER_PARTS_MANIFEST)


def starter(client):
    """The starter the server serves: of a call with `client` (written by
    prepare as starter.py) or without it (the text of the bootstrap), without
    the trailing newline, as monitor_loader.go#mustStarter builds it."""
    with open(STARTER_SRC, "r") as fh:
        text = fh.read()
    if text.count(STARTER_CLIENT_PLACEHOLDER) != 1:
        raise AssertionError("the starter must carry the mode placeholder exactly once")
    return text.replace(STARTER_CLIENT_PLACEHOLDER, "1" if client else "0", 1).rstrip("\n")


def part_names():
    """The part file names of the manifest, in order."""
    names = []
    with open(PARTS_MANIFEST, "r") as fh:
        for line in fh:
            name = line.strip()
            if name and not name.startswith("#"):
                names.append(name)
    if not names:
        raise AssertionError("the parts manifest lists no part")
    return names


def registry_b64(path=REGISTRY_JSON):
    with open(path, "rb") as fh:
        return base64.b64encode(fh.read()).decode("ascii")


def render_runner(**substitutions):
    """The served body: substitute every placeholder the way agent_script.go
    does (render_parts). registry is the base64 registry document; the
    server's registry.json by default."""
    return "".join(text for _, text in render_parts(**substitutions))


def render_parts(poll_sec=1, ceiling_sec=10, limit_sec=15, stall_sec=180,
                 idle_sec=DEFAULT_TOKEN_IDLE_SEC, registry=None):
    """[(part file name, its text with every placeholder substituted)] in
    manifest order; joined, they are the served body."""
    registry = registry if registry is not None else registry_b64()
    out = []
    for name in part_names():
        with open(os.path.join(RUNNER_DIR, name), "r") as fh:
            text = fh.read()
        if text and not text.endswith("\n"):
            text += "\n"
        out.append((name, _substitute(text, poll_sec, ceiling_sec, limit_sec, stall_sec, idle_sec, registry)))
    if any("__SKEIN_" in text for _, text in out):
        raise AssertionError("placeholder left unsubstituted in rendered runner")
    return out


def _substitute(body, poll_sec, ceiling_sec, limit_sec, stall_sec, idle_sec, registry):
    body = body.replace("__SKEIN_POLL_INTERVAL_SEC__", str(poll_sec))
    body = body.replace("__SKEIN_BACKOFF_CEILING_SEC__", str(ceiling_sec))
    body = body.replace("__SKEIN_REQUEST_LIMIT_SEC__", str(limit_sec))
    body = body.replace("__SKEIN_STALL_THRESHOLD_SEC__", str(stall_sec))
    body = body.replace("__SKEIN_TOKEN_IDLE_SEC__", str(idle_sec))
    body = body.replace("__SKEIN_REGISTRY_B64__", registry)
    return body


def load_runner(namespace=None, **substitutions):
    """Execute the rendered body in a fresh module named RUNNER_MODULE (so
    `__name__ != "__main__"` and main() does not run) and return it.
    namespace: names put into it before the body runs (the loader's mode
    marker).

    The parts run in manifest order in the one namespace the served body
    has, each compiled under its own file and run with `__name__` set to the
    module of its file (PART_MODULE_PREFIX + its name), so every function
    and class carries the module of the part it is written in: a mutation
    tool that keys a function by its `__module__` (mutmut) finds the part it
    mutated. `__name__` is RUNNER_MODULE again at the end. The runner reads
    no `__name__` of its own but the `__main__` guard of its core part."""
    mod = types.ModuleType(RUNNER_MODULE)
    g = mod.__dict__
    g.update(namespace or {})
    for name, text in render_parts(**substitutions):
        g["__name__"] = PART_MODULE_PREFIX + os.path.splitext(name)[0]
        exec(compile(text, os.path.join(RUNNER_DIR, name), "exec"), g)
    g["__name__"] = RUNNER_MODULE
    return mod


class ThreadingHTTPServer(socketserver.ThreadingMixIn, HTTPServer):
    """http.server.ThreadingHTTPServer of Python 3.7, for Python 3.6."""

    daemon_threads = True


class _ScriptedHandler(BaseHTTPRequestHandler):
    """Answers each poll with whatever server.respond(handler, qs) decides."""

    def do_GET(self):
        parsed = urlparse(self.path)
        qs = parse_qs(parsed.query)
        self.server.requests.append(qs)
        self.server.respond(self, qs)

    def send_json(self, status, obj, headers=None):
        self.send_raw(status, json.dumps(obj).encode("utf-8"), headers)

    def send_raw(self, status, body, headers=None):
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def reset_connection(self):
        """Abort the TCP connection (RST) without any HTTP response."""
        self.request.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
        self.close_connection = True
        self.request.close()

    def log_message(self, *args):
        pass


class ScriptedServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, respond):
        super().__init__(("127.0.0.1", 0), _ScriptedHandler)
        self.respond = respond
        self.requests = []
        self.thread = threading.Thread(target=self.serve_forever, daemon=True)
        self.thread.start()

    def url(self):
        return "http://127.0.0.1:%d" % self.server_address[1]

    def handle_error(self, request, client_address):
        pass  # aborted connections are the point of some scenarios

    def stop(self):
        self.shutdown()
        self.server_close()


class _LoaderHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        parsed = urlparse(self.path)
        srv = self.server
        if parsed.path == "/agent/v1/monitor-script":
            srv.script_hits += 1
            srv.script(self)
        elif parsed.path == "/agent/v1/monitor-loader":
            srv.loader_hits += 1
            srv.loader(self)
        else:
            srv.poll(self)

    def send_body(self, status, body, declared_len=None):
        self.send_response(status)
        self.send_header("Content-Type", "text/x-python")
        self.send_header("Content-Length", str(declared_len if declared_len is not None else len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass

    def log_message(self, *args):
        pass


class LoaderServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, script, poll=None, loader=None):
        super().__init__(("127.0.0.1", 0), _LoaderHandler)
        self.script = script
        self.poll = poll or (lambda h: h.send_body(200, json.dumps(EMPTY).encode()))
        # The loader route answers with the served loader unless told otherwise.
        self.loader = loader or (lambda h: h.send_body(200, served_loader().encode("utf-8")))
        self.script_hits = 0
        self.loader_hits = 0
        threading.Thread(target=self.serve_forever, daemon=True).start()

    def url(self):
        return "http://127.0.0.1:%d" % self.server_address[1]

    def handle_error(self, request, client_address):
        pass

    def stop(self):
        self.shutdown()
        self.server_close()


class PushServer(ThreadingHTTPServer):
    """A poll server that answers empty pages until the test pushes
    notifications; each pushed notification is answered once per poll queue."""

    daemon_threads = True

    def __init__(self):
        super().__init__(("127.0.0.1", 0), _ScriptedHandler)
        self.requests = []
        self.lock = threading.Lock()
        self.pending = []
        self.next_id = 100
        self.respond = self._respond
        threading.Thread(target=self.serve_forever, daemon=True).start()

    def _respond(self, h, qs):
        with self.lock:
            notes, self.pending = self.pending, []
        h.send_json(200, {"notifications": notes,
                          "cursor": {"ts": "2026-01-01T00:00:00Z", "id": self.next_id}})

    def push(self):
        with self.lock:
            self.next_id += 1
            self.pending.append({"id": self.next_id, "type": "comment.created", "issueId": ROOT})
            return self.next_id

    def url(self):
        return "http://127.0.0.1:%d" % self.server_address[1]

    def handle_error(self, request, client_address):
        pass

    def stop(self):
        self.shutdown()
        self.server_close()
