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
# live runner of the token it answers already_running and changes nothing,
# unless that runner's addressee changed: then it writes the new result, and
# the next working start of it takes the monitor over (takeover.py).
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
# here find nothing and run no probe; the registry detector (detect.py) and
# the probe executors (executors.py#SystemProbeRunner) are the preflight's
# defaults.

import json
import ntpath
import os
import random
import re
import shutil
import sys

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
# The channel of the methods that deliver on the runner's stdout.
CHANNEL_STDOUT = "stdout"

# Steps 2-4 of the method order by the kind of a record's method.
_KIND_STEP = {"stream": 2, "delivery": 3, "exit-on-event": 4}

# The variables of the bootstrap the working command repeats, in its order.
# The variables of the preflight command the working command repeats.
BOOTSTRAP_VARS = ("SKEIN_PUBLIC_URL", "SKEIN_ISSUE", "SKEIN_TOKEN_FILE", "SKEIN_EXCLUDE_SESSION",
                  "SKEIN_RUNNER_SHA256", "SKEIN_LOADER_SHA256", "SKEIN_CLIENT", "SKEIN_PLATFORM")
# Start variables of the mint's parameters, always set by the preflight
# command (empty for the mint's default): SKEIN_SUBTREE=1 (subtree=true),
# SKEIN_EVENT_TYPES (comma-separated eventTypes other than the default).
MINT_PARAM_VARS = ("SKEIN_SUBTREE", "SKEIN_EVENT_TYPES")

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

    def capture(self, channel, env):
        """The values of the capture paths of the channel's addressee
        command, [[path, value], ...]; ProbeError when they cannot be
        read. This one reads none."""
        raise ProbeError("no executor for the addressee check")


class Detector(object):
    """Finds what the environment of the agent offers beyond the record's own
    methods. This one finds nothing."""

    def __init__(self):
        # Warnings of the detection for the preflight line.
        self.warnings = []
        # What the monitor is to report as degraded, {"cause", "variables"}:
        # saved with the result for the working runner.
        self.degraded = None

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

    def addressee_holds(self, ctx, channel):
        """Whether the addressee of a host or terminal environment of steps
        5-6 is this agent ("Addressee and check"): a forced one is checked
        the same way as a detected one. This one confirms none."""
        return False

    def blind(self, ctx):
        """Channel ids of the blind channels whose three conditions hold, in
        order."""
        return []

    def blind_reachable(self, ctx):
        """Channel ids of the blind channels whose text reaches this agent's
        pane (condition (3) and the multiplexer rules), for a forced one.
        This one finds none."""
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
                 identity=None, token_file=None):
        # token_file names the token directory (the message directory of a
        # .cmd program, US-11); None in a context without a token.
        self.token_file = token_file
        self.registry = registry
        self.settings = settings
        self.env = env
        self.placement = placement
        self.detector = detector
        self.prober = prober
        self.external = external
        self.identity = identity
        # {channel id: [[path, value], ...]}: what the addressee command of
        # an available channel gave at its capture paths.
        self.captures = {}


class Choice(object):
    """The chosen method: channel for detached, reasons why earlier methods
    were skipped and why this one was taken, fallbacks the next non-blind
    channels found available after it."""

    def __init__(self, method, channel, reasons, fallbacks=None, record_method=None):
        self.method = method
        self.channel = channel
        self.reasons = reasons
        self.fallbacks = fallbacks or []
        # The method of the record that was chosen (its own stream, delivery
        # or exit-on-event), None for a host, a blind channel or the journal.
        self.record_method = record_method

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


def _same_json_value(a, b):
    """Equality of two JSON values that keeps their types apart: a boolean
    equals only a boolean (in Python True == 1 and False == 0)."""
    if isinstance(a, bool) or isinstance(b, bool):
        return isinstance(a, bool) and isinstance(b, bool) and a == b
    if isinstance(a, list) and isinstance(b, list):
        return len(a) == len(b) and all(_same_json_value(x, y) for x, y in zip(a, b))
    if isinstance(a, dict) and isinstance(b, dict):
        return a.keys() == b.keys() and all(_same_json_value(a[k], b[k]) for k in a)
    if isinstance(a, (list, dict)) or isinstance(b, (list, dict)):
        return False
    return a == b


