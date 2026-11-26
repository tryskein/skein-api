# --- the client branch of the loader (P-4, US-12) -------------------------------
# The starter runs the loader with _SKEIN_LOADER_MODE = "client" in its
# namespace; the loader then takes this branch. The parts of the runner it
# is joined with (every part but core.py, which carries the values the
# serving handler templates) give it the runner's token predicate, its
# one-runner lock, its stop table and its end sequence (P-12), so a monitor
# whose runner cannot be downloaded ends by the runner's own rules.
#
# In order:
#   1. before the token is read, the token directory and starter.py pass the
#      predicate of the token (a directory of ours without group/other bits;
#      starter.py a regular file of ours, not a link, private by the token
#      predicate): else token_file_insecure, nothing read, no download;
#   2. the token is read by the same predicate;
#   3. the runner is downloaded with the token, no redirect followed, with a
#      timeout; only the bytes whose SHA-256 is SKEIN_RUNNER_SHA256 run, in
#      this namespace (the runner's working start, US-7);
#   4. 401 is token_expired, 403 visibility_lost; a network failure, a
#      timeout or another status is server_unreachable, which touches
#      nothing (the kept rest and the token stay for the next run).
#
# A stop that touches files (lost access, runner_mismatch, a token file to
# delete) takes the one-runner lock first. A live runner of the token holds
# it: the loader prints its reason and touches nothing - the owner ends the
# monitor on its own poll. Otherwise, holding the lock: the events the state
# keeps get their lines (lost access only), then the files their fate, then
# the lock is released as the last step; a competing start sees the final
# fate.

import hashlib
import os
import signal
import stat

# The most of the runner the loader reads, and how long the download may
# take.
LOADER_RUNNER_MAX = 4 << 20
LOADER_DOWNLOAD_TIMEOUT_SEC = 30
# The stops of the loader beside those of the runner: the stop line, its
# event and exit code.
LOADER_STOPS = {"runner_mismatch": ("closed", 1), "server_unreachable": ("paused", 1)}
# Stops whose waiting events get "access lost" before the files go.
LOADER_ACCESS_STOPS = ("token_expired", "visibility_lost")


def loader_client_trusted(token_file):
    """The token directory and starter.py pass the token predicate (P-4):
    checked before the token is read."""
    if not hasattr(os, "getuid"):
        # Windows: its own predicate comes with its platform layer.
        return True
    directory = os.path.dirname(os.path.abspath(token_file))
    try:
        dst = os.lstat(directory)
    except OSError:
        # No directory: the token is missing, the read says so.
        return True
    if not stat.S_ISDIR(dst.st_mode) or dst.st_uid != os.getuid() or dst.st_mode & 0o077:
        return False
    starter = os.path.join(directory, CLIENT_STARTER_FILE)
    try:
        fd = os.open(starter, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    except OSError:
        # A link, or no file where the starter ran from: not to be trusted.
        return False
    try:
        st = os.fstat(fd)
    finally:
        os.close(fd)
    return stat.S_ISREG(st.st_mode) and token_file_private(starter, st)


def loader_client_read_token(path):
    """Read the token by the token predicate. Nothing is deleted here: a
    stop that deletes takes the one-runner lock first. Raises Stop."""
    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    except FileNotFoundError:
        raise Stop("token_file_missing", delete_token=True)
    except OSError:
        raise Stop("token_file_missing")
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode):
            raise Stop("token_file_missing")
        if not token_file_private(path, st):
            # The token is burnt: ours goes under the lock.
            raise Stop("token_file_insecure", delete_token=hasattr(os, "getuid") and st.st_uid == os.getuid())
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


