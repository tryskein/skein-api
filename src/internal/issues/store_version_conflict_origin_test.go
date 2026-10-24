package issues

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"skein-api/internal/testsupport"
)

// sk-354 — measures, rather than assumes, whether the ErrVersionConflict a
// SERIALIZABLE write path hands out on a class-40 abort is a REAL version
// disagreement or one manufactured by an unrelated SSI conflict (see
// metrics.go's versionConflictOriginTotal doc comment).
//
// sk-622.21 (T012, decision R17, matrix row No. 22b) changed what this file
// stands for. The paths that call the fan-out — PatchIssue, UpdateStatus,
// Close, Accept, the label paths, Unclaim, DismissDelegated — now replay a
// class-40 abort through withSerializableRetry; their absorption and their
// classification on an EXHAUSTED budget are pinned per path by
// store_write_paths_serialization_test.go (No. 22a). Here:
//
//   - the old one-conflict scenarios of PatchIssue and AddLabel now pass
//     through the retry: the operation succeeds and nothing is counted;
//   - the genuine version conflict keeps its contract — the retry re-reads,
//     the version check refuses with the committed version — and no
//     serialization origin is counted;
//   - the four paths R17 does NOT move (ReplaceComponents, AttachCommits,
//     DetachCommits, ResumeDelegated) still run one SERIALIZABLE attempt
//     and degrade a single 40001 through degradeSerializationAbort to
//     ErrVersionConflict, counted as no_expected_version under their own op.
//     A success there would mean the path silently gained a retry.

// TestPatchIssue_VersionConflictOrigin_SerializationDerived — the sk-354
// headline scenario: a concurrent writer touches the SAME issue row WITHOUT
// bumping version, so PatchIssue's first attempt aborts with 40001 although
// nothing the caller cares about changed. Since R17 the retry absorbs it:
// the patch succeeds and no version-conflict origin is counted. (Exhausting
// the budget — where serialization_derived is still counted — is
// store_write_paths_serialization_test.go#TestWritePathRetry_PatchIssue_Exhausts.)
func TestPatchIssue_VersionConflictOrigin_SerializationDerived(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	appName := "sk354-patch-derived-" + uniqueTag()
	opPool := newTaggedPool(t, appName)

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	store := NewStore(pool)
	opStore := NewStore(opPool)

	issue, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{Title: "sk-354 patch derived repro", Type: "task", Priority: 2},
		creator, HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	before := testutil.ToFloat64(versionConflictOriginTotal.WithLabelValues("patchIssue", originSerializationDerived))
	beforeGenuine := testutil.ToFloat64(versionConflictOriginTotal.WithLabelValues("patchIssue", originGenuine))

	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin concurrent writer: %v", err)
	}
	defer func() { _ = other.Rollback(ctx) }()
	if _, err := other.Exec(ctx,
		`UPDATE issues SET updated_at = now() WHERE id = $1`, issue.ID); err != nil {
		t.Fatalf("concurrent writer UPDATE: %v", err)
	}

	type patchResult struct {
		dto *IssueDetail
		err error
	}
	resCh := make(chan patchResult, 1)
	done := make(chan struct{})
	opCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	newTitle := "patched title"
	go func() {
		defer close(done)
		d, err := opStore.PatchIssue(opCtx, issue.ID, PatchIssueInput{
			ExpectedVersion: issue.Version,
			Title:           &newTitle,
		}, HumanActor(creator, creatorName))
		resCh <- patchResult{d, err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("patch goroutine did not finish after cancellation")
		}
	})

	if !waitForBlocked(t, pool, appName, 10*time.Second) {
		t.Fatalf("could not orchestrate the race: PatchIssue never blocked on the concurrent writer's row lock")
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatalf("commit concurrent writer: %v", err)
	}

	var got patchResult
	select {
	case got = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("PatchIssue did not return within 30s")
	}

	if got.err != nil {
		t.Fatalf("PatchIssue error = %v, want success — one concurrent writer that did not move version "+
			"must be absorbed by the retry (R17)", got.err)
	}
	if got.dto == nil || got.dto.Title != newTitle {
		t.Fatalf("PatchIssue result = %+v, want title %q", got.dto, newTitle)
	}

	if got := testutil.ToFloat64(versionConflictOriginTotal.WithLabelValues("patchIssue", originSerializationDerived)) - before; got != 0 {
		t.Errorf("versionConflictOriginTotal{op=patchIssue,origin=serialization_derived} moved by %v, want 0 — "+
			"an absorbed conflict is not a version conflict", got)
	}
	if got := testutil.ToFloat64(versionConflictOriginTotal.WithLabelValues("patchIssue", originGenuine)) - beforeGenuine; got != 0 {
		t.Errorf("versionConflictOriginTotal{op=patchIssue,origin=genuine} moved by %v, want 0", got)
	}
}

