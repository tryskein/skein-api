# --- detection of hosts (steps 5-6) and blind channels (step 7) --------------
# The Detector of the preflight built on the registry.
#
# A host or terminal environment of steps 5-6 is detected only when its
# variables are set (the env conditions of its `detect`), the agent process
# is determined and not nested, its address is not an outer agent's
# (identity), and, for Zavod, the engine matches the client by the engine map
# of the registry. Detected is not available: whether its channel can be used
# is the preflight's probe; a detected host whose channel is unavailable is
# skipped with its cause and still counts as detected.
#
# Step 6 is skipped when the runner is a descendant of a tmux or Zellij pane.
# Two terminal environments at once are ambiguous: neither is used, no blind
# channel either, and the preflight warns ambiguous_terminal with their
# variables.
#
# A blind channel is enabled automatically only under the three conditions
# of US-7: (1) no own method of the record is available - the preflight only
# reaches step 7 when none was; (2) no host of steps 5-6 is detected; (3) the
# binding to the pane is confirmed. Multiplexers come first, the nearest to
# the runner first; the other blind channels are skipped when the runner is
# in a multiplexer pane.
#
# The binding to a pane is a boundary object (PaneBinder): bind() answers for
# one blind channel whether the runner is a descendant of the agent process
# in a pane of that channel. The one here confirms nothing.

import re

# The Zavod host: its channel id, the variable of its engine, and the two
# variables of the agent's own address (US-5: ZAVOD_ME is a UUID equal to
# ZAVOD_AGENT_ID).
ZAVOD_CHANNEL = "zavod"
ZAVOD_ENGINE_VAR = "ZAVOD_AGENT_ENGINE_ID"
ZAVOD_ME_VAR = "ZAVOD_ME"
ZAVOD_AGENT_ID_VAR = "ZAVOD_AGENT_ID"
ZAVOD_UUID = re.compile(r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}")
# The cause of the warning about two terminal environments.
AMBIGUOUS_TERMINAL = "ambiguous_terminal"
# The warning of a chosen blind channel.
BLIND_WARNING = "Blind delivery: the text is typed into the agent's pane and can mix with what a person types there."


class Binding(object):
    """The binding of the runner to a pane of a blind channel. Two facts, two
    flags: descendant when the runner is a descendant of a pane of that
    channel (it decides the skip of step 6 and of the other blind channels
    inside a multiplexer pane); confirmed when, beyond that, the runner is a
    descendant of the agent process in the pane and the pane's id, PID and
    the agent process's start are known (it decides whether the channel may
    be used). A confirmed binding is a descendant one. multiplexer for a
    tmux/Zellij pane; depth the distance of the pane's process from the
    runner in the chain (nearest first); pane what the delivery needs."""

    def __init__(self, channel, confirmed, multiplexer=False, depth=None, pane=None, descendant=None):
        self.channel = channel
        self.confirmed = confirmed
        self.descendant = bool(confirmed) if descendant is None else (descendant or bool(confirmed))
        self.multiplexer = multiplexer
        self.depth = depth
        self.pane = pane or {}


class PaneBinder(object):
    """Binds the runner to a pane of a blind channel. This one confirms
    nothing."""

    def bind(self, ctx, channel):
        return None


def env_names(conds):
    return [c.get("name") for c in (conds or {}).get("env") or []]


