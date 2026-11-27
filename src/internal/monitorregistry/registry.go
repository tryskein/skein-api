// Package monitorregistry holds the one declarative registry of the Skein
// monitor: the agents (how each one runs the monitor runner and what it may
// be told), the hosts and channels that can deliver an event to an agent
// (how to recognise them, which program to call, how to classify its answer)
// and the general record used for a client name the registry does not know.
//
// The registry is a single JSON document, registry.json, embedded into the
// binary. The server loads and validates it at startup (Load) and refuses to
// start on any violation; the same bytes travel inside the served runner
// body, so the server and the runner never read two different copies.
//
// Every record names its source (a file and symbol with a commit or version,
// or the documentation of a closed product): the values of a record are
// taken from that source, and the test of a record takes its expected values
// from the same source.
package monitorregistry

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
)

//go:embed registry.json
var embedded []byte

// Embedded returns a copy of the registry document compiled into the binary.
// The startup path of the server passes it to Load.
func Embedded() []byte {
	return bytes.Clone(embedded)
}

// MustEmbedded loads the embedded registry. The document is compiled into
// the binary and the startup path refuses to serve on an invalid one, so a
// failure here is a defect of the build: it panics.
func MustEmbedded() *Registry {
	reg, err := Load(embedded)
	if err != nil {
		panic(fmt.Sprintf("monitor registry: the embedded registry.json is invalid: %v", err))
	}
	return reg
}

// Registry is the parsed and validated registry document.
type Registry struct {
	// SchemaVersion is the version of the document format. Only
	// SchemaVersion 1 exists.
	SchemaVersion int `json:"schemaVersion"`
	// General is the record used for a client name that no agent record
	// carries. It has no id and no process sign: its agent process is the
	// nearest ancestor of the runner that is neither the loader, the runner
	// nor a shell from Shells.
	General General `json:"general"`
	// Agents are the named agent records, one per harness that runs an
	// agent (not per model).
	Agents []Agent `json:"agents"`
	// Channels are the delivery channels: own delivery commands of agents
	// (step 3 of the method order), hosts (step 5), terminal environments
	// (step 6) and blind channels (step 7).
	Channels []Channel `json:"channels"`
	// Shells is the list of shell executables that are never taken for the
	// agent process of the general record.
	Shells Shells `json:"shells"`
	// ZavodEngines maps a Zavod engine id to the id of the agent record it
	// runs. A Zavod host is detected only when its engine maps to the
	// client's record (any mapped engine for the general record).
	ZavodEngines map[string]string `json:"zavodEngines"`
}

// General is the record of an unknown client name.
type General struct {
	// About says in one sentence what the record covers.
	About string `json:"about"`
	// Source names where the behaviour of the record comes from.
	Source string `json:"source"`
	// Warnings are shown to the agent with the instructions of the record.
	Warnings []string `json:"warnings,omitempty"`
}

// Agent is the record of one agent harness.
type Agent struct {
	// ID is the client name an agent passes to skein_get_monitor_script.
	ID string `json:"id"`
	// About says in one sentence what the record covers.
	About string `json:"about"`
	// Source names the files, symbols and versions the record is taken from.
	Source string `json:"source"`
	// Process is the sign of the agent process among the ancestors of the
	// runner.
	Process ProcessSign `json:"process"`
	// Methods are the agent's own ways to receive an event, in the order the
	// preflight tries them. An agent with no method of its own is woken only
	// by a host or a blind channel, or gets the journal.
	Methods []Method `json:"methods"`
	// WindowsShell is the shell the agent runs commands with on Windows: one
	// of the WindowsShell* names; empty means none is named, and the agent is
	// given Windows PowerShell.
	WindowsShell string `json:"windowsShell,omitempty"`
	// Warnings are shown to the agent with the instructions of the record.
	Warnings []string `json:"warnings,omitempty"`
}

// Windows shells an agent record may name.
const (
	WindowsShellPowerShell = "powershell"
	WindowsShellPwsh       = "pwsh"
	WindowsShellGitBash    = "git-bash"
)

// ProcessSign recognises the agent process by its executable name or by an
// argument of its command line. At least one of the lists is non-empty.
type ProcessSign struct {
	// Executables are base names of the agent executable.
	Executables []string `json:"executables,omitempty"`
	// Argv are arguments any one of which marks the agent process (for an
	// agent run by an interpreter, e.g. a script path).
	Argv []string `json:"argv,omitempty"`
}