// TestPatchIssue_VersionConflictOrigin_Genuine is the control: the
// concurrent writer bumps version FOR REAL. PatchIssue's first attempt
// aborts on the row; its retry re-reads the committed version, which now
// disagrees with what the caller declared, and the version check refuses —
// *ErrVersionConflict{CurrentVersion = declared + 1}. The refusal comes from
// the check, not from an exhausted budget, so no version-conflict origin is
// counted at all (R17, row No. 22b).
func TestPatchIssue_VersionConflictOrigin_Genuine(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	appName := "sk354-patch-genuine-" + uniqueTag()
	opPool := newTaggedPool(t, appName)

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	store := NewStore(pool)
	opStore := NewStore(opPool)

	issue, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{Title: "sk-354 patch genuine repro", Type: "task", Priority: 2},
		creator, HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	before := testutil.ToFloat64(versionConflictOriginTotal.WithLabelValues("patchIssue", originGenuine))
	beforeDerived := testutil.ToFloat64(versionConflictOriginTotal.WithLabelValues("patchIssue", originSerializationDerived))

	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin concurrent writer: %v", err)
	}
	defer func() { _ = other.Rollback(ctx) }()
	// Real version bump — a competing write this caller SHOULD hear about.
	if _, err := other.Exec(ctx,
		`UPDATE issues SET version = version + 1, updated_at = now() WHERE id = $1`, issue.ID); err != nil {
		t.Fatalf("concurrent writer UPDATE: %v", err)
	}

	type patchResult struct {
		dto *IssueDetail
		err error
	}
	resCh := make(chan patchResult, 1)
	done := make(chan struct{})
	opCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	newTitle := "patched title"
	go func() {
		defer close(done)
		d, err := opStore.PatchIssue(opCtx, issue.ID, PatchIssueInput{
			ExpectedVersion: issue.Version,
			Title:           &newTitle,
		}, HumanActor(creator, creatorName))
		resCh <- patchResult{d, err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("patch goroutine did not finish after cancellation")
		}
	})

	if !waitForBlocked(t, pool, appName, 10*time.Second) {
		t.Fatalf("could not orchestrate the race: PatchIssue never blocked on the concurrent writer's row lock")
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatalf("commit concurrent writer: %v", err)
	}

	var got patchResult
	select {
	case got = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("PatchIssue did not return within 30s")
	}

	var verr *ErrVersionConflict
	if !errors.As(got.err, &verr) {
		t.Fatalf("PatchIssue error = %v, want *ErrVersionConflict", got.err)
	}
	if verr.CurrentVersion != issue.Version+1 {
		t.Fatalf("CurrentVersion = %d, want %d (the concurrent writer's committed bump)",
			verr.CurrentVersion, issue.Version+1)
	}

	if got := testutil.ToFloat64(versionConflictOriginTotal.WithLabelValues("patchIssue", originGenuine)) - before; got != 0 {
		t.Errorf("versionConflictOriginTotal{op=patchIssue,origin=genuine} moved by %v, want 0 — "+
			"the refusal came from the version check of the retried attempt, not from degradeSerializationAbort", got)
	}
	if got := testutil.ToFloat64(versionConflictOriginTotal.WithLabelValues("patchIssue", originSerializationDerived)) - beforeDerived; got != 0 {
		t.Errorf("versionConflictOriginTotal{op=patchIssue,origin=serialization_derived} moved by %v, want 0", got)
	}
}