def loader_client_download(base, token, want):
    """The runner body. Raises Stop by the answer."""
    import http.client
    import urllib.error
    import urllib.request

    class NoRedirect(urllib.request.HTTPRedirectHandler):
        # A redirect would leak the token and run foreign code.
        def redirect_request(self, *args):
            return None
    req = urllib.request.Request(base.rstrip("/") + "/agent/v1/monitor-script", headers={"X-Skein-Api-Key": token})
    try:
        with urllib.request.build_opener(NoRedirect).open(req, timeout=LOADER_DOWNLOAD_TIMEOUT_SEC) as resp:
            if resp.getcode() != 200:
                raise Stop("server_unreachable")
            body = resp.read(LOADER_RUNNER_MAX + 1)
    except urllib.error.HTTPError as exc:
        if exc.code == 401:
            raise Stop("token_expired", status=401, delete_token=True)
        if exc.code == 403:
            raise Stop("visibility_lost", status=403, delete_token=True)
        raise Stop("server_unreachable")
    except (urllib.error.URLError, http.client.HTTPException, OSError, ValueError):
        raise Stop("server_unreachable")
    if len(body) > LOADER_RUNNER_MAX or hashlib.sha256(body).hexdigest() != want:
        raise Stop("runner_mismatch", delete_token=True)
    return body.decode("utf-8")


def loader_client_end(stop, token_file, issue, env, token=None):
    """The end of a loader start: files are touched only under the
    one-runner lock, the stop line is printed last. Returns the exit code."""
    if stop.delete_token and token_file:
        try:
            lock = current_platform().acquire_runner_lock(token_file, 0, env)
        except (LockBusy, PlatformRefusal):
            # A live runner holds it (or it cannot be taken): its owner ends
            # the monitor; nothing here is touched.
            lock = None
        if lock is not None:
            try:
                if stop.reason in LOADER_ACCESS_STOPS:
                    finalize_stored(token_file, issue, stop.reason, env, secrets=[token] if token else [])
                apply_file_fate(token_file, True)
            finally:
                lock.release()
    return loader_client_line(stop, issue, token_file)


def loader_client_line(stop, issue, token_file):
    """Print the one stop line; returns its exit code."""
    if stop.reason in LOADER_STOPS:
        event, code = LOADER_STOPS[stop.reason]
        emit({"event": event, "reason": stop.reason, "action": loader_client_action(stop.reason, issue)})
        return code
    # The runner's line, with the files already given their fate.
    return finish(Stop(stop.reason, status=stop.status, detail=stop.detail), issue, token_file)


def loader_client_action(reason, issue):
    if reason == "runner_mismatch":
        return ("The runner downloaded for the monitor of issue %s differs from the one its start command "
                "names (the server was updated, or the download was altered), so it was not run. "
                "Mint a new monitor with %s." % (issue, mint_call(issue)))
    last = load_preflight(os.environ.get("SKEIN_TOKEN_FILE", "")) if os.environ.get("SKEIN_TOKEN_FILE") else None
    command = ("the working command of preflight %d" % last["preflightId"]) if last else "the same working command"
    return ("The monitor for issue %s could not download its runner: the Skein server is unreachable or "
            "refused the download. Run %s again later; the token and the state are kept." % (issue, command))


def loader_client_main():
    """The client branch of the loader; never returns."""
    global _stopping
    issue = os.environ.get("SKEIN_ISSUE", "")
    token_file = os.environ.get("SKEIN_TOKEN_FILE", "")
    env = dict(os.environ)
    token = None
    try:
        try:
            signal.signal(signal.SIGTERM, _on_signal)
            signal.signal(signal.SIGINT, _on_signal)
            base = env.get("SKEIN_PUBLIC_URL", "")
            want = env.get("SKEIN_RUNNER_SHA256", "")
            if not token_file or not base or not want:
                raise Stop("misconfigured", delete_token=bool(token_file))
            if not loader_client_trusted(token_file):
                raise Stop("token_file_insecure")
            token = loader_client_read_token(token_file)
            body = loader_client_download(base, token, want)
        except Stop as stop:
            _stopping = True
            sys.exit(loader_client_end(stop, token_file, issue, env, token))
    except StopSignal:
        sys.exit(loader_client_line(Stop("stopped"), issue, token_file))
    # The runner runs in this namespace (it reads the mode and hands its own
    # bytes to a detached child from here) and leaves through SystemExit.
    globals()[RUNNER_BODY_NAME] = body
    exec(compile(body, "<skein-runner>", "exec"), globals())
    sys.exit(0)