// Method kinds. They are the steps 2-4 of the method order.
const (
	// MethodStream: the runner's stdout lines become turns of the agent
	// through the agent's own streaming tool.
	MethodStream = "stream"
	// MethodDelivery: the runner detaches and calls the agent's own
	// delivery command, a channel record at step 3.
	MethodDelivery = "delivery"
	// MethodExitOnEvent: the runner exits after a batch, and the end of the
	// background command wakes the agent.
	MethodExitOnEvent = "exit-on-event"
)

// Method is one way an agent receives an event.
type Method struct {
	// Kind is one of MethodStream, MethodDelivery, MethodExitOnEvent.
	Kind string `json:"kind"`
	// Channel is the id of the step-3 channel of a MethodDelivery method;
	// empty for the other kinds.
	Channel string `json:"channel,omitempty"`
	// Conditions must all hold for the method to be available.
	Conditions Conditions `json:"conditions"`
	// Instruction tells the agent which tool runs the monitor in this
	// method and with which parameters.
	Instruction string `json:"instruction"`
	// Limits are the limits of the agent's tool in this method.
	Limits *Limits `json:"limits,omitempty"`
}

// Conditions of a method or of a channel detection. Empty lists hold.
type Conditions struct {
	// Env are environment variables of the agent process.
	Env []EnvCondition `json:"env,omitempty"`
	// Settings are values read from configuration files of the agent. A
	// setting that cannot be read does not hold.
	Settings []SettingCondition `json:"settings,omitempty"`
	// Programs are executables that must be found.
	Programs []string `json:"programs,omitempty"`
	// AgentOptionsAbsent are long options the argument list of the agent
	// process must not carry, neither as an element of its own ("--remote")
	// nor with a value ("--remote=URL"). An argument list that cannot be read
	// does not hold. Only a method of an agent record states it: a channel's
	// detection does not read the agent process.
	AgentOptionsAbsent []string `json:"agentOptionsAbsent,omitempty"`
	// Records are the ids of the agent records the channel serves: the
	// record the monitor runs by must be one of them. Only a channel's
	// detection states it, never together with RecordsAbsent.
	Records []string `json:"records,omitempty"`
	// RecordsAbsent are the ids of the agent records the channel does not
	// serve: the record the monitor runs by (the general record included)
	// must be none of them.
	RecordsAbsent []string `json:"recordsAbsent,omitempty"`
}

// EnvCondition holds when the variable is set and non-empty and, when
// Pattern is given, the WHOLE value matches it (Go: the pattern compiled as
// ^(?:Pattern)$; Python: re.fullmatch). The server validates the pattern
// with Go RE2 and the runner applies it with Python re, so Pattern is limited
// to a subset both read the same way (envPatternSubset in validate.go): no
// anchors, no escape classes like \d or \w, no flags, no POSIX classes.
type EnvCondition struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern,omitempty"`
	// Equals, when set, must equal the variable's value exactly.
	Equals *string `json:"equals,omitempty"`
}

// Setting file formats.
const (
	SettingJSON = "json"
	SettingTOML = "toml"
	SettingYAML = "yaml"
)

// SettingCondition holds when the value at Key of the file at Path equals
// Equals. Path may start with "~/" or "{env:NAME}/".
type SettingCondition struct {
	Path   string          `json:"path"`
	Format string          `json:"format"`
	Key    string          `json:"key"`
	Equals json.RawMessage `json:"equals"`
}

// Limit units, as the source of a record states them.
const (
	UnitChars  = "chars"
	UnitUTF16  = "utf16"
	UnitBytes  = "bytes"
	UnitLines  = "lines"
	UnitEvents = "events"
)

// Limits are the delivery limits of a tool or a channel.
type Limits struct {
	// Line limits one serialized line.
	Line *Limit `json:"line,omitempty"`
	// Batch limits one batch of lines.
	Batch *Limit `json:"batch,omitempty"`
	// Rate limits how often batches may be emitted.
	Rate *Rate `json:"rate,omitempty"`
	// Window is the output window of the agent's tool, when it reads the
	// runner's stdout in windows (a notifier that matches lines per time
	// window); absent when the tool has none.
	Window *Window `json:"window,omitempty"`
}

