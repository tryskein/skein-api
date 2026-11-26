package tools

import (
	"regexp"
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
// in: no ".." component, no control character, none of the characters
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
		if part == ".." {
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

// Windows shells a record may name (monitorregistry.Agent.WindowsShell).
const (
	windowsShellGitBash = "git-bash"
	windowsShellCmd     = "cmd"
)

// localAppDataCommand is the command that prints %LOCALAPPDATA% in a shell.
// A record without a named shell, and the general record, run Windows
// PowerShell; pwsh takes the same command.
func localAppDataCommand(shell string) string {
	switch shell {
	case windowsShellGitBash:
		return `echo "$LOCALAPPDATA"`
	case windowsShellCmd:
		return `echo %LOCALAPPDATA%`
	default:
		return `$env:LOCALAPPDATA`
	}
}

// monitorKnownClients is the sentence of the tool description that lists
// the client names the registry knows.
func monitorKnownClients(reg *monitorregistry.Registry) string {
	ids := reg.AgentIDs()
	if len(ids) == 0 {
		return "No client name has a record yet; any name gets the general record."
	}
	return "Known clients: " + strings.Join(ids, ", ") + "; any other name gets the general record."
}