def setting_holds(cond, env):
    value = read_setting(cond, env)
    return value is not _MISSING and _same_json_value(value, cond.get("equals"))


def program_found(name, env):
    if os.path.isabs(name):
        return os.path.isfile(name) and os.access(name, os.X_OK)
    return shutil.which(name, path=env.get("PATH", os.defpath)) is not None


def home_path(name, env):
    """name with a leading "~/" expanded from HOME (USERPROFILE on Windows
    when HOME is unset); name itself without one; None when it needs a home
    and none is set."""
    if not name.startswith("~/"):
        return name
    home = env.get("HOME", "") or (env.get("USERPROFILE", "") if os.name == "nt" else "")
    return os.path.join(home, name[2:]) if home else None


def channel_program(name, env, agent):
    """What the program of a channel named name runs as, or None when it is
    not found: the executable of the agent process when its base name is
    name, else name itself when the PATH of env finds it. The agent's own
    executable comes first: a launcher such as the npm wrapper of Codex
    execs a native program outside every PATH (bin/codex.js), and the login
    shell of the agent's commands rebuilds its PATH without the wrapper's
    directory, while the program is the build that owns the agent's thread.
    A name "{env:NAME}" is the program the variable names (a name or a
    path), None when it is unset or empty."""
    if name.startswith("{env:") and name.endswith("}"):
        name = env.get(name[len("{env:"):-1], "")
        if not name or (name.startswith("{env:") and name.endswith("}")):
            return None
    if name.startswith("~/"):
        path = home_path(name, env)
        return path if path is not None and program_found(path, env) else None
    executable = agent.executable if agent is not None else None
    if (not os.path.isabs(name) and executable and _program_name(executable) == _program_name(name)
            and os.path.isfile(executable) and os.access(executable, os.X_OK)):
        return executable
    if os.name == "nt" and not os.path.isabs(name):
        # Windows runs a program by the path PATH and PATHEXT give, a .cmd
        # shim among them: that path is the program, kept as found.
        return shutil.which(name, path=env.get("PATH", os.defpath))
    return name if program_found(name, env) else None


def _program_name(path):
    """The name of a program for comparing a path with a name of a channel
    record: its base name, on Windows without ".exe" and in any case (codex,
    codex.exe and CODEX.EXE are one program there)."""
    base = os.path.basename(path)
    if os.name == "nt":
        base = base.lower()
        return base[:-4] if base.endswith(".exe") else base
    return base


def channel_programs(channel, env, agent):
    """The program the executor of the channel names (its first argument),
    when it runs from a path other than that name: {name: path}, the path of
    the first of the channel's programs of this platform that is found (the
    executable of the agent process, a path under the home directory, the
    PATH). The working runner and the probes run it by that path
    (preflight.json "programs")."""
    argv = (channel.get("executor") or {}).get("argv") or []
    for name in (channel.get("binaries") or {}).get(_platform_key()) or []:
        path = channel_program(name, env, agent)
        if path is None:
            continue
        key = argv[0] if argv else name
        if path == key:
            return {}
        if not os.path.isabs(path):
            # A program of another name than the executor's, found on PATH
            # (orca-ide for orca): it runs by its path, the only form the
            # executor takes (executors.py#ArgvExecutor).
            path = shutil.which(path, path=env.get("PATH", os.defpath))
            if path is None:
                continue
        return {key: path}
    return {}