// Limit is an upper bound in a unit.
type Limit struct {
	Max  int    `json:"max"`
	Unit string `json:"unit"`
}

// Rate is a burst followed by at most one batch per interval.
type Rate struct {
	Burst       int `json:"burst"`
	IntervalSec int `json:"intervalSec"`
}

// Window is the window model of a tool that reads stdout in time windows:
// lines that arrive within one window of LengthMs are matched together, at
// most Lines of them (the rest are dropped by the tool), and one background
// task of the tool has a budget of Budget windows that grows by one every
// RefillSec seconds. The runner puts a whole batch, its service lines
// included, into one window and starts the next batch only after that
// window has closed. Every field is positive; a window is given whole or
// not at all.
type Window struct {
	LengthMs  int `json:"lengthMs"`
	Lines     int `json:"lines"`
	Budget    int `json:"budget"`
	RefillSec int `json:"refillSec"`
}

// Steps of the method order a channel belongs to.
const (
	StepOwnDelivery = 3
	StepHost        = 5
	StepTerminal    = 6
	StepBlind       = 7
)

// Channel kinds.
const (
	// KindTerminal channels write text into the terminal input of the agent.
	KindTerminal = "terminal"
	// KindAddressed channels hand text to a queue or the API of a session
	// by its id.
	KindAddressed = "addressed"
)

// Channel is the record of a host or a delivery channel.
type Channel struct {
	ID     string `json:"id"`
	About  string `json:"about"`
	Source string `json:"source"`
	// Step is the step of the method order: StepOwnDelivery, StepHost,
	// StepTerminal or StepBlind.
	Step int `json:"step"`
	// Order is the position of the channel within its step; unique per step.
	Order int `json:"order"`
	// Kind is KindTerminal or KindAddressed.
	Kind string `json:"kind"`
	// Blind marks a channel that types text into a terminal pane without a
	// receipt of the turn. A blind channel is always at StepBlind and a
	// channel at StepBlind is always blind.
	Blind bool `json:"blind"`
	// Detect recognises the channel in the environment of the agent process.
	Detect Conditions `json:"detect"`
	// Executor delivers the text of an event.
	Executor Executor `json:"executor"`
	// Idle says how the delivery waits for the agent to be idle.
	Idle Idle `json:"idle"`
	// Outcomes is the closed table of the channel's answers. An answer that
	// matches no row is "unknown".
	Outcomes []OutcomeRow `json:"outcomes"`
	// Probes are non-delivering checks that the boundary of the channel is
	// reachable from the runner. Exactly one of Probes (non-empty) and
	// ProbeUndefined (true) is set.
	Probes []Probe `json:"probes,omitempty"`
	// ProbeUndefined states that the source of the channel gives no
	// non-delivering probe.
	ProbeUndefined bool `json:"probeUndefined,omitempty"`
	// Addressee says how the channel checks that the addressee is still the
	// one the preflight saw.
	Addressee AddresseeCheck `json:"addressee"`
	// PanePID tells a blind channel how to get the PID of the pane's
	// process. Required for a blind channel, absent otherwise.
	PanePID *PanePID `json:"panePid,omitempty"`
	// Multiplexer marks a blind channel of a terminal multiplexer (tmux,
	// Zellij): a runner in its pane skips step 6 and the other blind
	// channels, and of two multiplexers the one nearest the runner is used.
	Multiplexer bool `json:"multiplexer,omitempty"`
	// Stage hands the text to a blind channel without writing it into the
	// pane (a buffer of the multiplexer), before Executor, the insert,
	// writes it there; a stage that fails is a refusal before send. With a
	// stage the insert takes no text.
	Stage *Executor `json:"stage,omitempty"`
	// Unstage removes what Stage left when the insert did not take the
	// text; its answer changes no outcome. Only with a stage.
	Unstage *Executor `json:"unstage,omitempty"`
	// Submit is the final step of a two-step blind channel (Enter): run
	// only after the insert wrote the text and the addressee and the access
	// boundary were checked again; a failed check withholds it and the
	// outcome is "unknown".
	Submit *Submit `json:"submit,omitempty"`
	// Binaries are the programs of the channel per platform, in the order
	// they are tried: a bare name (the executable of the agent process of
	// that name, else PATH), a "~/" path, or "{env:NAME}" (the program the
	// variable names, a name or a path, when it is set).
	Binaries map[string][]string `json:"binaries"`
	// Limits are the limits of the channel.
	Limits *Limits `json:"limits,omitempty"`
	// Warnings are shown with the instructions when the channel is chosen.
	Warnings []string `json:"warnings,omitempty"`
	// TextNotes are sentences every text this channel delivers carries
	// right after the label, within the channel's limit (what its source
	// asks the addressee to know of each delivery).
	TextNotes []string `json:"textNotes,omitempty"`
	// Launcher is set for a host that launches agent processes itself: an
	// agent process it launched skips its own methods except those Keeps
	// names, and the host goes first at step 5 (the method order, "agent
	// under Paseo"); the runner tells such a launch from a terminal of the
	// host and from a nested agent by the host's process ("Addressee and
	// check", the ACP terminal). Only a host of step 5 with an address.
	Launcher *Launcher `json:"launcher,omitempty"`
	// Address is the address of the addressee in this channel, as the
	// preflight reads it from the environment of the agent process; it joins
	// the identity of the addressee (the ledger key and the check before a
	// transfer). Absent when the channel has no address of its own.
	Address *AddressSpec `json:"address,omitempty"`
}

