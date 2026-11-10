package publicapi_test

// attachments_test.go — the attachment operations of the public API
// (sk-878.3.1 T026, row 97; US-1, D-11): parity with the actions of
// skein_attachments on the success and the refusals, the upload URL that
// Prefer never hides, and storage unavailable — 503 attachments_unavailable.
// The reference of the parity is the MCP channel itself over twin issues;
// of the rest the spec («createAttachmentUpload always answers the upload
// URL»; D-11: «unavailability of a dependency, attachments_unavailable —
// 503»).

import (
	"net/http"
	"testing"

	"skein-api/internal/apikeys"
	"skein-api/internal/testsupport"
)

// uploadBody is a valid createAttachmentUpload body.
func uploadBody(filename string, size int) map[string]any {
	return map[string]any{"filename": filename, "contentType": "text/plain", "sizeBytes": size}
}

// TestAttachments_ParityWithMCP — row 97 (US-1, INV-4). Each attachment
// operation asked with the same key on REST and on MCP over twin issues
// gives the same code for the success and for its refusals, and the same
// events. Red when a code or the events differ from MCP.
func TestAttachments_ParityWithMCP(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	ko := w.key(w.owner, apikeys.LevelWrite)
	km := w.key(w.member, apikeys.LevelWrite)

	// pendingREST and pendingMCP start an upload on one channel and return
	// the attachment id.
	pendingREST := func(issue string) string {
		c := rg.rest(ko, http.MethodPost, issuePath(issue, "/attachments"), uploadBody("note.txt", 12), nil)
		wantOK(t, "createAttachmentUpload", c, http.StatusCreated)
		return c.json(t)["attachmentId"].(string)
	}
	pendingMCP := func(issue string) string {
		a := rg.tool(ko, "skein_attachments", map[string]any{"action": "upload_url", "issueId": issue,
			"filename": "note.txt", "contentType": "text/plain", "sizeBytes": 12})
		if a.code != "" {
			t.Fatalf("upload_url: %s", a.text)
		}
		return a.json(t)["attachmentId"].(string)
	}
	ready := func(key, att string) {
		if c := rg.rest(key, http.MethodPost, "/api/v1/attachments/"+att+"/confirm", nil, nil); c.status != http.StatusOK {
			t.Fatalf("confirmAttachment: %d %s", c.status, c.body)
		}
	}

	type twin struct {
		name string
		// pending: the operation acts on a pending attachment of the
		// issue, confirmed when ready.
		pending, confirmed bool
		rest               func(issue, att string) restCall
		mcp                func(issue, att string) toolAnswer
		want               string
	}
	upload := func(body map[string]any) (func(issue, _ string) restCall, func(issue, _ string) toolAnswer) {
		return func(issue, _ string) restCall {
				return rg.rest(ko, http.MethodPost, issuePath(issue, "/attachments"), body, nil)
			}, func(issue, _ string) toolAnswer {
				args := map[string]any{"action": "upload_url", "issueId": issue}
				for k, v := range body {
					args[k] = v
				}
				return rg.tool(ko, "skein_attachments", args)
			}
	}
	attOp := func(key, method, suffix, action string) (func(_, att string) restCall, func(_, att string) toolAnswer) {
		return func(_, att string) restCall {
				return rg.rest(key, method, "/api/v1/attachments/"+att+suffix, nil, nil)
			}, func(_, att string) toolAnswer {
				return rg.tool(key, "skein_attachments", map[string]any{"action": action, "attachmentId": att})
			}
	}
	uploadOK, mcpUploadOK := upload(uploadBody("note.txt", 12))
	uploadBig, mcpUploadBig := upload(uploadBody("big.txt", 11<<20))
	uploadExe, mcpUploadExe := upload(map[string]any{"filename": "run.exe", "contentType": "application/octet-stream", "sizeBytes": 12})
	confirm, mcpConfirm := attOp(ko, http.MethodPost, "/confirm", "confirm")
	confirmOther, mcpConfirmOther := attOp(km, http.MethodPost, "/confirm", "confirm")
	meta, mcpMeta := attOp(ko, http.MethodGet, "", "metadata")
	url, mcpURL := attOp(ko, http.MethodGet, "/download-url", "get_url")
	del, mcpDel := attOp(ko, http.MethodDelete, "", "delete")
	for _, op := range []twin{
		{name: "createAttachmentUpload", rest: uploadOK, mcp: mcpUploadOK},
		{name: "createAttachmentUpload over the size limit", rest: uploadBig, mcp: mcpUploadBig, want: "attachment_too_large"},
		{name: "createAttachmentUpload of an executable", rest: uploadExe, mcp: mcpUploadExe, want: "attachment_mime_blocked"},
		{name: "confirmAttachment", pending: true, rest: confirm, mcp: mcpConfirm},
		{name: "confirmAttachment by another user", pending: true, rest: confirmOther, mcp: mcpConfirmOther, want: "attachment_not_found"},
		{name: "getAttachment", pending: true, confirmed: true, rest: meta, mcp: mcpMeta},
		{name: "getAttachmentDownloadUrl", pending: true, confirmed: true, rest: url, mcp: mcpURL},
		{name: "getAttachmentDownloadUrl of a pending upload", pending: true, rest: url, mcp: mcpURL, want: "attachment_not_ready"},
		{name: "deleteAttachment", pending: true, confirmed: true, rest: del, mcp: mcpDel},
	} {
		t.Run(op.name, func(t *testing.T) {
			r, m := w.issue(op.name), w.issue(op.name)
			var ra, ma string
			if op.pending {
				ra, ma = pendingREST(r), pendingMCP(m)
				if op.confirmed {
					ready(ko, ra)
					ready(ko, ma)
				}
			}
			since := lastEventID(t, pool)
			rest, mcp := op.rest(r, ra), op.mcp(m, ma)
			parity(t, op.name, rest, mcp, op.want)
			if op.want == "" {
				if rest.status != http.StatusOK && rest.status != http.StatusCreated {
					t.Fatalf("REST answered %d %s", rest.status, rest.body)
				}
				if got, ref := w.issueEvents(r, since), w.issueEvents(m, since); len(got) != len(ref) || (len(got) > 0 && got[0] != ref[0]) {
					t.Errorf("REST wrote %v, MCP wrote %v", got, ref)
				}
			}
		})
	}

	t.Run("listAttachments", func(t *testing.T) {
		r, m := w.issue("list"), w.issue("list")
		ready(ko, pendingREST(r))
		ready(ko, pendingMCP(m))
		pendingREST(r) // an unconfirmed upload is not listed
		got := rg.rest(ko, http.MethodGet, issuePath(r, "/attachments"), nil, nil)
		wantOK(t, "listAttachments", got, http.StatusOK)
		ref := rg.tool(ko, "skein_attachments", map[string]any{"action": "list", "issueId": m})
		items, _ := got.json(t)["items"].([]any)
		refItems, _ := ref.json(t)["items"].([]any)
		if len(items) != 1 || len(items) != len(refItems) {
			t.Errorf("REST listed %d, MCP %d, want the one confirmed attachment", len(items), len(refItems))
		}
		if body := got.json(t); body["hasMore"] != false {
			t.Errorf("hasMore %v, want false", body["hasMore"])
		}
	})
}

