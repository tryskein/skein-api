package tools

import (
	"path"
	"regexp"
	"slices"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	skmcp "skein-api/internal/mcp"
	"skein-api/internal/monitorregistry"
)

// Platforms of the `platform` parameter of skein_get_monitor_script.
const (
	monitorPlatformPOSIX   = "posix"
	monitorPlatformWindows = "windows"
)

// monitorGeneralClient is the `client` field of a response whose client name
// has no record of its own: the general record was used.
const monitorGeneralClient = "general"

// Codes of the validation refusals of client, platform and tokenDir. They
// ride in the `code` field next to error=validation_error, so a caller (and
// a test) tells the refusals apart without reading the message.
const (
	monitorCodeClientInvalid         = "client_invalid"
	monitorCodePlatformInvalid       = "platform_invalid"
	monitorCodePlatformWithoutClient = "platform_without_client"
	monitorCodeTokenDirWithoutClient = "token_dir_without_client"
	monitorCodeTokenDirNotWindows    = "token_dir_without_windows"
	monitorCodeTokenDirRequired      = "token_dir_required"
	monitorCodeTokenDirInvalid       = "token_dir_invalid"
)

// monitorClientPattern is the form of a client name: the form of a registry
// record id.
var monitorClientPattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// monitorDrivePathPattern is an absolute Windows path with a drive letter.
var monitorDrivePathPattern = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// monitorClientArgs are the validated client parameters of one call. A zero
// value (Client == "") is a call without `client`: the response, prepare and
// bootstrap stay as they were before these parameters existed.
type monitorClientArgs struct {
	// Client is the name the caller passed.
	Client string
	// Platform is monitorPlatformPOSIX or monitorPlatformWindows.
	Platform string
	// TokenDir is the private directory of the caller for the token
	// directory on Windows; empty on POSIX.
	TokenDir string
	// Resolution is the registry record the name resolves to.
	Resolution monitorregistry.Resolution
}

// parseMonitorClientArgs validates `client`, `platform` and `tokenDir` from
// the raw arguments. It runs before the watch and the mint, so a refusal
// leaves no trace. A refusal is a validation_error with a code from the
// monitorCode* set and the name of the offending field.
func parseMonitorClientArgs(args map[string]any, reg *monitorregistry.Registry) (monitorClientArgs, *mcpgo.CallToolResult) {
	rawClient, hasClient := args["client"]
	rawPlatform, hasPlatform := args["platform"]
	rawTokenDir, hasTokenDir := args["tokenDir"]

	if !hasClient {
		switch {
		case hasPlatform:
			return monitorClientArgs{}, monitorRefusal(monitorCodePlatformWithoutClient, "platform",
				"platform needs client: pass the name of your agent in client")
		case hasTokenDir:
			return monitorClientArgs{}, monitorRefusal(monitorCodeTokenDirWithoutClient, "tokenDir",
				"tokenDir needs client and platform=\"windows\"")
		}
		return monitorClientArgs{}, nil
	}

	client, ok := rawClient.(string)
	if !ok || !monitorClientPattern.MatchString(client) {
		return monitorClientArgs{}, monitorRefusal(monitorCodeClientInvalid, "client",
			"client must be a name of 1-64 characters a-z, 0-9 and \"-\"")
	}
	out := monitorClientArgs{Client: client, Platform: monitorPlatformPOSIX, Resolution: reg.Resolve(client)}

	if hasPlatform {
		platform, ok := rawPlatform.(string)
		if !ok || (platform != monitorPlatformPOSIX && platform != monitorPlatformWindows) {
			return monitorClientArgs{}, monitorRefusal(monitorCodePlatformInvalid, "platform",
				"platform must be \"posix\" or \"windows\"")
		}
		out.Platform = platform
	}

	switch {
	case hasTokenDir && out.Platform != monitorPlatformWindows:
		return monitorClientArgs{}, monitorRefusal(monitorCodeTokenDirNotWindows, "tokenDir",
			"tokenDir is taken only with platform=\"windows\"")
	case !hasTokenDir && out.Platform == monitorPlatformWindows:
		return monitorClientArgs{}, monitorRefusal(monitorCodeTokenDirRequired, "tokenDir",
			"platform=\"windows\" needs tokenDir, an absolute private directory of yours; "+
				"get %LOCALAPPDATA% with: "+localAppDataCommand(out.Resolution.WindowsShell()))
	case hasTokenDir:
		dir, ok := rawTokenDir.(string)
		if !ok || !validWindowsTokenDir(dir) {
			return monitorClientArgs{}, monitorRefusal(monitorCodeTokenDirInvalid, "tokenDir",
				"tokenDir must be an absolute path with a drive letter (e.g. C:\\Users\\me\\AppData\\Local), "+
					"without \"..\", control characters or <>\"|?*")
		}
		out.TokenDir = dir
	}
	return out, nil
}

