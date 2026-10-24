package handler

// attachments_actions_test.go — sk-674.18 T066 (sk-689.37), changed by
// sk-741: the attachment half of «for every issue write path — a permission
// to a participant of the project and to an instance admin without
// membership» (the other paths, both channels:
// cmd/api/issue_write_actions_test.go; the MCP attachment tools:
// mcp/tools/attachment_actions_test.go). Reference — the action table of
// US-6 as changed by the owner 2026-10-17: attaching is writing issues,
// which the instance admin may do in a non-personal project with the
// owner's rights. Project A.members of testsupport.SeedTenancy; adm0 sees it
// without any membership, uA is its member.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

func TestAttachmentWrites_FollowTheActionTable(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	f := testsupport.SeedTenancy(t, pool)
	p := f.AMembers

	ah := NewAttachmentsHandler(pool, quotaS3{}, "skein-attachments", 1<<40, 600)
	router := chi.NewRouter()
	router.Route("/api/v1", func(r chi.Router) {
		r.Use(ParseUserHeaders)
		r.Use(RequireUser)
		r.Post("/issues/{issueID}/attachments/upload-url", ah.UploadURL)
		r.Post("/attachments/{id}/confirm", ah.Confirm)
		r.Delete("/attachments/{id}", ah.Delete)
	})
	do := func(a testsupport.Actor, method, path string, body any) (int, map[string]any) {
		t.Helper()
		u := f.User(t, a)
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(auth.HeaderUserID, u.ID.String())
		req.Header.Set(auth.HeaderUsername, u.Username)
		req.Header.Set(auth.HeaderUserRole, u.Role)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	n := 0
	fresh := func() string {
		n++
		id := fmt.Sprintf("%s-%d", p.IDPrefix, 8000+n)
		return testsupport.SeedIssue(t, pool, p.ID, p.OwnerID, id, "t066 "+id, "open", 2)
	}

	type path struct {
		name string
		do   func(a testsupport.Actor) (int, map[string]any)
	}
	paths := []path{
		{"upload url", func(a testsupport.Actor) (int, map[string]any) {
			return do(a, http.MethodPost, "/api/v1/issues/"+fresh()+"/attachments/upload-url",
				map[string]any{"filename": "t066.txt", "contentType": "text/plain", "sizeBytes": 10})
		}},
		{"confirm own upload", func(a testsupport.Actor) (int, map[string]any) {
			id := testsupport.SeedAttachment(t, pool, fresh(), f.User(t, a).ID, "pending")
			return do(a, http.MethodPost, "/api/v1/attachments/"+id.String()+"/confirm", nil)
		}},
		{"delete", func(a testsupport.Actor) (int, map[string]any) {
			id := testsupport.SeedAttachment(t, pool, fresh(), p.OwnerID, "ready")
			return do(a, http.MethodDelete, "/api/v1/attachments/"+id.String(), nil)
		}},
	}
	// The table allows every path to both actors.
	for _, actor := range []testsupport.Actor{testsupport.ActorAdmin0, testsupport.ActorOnlyA} {
		for _, pth := range paths {
			t.Run(string(actor)+"/"+pth.name, func(t *testing.T) {
				if status, body := pth.do(actor); status < 200 || status >= 300 {
					t.Errorf("the table allows it, got %d %v", status, body)
				}
			})
		}
	}
}