// AddressSpec says where the address of the addressee in a channel comes
// from ("Addressee and check", items 2 and 5).
type AddressSpec struct {
	// Env is the variable that holds the address.
	Env string `json:"env"`
	// Qualifiers are variables whose values qualify the address: one value
	// under two qualifiers is two addressees.
	Qualifiers []string `json:"qualifiers,omitempty"`
	// PerProcess states that the harness sets Env on every agent process
	// itself, so a nested agent still owns it.
	PerProcess bool `json:"perProcess,omitempty"`
	// RootPIDEnv names the variable with the root process of the host's
	// agent: the address is the agent's only when its agent process is that
	// root or the first agent process under it.
	RootPIDEnv string `json:"rootPidEnv,omitempty"`
	// QualifierPaths names the qualifiers whose value is a directory path,
	// each with the path the harness uses when the variable is unset: the
	// runner compares such a qualifier as that path made absolute and
	// normal ("~/" from HOME), so an explicit default and an unset variable
	// are one addressee.
	QualifierPaths map[string]string `json:"qualifierPaths,omitempty"`
}

// Launcher is the process of a host that launches agents and the own
// methods its agents keep.
type Launcher struct {
	// Process recognises the host's process among the ancestors of the
	// runner.
	Process ProcessSign `json:"process"`
	// Keeps maps the id of an agent record to the kinds of its own methods
	// that stay in use when the host launched it; a record it does not name
	// keeps none.
	Keeps map[string][]string `json:"keeps,omitempty"`
}

// Platforms of Channel.Binaries.
const (
	PlatformLinux   = "linux"
	PlatformMacOS   = "macos"
	PlatformWindows = "windows"
)

// Executor types.
const (
	// ExecutorArgv runs a program with an argument list, without a shell.
	ExecutorArgv = "argv"
	// ExecutorHTTP sends one HTTP request without following redirects.
	ExecutorHTTP = "http"
)

// Text feeds of an argv executor.
const (
	// FeedStdin writes the text to the program's stdin.
	FeedStdin = "stdin"
	// FeedFile writes the text to a private file whose path is the
	// {textFile} element of the argument list.
	FeedFile = "file"
	// FeedArg passes the text as the {text} element of the argument list.
	FeedArg = "arg"
)

// Argument-list placeholders. An element of an argument list is either a
// literal without braces or exactly one placeholder.
const (
	PlaceholderText     = "{text}"
	PlaceholderTextFile = "{textFile}"
	// PlaceholderEnvPrefix starts "{env:NAME}", the value of variable NAME
	// of the agent process.
	PlaceholderEnvPrefix = "{env:"
	// PlaceholderDelivery is a name unique to one transfer, the same in
	// every call of it; only the calls of a blind channel name it.
	PlaceholderDelivery = "{delivery}"
)

