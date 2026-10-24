package attachments

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// stubS3 implements S3Access for cleanup tests.
//
// listed is the bucket content handed to the ListKeys callback, in order.
// listErr, when set, is returned AFTER every key has been offered — it
// simulates a listing that breaks partway once the caller has already
// seen some keys. removeErr keys make RemoveObject fail for that key
// alone.
type stubS3 struct {
	removed   []string
	listed    []string
	listErr   error
	removeErr map[string]error
}

func (s *stubS3) PresignedPostPolicy(_ context.Context, _, _ string, _ int64, _ time.Duration) (PostPolicy, error) {
	return PostPolicy{}, errors.New("not used in cleanup")
}
func (s *stubS3) PresignedGetURL(_ context.Context, _, _ string, _ time.Duration) (string, error) {
	return "", errors.New("not used in cleanup")
}
func (s *stubS3) RemoveObject(_ context.Context, key string) error {
	if err, ok := s.removeErr[key]; ok {
		return err
	}
	s.removed = append(s.removed, key)
	return nil
}
func (s *stubS3) StatObject(_ context.Context, _ string) error { return nil }
func (s *stubS3) ListKeys(_ context.Context, _ string, fn func(key string) error) error {
	for _, k := range s.listed {
		if err := fn(k); err != nil {
			return err
		}
	}
	return s.listErr
}

// TestCleanupTick_DeletesStalePending verifies the stale-pending sweep
// hard-deletes rows older than the threshold and removes their objects.
func TestCleanupTick_DeletesStalePending(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	iid := prefix + "-1"
	seedIssue(t, pool, pid, owner, iid, "test")

	store := NewStore(pool)
	id := uuid.Must(uuid.NewV7())
	key := BuildS3Key(pid, iid, id, "f.png")
	if _, err := store.CreatePending(context.Background(),
		id, iid, nil, "f.png", "image/png", 1,
		"skein-attachments", key, owner); err != nil {
		t.Fatalf("CreatePending: %v", err)
	}
	// Backdate so it qualifies as stale.
	if _, err := pool.Exec(context.Background(),
		`UPDATE attachments SET uploaded_at = now() - interval '2 hours' WHERE id = $1`, id); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	mio := &stubS3{}
	// Loop the cleanup tick until our row is gone — the integration DB
	// can carry stale rows from prior test runs that exceed BatchSize on
	// a single tick. We bound the loop tightly so a real bug still fails
	// fast.
	for i := 0; i < 10; i++ {
		cleanupTick(context.Background(), pool, store, mio, CleanupConfig{
			PendingMaxAge: 1 * time.Hour,
			BatchSize:     200,
		})
		exists, err := store.ExistsByID(context.Background(), id)
		if err != nil {
			t.Fatalf("ExistsByID: %v", err)
		}
		if !exists {
			break
		}
	}

	exists, err := store.ExistsByID(context.Background(), id)
	if err != nil {
		t.Fatalf("ExistsByID: %v", err)
	}
	if exists {
		t.Fatalf("expected stale pending row hard-deleted")
	}
	// Among RemoveObject calls there must be one for our key.
	found := false
	for _, k := range mio.removed {
		if k == key {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected RemoveObject(%q) somewhere in %v", key, mio.removed)
	}
}

// TestReconcileOrphans_DeletesUnreferencedObjects verifies the
// orphan reconciler walks the bucket and removes objects whose row is
// gone, while preserving objects that still have a row.
func TestReconcileOrphans_DeletesUnreferencedObjects(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	iid := prefix + "-1"
	seedIssue(t, pool, pid, owner, iid, "test")

	store := NewStore(pool)
	// Live: row + key.
	idLive := uuid.Must(uuid.NewV7())
	keyLive := BuildS3Key(pid, iid, idLive, "live.png")
	if _, err := store.CreatePending(context.Background(),
		idLive, iid, nil, "live.png", "image/png", 1,
		"skein-attachments", keyLive, owner); err != nil {
		t.Fatalf("CreatePending live: %v", err)
	}

	// Orphan: a key whose attachment uuid is not in the DB.
	orphanID := uuid.Must(uuid.NewV7())
	keyOrphan := BuildS3Key(pid, iid, orphanID, "orphan.png")

	// Non-skein object — must NOT be removed.
	keyForeign := "manually-uploaded.txt"

	mio := &stubS3{listed: []string{keyLive, keyOrphan, keyForeign}}
	reconcileOrphans(context.Background(), store, mio, 100)

	if len(mio.removed) != 1 {
		t.Fatalf("expected 1 RemoveObject call, got %d (%v)", len(mio.removed), mio.removed)
	}
	if mio.removed[0] != keyOrphan {
		t.Fatalf("expected RemoveObject(%q), got %q", keyOrphan, mio.removed[0])
	}
}
