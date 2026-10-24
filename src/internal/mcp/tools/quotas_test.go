package tools

// quotas_test.go — sk-674.18 T026, matrix rows 110 and 111, the MCP half
// (REST — handler/quotas_test.go). Reference — SC-012 and INV-15 of the
// spec: a quota refusal is the same machine code in REST and MCP —
// project_quota for a project over the space's project quota (archived
// counted), storage_quota for a confirmation while the space's used bytes
// are at or over its storage quota.

import (
	"context"
	"strconv"
	"testing"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/attachments"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

func TestQuotas_MCPRefusesWithTheRESTCodes(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	me, _ := seedUser(t, pool, "user")
	space := testsupport.SeedSpace(t, pool, me)
	var short string
	if err := pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, space).Scan(&short); err != nil {
		t.Fatal(err)
	}

	t.Run("project_quota", func(t *testing.T) {
		store := projects.NewStore(pool)
		one := 1
		store.SetProjectQuotas(nil, &one)
		archived, _ := testsupport.SeedProjectInSpace(t, pool, me, space, "qmcp")
		if _, err := pool.Exec(ctx, `UPDATE projects SET archived_at = now() WHERE id = $1`, archived); err != nil {
			t.Fatal(err)
		}
		req := mcpgo.CallToolRequest{}
		prefix := "qm" + uuid.NewString()[:6]
		req.Params.Arguments = map[string]any{"name": prefix, "idPrefix": prefix, "space": short}
		res, err := handleProjectCreate(pool, store, "members")(withCtx(t, me, "user"), req)
		if err != nil {
			t.Fatal(err)
		}
		if body := extractToolJSON(t, res); !res.IsError || body["error"] != "project_quota" {
			t.Errorf("a project over the quota of 1 (one archived): %v, want project_quota", body)
		}
	})

	t.Run("storage_quota", func(t *testing.T) {
		project, prefix := testsupport.SeedProjectInSpace(t, pool, me, space, "qmst")
		issue := testsupport.SeedIssue(t, pool, project, me, prefix+"-1", "quota", "open", 2)
		store := attachments.NewStore(pool)
		quota := int64(100)
		store.SetStorageQuotas(nil, &quota)
		confirm := func(size int64) map[string]any {
			t.Helper()
			id := testsupport.SeedAttachment(t, pool, issue, me, "pending")
			if _, err := pool.Exec(ctx, `UPDATE attachments SET size_bytes = $2 WHERE id = $1`, id, size); err != nil {
				t.Fatal(err)
			}
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{"attachmentId": id.String()}
			res, err := handleAttachmentConfirm(attachmentDeps{pool: pool, store: store, s3: &fakeS3{},
				bucket: "skein-attachments", maxSize: 1 << 30})(withCtx(t, me, "user"), req)
			if err != nil {
				t.Fatal(err)
			}
			body := extractToolJSON(t, res)
			if !res.IsError {
				return nil
			}
			return body
		}
		if body := confirm(150); body != nil {
			t.Fatalf("0 of 100 used: %v", body)
		}
		if body := confirm(1); body == nil || body["error"] != "storage_quota" {
			t.Errorf("150 of 100 used: %v, want storage_quota", body)
		}
	})
}

// TestQuotas_MCPStorageRefusalToAnExternalCarriesNoAggregates — review
// api/6 #2 (codex G-C2), the MCP half: INV-28 (3) gives the aggregates of a
// space to its participants only; an external user's storage_quota carries
// no spaceId and no byte counts, a participant's carries them.
func TestQuotas_MCPStorageRefusalToAnExternalCarriesNoAggregates(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	owner, _ := seedUser(t, pool, "user")
	participant, _ := seedUser(t, pool, "user")
	external, _ := seedUser(t, pool, "user")
	if _, err := pool.Exec(ctx, `UPDATE users SET is_external = true WHERE id = $1`, external); err != nil {
		t.Fatal(err)
	}
	space := testsupport.SeedSpace(t, pool, owner)
	testsupport.AddSpaceMember(t, pool, space, participant)
	shared, sharedPrefix := testsupport.SeedProjectInSpace(t, pool, owner, space, "qmx")
	hidden, hiddenPrefix := testsupport.SeedProjectInSpace(t, pool, owner, space, "qmh")
	testsupport.AddProjectMember(t, pool, shared, external, "member")
	testsupport.AddProjectMember(t, pool, shared, participant, "member")
	store := attachments.NewStore(pool)
	quota := int64(100)
	store.SetStorageQuotas(nil, &quota)
	n := 0
	confirm := func(who uuid.UUID, project uuid.UUID, prefix string, size int64) (bool, map[string]any) {
		t.Helper()
		n++
		issue := testsupport.SeedIssue(t, pool, project, owner, prefix+"-"+strconv.Itoa(n), "quota", "open", 2)
		id := testsupport.SeedAttachment(t, pool, issue, who, "pending")
		if _, err := pool.Exec(ctx, `UPDATE attachments SET size_bytes = $2 WHERE id = $1`, id, size); err != nil {
			t.Fatal(err)
		}
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{"attachmentId": id.String()}
		res, err := handleAttachmentConfirm(attachmentDeps{pool: pool, store: store, s3: &fakeS3{},
			bucket: "skein-attachments", maxSize: 1 << 30})(withCtx(t, who, "user"), req)
		if err != nil {
			t.Fatal(err)
		}
		return res.IsError, extractToolJSON(t, res)
	}
	if isErr, body := confirm(owner, hidden, hiddenPrefix, 150); isErr {
		t.Fatalf("filling the quota: %v", body)
	}
	for _, c := range []struct {
		who      uuid.UUID
		name     string
		disclose bool
	}{{external, "the external user", false}, {participant, "the participant", true}} {
		isErr, body := confirm(c.who, shared, sharedPrefix, 1)
		if !isErr || body["error"] != "storage_quota" {
			t.Fatalf("%s: %v, want storage_quota", c.name, body)
		}
		_, used := body["storageUsedBytes"]
		_, quota := body["storageQuotaBytes"]
		_, space := body["spaceId"]
		if c.disclose != used || c.disclose != quota || c.disclose != space {
			t.Errorf("%s: %v — aggregates disclosed %v, %v, %v; want %v", c.name, body, used, quota, space, c.disclose)
		}
	}
}
