package tools

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"skein-api/internal/monitorassets"
)

// monitorAssets holds the parts of the served loader, their manifest and the
// prepare script of a call with `client`.
//
//go:embed assets/monitor_loader.py assets/monitor_prepare.py assets/loader_parts.txt
var monitorAssets embed.FS

// monitorStarterSource is the starter of every monitor start command (R-10):
// short ASCII Python that any Python 3 and Python 2.7 parse. In order it
// checks the Python version (3.6; 3.12.4 on Windows with `client`), downloads
// GET <SKEIN_PUBLIC_URL>/agent/v1/monitor-loader without a token, without
// following a redirect and with a timeout, and runs the body only when its
// SHA-256 equals SKEIN_LOADER_SHA256, in a fresh __main__ namespace that
// names the mode of the call (_SKEIN_LOADER_MODE = "client", or nothing for
// a call without `client`). It reads no token and touches no file; every
// refusal is one stop line {event, reason, action} of the starter refusal
// table, with no traceback and no exception text. Without `client` it travels
// as text in `bootstrap`; with `client` `prepare` writes it as starter.py.
//
//go:embed assets/monitor_starter.py
var monitorStarterSource string

// starterClientPlaceholder is replaced in the starter by 1 for a call with
// `client` and by 0 for one without: the mode travels in the starter's own
// text, never in an environment variable of the agent's machine (INV-7).
const starterClientPlaceholder = "__SKEIN_STARTER_CLIENT__"

// monitorStarterLegacy and monitorStarterClient are the starter of a call
// without `client` and with it, without the trailing newline.
var (
	monitorStarterLegacy = mustStarter("0")
	monitorStarterClient = mustStarter("1")
)

// monitorStarter returns the starter of a call with `client` (client) or
// without it.
func monitorStarter(client bool) string {
	if client {
		return monitorStarterClient
	}
	return monitorStarterLegacy
}

// mustStarter substitutes the mode into the starter. The asset is compiled
// into the binary, so a placeholder that is missing or repeated is a defect
// of the build: it panics at start-up.
func mustStarter(mode string) string {
	if strings.Count(monitorStarterSource, starterClientPlaceholder) != 1 {
		panic("monitor starter: the starter must carry the mode placeholder exactly once")
	}
	return strings.TrimRight(strings.Replace(monitorStarterSource, starterClientPlaceholder, mode, 1), "\n")
}

// servedLoaderSource is the loader GET /agent/v1/monitor-loader serves: the
// parts of the client branch (internal/monitorassets, loader_client/parts.txt:
// the runner's shared parts and the branch itself), then the parts listed in
// assets/loader_parts.txt, joined in order. It is static code of this build,
// the same for every caller; the starter runs it with its mode in the
// loader's namespace, so one body serves both the call without `client` and
// the call with it.
var servedLoaderSource = mustAssembleAssets(monitorassets.LoaderClient, monitorassets.LoaderClientDir, "parts.txt") +
	mustAssembleAssets(monitorAssets, "assets", "loader_parts.txt")

// ServedLoader returns the loader source GET /agent/v1/monitor-loader serves.
// The handler of that route digests it once; the digest is the
// SKEIN_LOADER_SHA256 of every start command.
func ServedLoader() string {
	return servedLoaderSource
}

// monitorPrepareScript is the prepare script of a call with `client` on
// POSIX, with the starter of such a call in it as a Python string literal.
var monitorPrepareScript = mustPrepareScript()

// starterPlaceholder is replaced in monitor_prepare.py by the starter of a
// call with `client` as a Python string literal.
const starterPlaceholder = "__SKEIN_STARTER__"

// mustAssembleAssets joins the files that the manifest lists (paths relative
// to dir) in order. The assets are compiled into the binary, so a missing
// part is a defect of the build: it panics at start-up.
func mustAssembleAssets(fsys fs.FS, dir, manifest string) string {
	list, err := fs.ReadFile(fsys, path.Join(dir, manifest))
	if err != nil {
		panic(fmt.Sprintf("monitor loader: reading %s: %v", manifest, err))
	}
	var b strings.Builder
	parts := 0
	sc := bufio.NewScanner(bytes.NewReader(list))
	for sc.Scan() {
		name := strings.TrimSpace(sc.Text())
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		part, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			panic(fmt.Sprintf("monitor loader: reading part %q: %v", name, err))
		}
		b.Write(part)
		if len(part) > 0 && part[len(part)-1] != '\n' {
			b.WriteByte('\n')
		}
		parts++
	}
	if err := sc.Err(); err != nil || parts == 0 {
		panic(fmt.Sprintf("monitor loader: manifest %s lists no part (%v)", manifest, err))
	}
	return b.String()
}

// mustPrepareScript substitutes the starter of a call with `client` into the
// prepare script as a JSON string, which is also a valid Python string
// literal (the encoder escapes quotes, backslashes and control characters and
// emits no "\/"). prepare writes it as starter.py byte for byte.
func mustPrepareScript() string {
	script, err := fs.ReadFile(monitorAssets, "assets/monitor_prepare.py")
	if err != nil {
		panic(fmt.Sprintf("monitor loader: reading the prepare script: %v", err))
	}
	var lit bytes.Buffer
	enc := json.NewEncoder(&lit)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(monitorStarter(true)); err != nil {
		panic(fmt.Sprintf("monitor loader: encoding the starter: %v", err))
	}
	if bytes.Count(script, []byte(starterPlaceholder)) != 1 {
		panic("monitor loader: the prepare script must carry the starter placeholder exactly once")
	}
	out := strings.Replace(stripPythonCommentLines(string(script)), starterPlaceholder, strings.TrimRight(lit.String(), "\n"), 1)
	return strings.TrimRight(out, "\n")
}

// stripPythonCommentLines drops every full-line comment (a line whose first
// non-blank character is "#") from Python source: the prepare script travels
// inside the response with `client`, and its comments would cost every such
// response bytes (SC-009). The sources keep their
// comments; only the served bytes lose them.
//
// A line starting with "#" is a comment unless it continues a string
// literal, and a string literal spans lines only in triple quotes or after a
// backslash at the end of a line. The sources that go through here use
// neither; a source that starts to is refused at start-up instead of being
// stripped wrongly. So is a CR outside CRLF: Python reads a lone CR as a line
// break, and a comment line would swallow the code after it.
func stripPythonCommentLines(src string) string {
	if strings.Contains(src, `"""`) || strings.Contains(src, "'''") {
		panic("monitor loader: a stripped source must not use triple-quoted strings")
	}
	if strings.Count(src, "\r") != strings.Count(src, "\r\n") {
		panic("monitor loader: a stripped source must not hold a CR outside CRLF (Python reads it as a line break)")
	}
	lines := strings.SplitAfter(src, "\n")
	var b strings.Builder
	for _, line := range lines {
		body := strings.TrimRight(line, "\r\n")
		if strings.HasSuffix(body, "\\") {
			panic("monitor loader: a stripped source must not continue a line with a backslash")
		}
		if strings.HasPrefix(strings.TrimLeft(body, " \t"), "#") {
			continue
		}
		b.WriteString(line)
	}
	return b.String()
}
