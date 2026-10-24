package issues

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// versionConflictOriginTotal is the sk-354 instrument.
//
// It counts the write paths of this package that classify an exhausted or
// single class-40 abort (isSerializationFailure) as &ErrVersionConflict{cv}
// through degradeSerializationAbort — the SAME translation Claim used
// pre-sk-202, when it was known to mislabel an unrelated SSI conflict as
// "already claimed". Eleven ops reach it (store.go): PatchIssue,
// UpdateStatus, Close, AddLabel, RemoveLabel, Accept, DismissDelegated,
// ReplaceComponents, AttachCommits, DetachCommits, ResumeDelegated.
//
// sk-303.17 left the first ten single-attempt, without a
// withSerializableRetry wrapper (no production evidence of contention, and
// "widen a reliability fix on suspicion" is the failure mode that task's own
// card warns against); DetachCommits joined later with the same shape.
// sk-622.21 (R17, K2) changed the denominator: PatchIssue, UpdateStatus,
// Close, AddLabel, RemoveLabel, Accept and DismissDelegated now replay
// through withSerializableRetry with their own budgets (store.go,
// *MaxAttempts), so for those seven ops an increment means the retry BUDGET
// was exhausted, not that a single 40001 happened. ReplaceComponents,
// AttachCommits, DetachCommits and ResumeDelegated still run one attempt and
// count a lone abort.
//
// That translation has been silently ambiguous since it was written: a
// class-40 abort means Postgres detected SOME read/write dependency cycle
// across the WHOLE transaction, not necessarily that the row's version
// column moved. A caller told "version_conflict, currentVersion=cv" who
// re-reads and finds cv equal to the version they already sent walks
// straight back into the same write having learned nothing from the round
// trip — the write would have succeeded on a bare retry. This counter is
// the only way to see, from outside the process, how often that happens
// versus how often the conflict is real.
//
// @metric description Every time one of eleven SERIALIZABLE write paths of the issues store ends in a class-40 abort that degradeSerializationAbort turns into ErrVersionConflict (surfaced to callers as the `version_conflict` MCP/REST error code), classified by whether the freshly re-read live version actually disagrees with what the caller declared. For the seven paths that replay through withSerializableRetry (sk-622.21, R17) one increment is one exhausted retry budget; for the four single-attempt paths it is one abort. Answers the sk-354 question directly: query origin="serialization_derived" over origin="genuine" for the fraction of version_conflict responses where a bare retry (no re-read needed) would have succeeded. A sustained non-trivial serialization_derived share on a single-attempt op is the signal to route it through withSerializableRetry, per-op; on a replaying op it is the signal to look at its budget — origin="no_expected_version" op values are excluded from that decision by construction (see the label doc).
// @metric calculation Incremented exactly once per operation in degradeSerializationAbort, when the operation ends in a class-40 abort (isSerializationFailure). patchIssue, updateStatus, close, addLabel, removeLabel, accept and dismissDelegated reach it only after their withSerializableRetry budget is exhausted (the count is of exhausted budgets, not of single aborts); replaceComponents, attachCommits, detachCommits and resumeDelegated run one attempt and reach it on a single abort. A replayed abort that a later attempt absorbs is not counted. origin is computed by comparing the live version read for the ErrVersionConflict payload (Store.currentVersion) against the caller-declared expected version, when that op's input carries one.
// @label op Which write path ended in the abort. Values: "patchIssue", "updateStatus", "close", "addLabel", "removeLabel", "accept", "dismissDelegated" (replay, count = exhausted budgets); "replaceComponents", "attachCommits", "detachCommits", "resumeDelegated" (single attempt, count = aborts). Fixed set of eleven. Unclaim also replays but degrades its exhausted budget to a different sentinel, ErrNotLockHolder, not ErrVersionConflict — out of this metric's scope.
// @label origin "genuine" — the live version differs from what the caller declared: a real concurrent write landed, and re-reading before retrying is the correct client behaviour. "serialization_derived" — the live version EQUALS what the caller declared: the abort came from a read/write dependency elsewhere in the transaction (a touched-but-not-version-bumping row, a different table's SSI conflict), not from a competing write to this row's version; re-reading teaches the caller nothing they didn't already know, and a bare retry would very likely have succeeded. "no_expected_version" — the operation's input has no caller-declared expected version to compare against at all (ReplaceComponents, AddLabel, RemoveLabel, AttachCommits, DetachCommits, DismissDelegated, ResumeDelegated take no version argument; Accept's is optional and this bucket also covers a caller who passed none) — for these the genuine/serialization_derived distinction does not apply by construction, and "version_conflict" is arguably the wrong name for the event regardless of origin; that is itself part of sk-354's finding, not a defect in this metric.
var versionConflictOriginTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "skein_issues_version_conflict_origin_total",
		Help: "ErrVersionConflict from eleven SERIALIZABLE write paths of the issues store (an exhausted retry budget on seven, a single abort on four), by op and by whether the live version actually disagreed with the caller's",
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
