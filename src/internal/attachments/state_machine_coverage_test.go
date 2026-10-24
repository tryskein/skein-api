package attachments

import "testing"

// sk-105.5 (S4) — attachments state-machine coverage map + real-S3 caveat.
//
// The attachment lifecycle (pending → ready confirm, max-size reject,
// executable-extension reject, hard-delete) is ALREADY fully covered by
// the existing stub-driven tests in this repo — this slice deliberately
// does NOT duplicate them. The authoritative coverage lives in:
//
//   internal/attachments/store_test.go
//     - TestStore_CreatePending_HappyPath         pending insert
//     - TestStore_Confirm_FlipsStatusAndIsIdempotent
//                                                 pending→ready + idempotent
//                                                 + exactly-one attachment.added
//     - TestStore_Delete_HardDeletesRow           hard-delete + audit event
//     - TestStore_ListByIssue_OnlyReady           pending hidden from wire
//
//   internal/attachments/validate_test.go
//     - TestValidateSize                          max-size reject (file_too_large)
//     - TestValidateFilename                      executable-extension reject (mime_blocked)
//     - TestValidateContentType                   MIME shape
//
//   internal/mcp/tools/attachment_test.go (drives the MCP handler over the
//   fakeS3 stub — confirms the harness needs NO real S3):
//     - TestAttachmentUploadURL_HappyPath / _TooLarge / _BlockedExt / _DisabledNoS3
//     - TestAttachmentConfirm_FailsWithoutObject  StatObject probe
//     - TestAttachmentList_OnlyReady
//     - TestAttachmentDelete_HardDeletesRowAndRemovesObject
//
// OUT OF SCOPE for this (and the whole Go integration) harness (spec §5
// M9): the following paths exercise REAL object storage and are NOT
// integration-tested here because the harness stands up Postgres only and
// drives S3 through the `fakeS3` stub (S3Access interface in
// internal/attachments/client.go):
//
//   - presigned POST policy SIGNATURE correctness (the actual AWS SigV4
//     bytes S3 would accept) — the stub returns a fixed fake policy;
//   - the browser multipart-POST upload BINDING (size/key/policy fields
//     enforced by S3 at upload time) — never executed in tests;
//   - RemoveObject ORPHAN cleanup against live storage and the
//     stale-pending reconciler's real S3 reachability — the stub records
//     the call but no object exists.
//
// These require a real S3 container (a separate e2e tier), not this
// Postgres-only integration harness. This file is the documented anchor
// for that boundary so a future reader does not mistake the stub coverage
// for real-S3 coverage.

func TestAttachmentStateMachine_CoverageMapAnchor(t *testing.T) {
	// Intentionally empty: this file is a coverage-map + caveat anchor, not
	// a behavioral test. The real assertions live in the files enumerated
	// in the package comment above. Keeping a named test here makes the
	// caveat discoverable via `go test -run` and grep.
	t.Skip("coverage-map anchor only — see store_test.go / validate_test.go / mcp/tools/attachment_test.go")
}
