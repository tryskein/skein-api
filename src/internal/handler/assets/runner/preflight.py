# --- preflight: the choice of the method (SKEIN_MONITOR_MODE=preflight) ------
# Client mode only. The preflight runs synchronously, chooses how the monitor
# runs by the method order of the spec and prints one line
#   {"event": "preflight", "preflightId", "client", "method", "channel",
#    "reason", "warnings", "journal", "command", "instruction"}
# client is the record used (its id, or "general" for an unknown client name:
# the warning of the general record is that value, not a text in warnings).
# command is the working command: the variables of the preflight command, the
# starter path and both digests, but SKEIN_MONITOR_MODE=run, with the number
# of this result (SKEIN_MONITOR_PREFLIGHT) and the overrides it ran with; it
# sets every SKEIN_* variable it reads, so values left in the shell by an
# earlier command do not change it. instruction is what the agent does in the
# chosen method: the tool and parameters of the record's method, the restart,
# the warnings of the record and of the chosen channel; never the journal
# path. The result, with the identity of the addressee, is saved in
# preflight.json next to the token under state.lock; the next preflight gets
# the next number. A preflight never polls and never moves the cursor. With a
# live runner of the token it answers already_running and changes nothing.
#
# Method order (first available, and only it):
#   1. the override SKEIN_MONITOR_CHANNEL (strict: unavailable -> journal);
#   2-4. the record's own methods: stream, own delivery command (a channel
#        at step 3), exit-on-event;
#   5-6. hosts and terminal environments the detector finds;
#   7. blind channels the detector finds (none under SKEIN_MONITOR_BLIND=off);
#   8. the journal.
# An agent launched by a host (the detector's under_host) skips its own
# methods except the kinds the host keeps, and that host goes first at
# step 5. A method is available when all its conditions hold (variables set,
# settings read and equal, programs found) and, for a channel, the channel's
# probes say "available"; a probe answering "unavailable" or failing makes it
# unavailable with the cause in `reason`; a channel whose source gives no
# probe (probeUndefined) is available on its other conditions.
#
# Two boundaries are objects: the Detector (hosts of steps 5-6, blind
# channels of step 7, the host an agent runs under and the address specs of
# the identity) and the ProbeRunner (runs one probe of a channel). The ones
# here find nothing and run no probe; the registry detector (detect.py) is
# the preflight's default, the probe executors plug in through ProbeRunner.

import json
import os
import random
import re
import shutil
import sys

try:
    import fcntl as _preflight_fcntl
except ImportError:  # not POSIX
    _preflight_fcntl = None

# The result of the last preflight, next to the token.
PREFLIGHT_FILE_NAME = "preflight.json"
PREFLIGHT_VERSION = 1
# The short lock next to the token: the write of a result and its check by
# the runner that starts.
STATE_LOCK_NAME = "state.lock"
# The starter file `prepare` writes into the token directory.
CLIENT_STARTER_FILE = "starter.py"
# The ledger directory under the user directory.
LEDGER_DIR_NAME = "ledger"
# The lowest Python the runner supports.
MIN_PYTHON = (3, 6)

METHOD_STREAM = "stream"
METHOD_EXIT_ON_EVENT = "exit-on-event"
METHOD_DETACHED = "detached"
METHOD_JOURNAL = "journal"

# Steps 2-4 of the method order by the kind of a record's method.
_KIND_STEP = {"stream": 2, "delivery": 3, "exit-on-event": 4}

# The variables of the bootstrap the working command repeats, in its order.
# The variables of the preflight command the working command repeats.
BOOTSTRAP_VARS = ("SKEIN_PUBLIC_URL", "SKEIN_ISSUE", "SKEIN_TOKEN_FILE", "SKEIN_EXCLUDE_SESSION",
                  "SKEIN_RUNNER_SHA256", "SKEIN_LOADER_SHA256", "SKEIN_CLIENT", "SKEIN_PLATFORM")

