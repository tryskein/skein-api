package main

// Test matrix row #133 (R-1, INV-13 "one registry"): the server and the
// runner body it serves read one copy of the registry. The router is built
// twice through the registry seam of buildServingRouter (as at startup),
// each time with another version of one fixture record; each version
// carries its own mark, which the contract shows both in the MCP response
// (the warning of the general record in `usage`) and in the delivery (an
// element of the argument list of the channel program). For each version
// the mark of that version is in the response and in the channel call, and
// the mark of the other version is in neither. The commands of the response
// run as an agent runs them (prepare, preflight, the working command of its
// line); the starter, the loader and the runner body come from the same
// router through the adapter that adds the internal secret, as the proxy
// does, so the digests are those of the bytes actually served.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"skein-api/internal/auth"
	"skein-api/internal/monitorregistry"
	"skein-api/internal/testsupport"
)

const singleSourceChannelID = "single-source-host"

// singleSourceRegistry is a registry whose general record warns with mark
// and whose one host channel (step 5) passes mark as an argument of its
// program; nothing else differs between two versions.
func singleSourceRegistry(t *testing.T, mark string) []byte {
	t.Helper()
	exit0 := 0
	reg := monitorregistry.Registry{
		SchemaVersion: 1,
		General: monitorregistry.General{About: "general record", Source: "fixture",
			Warnings: []string{"Fixture version " + mark + "."}},
		Agents: []monitorregistry.Agent{},
		Channels: []monitorregistry.Channel{{
			ID: singleSourceChannelID, About: "fixture host", Source: "fixture",
			Step: monitorregistry.StepHost, Order: 1, Kind: monitorregistry.KindAddressed,
			Detect: monitorregistry.Conditions{Env: []monitorregistry.EnvCondition{{Name: "SINGLE_SOURCE_ID"}}},
			Executor: monitorregistry.Executor{Type: monitorregistry.ExecutorArgv,
				Argv: []string{"singleq", "--tag", mark, "{env:SINGLE_SOURCE_ID}"}, Feed: monitorregistry.FeedStdin,
				TimeoutSec: 30},
			Idle:           monitorregistry.Idle{Type: monitorregistry.IdleNone},
			Outcomes:       []monitorregistry.OutcomeRow{{Match: monitorregistry.Match{ExitCode: &exit0}, Outcome: monitorregistry.OutcomeQueued}},
			ProbeUndefined: true,
			Addressee:      monitorregistry.AddresseeCheck{Process: monitorregistry.ProcessNone},
			Binaries:       map[string][]string{monitorregistry.PlatformLinux: {"singleq"}},
		}},
		Shells:       monitorregistry.Shells{Source: "fixture", Names: []string{"sh", "bash"}},
		ZavodEngines: map[string]string{},
	}
	doc, err := json.Marshal(reg)
	if err != nil {
		t.Fatalf("marshal registry: %v", err)
	}
	return doc
}