// TestAddLabel_VersionConflictOrigin_NoExpectedVersion — AddLabel takes no
// caller-declared version. The same one-writer scenario as above is now
// absorbed by its retry (R17): the label is there and no_expected_version
// is not counted. (Its exhaustion is
// store_write_paths_serialization_test.go#TestWritePathRetry_AddLabel_Exhausts.)
func TestAddLabel_VersionConflictOrigin_NoExpectedVersion(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	appName := "sk354-addlabel-noexp-" + uniqueTag()
	opPool := newTaggedPool(t, appName)

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	store := NewStore(pool)
	opStore := NewStore(opPool)

	issue, err := store.CreateIssue(ctx, pid, prefix, "counter",
		CreateIssueInput{Title: "sk-354 addlabel no-expected-version repro", Type: "task", Priority: 2},
		creator, HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	before := testutil.ToFloat64(versionConflictOriginTotal.WithLabelValues("addLabel", originNoExpectedVersion))
	beforeGenuine := testutil.ToFloat64(versionConflictOriginTotal.WithLabelValues("addLabel", originGenuine))

	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin concurrent writer: %v", err)
	}
	defer func() { _ = other.Rollback(ctx) }()
	if _, err := other.Exec(ctx,
		`UPDATE issues SET updated_at = now() WHERE id = $1`, issue.ID); err != nil {
		t.Fatalf("concurrent writer UPDATE: %v", err)
	}

	type labelResult struct {
		dto *IssueDetail
		err error
	}
	resCh := make(chan labelResult, 1)
	done := make(chan struct{})
	opCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		defer close(done)
		d, err := opStore.AddLabel(opCtx, issue.ID, "urgent", HumanActor(creator, creatorName), LockOpts{})
		resCh <- labelResult{d, err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("add-label goroutine did not finish after cancellation")
		}
	})

	if !waitForBlocked(t, pool, appName, 10*time.Second) {
		t.Fatalf("could not orchestrate the race: AddLabel never blocked on the concurrent writer's row lock")
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatalf("commit concurrent writer: %v", err)
	}

	var got labelResult
	select {
	case got = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("AddLabel did not return within 30s")
	}

	if got.err != nil {
		t.Fatalf("AddLabel error = %v, want success — one concurrent writer must be absorbed by the retry (R17)", got.err)
	}
	var labels int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM issue_labels WHERE issue_id = $1 AND label = 'urgent'`, issue.ID).Scan(&labels); err != nil {
		t.Fatalf("count labels: %v", err)
	}
	if labels != 1 {
		t.Errorf("label rows = %d, want 1", labels)
	}

	if got := testutil.ToFloat64(versionConflictOriginTotal.WithLabelValues("addLabel", originNoExpectedVersion)) - before; got != 0 {
		t.Errorf("versionConflictOriginTotal{op=addLabel,origin=no_expected_version} moved by %v, want 0 — "+
			"an absorbed conflict is not a version conflict", got)
	}
	if got := testutil.ToFloat64(versionConflictOriginTotal.WithLabelValues("addLabel", originGenuine)) - beforeGenuine; got != 0 {
		t.Errorf("versionConflictOriginTotal{op=addLabel,origin=genuine} moved by %v, want 0 — "+
			"AddLabel never had an expected version to be genuinely stale against", got)
	}
}

// ---- The four paths R17 does NOT move (row No. 22b): one SERIALIZABLE attempt,
// a single 40001 degraded by degradeSerializationAbort to ErrVersionConflict
// and counted as no_expected_version under the path's own op. Each conflict
// is built on a row the path itself writes (a concurrent UPDATE issues only
// conflicts with a path that later writes issues), waitForBlocked confirms it
// hit the target operation, and the stand is one where the path really
// writes. A success here would mean the path quietly gained a retry.

type singleConflictCase struct {
	op string
	// setup prepares the stand and returns the issue id.
	setup func(t *testing.T, pool *pgxpool.Pool, store *Store, actor Actor, pid uuid.UUID, prefix string) string
	// writer is the concurrent writer's statement (run in its own open
	// transaction, committed after the target blocked).
	writer func(ctx context.Context, tx pgx.Tx, id string) error
	call   func(ctx context.Context, s *Store, actor Actor, id string) error
	// notWritten asserts the operation's result is absent.
	notWritten func(t *testing.T, pool *pgxpool.Pool, id string)
}

func runSingleConflictDegrades(t *testing.T, c singleConflictCase) {
	pool := newTestPool(t)
	ctx := context.Background()
	appName := "sk63416-" + c.op[:6] + "-" + uniqueTag()
	opPool := newTaggedPool(t, appName)

	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	t.Cleanup(func() { releaseOutboxBacklog(t, pool, pid) })
	actor := HumanActor(creator, creatorName)
	id := c.setup(t, pool, NewStore(pool), actor, pid, prefix)

	before := testutil.ToFloat64(versionConflictOriginTotal.WithLabelValues(c.op, originNoExpectedVersion))

	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin concurrent writer: %v", err)
	}
	defer func() { _ = other.Rollback(ctx) }()
	if err := c.writer(ctx, other, id); err != nil {
		t.Fatalf("concurrent writer: %v", err)
	}

	resCh := make(chan error, 1)
	go func() { resCh <- c.call(ctx, NewStore(opPool), actor, id) }()
	if !waitForBlocked(t, pool, appName, 10*time.Second) {
		t.Fatalf("could not orchestrate the race: %s never blocked on the concurrent writer", c.op)
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatalf("commit concurrent writer: %v", err)
	}
	var callErr error
	select {
	case callErr = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not return within 30s", c.op)
	}

	var verr *ErrVersionConflict
	if !errors.As(callErr, &verr) {
		t.Fatalf("%s error = %v, want *ErrVersionConflict — a path outside R17 degrades one class-40 abort "+
			"(a success would mean it silently gained a retry)", c.op, callErr)
	}
	if got := testutil.ToFloat64(versionConflictOriginTotal.WithLabelValues(c.op, originNoExpectedVersion)) - before; got != 1 {
		t.Errorf("versionConflictOriginTotal{op=%s,origin=no_expected_version} moved by %v, want 1", c.op, got)
	}
	c.notWritten(t, pool, id)
}

func TestReplaceComponents_VersionConflictOrigin_NoExpectedVersion(t *testing.T) {
	var component uuid.UUID
	runSingleConflictDegrades(t, singleConflictCase{
		op: "replaceComponents",
		setup: func(t *testing.T, pool *pgxpool.Pool, _ *Store, actor Actor, pid uuid.UUID, prefix string) string {
			id := prefix + "-1"
			seedIssue(t, pool, pid, actor.UserID, id, "components under contention")
			component = seedComponent(t, pool, pid, actor.UserID, "c-"+uniqueTag())
			return id
		},
		writer: func(ctx context.Context, tx pgx.Tx, id string) error {
			_, err := tx.Exec(ctx, `UPDATE issues SET updated_at = now() WHERE id = $1`, id)
			return err
		},
		call: func(ctx context.Context, s *Store, actor Actor, id string) error {
			_, err := s.ReplaceComponents(ctx, id, []uuid.UUID{component}, actor, LockOpts{})
			return err
		},
		notWritten: func(t *testing.T, pool *pgxpool.Pool, id string) {
			var n int
			if err := pool.QueryRow(context.Background(),
				`SELECT count(*) FROM issue_components WHERE issue_id = $1`, id).Scan(&n); err != nil {
				t.Fatalf("count components: %v", err)
			}
			if n != 0 {
				t.Errorf("issue_components rows = %d after the degraded abort, want 0", n)
			}
		},
	})
}

func TestResumeDelegated_VersionConflictOrigin_NoExpectedVersion(t *testing.T) {
	runSingleConflictDegrades(t, singleConflictCase{
		op: "resumeDelegated",
		setup: func(t *testing.T, pool *pgxpool.Pool, _ *Store, actor Actor, pid uuid.UUID, prefix string) string {
			id := prefix + "-1"
			seedIssue(t, pool, pid, actor.UserID, id, "resume under contention")
			// The caller must be in dismissed_by, or Resume exits without a write.
			if _, err := pool.Exec(context.Background(),
				`UPDATE issues SET dismissed_by = array_append(dismissed_by, $2::uuid) WHERE id = $1`,
				id, actor.UserID); err != nil {
				t.Fatalf("seed dismissed_by: %v", err)
			}
			return id
		},
		writer: func(ctx context.Context, tx pgx.Tx, id string) error {
			_, err := tx.Exec(ctx, `UPDATE issues SET updated_at = now() WHERE id = $1`, id)
			return err
		},
		call: func(ctx context.Context, s *Store, actor Actor, id string) error {
			_, err := s.ResumeDelegated(ctx, DelegationActionInput{ID: id, Actor: actor})
			return err
		},
		notWritten: func(t *testing.T, pool *pgxpool.Pool, id string) {
			var still bool
			if err := pool.QueryRow(context.Background(),
				`SELECT cardinality(dismissed_by) > 0 FROM issues WHERE id = $1`, id).Scan(&still); err != nil {
				t.Fatalf("read dismissed_by: %v", err)
			}
			if !still {
				t.Errorf("dismissed_by was cleared by a degraded abort")
			}
		},
	})
}

const contendedHash = "c0ffee1"

func TestAttachCommits_VersionConflictOrigin_NoExpectedVersion(t *testing.T) {
	var repo uuid.UUID
	runSingleConflictDegrades(t, singleConflictCase{
		op: "attachCommits",
		setup: func(t *testing.T, pool *pgxpool.Pool, _ *Store, actor Actor, pid uuid.UUID, prefix string) string {
			id := prefix + "-1"
			seedIssue(t, pool, pid, actor.UserID, id, "attach under contention")
			repo = testsupport.SeedRepo(t, pool, pid, "repo1", "https://example.com/attach-contended.git")
			return id
		},
		// The same link, inserted by the competitor and left open: the
		// path's INSERT … ON CONFLICT waits on the unique key, and the
		// committed row invisible to its snapshot is a serialization failure.
		writer: func(ctx context.Context, tx pgx.Tx, id string) error {
			_, err := tx.Exec(ctx, `
