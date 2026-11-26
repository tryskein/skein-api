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

// monitorAssets holds the parts of the client-mode loader, their manifest
// and the prepare script of a call with `client`.
//
//go:embed assets/monitor_loader.py assets/monitor_prepare.py assets/loader_client/parts.txt assets/loader_client/*.py
var monitorAssets embed.FS

// clientLoaderSource is the client-mode loader that `prepare` writes next to
// the token: the parts listed in assets/loader_client/parts.txt, joined in
// order. Its first part sets _SKEIN_LOADER_MODE = "client" before the body is
// exec()'d, so the mode follows the loader the agent runs, never an
// environment variable.
var clientLoaderSource = mustAssembleAssets(monitorAssets, "assets", "loader_client/parts.txt")

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
	out := strings.Replace(string(script), clientLoaderPlaceholder, strings.TrimRight(lit.String(), "\n"), 1)
	return strings.TrimRight(out, "\n")
}
