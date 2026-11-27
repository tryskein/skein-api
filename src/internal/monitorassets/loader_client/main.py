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
import ntpath
import traceback
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
    directory = os.path.dirname(os.path.abspath(token_file))
    try:
        dst = os.lstat(directory)
    except OSError:
        # No directory: the token is missing, the read says so.
        return True
    if os.name != "posix":
        # Windows (P-9 (a)): the token directory and starter.py, checked on
        # their handles; starter.py is read through the checked handle.
        try:
            current_platform().read_checked(os.path.join(directory, CLIENT_STARTER_FILE), None)
        except PlatformRefusal:
            return False
        return True
    if not stat.S_ISDIR(dst.st_mode) or not current_platform().owns(dst) or dst.st_mode & 0o077:
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


def loader_client_ours(path):
    """The token file is ours (looked at, not opened)."""
    try:
        st = os.lstat(path)
    except OSError:
        return False
    if os.name != "posix":
        # Windows: the token file of a token directory prepare created new
        # and private (R-10) is ours.
        return stat.S_ISREG(st.st_mode)
    return current_platform().owns(st)


def loader_client_read_token(path):
    """Read the token by the token predicate. Nothing is deleted here: a
    stop that deletes takes the one-runner lock first. Raises Stop."""
    if os.name != "posix":
        return loader_client_read_token_windows(path)
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
            raise Stop("token_file_insecure", delete_token=current_platform().owns(st))
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


def loader_client_read_token_windows(path):
    """Read the token on Windows: its directory and the opened file pass
    criterion (a) of P-9 (platform layer, the handle closed before return)."""
    try:
        raw = current_platform().read_token(path)
    except PlatformRefusal as exc:
        if exc.reason == "not_found":
            raise Stop("token_file_missing", delete_token=True)
        if exc.reason in ("open_to_others", "symlink"):
            raise Stop("token_file_insecure", delete_token=loader_client_ours(path))
        raise Stop("token_file_missing")
    try:
        token = raw.decode("utf-8").strip()
    except UnicodeDecodeError:
        raise Stop("token_file_missing")
    if not token:
        raise Stop("token_file_missing", delete_token=True)
    return token


def loader_client_download(base, token, want):
    """The runner body. Raises Stop by the answer. The whole download -
    connection, answer and body - ends within LOADER_DOWNLOAD_TIMEOUT_SEC:
    the timeout of the request bounds each socket operation only, so a
    server that trickles bytes would hold the start past it. The download
    runs in a daemon thread the start stops waiting for (it ends with the
    process)."""
    import threading
    got = {}

    def fetch():
        try:
            got["body"] = _loader_client_fetch(base, token, want)
        except Stop as stop:
            got["stop"] = stop
        except BaseException:
            got["stop"] = Stop("server_unreachable")
    worker = threading.Thread(target=fetch)
    worker.daemon = True
    worker.start()
    worker.join(LOADER_DOWNLOAD_TIMEOUT_SEC)
    if worker.is_alive():
        raise Stop("server_unreachable")
    if "stop" in got:
        raise got["stop"]
    return got["body"]


def _loader_client_fetch(base, token, want):
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
                loader_client_journal_stop(stop, token_file, issue, env, token)
                apply_file_fate(token_file, True)
            finally:
                lock.release()
    return loader_client_line(stop, issue, token_file)


def loader_client_journal_stop(stop, token_file, issue, env, token=None):
    """The stop line of a monitor this start ends (under the one-runner
    lock): to the journal, or to the deferred tail when the journal takes no
    write, before the files go (P-12)."""
    if stop.reason in LOADER_STOPS:
        event, action = LOADER_STOPS[stop.reason][0], loader_client_action(stop.reason, issue)
    else:
        last = load_preflight(token_file)
        event = STOPS[stop.reason][0]
        action = stop_action(stop.reason, issue, stop.detail, last["preflightId"] if last else None)
    line = {"reason": stop.reason, "action": action}
    if stop.status is not None:
        line["status"] = stop.status
    try:
        journal = Journal(token_file, issue, secrets=[token] if token else [], env=env)
    except (JournalUnavailable, OSError, PlatformRefusal):
        return
    try:
        journal.lifecycle(event, **line)
    except JournalUnavailable:
        try:
            defer_lines(journal.path, [journal.render(dict(line, event=event), line_id=uuid.uuid4().hex)], env)
        except (LedgerUnavailable, JournalUnavailable):
            pass


def loader_client_line(stop, issue, token_file):
    """Print the one stop line; returns its exit code."""
    if stop.reason in LOADER_STOPS:
        event, code = LOADER_STOPS[stop.reason]
        emit({"event": event, "reason": stop.reason, "action": loader_client_action(stop.reason, issue)})
        return code
    # The runner's line, with the files already given their fate; a stop the
    # working command resumes names its preflight result.
    last = load_preflight(token_file) if token_file else None
    return finish(Stop(stop.reason, status=stop.status, detail=stop.detail,
                       preflight=last["preflightId"] if last else None), issue, token_file)


def loader_client_action(reason, issue):
    if reason == "runner_mismatch":
        return ("The runner downloaded for the monitor of issue %s differs from the one its start command "
                "names (the server was updated, or the download was altered), so it was not run. "
                "Mint a new monitor with %s." % (issue, mint_call(issue)))
    last = load_preflight(os.environ.get("SKEIN_TOKEN_FILE", "")) if os.environ.get("SKEIN_TOKEN_FILE") else None
    command = ("the working command of preflight %d" % last["preflightId"]) if last else "the same working command"
    return ("The monitor for issue %s could not download its runner: the Skein server is unreachable or "
            "refused the download. Run %s again later; the token and the state are kept." % (issue, command))


