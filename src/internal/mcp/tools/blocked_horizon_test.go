package tools

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

// sk-423 through MCP: skein_show (flag, blockers[], unavailableBlockingLinks)
// and skein_prime (the isBlocked of in-progress rows and membership of the
// «blocked» section). For a reader outside the blocker's project nothing
// here may depend on whether the hidden blocker is closed. On the code
// before sk-423 the open case reads «blocked».

var bhMCPStatuses = []string{"open", "in_progress", "deferred", "closed", "standing"}

func bhMCPPick(m map[string]any, keys ...string) string {
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		} else {
			out[k] = "<absent>"
		}
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}

func bhMCPIndistinguishable(t *testing.T, surface string, project func(status string) string) {
	t.Helper()
	ref := project(bhMCPStatuses[0])
	for _, st := range bhMCPStatuses[1:] {
		if got := project(st); got != ref {
			t.Errorf("%s: hidden blocker open answers %s, hidden blocker %s answers %s — "+
				"the response reveals the hidden issue's status", surface, ref, st, got)
		}
	}
}

func TestBlockedHorizonMCP_ShowAndPrime(t *testing.T) {
	pool := newTestPool(t)
	reader, readerUN := testsupport.SeedUser(t, pool, "user")
	qOwner, _ := testsupport.SeedUser(t, pool, "user")
	pID, pPrefix := testsupport.SeedProject(t, pool, reader, "bhmp")
	qID, qPrefix := testsupport.SeedProject(t, pool, qOwner, "bhmq")
	ext, extUN := testsupport.SeedUser(t, pool, "user")
	testsupport.AddProjectMember(t, pool, pID, ext, "member")

	// One in_progress subject per hidden status for each reader, assigned
	// to that reader so skein_prime lists it.
	type subjects map[string]string
	seed := func(assignee string, base int) subjects {
		out := subjects{}
		for k, st := range bhMCPStatuses {
			subject := testsupport.SeedIssue(t, pool, pID, reader, fmt.Sprintf("%s-%d", pPrefix, base+k), "bh subject", "in_progress", 2)
			testsupport.SetAssignee(t, pool, subject, assignee)
			blocker := testsupport.SeedIssue(t, pool, qID, qOwner, fmt.Sprintf("%s-%d", qPrefix, base+k), "bh hidden blocker", st, 2)
			testsupport.SeedDep(t, pool, subject, blocker, dependencies.DepBlocks, reader)
			out[st] = subject
		}
		return out
	}

	readers := []struct {
		name     string
		user     uuid.UUID
		subjects subjects
		external bool
	}{
		{name: "member of P, not of Q", user: reader, subjects: seed(readerUN, 10)},
		{name: "external member of P", user: ext, subjects: seed(extUN, 30), external: true},
	}

	show := handleShow(pool, issues.NewStore(pool), dependencies.NewStore(pool))
	prime := handlePrime(pool, issues.NewStore(pool), dependencies.NewStore(pool), projects.NewStore(pool))

	for _, r := range readers {
		t.Run(r.name, func(t *testing.T) {
			ctx := withCtx(t, r.user, "user")
			if r.external {
				ctx = auth.WithIsExternal(ctx, true)
			}

			bhMCPIndistinguishable(t, "skein_show", func(st string) string {
				body := callShow(t, show, ctx, map[string]any{"id": r.subjects[st], "includeComments": false})
				issue, ok := body["issue"].(map[string]any)
				if !ok {
					t.Fatalf("issue missing: %v", body)
				}
				return bhMCPPick(issue, "isBlocked", "blockers", "unavailableBlockingLinks")
			})

			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{}
			res, err := prime(ctx, req)
			if err != nil || res.IsError {
				t.Fatalf("skein_prime: err=%v res=%+v", err, res)
			}
			body := extractToolJSON(t, res)
			inProgress := map[string]map[string]any{}
			for _, row := range primeSection(t, body, "myInProgress") {
				inProgress[row["id"].(string)] = row
			}
			blocked := map[string]bool{}
			for _, row := range primeSection(t, body, "blocked") {
				blocked[row["id"].(string)] = true
			}
			bhMCPIndistinguishable(t, "skein_prime", func(st string) string {
				id := r.subjects[st]
				row, ok := inProgress[id]
				if !ok {
					t.Fatalf("subject %s missing from myInProgress", id)
				}
				return fmt.Sprintf("%s blockedSection=%v", bhMCPPick(row, "isBlocked"), blocked[id])
			})
		})
	}
}