def _missing_programs_cause(channel_id, binaries, env):
    """Why no program of the channel was found, naming a path under the
    home directory that HOME does not give."""
    unexpanded = [b for b in binaries if b.startswith("~/") and home_path(b, env) is None]
    unset = [b[len("{env:"):-1] for b in binaries
             if b.startswith("{env:") and b.endswith("}") and not env.get(b[len("{env:"):-1], "")]
    cause = "no program of channel %s found" % channel_id
    notes = []
    if unexpanded:
        notes.append("%s: HOME is not set" % ", ".join(unexpanded))
    if unset:
        notes.append("%s not set" % ", ".join(unset))
    if notes:
        cause += " (%s)" % "; ".join(notes)
    return cause


def agent_options_absent(options, agent):
    """(True, None) when the argument list of the agent process carries none
    of the long options, neither alone nor as "--name=value"; an agent
    process that is not determined, or whose arguments cannot be read, does
    not hold."""
    argv = agent.argv if agent is not None else None
    if argv is None:
        return False, "the arguments of the agent process cannot be read"
    for option in options:
        for arg in argv[1:]:
            if arg == option or arg.startswith(option + "="):
                return False, "the agent process runs with %s" % option
    return True, None


def conditions_hold(conds, env, agent=None):
    """(True, None) when every condition holds, else (False, cause). agent is
    the agent process (identity.Placement.agent) a method's condition on its
    options reads."""
    conds = conds or {}
    if conds.get("agentOptionsAbsent"):
        ok, cause = agent_options_absent(conds["agentOptionsAbsent"], agent)
        if not ok:
            return False, cause
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
    spec = channel.get("executor") or {}
    if spec.get("type") == "http":
        # An http channel runs no program: its URL and its key must be
        # given by the variables of the agent process.
        http = spec.get("http") or {}
        try:
            expand_url(http.get("url") or "", ctx.env)
        except ExecutorRefusal as exc:
            return False, "channel %s: %s" % (channel_id, exc.reason)
        if http.get("keyEnv") and http_key(http, ctx.env) is None:
            return False, "channel %s: no key variable is set: %s" % (channel_id, ", ".join(http["keyEnv"]))
        return _probe(ctx, channel_id, channel)
    # Every channel names its programs per platform; a platform it names
    # none for has no program, so the channel is unavailable there.
    binaries = (channel.get("binaries") or {}).get(_platform_key())
    if not binaries:
        return False, "channel %s has no program for this platform" % channel_id
    programs = [channel_program(b, ctx.env, ctx.placement.agent) for b in binaries]
    if not any(p is not None for p in programs):
        return False, _missing_programs_cause(channel_id, binaries, ctx.env)
    if any(is_batch_program(p) for p in programs if p is not None) and \
            (channel.get("executor") or {}).get("feed") == "file" and \
            ctx.token_file is not None and batch_message_dir(ctx.token_file, ctx.env) is None:
        # US-11: a .cmd program takes a message file only by an ASCII path.
        return False, "no private message directory of an ASCII path for the .cmd program of %s" % channel_id
    return _probe(ctx, channel_id, channel)


def _probe(ctx, channel_id, channel):
    """The probes of an available channel, then its capture."""
    if channel.get("probeUndefined"):
        return _capture(ctx, channel_id, channel)
    for probe in channel.get("probes") or []:
        try:
            answer = ctx.prober.run(channel, probe, ctx.env)
        except Exception as exc:  # any failure of a probe: unavailable, not a crash
            return False, "probe %s failed: %s" % (probe.get("type"), exc)
        if answer != PROBE_AVAILABLE:
            return False, "probe %s: unavailable" % probe.get("type")
    return _capture(ctx, channel_id, channel)


def _capture(ctx, channel_id, channel):
    """The values the addressee check compares before each transfer, read
    once here (registry.go#AddresseeCommand.Capture): no value, no channel."""
    command = (channel.get("addressee") or {}).get("command") or {}
    if not command.get("capture"):
        return True, None
    try:
        ctx.captures[channel_id] = ctx.prober.capture(channel, ctx.env)
    except Exception as exc:  # any failure of the check: unavailable, not a crash
        return False, "addressee check of %s: %s" % (channel_id, exc)
    return True, None


