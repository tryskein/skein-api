// Package instructions embeds the static text Skein returns at the MCP
// handshake (initialize) and via the skein_get_instructions tool.
//
// Both files are bundled into the binary at compile time so the running
// service has zero filesystem dependency on /app/migrations or
// /app/instructions — particularly important when the binary lives in a
// scratch/distroless image without the source tree.
package instructions

import (
	_ "embed"
	"fmt"
)

// MCP spec 2025-11-25 caps server `instructions` at ~2 KB (Claude Code
// 2.1.85+ silently truncates beyond that). We assert the budget at init
// time so a future edit to initialize.txt that pushes past the limit
// crashes the binary on boot rather than silently degrading the agent
// onboarding signal.
const maxInitializeBytes = 2048

//go:embed initialize.txt
var Initialize string

//go:embed full.md
var FullGuide string

func init() {
	if len(Initialize) > maxInitializeBytes {
		panic(fmt.Sprintf(
			"mcp/instructions: initialize.txt is %d bytes, MCP spec caps server "+
				"instructions at %d bytes — trim before booting",
			len(Initialize), maxInitializeBytes,
		))
	}
}