# The parts of the instruction that are not the record's.
INSTRUCTION_DETACHED = ("Run `command` once in a shell: it returns after the line "
                        "{\"event\":\"monitor_started\",...} and the monitor keeps running on its own.")
INSTRUCTION_JOURNAL = (INSTRUCTION_DETACHED + " No channel wakes you: events go only to the journal "
                       "(the `journal` field), read it yourself.")
INSTRUCTION_RESTART = "Every restart runs the same `command`: no new preflight, no new mint."

PROBE_AVAILABLE = "available"
PROBE_UNAVAILABLE = "unavailable"


class ProbeError(Exception):
    """A probe that could not run; the channel is unavailable."""


class ProbeRunner(object):
    """Runs one probe of a channel and answers PROBE_AVAILABLE or
    PROBE_UNAVAILABLE. This one has no executor: every probe fails."""

    def run(self, channel, probe, env):
        raise ProbeError("no probe executor for type %s" % probe.get("type"))


class Detector(object):
    """Finds what the environment of the agent offers beyond the record's own
    methods. This one finds nothing."""

    def __init__(self):
        # Warnings of the detection for the preflight line.
        self.warnings = []

    def under_host(self, ctx):
        """(channel id, kinds of own methods the host keeps) when the agent
        process was launched by a host that goes first at step 5, else None."""
        return None

    def hosts(self, ctx):
        """Channel ids of the detected hosts and terminal environments of
        steps 5-6, in order."""
        return []

    def binding(self, ctx, channel_id):
        """The confirmed binding of the runner to a pane of the blind
        channel, or None. This one confirms none."""
        return None

    def blind(self, ctx):
        """Channel ids of the blind channels whose three conditions hold, in
        order."""
        return []

    def address_specs(self, ctx):
        """The AddressSpecs of the identity of the addressee."""
        return []


class Context(object):
    """What the choice reads: the registry, the settings of the start, the
    environment of the agent, where the agent stands in its chain of
    processes, the identity of the addressee, the two boundaries, and whether
    channels that need the ledger may be used."""

    def __init__(self, registry, settings, env, placement, detector, prober, external=True,
                 identity=None):
        self.registry = registry
        self.settings = settings
        self.env = env
        self.placement = placement
        self.detector = detector
        self.prober = prober
        self.external = external
        self.identity = identity


class Choice(object):
    """The chosen method: channel for detached, reasons why earlier methods
    were skipped and why this one was taken, fallbacks the next non-blind
    channels found available after it."""

    def __init__(self, method, channel, reasons, fallbacks=None):
        self.method = method
        self.channel = channel
        self.reasons = reasons
        self.fallbacks = fallbacks or []

    @property
    def reason(self):
        return "; ".join(self.reasons)


# --- conditions --------------------------------------------------------------

def env_holds(cond, env):
    value = env.get(cond.get("name", ""), "")
    if not value:
        return False
    if cond.get("pattern") is not None and not re.fullmatch(cond["pattern"], value):
        return False
    return cond.get("equals") is None or value == cond["equals"]


def expand_path(path, env):
    """A setting path: "~/" is the home directory, "{env:NAME}/" the value
    of NAME. None when the variable it needs is not set."""
    if path.startswith("~/"):
        home = env.get("HOME", "")
        return os.path.join(home, path[2:]) if home else None
    if path.startswith("{env:"):
        name, _, rest = path[len("{env:"):].partition("}")
        value = env.get(name, "")
        if not value:
            return None
        return os.path.join(value, rest.lstrip("/")) if rest else value
    return path


_MISSING = object()