// Executor is the transport of a channel.
type Executor struct {
	// Type is ExecutorArgv or ExecutorHTTP.
	Type string `json:"type"`
	// Argv is the argument list of an argv executor.
	Argv []string `json:"argv,omitempty"`
	// Feed is the text feed of an argv executor.
	Feed string `json:"feed,omitempty"`
	// HTTP describes the request of an http executor.
	HTTP *HTTPRequest `json:"http,omitempty"`
	// TimeoutSec bounds one call.
	TimeoutSec int `json:"timeoutSec"`
}

// HTTPRequest is the request of an http executor. The text of an event is
// the string value of BodyField in a JSON object body. An http channel runs
// no program, so it names no Binaries.
type HTTPRequest struct {
	Method string `json:"method"`
	// URL is a template of literals and "{env:NAME}" placeholders of the
	// agent process. A placeholder that starts the template is the base (a
	// scheme, a host and maybe a path, taken as is); every later one is
	// escaped as one element of the path. The scheme is https, or http for
	// localhost and 127.0.0.1 only; an unset variable leaves the channel
	// unavailable.
	URL string `json:"url"`
	// KeyEnv names, in order, the variables whose value is the key; the
	// first one set gives it, sent as "Authorization: Bearer <key>". Empty
	// when the channel takes no key; when it takes one and none is set, the
	// channel is unavailable. The key never goes to an argument list or a
	// stdin, it is redacted like the token and taken out of the environment
	// of every channel program.
	KeyEnv []string `json:"keyEnv,omitempty"`
	// BodyField is the field of the JSON body that carries the text.
	BodyField string `json:"bodyField"`
}

// Idle wait types.
const (
	// IdleNone: the channel does not wait.
	IdleNone = "none"
	// IdleBuiltin: the channel waits by itself before handing the text on.
	IdleBuiltin = "builtin"
	// IdleArgv: the runner runs Idle.Argv before the delivery; exit code 0
	// means idle.
	IdleArgv = "argv"
)

// Idle is how a delivery waits for the agent to be idle.
type Idle struct {
	Type       string   `json:"type"`
	Argv       []string `json:"argv,omitempty"`
	TimeoutSec int      `json:"timeoutSec,omitempty"`
}

// Outcomes of a delivery a channel answer can prove. "Does not fit" is not
// among them: the runner decides it before calling any channel.
const (
	OutcomeRefused          = "refused-before-send"
	OutcomeQueued           = "queued"
	OutcomeInputWritten     = "input-written"
	OutcomeInputNoReceipt   = "input-without-receipt"
	OutcomeTurnStarted      = "turn-started"
	OutcomeUnknown          = "unknown"
	OutcomeAddresseeChanged = "addressee-changed"
	OutcomeAddresseeGone    = "addressee-gone"
)

// OutcomeRow maps one answer sign to exactly one outcome.
type OutcomeRow struct {
	Match   Match  `json:"match"`
	Outcome string `json:"outcome"`
	// Shorten marks a refusal before send that the channel gave because the
	// text is too long: the next call of the same delivery carries the
	// minimal form of its events. Only a row of OutcomeRefused sets it.
	Shorten bool `json:"shorten,omitempty"`
}

// Match is an answer sign. Every given criterion must hold; at least one is
// given.
type Match struct {
	// ExitCode is the exit code of an argv program.
	ExitCode *int `json:"exitCode,omitempty"`
	// JSON maps a dotted path of the JSON document the program printed (or
	// the HTTP response body) to the string the value must equal. An
	// element of a path that is a decimal number indexes a list
	// ("stages.1": the second element).
	JSON map[string]string `json:"json,omitempty"`
	// JSONAbsent are dotted paths (as in JSON) that must be absent from the
	// document: a list shorter than the index, an object without the key.
	JSONAbsent []string `json:"jsonAbsent,omitempty"`
	// StderrJSON is JSON for the JSON document an argv program wrote to its
	// stderr (a CLI that reports its errors there).
	StderrJSON map[string]string `json:"stderrJson,omitempty"`
	// StderrContains is a literal phrase the stderr of an argv program must
	// contain (a CLI that reports its errors only as text there). A CLI
	// reports one error per call, so rows of two different phrases are
	// taken as apart; an answer that carries both matches two rows and is
	// unknown.
	StderrContains string `json:"stderrContains,omitempty"`
	// HTTPStatus is the status of an http response.
	HTTPStatus *int `json:"httpStatus,omitempty"`
	// StderrEmpty, when set, is whether the argv program wrote nothing to
	// its stderr: true holds for an empty stderr, false for any output there
	// (a warning).
	StderrEmpty *bool `json:"stderrEmpty,omitempty"`
}

