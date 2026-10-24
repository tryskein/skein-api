package attachments

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/google/uuid"
)

// The orphan reconciler classifies a whole tick's worth of bucket keys
// with ONE existence query instead of one SELECT per key. These tests pin
// the behaviour that must survive that batching:
//
//   - only keys whose attachment id has no row are removed (N > 1);
//   - keys that are not ours (AttachmentIDFromKey fails) are never touched
//     (they never enter a batch);
//   - the scan reaches keys beyond the first batch (batch size is not a cap);
//   - a RemoveObject failure stays isolated to its own key;
//   - keys enumerated before a mid-list failure are still processed.

// orphanFixture seeds a project + issue and returns the store plus a
// helper that mints S3 keys under them.
func orphanFixture(t *testing.T) (*Store, func(id uuid.UUID, name string) string, uuid.UUID, string) {
	t.Helper()
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	iid := prefix + "-1"
	seedIssue(t, pool, pid, owner, iid, "orphan batch")

	key := func(id uuid.UUID, name string) string {
		return BuildS3Key(pid, iid, id, name)
	}
	return NewStore(pool), key, owner, iid
}

// seedLive creates a pending row so the key counts as live (has a row).
func seedLive(t *testing.T, store *Store, iid string, owner uuid.UUID, id uuid.UUID, key, name string) {
	t.Helper()
	if _, err := store.CreatePending(context.Background(),
		id, iid, nil, name, "image/png", 1,
		"skein-attachments", key, owner); err != nil {
		t.Fatalf("CreatePending %s: %v", name, err)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestReconcileOrphans_BatchMultipleOrphans: 3 orphans, 2 live rows and
// one foreign object in a single pass. Exactly the 3 orphans go.
func TestReconcileOrphans_BatchMultipleOrphans(t *testing.T) {
	store, key, owner, iid := orphanFixture(t)

	live1, live2 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	keyLive1, keyLive2 := key(live1, "l1.png"), key(live2, "l2.png")
	seedLive(t, store, iid, owner, live1, keyLive1, "l1.png")
	seedLive(t, store, iid, owner, live2, keyLive2, "l2.png")

	keyOrphans := []string{
		key(uuid.Must(uuid.NewV7()), "o1.png"),
		key(uuid.Must(uuid.NewV7()), "o2.png"),
		key(uuid.Must(uuid.NewV7()), "o3.png"),
	}
	keyForeign := "manually-uploaded.txt"

	mio := &stubS3{listed: []string{
		keyLive1, keyOrphans[0], keyForeign, keyOrphans[1], keyLive2, keyOrphans[2],
	}}
	reconcileOrphans(context.Background(), store, mio, 100)

	want := sortedCopy(keyOrphans)
	got := sortedCopy(mio.removed)
	if len(got) != len(want) {
		t.Fatalf("removed = %v, want exactly %v", mio.removed, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("removed = %v, want %v", got, want)
		}
	}
}

// TestReconcileOrphans_AllLive: nothing to delete when every key has a
// row — the batch query must not produce false orphans.
func TestReconcileOrphans_AllLive(t *testing.T) {
	store, key, owner, iid := orphanFixture(t)

	var keys []string
	for _, name := range []string{"a.png", "b.png", "c.png"} {
		id := uuid.Must(uuid.NewV7())
		k := key(id, name)
		seedLive(t, store, iid, owner, id, k, name)
		keys = append(keys, k)
	}

	mio := &stubS3{listed: keys}
	reconcileOrphans(context.Background(), store, mio, 100)

	if len(mio.removed) != 0 {
		t.Fatalf("removed = %v, want none", mio.removed)
	}
}

// TestReconcileOrphans_EmptyBucket is the N=0 boundary: no keys at all →
// no existence query, no deletes, no panic.
func TestReconcileOrphans_EmptyBucket(t *testing.T) {
	store, _, _, _ := orphanFixture(t)

	mio := &stubS3{}
	reconcileOrphans(context.Background(), store, mio, 100)

	if len(mio.removed) != 0 {
		t.Fatalf("removed = %v, want none", mio.removed)
	}
}

// TestReconcileOrphans_SingleOrphan is the N=1 boundary.
func TestReconcileOrphans_SingleOrphan(t *testing.T) {
	store, key, _, _ := orphanFixture(t)

	keyOrphan := key(uuid.Must(uuid.NewV7()), "only.png")
	mio := &stubS3{listed: []string{keyOrphan}}
	reconcileOrphans(context.Background(), store, mio, 100)

	if len(mio.removed) != 1 || mio.removed[0] != keyOrphan {
		t.Fatalf("removed = %v, want [%s]", mio.removed, keyOrphan)
	}
}

// TestReconcileOrphans_ReachesPastFirstBatch: the batch size bounds one
// existence query, not how far into the bucket a pass reaches. The listing
// is lexicographic and the scan keeps no cursor, so a pass that stopped at
// the first `batchSize` keys would never see an orphan that sorts after
// them. With batchSize=2, foreign objects and live rows ahead of it, the
// orphans in later batches must still go in a single pass.
func TestReconcileOrphans_ReachesPastFirstBatch(t *testing.T) {
	store, key, owner, iid := orphanFixture(t)

	live1, live2 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	keyLive1, keyLive2 := key(live1, "l1.png"), key(live2, "l2.png")
	seedLive(t, store, iid, owner, live1, keyLive1, "l1.png")
	seedLive(t, store, iid, owner, live2, keyLive2, "l2.png")

	o1 := key(uuid.Must(uuid.NewV7()), "o1.png")
	o2 := key(uuid.Must(uuid.NewV7()), "o2.png")
	o3 := key(uuid.Must(uuid.NewV7()), "o3.png")
	mio := &stubS3{listed: []string{
		"stray-1.txt", keyLive1, keyLive2, "stray-2.txt", o1, "stray-3.txt", o2, o3,
	}}

	reconcileOrphans(context.Background(), store, mio, 2)

	want := sortedCopy([]string{o1, o2, o3})
	got := sortedCopy(mio.removed)
	if len(got) != len(want) {
		t.Fatalf("removed = %v, want exactly %v", mio.removed, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("removed = %v, want %v", got, want)
		}
	}
}

// TestReconcileOrphans_ExistenceFailureDeletesNothing: when the existence
// lookup fails the batch is not classified, so nothing may be removed —
// deleting an object whose row might exist is unrecoverable.
func TestReconcileOrphans_ExistenceFailureDeletesNothing(t *testing.T) {
	store, key, _, _ := orphanFixture(t)

	// A cancelled context makes the existence query fail while the stub
	// listing still hands the key over.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	orphan := key(uuid.Must(uuid.NewV7()), "o.png")
	mio := &stubS3{listed: []string{orphan}}

	reconcileOrphans(ctx, store, mio, 100)

	if len(mio.removed) != 0 {
		t.Fatalf("removed = %v, want none when the existence check fails", mio.removed)
	}
}

// TestReconcileOrphans_DeleteFailureIsolated: a RemoveObject error must
// not abort the rest of the batch.
func TestReconcileOrphans_DeleteFailureIsolated(t *testing.T) {
	store, key, _, _ := orphanFixture(t)

	bad := key(uuid.Must(uuid.NewV7()), "bad.png")
	good := key(uuid.Must(uuid.NewV7()), "good.png")
	mio := &stubS3{
		listed:    []string{bad, good},
		removeErr: map[string]error{bad: errors.New("s3 down for this key")},
	}

	reconcileOrphans(context.Background(), store, mio, 100)

	if len(mio.removed) != 1 || mio.removed[0] != good {
		t.Fatalf("removed = %v, want [%s] — one failed delete must not stop the batch",
			mio.removed, good)
	}
}

// TestReconcileOrphans_ListErrorKeepsEnumeratedWork: when the bucket
// listing breaks after handing over some keys, the orphans among those
// keys are still removed (the streaming implementation deleted them
// before the break, and batching must not lose that progress).
func TestReconcileOrphans_ListErrorKeepsEnumeratedWork(t *testing.T) {
	store, key, _, _ := orphanFixture(t)

	orphan := key(uuid.Must(uuid.NewV7()), "seen.png")
	mio := &stubS3{
		listed:  []string{orphan},
		listErr: errors.New("bucket listing interrupted"),
	}

	reconcileOrphans(context.Background(), store, mio, 100)

	if len(mio.removed) != 1 || mio.removed[0] != orphan {
		t.Fatalf("removed = %v, want [%s]", mio.removed, orphan)
	}
}

// TestExistingIDs verifies the batch existence primitive directly: the
// live id is reported, the unknown id is absent, duplicates collapse, and
// an empty input issues no query.
func TestExistingIDs(t *testing.T) {
	store, key, owner, iid := orphanFixture(t)

	live := uuid.Must(uuid.NewV7())
	seedLive(t, store, iid, owner, live, key(live, "live.png"), "live.png")
	missing := uuid.Must(uuid.NewV7())

	got, err := store.ExistingIDs(context.Background(),
		[]uuid.UUID{live, missing, live})
	if err != nil {
		t.Fatalf("ExistingIDs: %v", err)
	}
	if _, ok := got[live]; !ok {
		t.Errorf("live id %s missing from result", live)
	}
	if _, ok := got[missing]; ok {
		t.Errorf("unknown id %s must not be reported as existing", missing)
	}
	if len(got) != 1 {
		t.Errorf("len(result) = %d, want 1", len(got))
	}

	empty, err := store.ExistingIDs(context.Background(), nil)
	if err != nil {
		t.Fatalf("ExistingIDs(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("ExistingIDs(nil) = %v, want empty set", empty)
	}
}