def read_setting(cond, env):
    """The value at the dotted key of the setting file, or _MISSING when the
    file, the key or the format cannot be read."""
    path = expand_path(cond.get("path", ""), env)
    if path is None:
        return _MISSING
    try:
        with open(path, "rb") as fh:
            raw = fh.read()
    except OSError:
        return _MISSING
    fmt = cond.get("format")
    try:
        if fmt == "json":
            doc = json.loads(raw.decode("utf-8"))
        elif fmt == "toml":
            try:
                import tomllib
            except ImportError:  # Python < 3.11: no TOML reader in the stdlib
                return _MISSING
            doc = tomllib.loads(raw.decode("utf-8"))
        else:
            return _MISSING
    except ValueError:
        return _MISSING
    for part in cond.get("key", "").split("."):
        if not isinstance(doc, dict) or part not in doc:
            return _MISSING
        doc = doc[part]
    return doc


def setting_holds(cond, env):
    value = read_setting(cond, env)
    return value is not _MISSING and value == cond.get("equals")


def program_found(name, env):
    if os.path.isabs(name):
        return os.path.isfile(name) and os.access(name, os.X_OK)
    return shutil.which(name, path=env.get("PATH", os.defpath)) is not None


def conditions_hold(conds, env):
    """(True, None) when every condition holds, else (False, cause)."""
    conds = conds or {}
    for cond in conds.get("env") or []:
        if not env_holds(cond, env):
            return False, "variable %s not set as required" % cond.get("name")
    for cond in conds.get("settings") or []:
        if not setting_holds(cond, env):
            return False, "setting %s of %s not read or not as required" % (cond.get("key"), cond.get("path"))
    for name in conds.get("programs") or []:
        if not program_found(name, env):
            return False, "program %s not found" % name
    return True, None


def _platform_key():
    if sys.platform == "darwin":
        return "macos"
    return "windows" if os.name == "nt" else "linux"


def channel_available(ctx, channel_id):
    """(True, None) when the channel can be used from here, else (False,
    cause): its record exists, its detection holds, a binary of this
    platform is found, and its probes answer available."""
    channel = ctx.registry.channel(channel_id)
    if channel is None:
        return False, "channel %s is not in the registry" % channel_id
    if not ctx.external:
        return False, "the ledger directory is not writable"
    ok, cause = conditions_hold(channel.get("detect"), ctx.env)
    if not ok:
        return False, cause
    binaries = (channel.get("binaries") or {}).get(_platform_key())
    if binaries is not None and not any(program_found(b, ctx.env) for b in binaries):
        return False, "no program of channel %s found" % channel_id
    if channel.get("probeUndefined"):
        return True, None
    for probe in channel.get("probes") or []:
        try:
            answer = ctx.prober.run(channel, probe, ctx.env)
        except Exception as exc:  # any failure of a probe: unavailable, not a crash
            return False, "probe %s failed: %s" % (probe.get("type"), exc)
        if answer != PROBE_AVAILABLE:
            return False, "probe %s: unavailable" % probe.get("type")
    return True, None


# --- the method order --------------------------------------------------------

def _inherited(ctx, channel_id):
    return ctx.identity is not None and channel_id in ctx.identity.inherited


