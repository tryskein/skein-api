package tools

import "time"

// Wire contract of the stalled-monitor hint (sk-579, US-5). Any MCP tool
// response may carry it; the layer that attaches it (T015) and the selection
// that fills it (T014) build on these shapes and names.
//
// Placement on the wire:
//   - a response whose text is a JSON object gets MonitorStalledKey (and,
//     when the list was cut, MonitorStalledOmittedKey) as top-level keys;
//   - a text response that is not a JSON object gets an extra content block
//     carrying a MonitorStalledHint object;
//   - a response with no body gets no hint.
//
// An empty list means no hint at all: neither key is present.
const (
	// MonitorStalledKey names the list of stalled-monitor items.
	MonitorStalledKey = "monitorStalled"
	// MonitorStalledOmittedKey names the number of stalled monitors that
	// qualified but were not shown because the list is capped (at most
	// MonitorStalledMaxItems items per response). Present only when > 0.
	MonitorStalledOmittedKey = "monitorStalledOmitted"
)

// MonitorStalledMaxItems caps the items in one response (US-5): mine=true
// first, then the longest-silent first; the rest are counted in
// MonitorStalledOmittedKey.
const MonitorStalledMaxItems = 10

// MonitorStalledItem is one stalled monitor, as the agent sees it (US-5).
type MonitorStalledItem struct {
	// MonitorID is the monitor's id — the same monitorId skein_get_monitor_script
	// returned and skein_watchers(action="monitor_stop") takes.
	MonitorID string `json:"monitorId"`
	// IssueID is the monitor's root issue.
	IssueID string `json:"issueId"`
	// LastPollSucceededAt is the last successful poll (RFC3339, UTC); null
	// when the monitor has never polled successfully.
	LastPollSucceededAt *time.Time `json:"lastPollSucceededAt"`
	// Mine is true when the calling session is the one that minted the
	// monitor (two empty sessions — clients without Mcp-Session-Id — count
	// as equal).
	Mine bool `json:"mine"`
	// Bootstrap is the command that restarts the runner on the existing
	// token file, rebuilt from the literals stored at mint.
	Bootstrap string `json:"bootstrap"`
	// Action is what the agent should do, taken verbatim from the US-5
	// action table for this value of Mine (INV-14).
	Action string `json:"action"`
}

// MonitorStalledHint is the JSON carried by the separate content block when
// the tool's own text is not a JSON object. Its keys are exactly
// MonitorStalledKey and MonitorStalledOmittedKey.
type MonitorStalledHint struct {
	// MonitorStalled is the list of items; never empty when the hint exists.
	MonitorStalled []MonitorStalledItem `json:"monitorStalled"`
	// MonitorStalledOmitted is how many qualifying monitors were not shown;
	// omitted when zero.
	MonitorStalledOmitted int `json:"monitorStalledOmitted,omitempty"`
}