# --- the method order --------------------------------------------------------

def _inherited(ctx, channel_id):
    return ctx.identity is not None and channel_id in ctx.identity.inherited


def _delivery_refusal(ctx, method, channel_id):
    """Why the own delivery method of a forced channel of step 3 may not be
    used, or None: the same checks the order makes for it."""
    if method is None:
        return "no delivery method of this agent's record names it"
    under = ctx.detector.under_host(ctx)
    if under and "delivery" not in set(under[1]):
        return "not used under host %s" % under[0]
    agent_proc = ctx.placement.agent if (method.get("conditions") or {}).get("agentOptionsAbsent") else None
    ok, cause = conditions_hold(method.get("conditions"), ctx.env, agent_proc)
    return None if ok else cause


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
        # A forced host is still this agent's only (US-5, "Addressee and
        # check" pp. 2, 6): the checks of the detection hold for it too.
        if channel and channel.get("step") in (5, 6) and not ctx.detector.addressee_holds(ctx, channel):
            # The more precise cause when the channel itself cannot run here
            # (no program: a path under a home that HOME does not give).
            ok, cause = channel_available(ctx, forced)
            return Choice(METHOD_JOURNAL, None,
                          ["forced %s unavailable: %s" % (forced, cause if not ok else "its addressee is not this agent")])
        if ctx.registry.is_blind(forced) and ctx.detector.binding(ctx, forced) is None:
            return Choice(METHOD_JOURNAL, None,
                          ["forced %s unavailable: the binding to its pane is not confirmed" % forced])
        if ctx.registry.is_blind(forced) and forced not in ctx.detector.blind_reachable(ctx):
            return Choice(METHOD_JOURNAL, None,
                          ["forced %s unavailable: its text would not reach this agent's pane" % forced])
        agent = settings.record.agent if settings.record is not None else None
        own = [m for m in (agent or {}).get("methods") or []
               if m.get("kind") == "delivery" and m.get("channel") == forced]
        if channel and channel.get("step") == 3:
            # A forced own delivery passes every check the order makes for
            # it: a method of this record, the kinds a host keeps, the
            # conditions of the method (US-5, INV-4).
            cause = _delivery_refusal(ctx, own[0] if own else None, forced)
            if cause is not None:
                return Choice(METHOD_JOURNAL, None, ["forced %s unavailable: %s" % (forced, cause)])
        ok, cause = channel_available(ctx, forced)
        if ok:
            return Choice(METHOD_DETACHED, forced, ["forced: %s" % forced],
                          record_method=own[0] if own else None)
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
        agent_proc = ctx.placement.agent if (m.get("conditions") or {}).get("agentOptionsAbsent") else None
        ok, cause = conditions_hold(m.get("conditions"), ctx.env, agent_proc)
        if not ok:
            reasons.append("%s: %s" % (m.get("channel") or kind, cause))
            continue
        if kind == "delivery":
            if _inherited(ctx, m.get("channel")):
                reasons.append("%s: its id belongs to an outer agent" % m.get("channel"))
                continue
            candidates.append((3, kind, m.get("channel"), m))
        else:
            candidates.append((_KIND_STEP.get(kind, 9), kind, None, m))
    # Steps 2-4 first; steps 5-6 and 7 are asked for only when the walk
    # reaches them, so their warnings (ambiguous_terminal) are raised only
    # then.
    chosen, done = _walk(ctx, candidates, reasons, None, [])
    hosts = []
    if not done:
        if under:
            hosts.append(under[0])
        hosts.extend(c for c in ctx.detector.hosts(ctx) if c not in hosts)
        chosen, done = _walk(ctx, [(5, "host", c, None) for c in hosts], reasons, chosen,
                             chosen.fallbacks if chosen else [])
    # Condition (2) of US-7: a detected host, available or not, keeps the
    # blind channels off.
    if not done and chosen is None and not hosts and not settings.blind_off:
        chosen, done = _walk(ctx, [(7, "blind", c, None) for c in ctx.detector.blind(ctx)], reasons, chosen, [])
    if chosen is None:
        return Choice(METHOD_JOURNAL, None, reasons + ["journal: no other method available"])
    return chosen