def choose(ctx):
    """The Choice of the method order. Raises ConfigInvalid for a forced
    channel that belongs to an outer agent."""
    settings = ctx.settings
    forced = settings.forced
    if forced == JOURNAL_CHANNEL:
        return Choice(METHOD_JOURNAL, None, ["forced: journal"])
    if forced is not None:
        channel = ctx.registry.channel(forced)
        if _inherited(ctx, forced) or (channel and channel.get("step") in (5, 6) and ctx.placement.nested):
            raise ConfigInvalid("SKEIN_MONITOR_CHANNEL",
                                "the channel %s belongs to an outer agent (nested agent)" % forced)
        # A forced blind channel lifts conditions (1) and (2) of US-7, not
        # the binding to the pane (decision 8).
        if ctx.registry.is_blind(forced) and ctx.detector.binding(ctx, forced) is None:
            return Choice(METHOD_JOURNAL, None,
                          ["forced %s unavailable: the binding to its pane is not confirmed" % forced])
        ok, cause = channel_available(ctx, forced)
        if ok:
            return Choice(METHOD_DETACHED, forced, ["forced: %s" % forced])
        # A forced channel has no fallback but the journal.
        return Choice(METHOD_JOURNAL, None, ["forced %s unavailable: %s" % (forced, cause)])

    reasons = []
    under = ctx.detector.under_host(ctx)
    kept = set(under[1]) if under else None
    agent = settings.record.agent if settings.record is not None else None
    methods = sorted((agent or {}).get("methods") or [], key=lambda m: _KIND_STEP.get(m.get("kind"), 9))
    candidates = []  # (step, kind, channel) in order
    for m in methods:
        kind = m.get("kind")
        if kept is not None and kind not in kept:
            reasons.append("%s: not used under host %s" % (kind, under[0]))
            continue
        ok, cause = conditions_hold(m.get("conditions"), ctx.env)
        if not ok:
            reasons.append("%s: %s" % (m.get("channel") or kind, cause))
            continue
        if kind == "delivery":
            if _inherited(ctx, m.get("channel")):
                reasons.append("%s: its id belongs to an outer agent" % m.get("channel"))
                continue
            candidates.append((3, kind, m.get("channel")))
        else:
            candidates.append((_KIND_STEP.get(kind, 9), kind, None))
    hosts = []
    if under:
        hosts.append(under[0])
    hosts.extend(c for c in ctx.detector.hosts(ctx) if c not in hosts)
    candidates.extend((5, "host", c) for c in hosts)
    if not settings.blind_off:
        candidates.extend((7, "blind", c) for c in ctx.detector.blind(ctx))

    chosen = None
    fallbacks = []
    for step, kind, channel in candidates:
        if channel is None:
            if chosen is None:
                chosen = Choice(kind, None, reasons + ["%s: available" % kind])
                break
            continue
        if chosen is not None and (kind == "blind" or ctx.registry.is_blind(channel)):
            continue
        ok, cause = channel_available(ctx, channel)
        if not ok:
            if chosen is None:
                reasons.append("%s: %s" % (channel, cause))
            continue
        if chosen is None:
            chosen = Choice(METHOD_DETACHED, channel, reasons + ["%s: available" % channel])
        else:
            fallbacks.append(channel)
    if chosen is None:
        return Choice(METHOD_JOURNAL, None, reasons + ["journal: no other method available"])
    chosen.fallbacks = fallbacks
    return chosen


# --- the sandbox ---------------------------------------------------------------

def dir_writable(path):
    """Whether a file can be created and removed in path."""
    probe = os.path.join(path, ".skein-probe-%d-%d" % (os.getpid(), random.randint(0, 1 << 30)))
    try:
        fd = os.open(probe, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
    except OSError:
        return False
    os.close(fd)
    try:
        os.remove(probe)
    except OSError:
        return False
    return True


def ledger_dir(env=None):
    return os.path.join(user_dir(env), LEDGER_DIR_NAME)


def check_sandbox(token_file, env):
    """The checks of the sandbox: the token directory (state, messages) and
    the journal directory take a write, or the start is config_invalid
    naming the directory; the ledger directory decides whether channels may
    be used. Returns (journal path, external)."""
    token_dir = os.path.dirname(os.path.abspath(token_file))
    if not dir_writable(token_dir):
        raise ConfigInvalid(token_dir, "the token directory does not take a write")
    try:
        probe_journal_dir(env)
    except JournalUnavailable as exc:
        raise ConfigInvalid(exc.path, "the journal directory is unavailable (%s)" % exc.reason)
    except PlatformRefusal as exc:
        raise ConfigInvalid(journal_dir_name(env), "the journal directory is unavailable (%s)" % exc.reason)
    try:
        ledger = ledger_dir(env)
        current_platform().ensure_private_dir(ledger)
        external = dir_writable(ledger)
    except PlatformRefusal:
        external = False
    return journal_path(token_file, env), external


def journal_dir_name(env):
    try:
        return journal_dir(env)
    except PlatformRefusal:
        return "the journal directory"


# --- preflight.json ------------------------------------------------------------

def preflight_file_path(token_file):
    return os.path.join(os.path.dirname(os.path.abspath(token_file)), PREFLIGHT_FILE_NAME)


class StateLock(object):
    """state.lock next to the token, held exclusively while open."""

    def __init__(self, token_file):
        if _preflight_fcntl is None:
            raise PlatformRefusal("unsupported", STATE_LOCK_NAME)
        path = os.path.join(os.path.dirname(os.path.abspath(token_file)), STATE_LOCK_NAME)
        try:
            self._fd = os.open(path, os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0), 0o600)
        except OSError:
            raise PlatformRefusal("unreadable", path)
        try:
            _preflight_fcntl.flock(self._fd, _preflight_fcntl.LOCK_EX)
        except OSError:
            os.close(self._fd)
            raise PlatformRefusal("unreadable", path)

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        os.close(self._fd)
        return False