func TestRegistrySingleSource_BothVersionsReachResponseAndChannel(t *testing.T) {
	if _, err := osexec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	t.Setenv("SKEIN_MONITOR_POLL_SAFETY_LAG", "1s")
	versions := []struct{ name, mark, other string }{
		{"version A", "single-mark-a-5d1e9c", "single-mark-b-8c3f27"},
		{"version B", "single-mark-b-8c3f27", "single-mark-a-5d1e9c"},
	}
	for _, v := range versions {
		t.Run(v.name, func(t *testing.T) {
			var router atomic.Value
			adapter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
				router.Load().(chi.Router).ServeHTTP(w, r)
			}))
			t.Cleanup(adapter.Close)
			s := newWiringStackWith(t, adapter.URL, singleSourceRegistry(t, v.mark))
			router.Store(s.router)

			actor, _ := testsupport.SeedUser(t, s.pool, "user")
			t.Cleanup(func() {
				_, _ = s.pool.Exec(context.Background(), `DELETE FROM events WHERE actor_user_id = $1`, actor)
			})

			minted := s.callTool(s.initialize(), "skein_get_monitor_script", map[string]any{
				"id": s.root, "client": "single-source-agent", "platform": "posix",
				"eventTypes": []string{"issue.status_changed"}})
			if !strings.Contains(minted[0], v.mark) {
				t.Fatalf("the response does not carry the mark of its version %s:\n%s", v.mark, minted[0])
			}
			if strings.Contains(minted[0], v.other) {
				t.Fatalf("the response carries the mark of the other version %s", v.other)
			}
			var mint struct {
				MonitorID     string `json:"monitorId"`
				Token         string `json:"token"`
				TokenFilePath string `json:"tokenFilePath"`
				Prepare       string `json:"prepare"`
				Preflight     string `json:"preflight"`
			}
			if err := json.Unmarshal([]byte(minted[0]), &mint); err != nil {
				t.Fatalf("mint response: %v\n%s", err, minted[0])
			}
			monitorID := uuid.MustParse(mint.MonitorID)
			t.Cleanup(func() {
				_, _ = s.pool.Exec(context.Background(), `DELETE FROM monitor_credentials WHERE id = $1`, monitorID)
				deadline := time.Now().Add(30 * time.Second)
				for time.Now().Before(deadline) {
					if _, err := os.Stat(filepath.Dir(mint.TokenFilePath)); os.IsNotExist(err) {
						return
					}
					time.Sleep(100 * time.Millisecond)
				}
			})

			home := t.TempDir()
			stateHome := filepath.Join(home, "state")
			bin := filepath.Join(home, "bin")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatalf("bin: %v", err)
			}
			calls := filepath.Join(home, "calls")
			fake := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' \"$*\" >> '" + calls + "'\nexit 0\n"
			if err := os.WriteFile(filepath.Join(bin, "singleq"), []byte(fake), 0o700); err != nil {
				t.Fatalf("fake channel: %v", err)
			}
			var env []string
			for _, kv := range os.Environ() {
				if !strings.HasPrefix(kv, "SKEIN_") && !strings.HasPrefix(kv, "PATH=") &&
					!strings.HasPrefix(kv, "XDG_STATE_HOME=") && !strings.HasPrefix(kv, "ZAVOD_") {
					env = append(env, kv)
				}
			}
			env = append(env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"XDG_STATE_HOME="+stateHome, "SINGLE_SOURCE_ID=host-133")

			if lines := wiringShell(t, mint.Prepare, env); len(lines) != 1 || lines[0]["event"] != "prepare_ready" {
				t.Fatalf("prepare = %v", lines)
			}
			if err := os.WriteFile(mint.TokenFilePath, []byte(mint.Token), 0o644); err != nil {
				t.Fatalf("write token: %v", err)
			}
			lines := wiringShell(t, mint.Preflight, env)
			pre := lines[len(lines)-1]
			if pre["event"] != "preflight" || pre["method"] != "detached" || pre["channel"] != singleSourceChannelID {
				t.Fatalf("preflight line = %v", pre)
			}
			command, _ := pre["command"].(string)
			started := wiringShell(t, command, env)
			if last := started[len(started)-1]; last["event"] != "monitor_started" {
				t.Fatalf("working command = %v", started)
			}

			polled := false
			for wait := time.Now().Add(60 * time.Second); !polled && time.Now().Before(wait); {
				if err := s.pool.QueryRow(context.Background(),
					`SELECT last_poll_succeeded_at IS NOT NULL FROM monitor_credentials WHERE id = $1`, monitorID).Scan(&polled); err != nil {
					t.Fatalf("read the poll stamp: %v", err)
				}
				if !polled {
					time.Sleep(100 * time.Millisecond)
				}
			}
			if !polled {
				t.Fatalf("the detached runner never polled")
			}
			s.exec(`INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                    actor, actor_user_id, actor_session_id, actor_type, payload)
VALUES (now(), 'issue.status_changed', 'issue', $1, $2, 'tester', $3, NULL, 'human', '{}'::jsonb)`,
				s.root, s.project, actor)

			var raw []byte
			deadline := time.Now().Add(90 * time.Second)
			for time.Now().Before(deadline) {
				raw, _ = os.ReadFile(calls)
				if len(raw) > 0 {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			call := strings.TrimSpace(string(raw))
			if call == "" {
				t.Fatalf("the channel was never called; journal: %v", wiringJournal(stateHome))
			}
			if want := "--tag " + v.mark + " host-133"; call != want {
				t.Fatalf("channel call = %q, want %q", call, want)
			}
			if strings.Contains(call, v.other) {
				t.Fatalf("the channel call carries the mark of the other version: %q", call)
			}
		})
	}
}
