package dashboard

// source_api_test.go — test matrix row 18 (INV-4, "Data: the people group"
// on the dashboard). Reference — the spec, written before the code: a record
// written with an API key (source api) counts on the agent_directed axis and
// in the "people" (human-driven) condition, so it neither disappears from
// the axes nor lands on the autonomous one.

import (
	"testing"
	"time"
)

func TestScoring_SourceAPICountsAsAgentDirected(t *testing.T) {
	env := newOutcomeEnv(t)
	at := time.Now().UTC().Add(-1 * time.Hour)
	pre := uniqueIDPrefix(t, "sapi")

	// human 1, agent_directed 1, api 2, agent_autonomous 1 — distinct
	// numbers, so a misplaced api shows on the wrong axis.
	seedClosedIssueAndEvent(t, env, pre+"-h1", "human", "done", at)
	seedClosedIssueAndEvent(t, env, pre+"-d1", "agent_directed", "done", at)
	seedClosedIssueAndEvent(t, env, pre+"-p1", "api", "done", at)
	seedClosedIssueAndEvent(t, env, pre+"-p2", "api", "wont_do", at)
	seedClosedIssueAndEvent(t, env, pre+"-a1", "agent_autonomous", "done", at)

	res := env.scoped(t)
	assertAxes(t, "issues_closed", findMetric(t, res, "issues_closed"), 1, 3, 1)
	assertAxes(t, "issues_closed_done", findMetric(t, res, "issues_closed_done"), 1, 2, 1)

	// closed_human_driven — the people condition: human, agent_directed and
	// api closes, not the autonomous one.
	var count int64 = -1
	for _, e := range res.Leaderboards["closed_human_driven"] {
		if e.Username == env.username {
			count = e.Count
		}
	}
	if count != 4 {
		t.Errorf("closed_human_driven of %s = %d, want 4 (human 1 + directed 1 + api 2)", env.username, count)
	}
}