// TestAttachments_UploadURLAlwaysAnswered — row 97 (spec «General rules»):
// Prefer: return=minimal does not shorten createAttachmentUpload — the
// answer carries the upload URL and its fields, and no Preference-Applied.
// Red when Prefer hides the URL.
func TestAttachments_UploadURLAlwaysAnswered(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	w := seedWriteWorld(t, pool)
	ko := w.key(w.owner, apikeys.LevelWrite)
	c := rg.rest(ko, http.MethodPost, issuePath(w.issue("prefer"), "/attachments"), uploadBody("note.txt", 12),
		map[string]string{"Prefer": "return=minimal"})
	wantOK(t, "createAttachmentUpload with Prefer", c, http.StatusCreated)
	body := c.json(t)
	if u, _ := body["url"].(string); u == "" {
		t.Errorf("the answer carries no upload URL: %s", c.body)
	}
	if f, _ := body["fields"].(map[string]any); len(f) == 0 {
		t.Errorf("the answer carries no upload fields: %s", c.body)
	}
	if h := c.header.Get("Preference-Applied"); h != "" {
		t.Errorf("Preference-Applied %q, want none", h)
	}
}

// TestAttachments_StorageUnavailable — row 97 (D-11). Without object
// storage the operations that need it answer 503 attachments_unavailable —
// the code MCP answers — and create nothing; listing reads metadata only
// and answers 200. Red when the unavailability answers anything but 503.
func TestAttachments_StorageUnavailable(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRigWithStorage(t, pool, nil)
	w := seedWriteWorld(t, pool)
	ko := w.key(w.owner, apikeys.LevelWrite)
	issue := w.issue("no storage")

	got := rg.rest(ko, http.MethodPost, issuePath(issue, "/attachments"), uploadBody("note.txt", 12), nil)
	ref := rg.tool(ko, "skein_attachments", map[string]any{"action": "upload_url", "issueId": issue,
		"filename": "note.txt", "contentType": "text/plain", "sizeBytes": 12})
	if got.status != http.StatusServiceUnavailable || got.code() != "attachments_unavailable" || ref.code != got.code() {
		t.Errorf("createAttachmentUpload: REST %d %s, MCP %q — want 503 attachments_unavailable on both", got.status, got.body, ref.code)
	}
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM attachments WHERE issue_id = $1`, issue).Scan(&n); err != nil || n != 0 {
		t.Errorf("attachments of the issue after the refusal: %d (%v), want 0", n, err)
	}
	if l := rg.rest(ko, http.MethodGet, issuePath(issue, "/attachments"), nil, nil); l.status != http.StatusOK {
		t.Errorf("listAttachments without storage: %d %s, want 200", l.status, l.body)
	}
}
