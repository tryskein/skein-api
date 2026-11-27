# --- blind channels (step 7): the binding to a pane and the delivery ---------
# A blind channel types the text into the terminal pane of the agent: no
# receipt of the turn, the text can mix with what a person types there
# (US-7). Two mechanisms of their own, both driven by the channel's record.
#
# The binding (condition (3) of US-7, "Addressee and check" items 1 and 3).
# The runner is bound to a pane of the channel when the variables of the
# channel's detection hold, the command of the record's panePid gives the
# PID of the pane's process, and that process is an ancestor of the runner.
# The binding is confirmed when, beyond that, the agent process of the
# preflight stands between the runner and the pane's process with no other
# agent between it and the pane (an outer agent in the pane would take the
# text), the pane's id is known (the variable of the channel's address; its
# qualifiers name the server and session of a multiplexer) and so is the
# start of the agent process. A channel whose pane PID cannot be had binds
# nothing and is never enabled; a multiplexer's own process (its server)
# among the runner's ancestors still places the runner in its pane. The
# depth of a binding is the distance of the pane's process (or of that
# multiplexer process) from the runner: of two multiplexers the nearest
# decides, and only a confirmed one is used (detect.py). A channel record
# marks a multiplexer (tmux, Zellij) with "multiplexer".
#
# Before every transfer the working runner checks that the agent process is
# alive with the start of the preflight and is the foreground process of its
# pane (deliver.py#DeliverySink.check_addressee, by the tpgid of its tty,
# P-8): an agent that ended is another addressee, one that is suspended or
# has another program in the foreground is a refusal before send.
#
# The delivery. A blind record names up to four calls, every one an argv
# executor of the channel's program:
#   stage   - optional: hands the text to the channel without writing it
#             into the pane (tmux load-buffer into a buffer of its own); it
#             failing means nothing reached the pane: a refusal before send;
#   executor - the insert, the call that may write the text into the pane,
#             classified by the channel's outcome table;
#   unstage - optional: removes what the stage left when the insert did not
#             take it (tmux delete-buffer); its answer changes no outcome;
#   submit  - optional: the final step (Enter) with its own outcome table
#             and its own literal input (text), run only after the insert
#             wrote the text and the addressee and the access boundary were
#             checked again; a refused check withholds it, and the outcome is
#             "unknown" (TwoStepDriver, OST-2-4).
# A record without a submit delivers in one call (text and Enter at once),
# checked before that call. The element "{delivery}" of an argument list is
# a name unique to one transfer, the same in all calls of it (tmux: a buffer
# of each delivery of its own, so two monitors never paste each other's
# text).

import binascii

# The argument-list element replaced by a name unique to one transfer.
DELIVERY_PLACEHOLDER = "{delivery}"
# The prefix of that name.
DELIVERY_NAME_PREFIX = "skein-"


def delivery_name():
    """A name unique to one transfer: the prefix and 16 random hex digits."""
    return DELIVERY_NAME_PREFIX + binascii.hexlify(os.urandom(8)).decode("ascii")


def with_delivery_name(spec, name):
    """A copy of an executor spec whose {delivery} elements are name."""
    if not spec:
        return spec
    out = dict(spec)
    out["argv"] = [name if a == DELIVERY_PLACEHOLDER else a for a in spec.get("argv") or []]
    return out


def pane_pid(channel, env, secrets=(), drop=(), agent=None):
    """The PID of the process of the channel's pane by its panePid command
    (a bare PID on stdout, or the number at jsonField of its JSON answer);
    ProbeError when the command cannot run, fails, or gives no PID."""
    spec = channel.get("panePid") or {}
    clean = child_env(env, secrets, drop)
    try:
        argv = expand_argv(spec.get("argv") or [], clean)
    except ExecutorRefusal as exc:
        raise ProbeError(exc.reason)
    argv = _probe_program(channel, argv, env, agent)
    timeout = min((channel.get("executor") or {}).get("timeoutSec") or PROBE_TIMEOUT_SEC, PROBE_TIMEOUT_SEC)
    try:
        result = run_program(argv, clean, None, timeout, secrets)
    except ExecutorRefusal as exc:
        raise ProbeError(exc.reason)
    if result.timed_out:
        raise ProbeError("the pane PID command timed out after %d s" % timeout)
    if result.exit_code != 0:
        raise ProbeError("the pane PID command exited with %s" % result.exit_code)
    field = spec.get("jsonField")
    if field:
        # The answer may be a list (kitty's ls): any JSON document.
        try:
            doc = None if result.truncated else json.loads(result.stdout.decode("utf-8"))
        except (UnicodeDecodeError, ValueError):
            doc = None
        value = json_path_value(doc, field) if isinstance(doc, (dict, list)) else None
    else:
        try:
            value = result.stdout.decode("utf-8").strip()
        except UnicodeDecodeError:
            value = None
    if value is None or not _ASCII_INDEX.match(value) or int(value) <= 1:
        raise ProbeError("the pane PID command gave no PID")
    return int(value)


