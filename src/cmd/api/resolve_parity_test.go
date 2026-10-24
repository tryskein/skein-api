package main

// resolve_parity_test.go — sk-674.18 T024, matrix row 102 (SC-024).
// Reference — SC-024 of the spec, written before the code: the resolution
// of a project named by a string is the same on every REST and MCP path
// that takes one (issue creation, project edit, members, components,
// watching, ready, list and search, the project search itself): two
// accessible same-named projects are the «ambiguous» refusal with the same
// candidates everywhere; a string that is the prefix of one accessible
// project and the name of another names the prefix's project everywhere.
// A path resolving with its own code answers differently and is red here.
//
// REST takes a project by string on GET /projects/resolve only (the other
// routes take the project id in the path); it is asked alongside.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/testsupport"
)

func TestResolveParity_OneOutcomeOnEveryPath(t *testing.T) {
	s := newParityStack(t)
	ctx := context.Background()
	a := testsupport.ActorProjectOwner
	me := s.f.User(t, a)
	uniq := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	spaceX := testsupport.SeedSpace(t, s.pool, me.ID)
	spaceY := testsupport.SeedSpace(t, s.pool, me.ID)
	named := func(space uuid.UUID, name string) (uuid.UUID, string) {
		t.Helper()
		id, prefix := testsupport.SeedProjectInSpace(t, s.pool, me.ID, space, "r102")
		if _, err := s.pool.Exec(ctx, `UPDATE projects SET name = $2 WHERE id = $1`, id, name); err != nil {
			t.Fatal(err)
		}
		return id, prefix
	}

	// Two accessible projects named alike, in two spaces.
	ambName := "r102-same-" + uniq
	_, ambX := named(spaceX, ambName)
	_, ambY := named(spaceY, ambName)
	wantCandidates := []string{ambX, ambY}
	sort.Strings(wantCandidates)

	// The prefix of one accessible project is the name of another.
	target, targetPrefix := named(spaceX, "r102-target-"+uniq)
	holder, _ := named(spaceY, targetPrefix)
	// A member only the target has: the member list tells the two apart.
	only, _ := testsupport.SeedUser(t, s.pool, "user")
	testsupport.AddSpaceMember(t, s.pool, spaceX, only)
	if _, err := s.pool.Exec(ctx, `INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)`, target, only, me.ID); err != nil {
		t.Fatal(err)
	}
	var onlyName string
	if err := s.pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, only).Scan(&onlyName); err != nil {
		t.Fatal(err)
	}

	sid := s.session(a)
	// owns answers whether project holds a row of the sql (by project id).
	owns := func(project uuid.UUID, sql string, args ...any) bool {
		var ok bool
		if err := s.pool.QueryRow(ctx, sql, append([]any{project}, args...)...).Scan(&ok); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return ok
	}
	issueID := ""
	// Each path: the MCP call for the string q, and — for the prefix string —
	// the check that it acted on the target and not on the holder.
	paths := []struct {
		name   string
		call   func(q string) toolResult
		target func(t *testing.T, r toolResult)
	}{
		{"skein_project_resolve", func(q string) toolResult {
			return s.tool(a, sid, "skein_project_resolve", map[string]any{"q": q})
		}, func(t *testing.T, r toolResult) {
			if !strings.Contains(r.text, target.String()) {
				t.Errorf("matched another project: %s", r.text)
			}
		}},
		{"skein_create", func(q string) toolResult {
			return s.tool(a, sid, "skein_create", map[string]any{"project": q, "title": "row 102 " + uniq, "type": "task", "kind": "directed"})
		}, func(t *testing.T, r toolResult) {
			// The issue id is <prefix>-<n> of the project it was created in.
			mark := `"id":"` + targetPrefix + "-"
			i := strings.Index(r.text, mark)
			if i < 0 {
				t.Fatalf("created outside %s: %s", targetPrefix, r.text)
			}
			rest := r.text[i+len(`"id":"`):]
			issueID = rest[:strings.Index(rest, `"`)]
		}},
		{"skein_list", func(q string) toolResult {
			return s.tool(a, sid, "skein_list", map[string]any{"project": q})
		}, func(t *testing.T, r toolResult) {
			if !strings.Contains(r.text, `"`+issueID+`"`) {
				t.Errorf("listed another project: %s", r.text)
			}
		}},
		{"skein_ready", func(q string) toolResult {
			return s.tool(a, sid, "skein_ready", map[string]any{"project": q})
		}, func(t *testing.T, r toolResult) {
			if !strings.Contains(r.text, `"`+issueID+`"`) {
				t.Errorf("ready of another project: %s", r.text)
			}
		}},
		{"skein_search", func(q string) toolResult {
			return s.tool(a, sid, "skein_search", map[string]any{"query": "row 102 " + uniq, "project": q})
		}, func(t *testing.T, r toolResult) {
			if !strings.Contains(r.text, `"`+issueID+`"`) {
				t.Errorf("searched another project: %s", r.text)
			}
		}},
		{"skein_project_update", func(q string) toolResult {
			return s.tool(a, sid, "skein_project_update", map[string]any{"project": q, "description": "row 102 " + uniq})
		}, func(t *testing.T, r toolResult) {
			if !owns(target, `SELECT description = $2 FROM projects WHERE id = $1`, "row 102 "+uniq) ||
				owns(holder, `SELECT description = $2 FROM projects WHERE id = $1`, "row 102 "+uniq) {
				t.Error("edited another project")
			}
		}},
		{"skein_project_members list", func(q string) toolResult {
			return s.tool(a, sid, "skein_project_members", map[string]any{"action": "list", "project": q})
		}, func(t *testing.T, r toolResult) {
			if !strings.Contains(r.text, onlyName) {
				t.Errorf("listed the members of another project: %s", r.text)
			}
		}},
		{"skein_component create", func(q string) toolResult {
			return s.tool(a, sid, "skein_component", map[string]any{"action": "create", "project": q, "name": "c102" + uniq})
		}, func(t *testing.T, r toolResult) {
			if !owns(target, `SELECT EXISTS (SELECT 1 FROM components WHERE project_id = $1 AND name = $2)`, "c102"+uniq) {
				t.Error("created the component in another project")
			}
		}},
		{"skein_watchers project_watch", func(q string) toolResult {
			return s.tool(a, sid, "skein_watchers", map[string]any{"action": "project_watch", "project": q})
		}, func(t *testing.T, r toolResult) {
			if !owns(target, `SELECT EXISTS (SELECT 1 FROM project_watchers WHERE project_id = $1 AND user_id = $2)`, me.ID) ||
				owns(holder, `SELECT EXISTS (SELECT 1 FROM project_watchers WHERE project_id = $1 AND user_id = $2)`, me.ID) {
				t.Error("watched another project")
			}
		}},
	}

	t.Run("two accessible same-named projects: ambiguous everywhere", func(t *testing.T) {
		code, raw := s.rest(a, http.MethodGet, "/api/v1/projects/resolve?q="+url.QueryEscape(ambName), nil)
		if code != http.StatusUnprocessableEntity {
			t.Errorf("REST: %d %s, want 422", code, raw)
		}
		if got := ambiguity(t, raw); got != fmt.Sprint(wantCandidates) {
			t.Errorf("REST: %s, want ambiguous_project %v", got, wantCandidates)
		}
		for _, p := range paths {
			r := p.call(ambName)
			if !r.isError {
				t.Errorf("%s: acted on %q: %s", p.name, ambName, r.text)
				continue
			}
			if got := ambiguity(t, []byte(r.text)); got != fmt.Sprint(wantCandidates) {
				t.Errorf("%s: %s, want ambiguous_project %v", p.name, got, wantCandidates)
			}
		}
	})

	t.Run("a prefix that is another's name: the prefix's project everywhere", func(t *testing.T) {
		code, raw := s.rest(a, http.MethodGet, "/api/v1/projects/resolve?q="+url.QueryEscape(targetPrefix), nil)
		if code != http.StatusOK || !strings.Contains(string(raw), `"id":"`+target.String()+`"`) {
			t.Errorf("REST: %d %s, want the match %s", code, raw, target)
		}
		for _, p := range paths {
			r := p.call(targetPrefix)
			if r.isError {
				t.Errorf("%s: refused: %s", p.name, r.text)
				continue
			}
			t.Run(p.name, func(t *testing.T) { p.target(t, r) })
		}
	})
}

// ambiguity renders a refusal as "code [sorted candidate prefixes]".
func ambiguity(t *testing.T, raw []byte) string {
	t.Helper()
	var body struct {
		Error      string `json:"error"`
		Candidates []struct {
			IDPrefix string `json:"idPrefix"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Sprintf("undecodable %s", raw)
	}
	var prefixes []string
	for _, c := range body.Candidates {
		prefixes = append(prefixes, c.IDPrefix)
	}
	sort.Strings(prefixes)
	if body.Error != "ambiguous_project" {
		return fmt.Sprintf("%s %v", body.Error, prefixes)
	}
	return fmt.Sprint(prefixes)
}