class RegistryDetector(Detector):
    """The Detector of the registry's channels."""

    def __init__(self, binder=None):
        Detector.__init__(self)
        self.binder = binder or PaneBinder()
        self._bindings = None

    def _channels(self, ctx, step):
        chans = [c for c in ctx.registry.channels.values() if c.get("step") == step]
        return sorted(chans, key=lambda c: c.get("order", 0))

    def bindings(self, ctx):
        """The bindings of the blind channels, computed once."""
        if self._bindings is None:
            self._bindings = []
            for channel in self._channels(ctx, 7):
                binding = self.binder.bind(ctx, channel)
                if binding is not None:
                    self._bindings.append(binding)
        return self._bindings

    def binding(self, ctx, channel_id):
        """The confirmed binding to a pane of the blind channel, or None."""
        for b in self.bindings(ctx):
            if b.channel["id"] == channel_id and b.confirmed:
                return b
        return None

    def in_multiplexer(self, ctx):
        return any(b.descendant and b.multiplexer for b in self.bindings(ctx))

    def zavod_holds(self, ctx, channel):
        """The conditions US-5 puts on detecting Zavod: ZAVOD_ME is a UUID
        equal to ZAVOD_AGENT_ID, the CLI of this platform is found, and the
        engine matches the client by the engine map."""
        me = ctx.env.get(ZAVOD_ME_VAR, "")
        if not ZAVOD_UUID.fullmatch(me) or me != ctx.env.get(ZAVOD_AGENT_ID_VAR, ""):
            return False
        binaries = (channel.get("binaries") or {}).get(_platform_key()) or []
        if not any(program_found(b, ctx.env) for b in binaries):
            return False
        return self.engine_matches(ctx)

    def engine_matches(self, ctx):
        engines = ctx.registry.doc.get("zavodEngines") or {}
        record = engines.get(ctx.env.get(ZAVOD_ENGINE_VAR, ""))
        if record is None:
            return False
        settings_record = ctx.settings.record
        return settings_record.agent is None or record == settings_record.id

    def detected(self, ctx, channel):
        """Whether a host of steps 5-6 is detected (not whether its channel
        is available)."""
        conds = channel.get("detect") or {}
        if not conds.get("env") or not all(env_holds(c, ctx.env) for c in conds["env"]):
            return False
        identity = ctx.identity
        if identity is not None and identity.terminal is not None:
            return channel["id"] == identity.terminal
        if ctx.placement.agent is None or ctx.placement.nested:
            return False
        if identity is not None and channel["id"] in identity.inherited:
            return False
        if channel["id"] == ZAVOD_CHANNEL and not self.zavod_holds(ctx, channel):
            return False
        return True

    def address_specs(self, ctx):
        """The AddressSpecs of the channels whose record names an address,
        in the order of the method (step, then order): the identity of the
        addressee holds every address the agent process owns. A host or
        terminal environment of steps 5-6 is never a nested agent's."""
        specs = []
        for step in (3, 5, 6, 7):
            for c in self._channels(ctx, step):
                a = c.get("address")
                if not a:
                    continue
                specs.append(AddressSpec(c["id"], a["env"], qualifiers=a.get("qualifiers") or (),
                                         per_process=bool(a.get("perProcess")), host=step in (5, 6),
                                         root_pid_env=a.get("rootPidEnv") or None))
        return specs

    def addressee_holds(self, ctx, channel):
        """A forced host passes the same checks as a detected one."""
        return self.detected(ctx, channel)

    def _detected_hosts(self, ctx):
        step5 = [c for c in self._channels(ctx, 5) if self.detected(ctx, c)]
        step6 = []
        if not self.in_multiplexer(ctx):
            step6 = [c for c in self._channels(ctx, 6) if self.detected(ctx, c)]
        return step5, step6

    def hosts(self, ctx):
        step5, step6 = self._detected_hosts(ctx)
        if len(step6) > 1:
            names = []
            for c in step6:
                names.extend(env_names(c.get("detect")))
            self.degraded = {"cause": AMBIGUOUS_TERMINAL, "variables": names}
            self.warnings.append("%s: %s are all set; no terminal environment and no blind channel "
                                 "is used until SKEIN_MONITOR_CHANNEL names one." % (AMBIGUOUS_TERMINAL, ", ".join(names)))
            step6 = []
        return [c["id"] for c in step5 + step6]

    def blind(self, ctx):
        step5, step6 = self._detected_hosts(ctx)
        # Condition (2): a detected host, available or not, keeps blind off;
        # so do two terminal environments.
        if step5 or step6:
            return []
        confirmed = [b for b in self.bindings(ctx) if b.confirmed]
        muxes = sorted((b for b in confirmed if b.multiplexer),
                       key=lambda b: b.depth if b.depth is not None else 1 << 30)
        if muxes:
            # Only the nearest: typing into an outer pane reaches the inner
            # multiplexer's client, not the agent.
            return [muxes[0].channel["id"]]
        # Inside a multiplexer pane the other blind channels are skipped,
        # even when the binding to that pane is not confirmed.
        if self.in_multiplexer(ctx):
            return []
        # Condition (3): only channels whose binding is confirmed.
        return [b.channel["id"] for b in confirmed]
