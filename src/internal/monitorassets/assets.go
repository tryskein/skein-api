// Package monitorassets holds the Python sources of the agent task monitor
// that more than one package serves: the parts of the poll runner, served by
// GET /agent/v1/monitor-script (internal/handler), and the parts of the
// client branch of the loader, served by GET /agent/v1/monitor-loader
// (internal/mcp/tools), which share the runner's platform layer, privacy
// checks and end sequence. It imports nothing of the product, so both may
// import it.
package monitorassets

import "embed"

// Runner holds the parts of the poll runner and the manifest that orders
// them (RunnerDir/parts.txt). They are embedded at compile time so the
// running binary has no filesystem dependency on the source tree.
//
//go:embed runner/parts.txt runner/*.py
var Runner embed.FS

// LoaderClient holds the parts of the client branch of the loader and their
// manifest (LoaderClientDir/parts.txt); the manifest names runner parts too,
// so the runner parts are in this FS as well.
//
//go:embed loader_client/parts.txt loader_client/*.py runner/parts.txt runner/*.py
var LoaderClient embed.FS

// LoaderClientDir is the directory of the client branch and its manifest in
// LoaderClient.
const LoaderClientDir = "loader_client"

// RunnerDir is the directory of the runner parts and of their manifest in
// Runner.
const RunnerDir = "runner"
