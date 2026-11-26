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
)

// monitorLoaderSource is the FIXED Python loader the bootstrap command runs
// via `python3 -c` (sk-579 T006: moved out of a Go string constant into an
// embedded asset so it can grow into multi-line code and be executed by the
// Python tests directly). It interpolates NO caller-supplied value: it reads
// the token from the private file named by SKEIN_TOKEN_FILE, fetches the runner
// body from SKEIN_PUBLIC_URL + /agent/v1/monitor-script with the token in the
// X-Skein-Api-Key header, verifies the HTTP status, and exec()s the body at
// module level (the runner runs as __main__, exactly as before).
//
// Two guards keep an error body from ever being exec()'d: the urllib opener
// raises HTTPError on any non-2xx response (so a 401/5xx never reaches exec),
// and the explicit `assert resp.getcode() == 200` is a secondary check. The
// opener follows no redirect, so a 3xx can neither carry the token to another
// host nor have its code run. The
// fetch carries an explicit 30s timeout so a hung connection cannot block the
// bootstrap forever before the runner starts. The asset uses only double
// quotes, so the shell-quoted bootstrap stays readable (shellSingleQuote
// would escape single quotes correctly anyway).
//
//go:embed assets/monitor_loader.py
var monitorLoaderSource string

// monitorLoader returns the loader code embedded in every bootstrap: the
// asset without its trailing newline.
func monitorLoader() string {
	return strings.TrimRight(monitorLoaderSource, "\n")
}

// monitorAssets holds the parts of the served loader and of the client-mode
// loader, their manifests and the prepare script of a call with `client`.
//
//go:embed assets/monitor_loader.py assets/monitor_prepare.py assets/loader_parts.txt assets/loader_client/parts.txt assets/loader_client/*.py
var monitorAssets embed.FS

// servedLoaderSource is the loader GET /agent/v1/monitor-loader serves: the
// parts listed in assets/loader_parts.txt, joined in order. It is static code
// of this build, the same for every caller; the starter runs it with its mode
// in the loader's namespace, so one body serves both the call without
// `client` and the call with it.
var servedLoaderSource = mustAssembleAssets(monitorAssets, "assets", "loader_parts.txt")

// ServedLoader returns the loader source GET /agent/v1/monitor-loader serves.
// The handler of that route digests it once; the digest is the
// SKEIN_LOADER_SHA256 of every start command.
func ServedLoader() string {
	return servedLoaderSource
}

// clientLoaderSource is the client-mode loader that `prepare` writes next to
// the token: the parts listed in assets/loader_client/parts.txt, joined in
// order. Its first part sets _SKEIN_LOADER_MODE = "client" before the body is
// exec()'d, so the mode follows the loader the agent runs, never an
// environment variable.
var clientLoaderSource = stripPythonCommentLines(mustAssembleAssets(monitorAssets, "assets", "loader_client/parts.txt"))

// monitorPrepareScript is the prepare script of a call with `client` on
// POSIX, with the client loader in it as a Python string literal.
var monitorPrepareScript = mustPrepareScript()

// clientLoaderPlaceholder is replaced in monitor_prepare.py by the client
// loader as a Python string literal.
const clientLoaderPlaceholder = "__SKEIN_CLIENT_LOADER__"

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

// mustPrepareScript substitutes the client loader into the prepare script
// as a JSON string, which is also a valid Python string literal (the encoder
// escapes quotes, backslashes and control characters and emits no "\/").
func mustPrepareScript() string {
	script, err := fs.ReadFile(monitorAssets, "assets/monitor_prepare.py")
	if err != nil {
		panic(fmt.Sprintf("monitor loader: reading the prepare script: %v", err))
	}
	var lit bytes.Buffer
	enc := json.NewEncoder(&lit)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(clientLoaderSource); err != nil {
		panic(fmt.Sprintf("monitor loader: encoding the client loader: %v", err))
	}
	if bytes.Count(script, []byte(clientLoaderPlaceholder)) != 1 {
		panic("monitor loader: the prepare script must carry the loader placeholder exactly once")
	}
	out := strings.Replace(stripPythonCommentLines(string(script)), clientLoaderPlaceholder, strings.TrimRight(lit.String(), "\n"), 1)
	return strings.TrimRight(out, "\n")
}

// stripPythonCommentLines drops every full-line comment (a line whose first
// non-blank character is "#") from Python source: the client loader and the
// prepare script travel inside the response with `client`, and their comments
// would cost every such response bytes (SC-009). The sources keep their
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
