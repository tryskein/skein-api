package handler

// hub_access_isolation_test.go — sk-674.18 T005, matrix row 33 (SC-003,
// US-19): the hub batch (POST /api/v1/hub/project-access) answers, for every
// actor of row 22, allowed/denied over every fixture project and the set of
// visible projects, equal to the legend (testsupport.TenancyVisible).
//
// One batch carries every actor with every fixture project requested and
// allVisible set, which is how the hub asks; the handler is the production
// one (the route and its service-secret gate are covered by
// cmd/api/hub_access_route_test.go).
//
// Every cell is asserted since T017 (it closed the last two: an admin on
// another person's "Personal", and an admin's visibleProjectIds — all
// non-personal + own "Personal", never another person's).
//
// The deactivated actor IS checkable here: the batch reads deactivated_at.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

func TestHubAccessIsolation_BatchByActor(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	f := testsupport.SeedTenancy(t, pool)
	fixture := f.Projects()

	var projectIDs []uuid.UUID
	for _, p := range fixture {
		projectIDs = append(projectIDs, p.ID)
	}
	req := projects.ProjectAccessRequest{}
	for _, a := range testsupport.TenancyActors {
		req.Users = append(req.Users, projects.ProjectAccessQuery{
			UserID: f.User(t, a).ID, ProjectIDs: projectIDs, AllVisible: true,
		})
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/hub/project-access", bytes.NewReader(raw))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewHubAccessHandler(pool).ProjectAccess(rec, httpReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("hub batch: %d %s", rec.Code, rec.Body.String())
	}
	var resp projects.ProjectAccessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Users) != len(testsupport.TenancyActors) {
		t.Fatalf("answers for %d users, want %d", len(resp.Users), len(testsupport.TenancyActors))
	}

	for i, a := range testsupport.TenancyActors {
		res := resp.Users[i]
		t.Run(string(a), func(t *testing.T) {
			u := f.User(t, a)
			if res.UserID != u.ID {
				t.Fatalf("answer %d is for %s, want %s", i, res.UserID, u.ID)
			}
			if res.Active == u.Deactivated {
				t.Errorf("active = %v for a user deactivated = %v", res.Active, u.Deactivated)
			}
			visible := map[uuid.UUID]bool{}
			for _, id := range res.VisibleProjectIDs {
				visible[id] = true
			}
			for _, p := range fixture {
				t.Run(p.Key(), func(t *testing.T) {
					sees := f.Sees(a, p)
					allowed := slices.Contains(res.Allowed, p.ID)
					denied := slices.Contains(res.Denied, p.ID)
					if allowed == denied {
						t.Fatalf("project is allowed=%v denied=%v — exactly one expected", allowed, denied)
					}
					if allowed != sees {
						t.Errorf("allowed = %v, want %v", allowed, sees)
					}
					if visible[p.ID] != sees {
						t.Errorf("in visibleProjectIds = %v, want %v", visible[p.ID], sees)
					}
				})
			}
		})
	}
}
