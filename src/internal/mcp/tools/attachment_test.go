package tools

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/attachments"
)

// fakeS3 is a stub S3Access we can hand the MCP tool deps so the
// tests do not require a real S3 container. It records calls so the
// test can assert e.g. "the policy was minted with this size".
type fakeS3 struct {
	postCalls   []postCall
	getCalls    []string
	statErr     error
	statCalls   []string
	removeCalls []string
	listKeys    []string
}

type postCall struct {
	Key         string
	ContentType string
	Size        int64
	TTL         time.Duration
}

func (f *fakeS3) PresignedPostPolicy(_ context.Context, key, contentType string, size int64, ttl time.Duration) (attachments.PostPolicy, error) {
	f.postCalls = append(f.postCalls, postCall{Key: key, ContentType: contentType, Size: size, TTL: ttl})
	return attachments.PostPolicy{
		URL:    "http://localhost:9000/skein-attachments",
		Fields: map[string]string{"key": key, "policy": "stub", "x-amz-signature": "stub"},
	}, nil
}

func (f *fakeS3) PresignedGetURL(_ context.Context, key, _ string, _ time.Duration) (string, error) {
	f.getCalls = append(f.getCalls, key)
	return "http://localhost:9000/skein-attachments/" + key + "?signed", nil
}

func (f *fakeS3) RemoveObject(_ context.Context, key string) error {
	f.removeCalls = append(f.removeCalls, key)
	return nil
}

func (f *fakeS3) StatObject(_ context.Context, key string) error {
	f.statCalls = append(f.statCalls, key)
	return f.statErr
}

func (f *fakeS3) ListKeys(_ context.Context, _ string, fn func(key string) error) error {
	for _, k := range f.listKeys {
		if err := fn(k); err != nil {
			return err
		}
	}
	return nil
}