def load_preflight(token_file):
    """The saved result, or None when there is none that can be read."""
    try:
        raw = _read_checked(preflight_file_path(token_file), _state_private)
        doc = json.loads(raw.decode("utf-8"))
    except (PlatformRefusal, OSError, ValueError):
        return None
    if not isinstance(doc, dict) or doc.get("version") != PREFLIGHT_VERSION:
        return None
    if not isinstance(doc.get("preflightId"), int) or isinstance(doc.get("preflightId"), bool):
        return None
    return doc


def save_preflight(token_file, doc):
    data = json.dumps(doc, separators=(",", ":"), sort_keys=True).encode("utf-8")
    PosixPlatform.atomic_write(preflight_file_path(token_file), data, dir_check=_state_dir_ok)


def runner_alive(token_file, env):
    """Whether a runner of this token holds its one-runner lock. A lock that
    cannot be taken for an OS error is config_invalid."""
    try:
        lock = current_platform().acquire_runner_lock(token_file, 0, env)
    except LockBusy:
        return True
    except PlatformRefusal as exc:
        raise ConfigInvalid(runner_lock_path(token_file, env), "the runner lock cannot be taken (%s)" % exc.reason)
    lock.release()
    return False


def shell_quote(s):
    return "'" + s.replace("'", "'\\''") + "'"


def working_command(env, token_file, preflight_id, settings):
    """The working command: the variables of the preflight command, the
    starter path and both digests with SKEIN_MONITOR_MODE=run, the number of
    the result and the overrides. Every SKEIN_* variable the runner reads is
    set, an override that was not given to an empty value."""
    parts = ["SKEIN_MONITOR_MODE=run", "SKEIN_MONITOR_PREFLIGHT=%d" % preflight_id,
             "SKEIN_MONITOR_CHANNEL=" + shell_quote(settings.channel or ""),
             "SKEIN_MONITOR_BLIND=" + shell_quote(BLIND_OFF if settings.blind_off else "")]
    parts.extend(name + "=" + shell_quote(env.get(name, "")) for name in BOOTSTRAP_VARS)
    starter = os.path.join(os.path.dirname(os.path.abspath(token_file)), CLIENT_STARTER_FILE)
    parts.append("python3 " + shell_quote(starter))
    return " ".join(parts)


def record_method(record, choice):
    """The method of the record the choice took, or None (a host, a blind
    channel, the journal)."""
    agent = record.agent if record is not None else None
    for m in (agent or {}).get("methods") or []:
        if choice.method == METHOD_DETACHED:
            if m.get("kind") == "delivery" and m.get("channel") == choice.channel:
                return m
        elif m.get("kind") == choice.method:
            return m
    return None


def instruction(registry, record, choice):
    """The instruction of the chosen method: the tool and parameters of the
    record's method (or how a detached or journal monitor starts), the
    restart, the warnings of the record and of the chosen channel."""
    method = record_method(record, choice)
    if method is not None:
        parts = [method.get("instruction") or ""]
    elif choice.method == METHOD_JOURNAL:
        parts = [INSTRUCTION_JOURNAL]
    else:
        parts = [INSTRUCTION_DETACHED]
    parts.append(INSTRUCTION_RESTART)
    if record is not None and record.agent is not None:
        parts.extend(record.warnings)
    channel = registry.channel(choice.channel) if choice.channel is not None else None
    if channel is not None:
        parts.extend(channel.get("warnings") or [])
    return " ".join(p for p in parts if p)