func monitorRefusal(code, field, message string) *mcpgo.CallToolResult {
	return skmcp.ToolError(ErrCodeValidationError, message, map[string]any{"code": code, "field": field})
}

// validWindowsTokenDir reports whether dir is an absolute Windows path with a
// drive letter that names a directory the server may build the token path
// in: no component ending in a dot or a space (Windows strips those, so
// ".. " and "..." resolve to the parent, and "." and ".." are such
// components too), no control character, none of the characters
// Windows forbids in a path (<>"|?* and a colon past the drive). A UNC path
// and a relative path have no drive letter. Spaces, Unicode and apostrophes
// are allowed: prepare and bootstrap quote them for the record's shell.
func validWindowsTokenDir(dir string) bool {
	if !monitorDrivePathPattern.MatchString(dir) {
		return false
	}
	for _, r := range dir {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return false
		}
	}
	if strings.ContainsAny(dir[2:], `<>"|?*:`) {
		return false
	}
	for _, part := range strings.FieldsFunc(dir[3:], func(r rune) bool { return r == '\\' || r == '/' }) {
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return false
		}
	}
	return true
}

// windowsTokenFilePath is the token-file path on Windows: a directory of its
// own with the mint discriminator inside the caller's tokenDir.
func windowsTokenFilePath(tokenDir, issueID, discriminator string) string {
	return strings.TrimRight(tokenDir, `\/`) + `\skein-mon-` + issueID + "-" + discriminator + `\token`
}

// localAppDataCommand is the command that prints %LOCALAPPDATA% in the
// record's Windows shell (monitorregistry.WindowsShell*, a closed set). A
// record without a named shell and the general record run Windows
// PowerShell; pwsh takes the same command.
func localAppDataCommand(shell string) string {
	switch shell {
	case monitorregistry.WindowsShellGitBash:
		return `echo "$LOCALAPPDATA"`
	case monitorregistry.WindowsShellCmd:
		return `echo %LOCALAPPDATA%`
	default:
		return `$env:LOCALAPPDATA`
	}
}

// monitorMainClients are the ids of the main agents the tool description
// names (SC-012, INV-17): not the full list, which only the warning of the
// general record carries (monitorUnknownClientWarning).
const monitorMainClients = "claude, codex, grok, kimi, gemini, opencode, qwen-code, cursor, copilot, hermes"

// monitorUnknownClientWarning is the warning of a response whose client name
// has no record: the general record's warnings and the full list of the ids
// of the registry.
func monitorUnknownClientWarning(r monitorregistry.Resolution, reg *monitorregistry.Registry) string {
	ids := strings.Join(reg.AgentIDs(), ", ")
	if ids == "" {
		ids = "none yet"
	}
	return strings.Join(r.Warnings(), " ") + " Client ids with a record: " + ids + "."
}

// clientCommandInput is what the commands of a call with `client` carry.
type clientCommandInput struct {
	publicURL, issueID, excludeSession, tokenFilePath string
	runnerSHA256, loaderSHA256                        string
	client, platform                                  string
	// subtree and eventTypes are the mint's parameters; the preflight
	// command carries them only when they are not the mint's default
	// (SKEIN_SUBTREE, SKEIN_EVENT_TYPES), so a default call does not grow
	// (INV-17) and the runner's reissue commands keep them (US-12).
	subtree    bool
	eventTypes []string
}

// clientCommands are the commands of a call with `client` on POSIX.
type clientCommands struct {
	prepare, preflight string
}