def _walk(ctx, candidates, reasons, chosen, fallbacks):
    """Walk candidates (step, kind, channel, record method) in order. The first available
    one is chosen; after a detached choice the next available non-blind
    channels are its fallbacks. Returns (chosen, done): done when a method
    on stdout was chosen, which ends the order."""
    for step, kind, channel, method in candidates:
        if channel is None:
            if chosen is None:
                # The record's own stream and exit-on-event deliver on the
                # runner's stdout (US-2, US-3).
                return Choice(kind, CHANNEL_STDOUT, reasons + ["%s: available" % kind],
                              record_method=method), True
            continue
        if chosen is not None and (kind == "blind" or ctx.registry.is_blind(channel)):
            continue
        ok, cause = channel_available(ctx, channel)
        if not ok:
            if chosen is None:
                reasons.append("%s: %s" % (channel, cause))
            continue
        if chosen is None:
            chosen = Choice(METHOD_DETACHED, channel, reasons + ["%s: available" % channel],
                            record_method=method)
        else:
            fallbacks.append(channel)
    if chosen is not None:
        chosen.fallbacks = fallbacks
    return chosen, False


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
    """state.lock next to the token, held exclusively while open. create=
    False opens only an existing one (PlatformRefusal "not_found" when there
    is none)."""

    def __init__(self, token_file, create=True):
        if not current_platform().has_file_locks():
            raise PlatformRefusal("unsupported", STATE_LOCK_NAME)
        path = os.path.join(os.path.dirname(os.path.abspath(token_file)), STATE_LOCK_NAME)
        flags = os.O_RDWR | getattr(os, "O_NOFOLLOW", 0) | (os.O_CREAT if create else 0)
        try:
            self._fd = os.open(path, flags, 0o600)
        except FileNotFoundError:
            raise PlatformRefusal("not_found", path)
        except OSError:
            raise PlatformRefusal("unreadable", path)
        try:
            current_platform().lock_fd(self._fd, blocking=True)
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
        raw = current_platform().read_checked(preflight_file_path(token_file), _state_private)
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
    current_platform().atomic_write(preflight_file_path(token_file), data, dir_check=_state_dir_ok)


def runner_alive(token_file, env):
    """Whether a runner of this token holds its one-runner lock (P-5:
    locks/<key>.lock in client mode). The preflight asks only while it holds
    state.lock, and holds the runner lock for that instant only: a working
    start takes the lock with take_runner_lock, which waits out that instant
    instead of reading it as a live runner. A lock that cannot be taken for
    an OS error is config_invalid."""
    try:
        lock = current_platform().acquire_runner_lock(token_file, 0, env)
    except LockBusy:
        return True
    except PlatformRefusal as exc:
        raise ConfigInvalid(runner_lock_path(token_file, env), "the runner lock cannot be taken (%s)" % exc.reason)
    lock.release()
    return False


def take_runner_lock(token_file, env):
    """Take the one-runner lock for a working start (P-5, P-6). A busy lock
    may be the instant a preflight holds it to see whether a runner is alive
    (runner_alive, under state.lock): then wait for state.lock, which that
    preflight releases after its check, and try once more. Returns the
    RunnerLock; raises LockBusy when a live runner holds it, PlatformRefusal
    for a lock that cannot be taken."""
    try:
        return current_platform().acquire_runner_lock(token_file, 0, env)
    except LockBusy:
        pass
    try:
        # Only an existing state.lock: a holder that is ending removes it with
        # the token and then the directory, and a competitor must not put it
        # back (P-12).
        state_lock = StateLock(token_file, create=False)
    except PlatformRefusal as exc:
        if exc.reason == "not_found":
            # The holder is ending (or has ended): busy, not broken.
            raise LockBusy(runner_lock_path(token_file, env))
        raise
    with state_lock:
        return current_platform().acquire_runner_lock(token_file, 0, env)