// TestAttachmentUploadURL_HappyPath inserts a pending row + verifies the
// stub minted a presigned POST policy bound to the right size.
func TestAttachmentUploadURL_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	uid, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, uid, "att")
	iid := prefix + "-1"
	seedIssue(t, pool, pid, uid, iid, "test", "open", 2)

	store := attachments.NewStore(pool)
	mio := &fakeS3{}
	deps := AttachmentDepsFor(pool, store, mio, "skein-attachments", 1<<20, 10*time.Minute)
	handler := handleAttachmentUploadURL(deps)
	ctx := withCtx(t, uid, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId":     iid,
		"filename":    "screenshot.png",
		"contentType": "image/png",
		"sizeBytes":   1024,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)
	aidStr, _ := body["attachmentId"].(string)
	if aidStr == "" {
		t.Fatalf("missing attachmentId in response")
	}
	if _, err := uuid.Parse(aidStr); err != nil {
		t.Fatalf("attachmentId not a UUID: %v", err)
	}
	if len(mio.postCalls) != 1 {
		t.Fatalf("expected 1 PresignedPostPolicy call, got %d", len(mio.postCalls))
	}
	if mio.postCalls[0].Size != 1024 {
		t.Fatalf("policy size = %d, want 1024", mio.postCalls[0].Size)
	}

	// Verify the row landed in pending state.
	var dbStatus string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM attachments WHERE id = $1`, aidStr).Scan(&dbStatus); err != nil {
		t.Fatalf("verify db row: %v", err)
	}
	if dbStatus != "pending" {
		t.Fatalf("db status = %q, want pending", dbStatus)
	}
}

// TestAttachmentUploadURL_BlockedExt rejects an executable filename even
// though the validation would otherwise pass.
func TestAttachmentUploadURL_BlockedExt(t *testing.T) {
	pool := newTestPool(t)
	uid, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, uid, "att")
	iid := prefix + "-1"
	seedIssue(t, pool, pid, uid, iid, "test", "open", 2)

	store := attachments.NewStore(pool)
	mio := &fakeS3{}
	deps := AttachmentDepsFor(pool, store, mio, "skein-attachments", 1<<20, 10*time.Minute)
	handler := handleAttachmentUploadURL(deps)
	ctx := withCtx(t, uid, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId":     iid,
		"filename":    "malware.exe",
		"contentType": "application/octet-stream",
		"sizeBytes":   1024,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for blocked .exe")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeAttachmentMIMEBlocked {
		t.Fatalf("got code %v, want %s", body["error"], ErrCodeAttachmentMIMEBlocked)
	}
}

// TestAttachmentUploadURL_TooLarge enforces the size cap.
func TestAttachmentUploadURL_TooLarge(t *testing.T) {
	pool := newTestPool(t)
	uid, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, uid, "att")
	iid := prefix + "-1"
	seedIssue(t, pool, pid, uid, iid, "test", "open", 2)

	store := attachments.NewStore(pool)
	mio := &fakeS3{}
	deps := AttachmentDepsFor(pool, store, mio, "skein-attachments", 100, 10*time.Minute)
	handler := handleAttachmentUploadURL(deps)
	ctx := withCtx(t, uid, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId":     iid,
		"filename":    "big.bin",
		"contentType": "application/octet-stream",
		"sizeBytes":   1024,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for oversized file")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeAttachmentTooLarge {
		t.Fatalf("got code %v, want %s", body["error"], ErrCodeAttachmentTooLarge)
	}
}

// TestAttachmentUploadURL_DisabledNoS3 returns the stable
// attachments_unavailable code when S3 is not configured.
func TestAttachmentUploadURL_DisabledNoS3(t *testing.T) {
	pool := newTestPool(t)
	uid, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, uid, "att")
	iid := prefix + "-1"
	seedIssue(t, pool, pid, uid, iid, "test", "open", 2)

	store := attachments.NewStore(pool)
	deps := AttachmentDepsFor(pool, store, nil, "skein-attachments", 1<<20, 10*time.Minute)
	handler := handleAttachmentUploadURL(deps)
	ctx := withCtx(t, uid, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"issueId":     iid,
		"filename":    "f.png",
		"contentType": "image/png",
		"sizeBytes":   1024,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true when S3 disabled")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeAttachmentsUnavailable {
		t.Fatalf("got code %v, want %s", body["error"], ErrCodeAttachmentsUnavailable)
	}
}

// TestAttachmentConfirm_FailsWithoutObject verifies the StatObject
// probe — when S3 doesn't have the object yet, confirm returns
// attachment_object_missing.
func TestAttachmentConfirm_FailsWithoutObject(t *testing.T) {
	pool := newTestPool(t)
	uid, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, uid, "att")
	iid := prefix + "-1"
	seedIssue(t, pool, pid, uid, iid, "test", "open", 2)

	store := attachments.NewStore(pool)
	mio := &fakeS3{statErr: errors.New("not found")}
	deps := AttachmentDepsFor(pool, store, mio, "skein-attachments", 1<<20, 10*time.Minute)

	// Pre-create a pending row.
	id := uuid.Must(uuid.NewV7())
	if _, err := store.CreatePending(context.Background(),
		id, iid, nil, "f.png", "image/png", 1024,
		"skein-attachments", attachments.BuildS3Key(pid, iid, id, "f.png"), uid); err != nil {
		t.Fatalf("CreatePending: %v", err)
	}

	handler := handleAttachmentConfirm(deps)
	ctx := withCtx(t, uid, "admin")
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"attachmentId": id.String()}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true, got %+v", res)
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeAttachmentObjectMissing {
		t.Fatalf("got code %v, want %s", body["error"], ErrCodeAttachmentObjectMissing)
	}
}

// TestAttachmentList_OnlyReady verifies the list tool returns only ready
// rows and respects visibility.
func TestAttachmentList_OnlyReady(t *testing.T) {
	pool := newTestPool(t)
	uid, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, uid, "att")
	iid := prefix + "-1"
	seedIssue(t, pool, pid, uid, iid, "test", "open", 2)

	store := attachments.NewStore(pool)
	mio := &fakeS3{}
	deps := AttachmentDepsFor(pool, store, mio, "skein-attachments", 1<<20, 10*time.Minute)
	actor := attachments.Actor{UserID: uid, Username: "admin", Role: "agent"}

	// Pending — not in result.
	idP := uuid.Must(uuid.NewV7())
	_, _ = store.CreatePending(context.Background(),
		idP, iid, nil, "p.png", "image/png", 1, "b",
		attachments.BuildS3Key(pid, iid, idP, "p.png"), uid)
	// Ready — in result.
	idR := uuid.Must(uuid.NewV7())
	_, _ = store.CreatePending(context.Background(),
		idR, iid, nil, "r.png", "image/png", 1, "b",
		attachments.BuildS3Key(pid, iid, idR, "r.png"), uid)
	if _, err := store.Confirm(context.Background(), idR, uid, "user", actor); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	handler := handleAttachmentList(deps)
	ctx := withCtx(t, uid, "admin")
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"issueId": iid}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	body := extractToolJSON(t, res)
	total, _ := body["total"].(float64)
	if int(total) != 1 {
		t.Fatalf("got total=%v, want 1 (ready only)", total)
	}
}

// TestAttachmentDelete_HardDeletesRowAndRemovesObject verifies the
// delete tool flow — row gone, RemoveObject called.
func TestAttachmentDelete_HardDeletesRowAndRemovesObject(t *testing.T) {
	pool := newTestPool(t)
	uid, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, uid, "att")
	iid := prefix + "-1"
	seedIssue(t, pool, pid, uid, iid, "test", "open", 2)

	store := attachments.NewStore(pool)
	mio := &fakeS3{}
	deps := AttachmentDepsFor(pool, store, mio, "skein-attachments", 1<<20, 10*time.Minute)

	id := uuid.Must(uuid.NewV7())
	key := attachments.BuildS3Key(pid, iid, id, "f.png")
	if _, err := store.CreatePending(context.Background(),
		id, iid, nil, "f.png", "image/png", 1, "skein-attachments", key, uid); err != nil {
		t.Fatalf("CreatePending: %v", err)
	}

	handler := handleAttachmentDelete(deps)
	ctx := withCtx(t, uid, "admin")
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"attachmentId": id.String()}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", res)
	}
	if len(mio.removeCalls) != 1 || mio.removeCalls[0] != key {
		t.Fatalf("expected RemoveObject(%q), got %v", key, mio.removeCalls)
	}
	// Row gone.
	exists, err := store.ExistsByID(context.Background(), id)
	if err != nil {
		t.Fatalf("ExistsByID: %v", err)
	}
	if exists {
		t.Fatalf("expected row hard-deleted")
	}
}