# --- prepare mode (R-10, US-11 "До записи токена") ---------------------------
# The Windows prepare runs the starter with SKEIN_MONITOR_MODE=prepare and
# the starter text in SKEIN_PREPARE_STARTER (its own process only). Here the
# loader creates the token directory new and private in one call (P-9 (a)),
# checks that a file can be created in it, opens every component of the path
# from the root of the drive down to it and checks criterion (b) of P-9 on
# their descriptors, then writes starter.py byte for byte and prints the
# ready line. The handles stay open (no FILE_SHARE_DELETE) until the process
# ends, after the ready line, so between the check and the Write no other
# account deletes, renames or rechanges a component. Every refusal is one
# prepare_failed line {reason, action}; no line repeats a path (INV-17).

LOADER_PREPARE_MINT = " Mint a new monitor with skein_get_monitor_script and the same parameters."
LOADER_PREPARE_FIX = (" Fix the cause, then mint a new monitor with skein_get_monitor_script and the"
                      " same parameters.")


class PrepareRefused(Exception):
    def __init__(self, reason, action):
        Exception.__init__(self, reason)
        self.reason = reason
        self.action = action


def loader_client_prepare(env, source):
    """Prepare the token directory; returns the exit code after printing the
    ready line or the refusal. source is the security source (winsec
    WinSecurity, or a fake in the tests)."""
    try:
        held = loader_client_prepare_dir(env, source)
    except PrepareRefused as exc:
        emit({"event": "prepare_failed", "reason": exc.reason, "action": exc.action})
        return 1
    emit({"event": "prepare_ready"})
    return 0


def loader_client_prepare_dir(env, source):
    """Create and check the token directory and write starter.py; returns
    the open handles of the path. Raises PrepareRefused."""
    token_dir = env.get("SKEIN_TOKEN_DIR", "")
    starter = env.get("SKEIN_PREPARE_STARTER", "")
    if not ntpath.isabs(token_dir) or not starter:
        raise PrepareRefused("misconfigured", "The prepare command is broken (no absolute token directory "
                             "or no starter)." + LOADER_PREPARE_MINT)
    try:
        source.make_private_dir(token_dir)
    except FileExistsError:
        raise PrepareRefused("token_dir_exists", "The token directory already exists, so it is not used."
                             + LOADER_PREPARE_MINT)
    except OSError as exc:
        raise PrepareRefused("token_dir_failed", "The token directory could not be created (%s)."
                             % exc.strerror + LOADER_PREPARE_FIX)
    probe = os.path.join(token_dir, ".skein-probe")
    try:
        os.close(os.open(probe, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600))
        os.remove(probe)
    except OSError as exc:
        raise PrepareRefused("token_dir_failed", "No file can be created in the token directory (%s)."
                             % exc.strerror + LOADER_PREPARE_FIX)
    held, views = [], []
    try:
        for path in path_components(token_dir):
            handle = source.open(path)
            held.append(handle)
            views.append((path, source.view(handle)))
        cause = chain_refusal(views, source.me())
    except OSError as exc:
        raise PrepareRefused("token_dir_insecure", "The path of the token directory could not be checked "
                             "(%s)." % exc.strerror + LOADER_PREPARE_FIX)
    if cause is not None:
        raise PrepareRefused("token_dir_insecure", "Another account could replace the path of the token "
                             "directory (%s)." % cause + LOADER_PREPARE_FIX)
    tmp = os.path.join(token_dir, ".starter.py.tmp")
    try:
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_BINARY", 0), 0o600)
        try:
            os.write(fd, starter.encode("ascii"))
            os.fsync(fd)
        finally:
            os.close(fd)
        os.rename(tmp, os.path.join(token_dir, CLIENT_STARTER_FILE))
    except (OSError, UnicodeEncodeError) as exc:
        try:
            os.remove(tmp)
        except OSError:
            pass
        raise PrepareRefused("starter_write_failed", "The starter could not be written into the token "
                             "directory (%s)." % getattr(exc, "strerror", exc) + LOADER_PREPARE_FIX)
    return held


def loader_client_main():
    """The client branch of the loader; never returns."""
    global _stopping
    if os.environ.get("SKEIN_MONITOR_MODE") == "prepare":
        if sys.platform != "win32":
            emit({"event": "prepare_failed", "reason": "misconfigured",
                  "action": "Prepare mode is for Windows only." + LOADER_PREPARE_MINT})
            sys.exit(1)
        sys.exit(loader_client_prepare(dict(os.environ), WinSecurity()))
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
                # Not read; a token of ours is burnt and goes under the lock.
                raise Stop("token_file_insecure", delete_token=loader_client_ours(token_file))
            token = loader_client_read_token(token_file)
            body = loader_client_download(base, token, want)
        except Stop as stop:
            _stopping = True
            sys.exit(loader_client_end(stop, token_file, issue, env, token))
    except StopSignal:
        sys.exit(loader_client_line(Stop("stopped"), issue, token_file))
    except Exception:
        # An error of this branch ends in the client-mode line (its working
        # command, its mint call), not in the outer loader's bootstrap text.
        _stopping = True
        log_stderr("monitor loader: unexpected error\n" + traceback.format_exc())
        sys.exit(loader_client_line(Stop("internal_error"), issue, token_file))
    # The runner runs in this namespace (it reads the mode and hands its own
    # bytes to a detached child from here) and leaves through SystemExit.
    globals()[RUNNER_BODY_NAME] = body
    exec(compile(body, "<skein-runner>", "exec"), globals())
    sys.exit(0)