def shell_quote(s):
    return "'" + s.replace("'", "'\\''") + "'"


# The Windows shells the commands of a start are built for (US-11): the
# shell a record names, Windows PowerShell for a record without one and for
# the general record.
SHELL_GIT_BASH = "git-bash"
SHELL_PWSH = "pwsh"
SHELL_POWERSHELL = "powershell"

# PowerShell takes the typographic single quotes as quotes of a
# single-quoted string too (about_Quoting_Rules); inside one, each of them is
# written twice to stand for itself.
_PS_QUOTES = u"'\u2018\u2019\u201a\u201b"

# Git Bash converts arguments and variables that look like POSIX paths
# before it runs a Windows program; these variables turn that off, so the
# literal paths reach Python unchanged.
_MSYS_NO_CONVERSION = "MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*' "

# The Python of a Windows start: the launcher of the python.org installer
# and of the Python install manager.
_WINDOWS_PYTHON = "py -3"


# Variables whose values are a word or a number of the runner's own; a
# POSIX shell gets them without quotes.
_BARE_VALUES = ("SKEIN_MONITOR_MODE", "SKEIN_MONITOR_PREFLIGHT")


def powershell_quote(s):
    return "'" + "".join(c + c if c in _PS_QUOTES else c for c in s) + "'"


def command_shell(settings):
    """The shell the commands of this start are written for: None on POSIX,
    else one of the SHELL_* names."""
    if settings.platform != "windows":
        return None
    shell = ((settings.record.agent if settings.record is not None else None) or {}).get("windowsShell")
    return shell if shell in (SHELL_GIT_BASH, SHELL_PWSH) else SHELL_POWERSHELL


def render_command(pairs, script, shell):
    """A command that sets every variable of pairs ((name, value) in order)
    and runs the Python file script, in the syntax of shell (None: POSIX).
    A value in _BARE_VALUES goes unquoted to a POSIX shell. PowerShell
    keeps a variable it sets for the rest of the session, so each command
    sets all of its own."""
    if shell in (SHELL_PWSH, SHELL_POWERSHELL):
        sets = "".join("$env:%s=%s; " % (name, powershell_quote(value)) for name, value in pairs)
        return sets + _WINDOWS_PYTHON + " " + powershell_quote(script)
    assigns = " ".join(name + "=" + (value if name in _BARE_VALUES else shell_quote(value))
                       for name, value in pairs)
    if shell == SHELL_GIT_BASH:
        return _MSYS_NO_CONVERSION + assigns + " " + _WINDOWS_PYTHON + " " + shell_quote(script)
    return assigns + " python3 " + shell_quote(script)


def working_command(env, token_file, preflight_id, settings):
    """The working command: the variables of the preflight command, the
    starter path and both digests with SKEIN_MONITOR_MODE=run, the number of
    the result and the overrides. Every SKEIN_* variable the runner reads is
    set, an override that was not given to an empty value. On Windows it is
    written for the shell of the record."""
    pairs = [("SKEIN_MONITOR_MODE", "run"), ("SKEIN_MONITOR_PREFLIGHT", "%d" % preflight_id),
             ("SKEIN_MONITOR_CHANNEL", settings.channel or ""),
             ("SKEIN_MONITOR_BLIND", BLIND_OFF if settings.blind_off else "")]
    pairs.extend((name, env.get(name, "")) for name in BOOTSTRAP_VARS)
    # The parameters of the mint, empty for its default: set either way, so
    # the environment the command runs in does not change what the reissue
    # command of an action repeats (US-12).
    pairs.extend((name, env.get(name, "")) for name in MINT_PARAM_VARS)
    shell = command_shell(settings)
    if shell is None:
        starter = os.path.join(os.path.dirname(os.path.abspath(token_file)), CLIENT_STARTER_FILE)
    else:
        # The path as the mint built it: a drive path whose separator is "\".
        starter = ntpath.join(ntpath.dirname(token_file), CLIENT_STARTER_FILE)
    return render_command(pairs, starter, shell)