// buildClientCommands builds prepare and preflight of a call with `client`
// on POSIX. prepare checks the Python version, creates the private token
// directory and writes the starter into it as starter.py
// (monitor_prepare.py); preflight, the one start command of the response,
// runs that file with SKEIN_MONITOR_MODE=preflight, the variables of the
// monitor and both digests. The working command is printed by the preflight
// line (`command`), not built here. Every interpolated value is
// shell-single-quoted, so a hostile issue id or path cannot leave its quotes;
// the token itself is in none of them.
func buildClientCommands(in clientCommandInput) clientCommands {
	dir := path.Dir(in.tokenFilePath)
	return clientCommands{
		prepare: "SKEIN_TOKEN_DIR=" + shellSingleQuote(dir) + " python3 -c " + shellSingleQuote(monitorPrepareScript),
		preflight: "SKEIN_MONITOR_MODE=preflight" +
			" SKEIN_PUBLIC_URL=" + shellSingleQuote(in.publicURL) +
			" SKEIN_ISSUE=" + shellSingleQuote(in.issueID) +
			" SKEIN_TOKEN_FILE=" + shellSingleQuote(in.tokenFilePath) +
			" SKEIN_EXCLUDE_SESSION=" + shellSingleQuote(in.excludeSession) +
			" SKEIN_RUNNER_SHA256=" + shellSingleQuote(in.runnerSHA256) +
			" SKEIN_LOADER_SHA256=" + shellSingleQuote(in.loaderSHA256) +
			" SKEIN_CLIENT=" + shellSingleQuote(in.client) +
			" SKEIN_PLATFORM=" + shellSingleQuote(in.platform) +
			mintParamVars(in.subtree, in.eventTypes) +
			" python3 " + shellSingleQuote(path.Join(dir, clientStarterFile)),
	}
}

// mintParamVars are the start variables of the mint's parameters that are
// not its default, each with a leading space: SKEIN_SUBTREE=1 for
// subtree=true, SKEIN_EVENT_TYPES with the comma-separated event types when
// they are not defaultMonitorEventTypes. Empty for a default call.
func mintParamVars(subtree bool, eventTypes []string) string {
	var b strings.Builder
	if subtree {
		b.WriteString(" SKEIN_SUBTREE=" + shellSingleQuote("1"))
	}
	if len(eventTypes) > 0 && !slices.Equal(eventTypes, defaultMonitorEventTypes) {
		b.WriteString(" SKEIN_EVENT_TYPES=" + shellSingleQuote(strings.Join(eventTypes, ",")))
	}
	return b.String()
}

// clientStarterFile is the name of the starter file prepare writes into the
// token directory.
const clientStarterFile = "starter.py"

// clientExcludeSessionFile is the name of the echo exclusion file next to
// the token (US-13).
const clientExcludeSessionFile = "exclude_session"

// clientExcludeSessionPath is the literal path of the echo exclusion file.
func clientExcludeSessionPath(tokenFilePath string) string {
	if strings.Contains(tokenFilePath, `\`) {
		return tokenFilePath[:strings.LastIndex(tokenFilePath, `\`)+1] + clientExcludeSessionFile
	}
	return path.Join(path.Dir(tokenFilePath), clientExcludeSessionFile)
}

// clientTokenInstruction is the tokenInstruction of a call with `client`:
// the Write of the token comes only after the ready line of prepare, and a
// failed prepare has no Write step.
const clientTokenInstruction = "Run `prepare`. Only after it prints " +
	`{"event":"prepare_ready"}` + " write `token` to `tokenFilePath` with the Write tool " +
	"(never in a shell command), then follow `usage`. If `prepare` prints " +
	`{"event":"prepare_failed",...}` + " or exits non-zero, stop and follow the `action` of that line."

// monitorClientUsage is the `usage` of a call with `client`: only the steps
// common to every method (INV-17) — run preflight, run its `command` as its
// `instruction` says, the echo exclusion file, the stop lines. How each
// method runs is the `instruction` of the preflight line, not repeated here.
// An unknown client name opens it with the warning that carries the full
// list of ids.
func monitorClientUsage(r monitorregistry.Resolution, reg *monitorregistry.Registry, excludePath string) string {
	var b strings.Builder
	if !r.Known() {
		b.WriteString(monitorUnknownClientWarning(r, reg))
		b.WriteString("\n")
	}
	b.WriteString("1. Run `preflight`. It prints one JSON line {\"event\":\"preflight\",...}.\n")
	b.WriteString("2. Run its `command` (the working command) as its `instruction` says.\n")
	b.WriteString("3. If a notification's actorSessionId is your current MCP session, write that " +
		"session id to " + excludePath + " and do not reply to it; after an MCP reconnect write " +
		"the new one.\n")
	b.WriteString("Every stop prints one final JSON line {event, reason, action}: follow its `action`. " +
		"If python3 cannot open starter.py, mint a new monitor with the same parameters. " +
		"Reply to comments with skein_comment as usual.")
	return b.String()
}