def _identity_doc(identity):
    if identity is None:
        return None
    return {
        "process": list(identity.process) if identity.process else None,
        "addresses": [{"channel": a.channel, "value": a.value, "qualifiers": [list(q) for q in a.qualifiers]}
                      for a in identity.addresses],
        "inherited": list(identity.inherited),
        "terminal": identity.terminal,
    }


def _binding_doc(binding):
    """The confirmed binding of a chosen blind channel as preflight.json
    keeps it: the working runner checks the addressee against it before each
    delivery (the server and session of the multiplexer, the pane id, the PID
    and start of the agent process in the pane)."""
    if binding is None:
        return None
    return {"channel": binding.channel["id"], "multiplexer": bool(binding.multiplexer),
            "depth": binding.depth, "pane": dict(binding.pane)}


def system_placement(registry, record):
    try:
        chain = ancestry(SystemProcesses, os.getpid())
    except PlatformRefusal:
        chain = []
    return place(chain, registry, record)


def preflight(token_file, env, registry, settings, detector=None, prober=None, placement=None):
    """Run the preflight and return its line. Raises ConfigInvalid, or Stop
    (already_running) with a live runner of the token."""
    if sys.version_info < MIN_PYTHON:
        raise ConfigInvalid("python3", "Python %d.%d or newer is needed" % MIN_PYTHON)
    detector = detector or RegistryDetector()
    prober = prober or ProbeRunner()
    journal, external = check_sandbox(token_file, env)
    if placement is None:
        placement = system_placement(registry, settings.record)
    ctx = Context(registry, settings, env, placement, detector, prober, external)
    ctx.identity = identity(placement, registry, env, detector.address_specs(ctx))
    choice = choose(ctx)
    # The record's warnings go into the instruction (a named record) or are
    # the `client` value "general" (the general record), not into warnings.
    record_warnings = settings.record.warnings if settings.record is not None else []
    warnings = [w for w in settings.warnings if w not in record_warnings] + list(detector.warnings)
    if not external:
        warnings.append("The ledger directory is not writable: no external channel is used.")
    binding = None
    if choice.channel is not None and registry.is_blind(choice.channel):
        warnings.append(BLIND_WARNING)
        binding = _binding_doc(detector.binding(ctx, choice.channel))
    try:
        with StateLock(token_file):
            if runner_alive(token_file, env):
                raise Stop("already_running")
            last = load_preflight(token_file)
            number = (last["preflightId"] if last else 0) + 1
            save_preflight(token_file, {
                "version": PREFLIGHT_VERSION,
                "preflightId": number,
                "client": settings.record.client,
                "record": settings.record.id,
                "platform": settings.platform,
                "method": choice.method,
                "channel": choice.channel,
                "fallbacks": choice.fallbacks,
                "overrides": {"SKEIN_MONITOR_CHANNEL": settings.channel,
                              "SKEIN_MONITOR_BLIND": BLIND_OFF if settings.blind_off else None},
                "identity": _identity_doc(ctx.identity),
                "binding": binding,
            })
    except (PlatformRefusal, OSError) as exc:
        raise ConfigInvalid(os.path.dirname(os.path.abspath(token_file)),
                            "the preflight result cannot be saved (%s)" % getattr(exc, "reason", exc))
    return {
        "event": "preflight",
        "preflightId": number,
        "client": settings.record.id,
        "method": choice.method,
        "channel": choice.channel,
        "reason": choice.reason,
        "warnings": warnings,
        "journal": journal,
        "command": working_command(env, token_file, number, settings),
        "instruction": instruction(registry, settings.record, choice),
    }