def instruction(registry, record, choice):
    """The instruction of the chosen method: the tool and parameters of the
    record's method (or how a detached or journal monitor starts), the
    restart, the warnings of the record and of the chosen channel."""
    # The very method the walk took: two methods of one kind are told
    # apart by which of them was available.
    method = choice.record_method
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


def _identity_doc(identity, captures=None):
    if identity is None:
        return None
    doc = {
        "process": list(identity.process) if identity.process else None,
        "addresses": [{"channel": a.channel, "value": a.value, "qualifiers": [list(q) for q in a.qualifiers],
                       "env": a.env} for a in identity.addresses],
        "inherited": list(identity.inherited),
        "terminal": identity.terminal,
    }
    if captures:
        # The values of the addressee checks of the chosen channel and its
        # fallbacks (registry.go#AddresseeCommand.Capture).
        doc["captures"] = captures
    return doc


def _binding_doc(binding):
    """The confirmed binding of a chosen blind channel as preflight.json
    keeps it: the working runner checks the addressee against it before each
    delivery (the server and session of the multiplexer, the pane id, the PID
    and start of the agent process in the pane)."""
    if binding is None:
        return None
    return {"channel": binding.channel["id"], "multiplexer": bool(binding.multiplexer),
            "depth": binding.depth, "pane": dict(binding.pane)}


def _programs_doc(registry, channel_ids, env, agent):
    """The programs of the chosen channel and its fallbacks that run from a
    path of the agent process, as preflight.json keeps them."""
    programs = {}
    for channel_id in channel_ids:
        channel = registry.channel(channel_id) if channel_id is not None else None
        if channel is not None:
            programs.update(channel_programs(channel, env, agent))
    return programs


def system_placement(registry, record):
    try:
        chain = ancestry(SystemProcesses, os.getpid())
    except PlatformRefusal:
        chain = []
    return place(chain, registry, record)


def preflight(token_file, env, registry, settings, detector=None, prober=None, placement=None):
    """Run the preflight and return its line. Raises ConfigInvalid, or Stop
    (already_running) with a live runner of the token whose addressee did
    not change."""
    if sys.version_info < MIN_PYTHON:
        raise ConfigInvalid("python3", "Python %d.%d or newer is needed" % MIN_PYTHON)
    detector = detector or RegistryDetector()
    # The keys of the http channels are secrets: no probe program sees one.
    prober = prober or SystemProbeRunner(http_key_secrets(registry, env))
    journal, external = check_sandbox(token_file, env)
    if placement is None:
        placement = system_placement(registry, settings.record)
    ctx = Context(registry, settings, env, placement, detector, prober, external, token_file=token_file)
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
            # A live runner whose addressee changed may be taken over: its
            # next working start does it (takeover.py).
            if runner_alive(token_file, env) and not addressee_changed(token_file):
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
                # The limits of the record's method the walk took: the
                # working runner keeps to them and does not choose again.
                "limits": (choice.record_method or {}).get("limits"),
                # The first full monitor_started of this result repeats
                # them (C1, INV-17): the working runner does not choose again.
                "reason": choice.reason,
                "warnings": warnings,
                "journal": journal,
                "overrides": {"SKEIN_MONITOR_CHANNEL": settings.channel,
                              "SKEIN_MONITOR_BLIND": BLIND_OFF if settings.blind_off else None},
                "identity": _identity_doc(ctx.identity, dict(
                    (c, ctx.captures[c]) for c in [choice.channel] + list(choice.fallbacks) if c in ctx.captures)),
                "binding": binding,
                "programs": _programs_doc(registry, [choice.channel] + list(choice.fallbacks), env,
                                          placement.agent),
                "degraded": detector.degraded,
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
