package issues

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// versionConflictOriginTotal classifies terminal serialization failures by
// the relation between the observed and caller-declared issue versions.
// The metric name is retained for dashboard compatibility. Since sk-648,
// serialization_derived no longer means a version_conflict response: an
// unchanged version produces serialization_retry instead.
//
// @metric description Terminal SQLSTATE 40001 aborts of twelve issue-write paths, classified by observed version. One count per exhausted retry budget for retrying paths or per abort for single-attempt paths; absorbed retries are not counted. serialization_derived now produces serialization_retry rather than version_conflict.
// @metric calculation Incremented once in degradeSerializationAbort after reading the live issue version. patchIssue, updateStatus, close, updateIssue, addLabel, removeLabel, accept and dismissDelegated have exhausted their retry budget (updateIssue: 10 attempts — a change of more than one step, or of components and labels only, through Store.UpdateIssue; a single patch, status or close step keeps its own name); replaceComponents, attachCommits, detachCommits and resumeDelegated have one attempt. A failed live-version read on a version-aware path is not classified or counted.
// @label op Issue write path. Values: "patchIssue", "updateStatus", "close", "updateIssue", "addLabel", "removeLabel", "accept", "dismissDelegated", "replaceComponents", "attachCommits", "detachCommits", "resumeDelegated".
// @label origin Values: "genuine" — live version differs from the declared version, response version_conflict; "serialization_derived" — versions match, response serialization_retry; "no_expected_version" — input has no declared version, the existing version_conflict contract is preserved.
var versionConflictOriginTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "skein_issues_version_conflict_origin_total",
		Help: "Terminal serialization aborts from issue-write paths, by operation and observed version relation",
	},
	[]string{"op", "origin"},
)

// Origin label values for versionConflictOriginTotal — see its doc comment.
const (
	originGenuine              = "genuine"
	originSerializationDerived = "serialization_derived"
	originNoExpectedVersion    = "no_expected_version"
)

// classifyVersionConflictOrigin is the pure decision versionConflictOriginTotal
// counts (split out from degradeSerializationAbort so it is unit-testable
// without a database). expectedVersion nil means the operation never took a
// caller-declared version at all.
func classifyVersionConflictOrigin(liveVersion int, expectedVersion *int) string {
	if expectedVersion == nil {
		return originNoExpectedVersion
	}
	if liveVersion != *expectedVersion {
		return originGenuine
	}
	return originSerializationDerived
}
