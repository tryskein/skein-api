package instructions

import (
	"strings"
	"testing"
)

// TestFullGuide_HasMonitorSection asserts the agent operational guide
// (returned by skein_get_instructions) documents the task-monitor /
// inter-agent pairing feature (sk-142.11). This is the only surface that
// teaches a headless agent how to subscribe to a task and stream new
// comments into its context — if the section is dropped, the feature is
// effectively undiscoverable. This test guards the section heading.
func TestFullGuide_HasMonitorSection(t *testing.T) {
	if !strings.Contains(FullGuide, "## Monitoring a task / pairing with another agent") {
		t.Fatal("full.md is missing the \"## Monitoring a task / pairing with another agent\" section")
	}
}

// TestFullGuide_MonitorOperationalFacts asserts the guide carries the
// load-bearing facts an agent needs to actually run a monitor correctly:
// the setup tool, the 0600 token-file hygiene, running under the Monitor
// tool, the stop protocol (sk-579), and the related subscribe/filter tools. Each string is a
// contiguous substring of the section — an edit that silently drops one of
// these breaks the workflow without failing any other test.
func TestFullGuide_MonitorOperationalFacts(t *testing.T) {
	wants := []string{
		"skein_get_monitor_script", // the setup MCP tool
		"tokenFilePath",            // write the token to the 0600 file
		"Claude Code `Monitor`",    // run the bootstrap under Claude Code's Monitor
		"one JSON object",          // the stdout line protocol
		// Subscribe without the runner. sk-255.3 folded skein_watch into
		// skein_watchers(action="watch") and renamed its id to issueId;
		// the pin moves with the tool because full.md is what an agent
		// actually executes, not documentation about it.
		"skein_watchers(action=\"watch\", issueId, subtree=true)",
		"skein_search(commentAuthorKinds=[…])", // filter comments by author kind
		// credential lifetime + security posture (must not be silently dropped)
		"read-only",        // token capability
		"renewal window",   // sliding window (sk-579: configurable, default 6h)
		"12-hour hard cap", // absolute cap
		// runner behavior facts
		"live-start",       // no historical backlog
		"same `bootstrap`", // sk-579: a paused monitor restarts on the same bootstrap
		"token_expired",    // the stop reason of a dead credential
		// sk-157: completion-by-default; sk-579: the runner ends itself on root close
		"issue.closed", // closure is in the default event set
		"root_closed",  // the runner's own stop on the root closing
		"ROOT task",    // ... only when the ROOT id closes
		"monitor_stop", // the agent's explicit stop by monitorId
	}
	for _, w := range wants {
		if !strings.Contains(FullGuide, w) {
			t.Errorf("full.md monitor section is missing operational fact %q", w)
		}
	}
}

// TestFullGuide_MonitorEchoAndVisibilityContract asserts the sk-164 FR-3
// agent-facing contract the guide MUST publish, or the fix provokes a
// self-loop after a session rotation. Two load-bearing rules replace the
// pre-fix "reply on every line" guidance:
//
//	(1) reader-side self-skip: ignore a notification line whose actorSessionId
//	    equals the agent's CURRENT MCP session — compared only when present /
//	    non-null; a session-less (absent) actor is never the agent's echo;
//	    lifecycle lines carry no actorSessionId and are exempt;
//	(2) the stop-line 403 semantics: visibility_lost (status 403) is loss of
//	    project visibility, NOT token death (token_expired, 401) — do not
//	    re-mint in a loop.
//
// Each string is a contiguous substring of the section; dropping one silently
// re-opens the self-loop / conflates 403 with 401 without failing any other
// test.
func TestFullGuide_MonitorEchoAndVisibilityContract(t *testing.T) {
	wants := []string{
		// (1) reader-side self-skip by actorSessionId.
		"actorSessionId",              // the per-line echo marker
		"notification.actorSessionId", // the exact nested field the reader reads
		"current",                     // compare against the CURRENT session
		"present and non-null",        // compare only when present
		"never your echo",             // absent / session-less ⇒ act on it
		// lifecycle lines are exempt from self-skip.
		"Lifecycle lines",
		// (2) stop-line status: 403 = visibility loss ≠ 401.
		"status: 401",     // credential dead → re-mint
		"status: 403",     // visibility lost → do NOT re-mint in a loop
		"visibility_lost", // the wire error for 403
	}
	for _, w := range wants {
		if !strings.Contains(FullGuide, w) {
			t.Errorf("full.md monitor section is missing sk-164 echo/visibility contract fact %q", w)
		}
	}
}
