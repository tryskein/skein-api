package tools

import (
	_ "embed"
	"strings"
)

// monitorLoaderSource is the FIXED Python loader the bootstrap command runs
// via `python3 -c` (sk-579 T006: moved out of a Go string constant into an
// embedded asset so it can grow into multi-line code and be executed by the
// Python tests directly). It interpolates NO caller-supplied value: it reads
// the token from the 0600 file named by SKEIN_TOKEN_FILE, fetches the runner
// body from SKEIN_PUBLIC_URL + /agent/v1/monitor-script with the token in the
// X-Skein-Api-Key header, verifies the HTTP status, and exec()s the body at
// module level (the runner runs as __main__, exactly as before).
//
// Two guards keep an error body from ever being exec()'d: urllib.urlopen
// raises HTTPError on any non-2xx response (so a 401/5xx never reaches exec),
// and the explicit `assert resp.getcode() == 200` is a secondary check. The
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