// Probe types (non-delivering availability checks).
const (
	// ProbeArgv runs a command without the event text; Results classify it.
	ProbeArgv = "argv"
	// ProbeDirWritable creates and removes a probe file in Path.
	ProbeDirWritable = "dir-writable"
	// ProbeSocketConnect connects to the unix socket or named pipe at Path
	// and closes the connection without sending anything.
	ProbeSocketConnect = "socket-connect"
	// ProbeTCPReach opens a TCP connection to Address and closes it.
	ProbeTCPReach = "tcp-reach"
)

// Probe results.
const (
	ProbeAvailable   = "available"
	ProbeUnavailable = "unavailable"
)

// Probe is one non-delivering availability check of a channel. For the
// dir-writable, socket-connect and tcp-reach types success means available
// and any error unavailable; only an argv probe carries a Results table.
type Probe struct {
	Type   string `json:"type"`
	Source string `json:"source"`
	// Argv is the command of an argv probe; it never carries the text of
	// an event.
	Argv []string `json:"argv,omitempty"`
	// Path is the directory of a dir-writable probe or the socket of a
	// socket-connect probe: a literal, "~/" followed by literal path
	// elements (the home directory of the agent process), or "{env:NAME}"
	// optionally followed by "/" and literal path elements.
	Path string `json:"path,omitempty"`
	// Paths replaces Path when the source names an order of places: the
	// first path whose variable is set (a "~/" path always resolves) is
	// probed. A probe of a path type gives exactly one of Path and Paths.
	Paths []string `json:"paths,omitempty"`
	// Address is the host:port of a tcp-reach probe, a literal or a
	// "{env:NAME}" placeholder.
	Address string `json:"address,omitempty"`
	// Results is the closed table of an argv probe; an answer outside it
	// is "unavailable".
	Results []ProbeResultRow `json:"results,omitempty"`
}

// ProbeResultRow maps one answer of an argv probe to a result.
type ProbeResultRow struct {
	Match  Match  `json:"match"`
	Result string `json:"result"`
}

// Addressee process checks.
const (
	// ProcessAlive: the agent process of the preflight is alive with the
	// same PID and start time.
	ProcessAlive = "alive"
	// ProcessForeground: in addition, it is the foreground process of the
	// pane.
	ProcessForeground = "foreground"
	// ProcessNone: an addressed channel does not need the agent process.
	ProcessNone = "none"
)

// Addressee command results.
const (
	AddresseeSame    = "same"
	AddresseeChanged = "changed"
	AddresseeGone    = "gone"
)

// AddresseeCheck is how a channel checks the addressee before a delivery.
type AddresseeCheck struct {
	// Process is ProcessAlive, ProcessForeground or ProcessNone.
	Process string `json:"process"`
	// Command is the record's own check, if its source gives one.
	Command *AddresseeCommand `json:"command,omitempty"`
	// RootLife names the variables of the life of the root process of the
	// addressee as the host recorded it: before each transfer that
	// process is alive, with the same start and boot id ("Addressee and
	// check", the same life). All three are qualifiers of the address, so
	// the values the preflight saw are the ones compared.
	RootLife *RootLife `json:"rootLife,omitempty"`
	// Limitation states what the channel cannot tell apart (for example a
	// session recreated under the same id).
	Limitation string `json:"limitation,omitempty"`
}

// RootLife is the life of a root process as Linux /proc gives it: the pid,
// field 22 of /proc/<pid>/stat (the start in clock ticks since boot) and
// /proc/sys/kernel/random/boot_id.
type RootLife struct {
	PIDEnv    string `json:"pidEnv"`
	StartEnv  string `json:"startEnv"`
	BootIDEnv string `json:"bootIdEnv"`
}