INSERT INTO issue_commits (issue_id, repo_id, hash, linked_by_kind) VALUES ($1, $2, $3, 'system')`,
				id, repo, contendedHash)
			return err
		},
		call: func(ctx context.Context, s *Store, actor Actor, id string) error {
			_, err := s.AttachCommits(ctx, AttachCommitsInput{ID: id, Actor: actor,
				Commits: []CloseCommitInput{{Hash: contendedHash, RepoURL: "https://example.com/attach-contended.git"}}})
			return err
		},
		notWritten: func(t *testing.T, pool *pgxpool.Pool, id string) {
			var rows, events int
			if err := pool.QueryRow(context.Background(), `
SELECT (SELECT count(*) FROM issue_commits WHERE issue_id = $1 AND linked_by_kind <> 'system'),
       (SELECT count(*) FROM events WHERE entity_id = $1 AND type = $2)`,
				id, EventIssueCommitsAttached).Scan(&rows, &events); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if rows != 0 || events != 0 {
				t.Errorf("after the degraded abort: path-written links = %d, attach events = %d; want 0 and 0", rows, events)
			}
		},
	})
}

func TestDetachCommits_VersionConflictOrigin_NoExpectedVersion(t *testing.T) {
	runSingleConflictDegrades(t, singleConflictCase{
		op: "detachCommits",
		setup: func(t *testing.T, pool *pgxpool.Pool, store *Store, actor Actor, pid uuid.UUID, prefix string) string {
			id := prefix + "-1"
			seedIssue(t, pool, pid, actor.UserID, id, "detach under contention")
			testsupport.SeedRepo(t, pool, pid, "repo1", "https://example.com/detach-contended.git")
			if _, err := store.AttachCommits(context.Background(), AttachCommitsInput{ID: id, Actor: actor,
				Commits: []CloseCommitInput{{Hash: contendedHash, RepoURL: "https://example.com/detach-contended.git"}}}); err != nil {
				t.Fatalf("seed link: %v", err)
			}
			return id
		},
		// A non-key change of the very row the path deletes, left open: the
		// DELETE waits, and the committed concurrent update is a
		// serialization failure. The competitor does not delete the row,
		// so the path does not degenerate into a no-op.
		writer: func(ctx context.Context, tx pgx.Tx, id string) error {
			_, err := tx.Exec(ctx,
				`UPDATE issue_commits SET message = 'touched' WHERE issue_id = $1 AND hash = $2`, id, contendedHash)
			return err
		},
		call: func(ctx context.Context, s *Store, actor Actor, id string) error {
			_, err := s.DetachCommits(ctx, DetachCommitsInput{ID: id, Hashes: []string{contendedHash}, Actor: actor})
			return err
		},
		notWritten: func(t *testing.T, pool *pgxpool.Pool, id string) {
			var n int
			if err := pool.QueryRow(context.Background(),
				`SELECT count(*) FROM issue_commits WHERE issue_id = $1 AND hash = $2`, id, contendedHash).Scan(&n); err != nil {
				t.Fatalf("count links: %v", err)
			}
			if n != 1 {
				t.Errorf("link rows = %d after the degraded abort, want 1 (nothing detached)", n)
			}
		},
	})
}