class PanePIDBinder(PaneBinder):
    """Binds the runner to a pane of a blind channel by the PID its record's
    panePid command gives (pane_pid by default)."""

    def __init__(self, pid_of=None):
        self.pid_of = pid_of or pane_pid

    def bind(self, ctx, channel):
        conds = channel.get("detect") or {}
        if not conds.get("env") or not all(env_holds(c, ctx.env) for c in conds["env"]):
            return None
        if not records_hold(conds, ctx.settings.record if ctx.settings is not None else None):
            return None
        multiplexer = bool(channel.get("multiplexer"))
        chain = ctx.placement.chain if ctx.placement is not None else []
        if os.name == "nt":
            # Windows gives a process no foreground group of its terminal:
            # the check before each transfer could never hold.
            return self._by_process(channel, multiplexer, chain, "the foreground of its pane cannot be checked here")
        if not channel.get("panePid"):
            return self._by_process(channel, multiplexer, chain, "its record gives no source of the pane PID")
        try:
            pid = self.pid_of(channel, ctx.env, getattr(ctx.prober, "secrets", ()), getattr(ctx.prober, "drop", ()),
                              ctx.placement.agent if ctx.placement is not None else None)
        except ProbeError as exc:
            return self._by_process(channel, multiplexer, chain, "no pane PID: %s" % exc)
        depth = None
        for i, proc in enumerate(chain):
            if i and proc.pid == pid:
                depth = i
                break
        if depth is None:
            # The variables are set, but the runner is not in that pane (an
            # inherited TMUX of another terminal).
            return self._unbound(channel, multiplexer, "the runner is not in its pane")
        agent_i = ctx.placement.agent_index
        if agent_i is None or agent_i >= depth:
            return self._unbound(channel, multiplexer, "no agent process in its pane", depth)
        outer = ctx.placement.outer
        if outer is not None and any(p is outer for p in chain[agent_i + 1:depth]):
            return self._unbound(channel, multiplexer, "another agent stands between the agent and its pane", depth)
        address = (channel.get("address") or {}).get("env")
        pane_id = ctx.env.get(address, "") if address else ""
        if not pane_id:
            return self._unbound(channel, multiplexer, "the id of its pane is not known", depth)
        agent = chain[agent_i]
        if not agent.start:
            return self._unbound(channel, multiplexer, "the start of the agent process is not known", depth)
        return Binding(channel, True, multiplexer=multiplexer, depth=depth,
                       pane={"id": pane_id, "pid": pid, "agent": [agent.pid, agent.start]})

    @classmethod
    def _by_process(cls, channel, multiplexer, chain, cause):
        """A binding that cannot be confirmed. A multiplexer's own process
        (one of its programs, its server) among the runner's ancestors still
        places the runner in its pane, which skips step 6 and the other
        blind channels and keeps an outer multiplexer from being used."""
        depth = None
        if multiplexer:
            names = set()
            for b in (channel.get("binaries") or {}).get(_platform_key()) or []:
                if not b.startswith("{env:"):
                    names.add(_program_name(b.replace("\\", "/").rsplit("/", 1)[-1]))
            for i, proc in enumerate(chain):
                if i and set(_program_name(n) for n in proc.names()) & names:
                    depth = i
                    break
        return cls._unbound(channel, multiplexer, cause, depth)

    @staticmethod
    def _unbound(channel, multiplexer, cause, depth=None):
        # In the pane (its process is an ancestor) though not confirmed: it
        # still decides the skip of step 6 and of the other blind channels.
        return Binding(channel, False, multiplexer=multiplexer, depth=depth, descendant=depth is not None,
                       cause=cause)


def channel_driver(channel, executor):
    """The driver of a channel's delivery: a blind record's calls (its
    {delivery} named in each), one call of the executor otherwise."""
    if channel.get("blind"):
        commit = channel.get("submit")
        return TwoStepDriver(channel, executor, {"executor": channel.get("executor") or {},
                                                 "outcomes": channel.get("outcomes")},
                             commit, stage=channel.get("stage"), unstage=channel.get("unstage"))
    return ArgvDriver(channel, executor)