// AddresseeCommand runs Argv; Results classify its answer, anything else is
// "changed" for a terminal channel and not proven for an addressed one.
type AddresseeCommand struct {
	Argv    []string             `json:"argv"`
	Results []AddresseeResultRow `json:"results"`
	// Capture are dotted paths (as in Match.JSON) of the command's JSON
	// answer whose values the preflight saves (it runs the command once; an
	// answer that is not "same" or not JSON makes the channel unavailable;
	// a path the answer does not hold is saved as absent); before each
	// transfer every path must hold the saved value again, or be absent
	// again, else the addressee changed (the process incarnation of a
	// terminal).
	Capture []string `json:"capture,omitempty"`
}

// AddresseeResultRow maps one answer of the addressee command to a result.
type AddresseeResultRow struct {
	Match  Match  `json:"match"`
	Result string `json:"result"`
}

// Submit is the final step of a two-step blind channel.
type Submit struct {
	Executor Executor `json:"executor"`
	// Text is the literal input of the step (a carriage return for a
	// channel that takes the key as text); empty for a key the program
	// names in its arguments.
	Text string `json:"text,omitempty"`
	// Outcomes is the closed table of the step's answers; the text is
	// already written, so no row is a refusal before send.
	Outcomes []OutcomeRow `json:"outcomes"`
}

// PanePID is how a blind channel gets the PID of the process of its pane.
type PanePID struct {
	// Argv is the command; an element that does not start with "{" may hold
	// braces of the program's own (the tmux format "#{pane_pid}").
	Argv []string `json:"argv"`
	// JSONField is the dotted path of the PID in the JSON the command
	// prints; empty when the command prints the bare PID.
	JSONField string `json:"jsonField,omitempty"`
}

// Shells is the list of shell executables with its source.
type Shells struct {
	Source string   `json:"source"`
	Names  []string `json:"names"`
}

// Load parses a registry document strictly (unknown fields, keys that match
// a field only up to case, repeated keys and trailing data are errors) and
// validates it. Any problem is returned as
// ValidationErrors; the registry is returned only when it is valid.
func Load(doc []byte) (*Registry, error) {
	if err := checkStrictJSON(doc, reflect.TypeOf(Registry{})); err != nil {
		return nil, ValidationErrors{{Rule: RuleMalformed, Path: "$", Detail: err.Error()}}
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	var reg Registry
	if err := dec.Decode(&reg); err != nil {
		return nil, ValidationErrors{{Rule: RuleMalformed, Path: "$", Detail: err.Error()}}
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, ValidationErrors{{Rule: RuleMalformed, Path: "$", Detail: "trailing data after the registry document"}}
	}
	if err := reg.Validate(); err != nil {
		return nil, err
	}
	return &reg, nil
}

// Resolution is the record a client name resolves to.
type Resolution struct {
	// Agent is the named record; nil when the general record applies.
	Agent *Agent
	// General is the general record of the registry.
	General *General
}

// Known reports whether the client name has its own record.
func (r Resolution) Known() bool { return r.Agent != nil }

// WindowsShell is the Windows shell of the named record; empty for the
// general record and for a record that names none.
func (r Resolution) WindowsShell() string {
	if r.Agent == nil {
		return ""
	}
	return r.Agent.WindowsShell
}

// Warnings are the warnings of the resolved record: the general record's
// (which say the name was not found) for an unknown name.
func (r Resolution) Warnings() []string {
	if r.Agent == nil {
		return r.General.Warnings
	}
	return r.Agent.Warnings
}

// Resolve returns the record of a client name: its own record when the
// registry has one, the general record otherwise.
func (r *Registry) Resolve(client string) Resolution {
	for i := range r.Agents {
		if r.Agents[i].ID == client {
			return Resolution{Agent: &r.Agents[i], General: &r.General}
		}
	}
	return Resolution{General: &r.General}
}

// AgentIDs returns the ids of the agent records in registry order.
func (r *Registry) AgentIDs() []string {
	ids := make([]string, 0, len(r.Agents))
	for _, a := range r.Agents {
		ids = append(ids, a.ID)
	}
	return ids
}

// Channel returns the channel record with the id, or nil.
func (r *Registry) Channel(id string) *Channel {
	for i := range r.Channels {
		if r.Channels[i].ID == id {
			return &r.Channels[i]
		}
	}
	return nil
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("monitor registry: %s at %s: %s", e.Rule, e.Path, e.Detail)
}
