package tools

// Test matrix rows #82 and #127: the boundary "MCP response -> commands ->
// body" on the named path, against the embedded registry.
//
// The response comes from handleGetMonitorScript with the registry the
// server embeds; its commands run with sh as an agent runs them; the
// starter downloads the loader the route serves (NewAgentLoaderHandler)
// and the loader the runner body the script handler serves
// (NewAgentScriptHandler), both digests taken from those very bytes. The
// poll server is fake. Channel programs are fake programs on PATH that
// record how they were called.

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/handler"
	"skein-api/internal/monitorregistry"
)

// namedPathZavodUUID is the Zavod address of the fake agent.
const namedPathZavodUUID = "2f4e6a8c-0b1d-4e3f-a5b7-c9d1e3f5a7b9"

// namedPathWorld is a poll and loader server with the served bytes, and the
// handler of the tool over the embedded registry pointed at that server.
type namedPathWorld struct {
	srv     *bootstrapServer
	handler func(t *testing.T, args map[string]any) map[string]any
}

func newNamedPathWorld(t *testing.T) *namedPathWorld {
	t.Helper()
	for _, prog := range []string{"sh", "python3"} {
		if _, err := exec.LookPath(prog); err != nil {
			t.Skipf("%s not available", prog)
		}
	}
	rec := httptest.NewRecorder()
	script := handler.NewAgentScriptHandler(1, 10*time.Second, 15*time.Second, 3*time.Minute, 6*time.Hour)
	script.Script(rec, httptest.NewRequest(http.MethodGet, "/agent/v1/monitor-script", nil))
	loader := handler.NewAgentLoaderHandler(ServedLoader())
	srv := newBootstrapServer(t, loader, rec.Body.Bytes())
	reg := monitorregistry.MustEmbedded()
	h := handleGetMonitorScript(&fakeMonitorIssues{}, &fakeMonitorWatcher{}, &fakeMonitorMinter{}, reg,
		srv.srv.URL, script.SHA256(), loader.SHA256())
	ctx := newGMSUnit(reg).ctx
	return &namedPathWorld{srv: srv, handler: func(t *testing.T, args map[string]any) map[string]any {
		t.Helper()
		req := callToolRequest(args)
		res, err := h(ctx, req)
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		return extractToolJSON(t, res)
	}}
}

func callToolRequest(args map[string]any) (req mcpgo.CallToolRequest) {
	req.Params.Arguments = args
	return req
}

// namedPathBin is a directory of fake programs; each records its argument
// list and stdin under log/<name>.
type namedPathBin struct {
	dir, log string
}

func newNamedPathBin(t *testing.T) *namedPathBin {
	t.Helper()
	root := t.TempDir()
	b := &namedPathBin{dir: filepath.Join(root, "bin"), log: filepath.Join(root, "log")}
	for _, d := range []string{b.dir, b.log} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// marker writes a program that only records that it ran.
func (b *namedPathBin) marker(t *testing.T, name string) {
	t.Helper()
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + filepath.Join(b.log, name) + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(b.dir, name), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

// zavod writes a fake zavod CLI: `version` answers like the daemon is up;
// `send` records its arguments and its stdin and answers accepted (the
// envelope of the CLI: exit_code and code at the top level).
func (b *namedPathBin) zavod(t *testing.T) {
	t.Helper()
	body := "#!/bin/sh\n" +
		"if [ \"$2\" = version ]; then echo '{\"zavod\":1,\"command\":\"version\",\"outcome\":\"ok\",\"exit_code\":0,\"code\":null}'; exit 0; fi\n" +
		"printf '%s\\n' \"$*\" >> '" + filepath.Join(b.log, "zavod.args") + "'\n" +
		"cat >> '" + filepath.Join(b.log, "zavod.stdin") + "'\n" +
		"echo '{\"zavod\":1,\"command\":\"send\",\"outcome\":\"ok\",\"exit_code\":0,\"code\":null}'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(b.dir, "zavod"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func (b *namedPathBin) read(name string) string {
	raw, _ := os.ReadFile(filepath.Join(b.log, name))
	return string(raw)
}

// prepareToken runs the prepare command of a response and writes its token
// the way the Write tool does; the token directory goes at the end.
func prepareToken(t *testing.T, body map[string]any, env []string) string {
	t.Helper()
	tokenFile, _ := body["tokenFilePath"].(string)
	prepare, _ := body["prepare"].(string)
	token, _ := body["token"].(string)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(tokenFile)) })
	cmd := exec.Command("sh", "-c", prepare)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("prepare: %v: %s", err, out)
	}
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatalf("token: %v", err)
	}
	return tokenFile
}

// cleanEnv is the environment of the test process without the variables a
// test sets itself.
func cleanEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "SKEIN_") || strings.HasPrefix(kv, "ZAVOD_") || strings.HasPrefix(kv, "TMUX") ||
			strings.HasPrefix(kv, "CODEX_") || strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "HOME=") ||
			strings.HasPrefix(kv, "XDG_STATE_HOME=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestMonitorNamedPath_WithoutClientIgnoresTheEnvironment — row #82 (SC-002
// of the epic "a call without a name under ZAVOD_ME and TMUX", SC-004): the
// response without `client` has the fields it had before; its prepare and
// bootstrap, run with ZAVOD_ME, TMUX, every new SKEIN_* variable and fake
// zavod, tmux and codex programs on PATH, start no external program; every
// event is a notification line on stdout.
func TestMonitorNamedPath_WithoutClientIgnoresTheEnvironment(t *testing.T) {
	w := newNamedPathWorld(t)
	body := w.handler(t, map[string]any{"id": "sk-42"})
	var keys []string
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"bootstrap", "eventTypes", "issueId", "monitorId", "prepare", "subtree", "token",
		"tokenFilePath", "tokenInstruction", "usage", "watching"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("response fields = %v, want the fields of a call without client %v", keys, want)
	}
	bin := newNamedPathBin(t)
	for _, prog := range []string{"zavod", "tmux", "codex"} {
		bin.marker(t, prog)
	}
	home := t.TempDir()
	env := cleanEnv("PATH="+bin.dir+string(os.PathListSeparator)+os.Getenv("PATH"), "HOME="+home,
		"XDG_STATE_HOME="+filepath.Join(home, "state"),
		"ZAVOD_ME="+namedPathZavodUUID, "ZAVOD_AGENT_ID="+namedPathZavodUUID, "ZAVOD_AGENT_ENGINE_ID=claude",
		"TMUX=/tmp/tmux-1000/default,1,0", "TMUX_PANE=%3", "CODEX_THREAD_ID=019a8e3c-0000-7000-8000-000000000082",
		"SKEIN_CLIENT=claude", "SKEIN_PLATFORM=posix", "SKEIN_MONITOR_MODE=preflight",
		"SKEIN_MONITOR_CHANNEL=zavod", "SKEIN_MONITOR_BLIND=off", "SKEIN_MONITOR_PREFLIGHT=1",
		"SKEIN_LOADER_SHA256="+strings.Repeat("0", 64))
	prepareToken(t, body, env)
	bootstrap, _ := body["bootstrap"].(string)

	cmd := exec.Command("sh", "-c", bootstrap)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	lines := make(chan map[string]any, 100)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var obj map[string]any
			if json.Unmarshal(sc.Bytes(), &obj) != nil {
				obj = map[string]any{"__not_json": sc.Text()}
			}
			lines <- obj
		}
		close(lines)
	}()
	next := func() map[string]any {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("the bootstrap ended")
			}
			return l
		case <-time.After(30 * time.Second):
			t.Fatalf("no line from the bootstrap")
		}
		return nil
	}
	if l := next(); l["event"] != "monitor_started" {
		t.Fatalf("first line = %v, want monitor_started", l)
	}
	select {
	case <-w.srv.polled:
	case <-time.After(30 * time.Second):
		t.Fatalf("the runner never polled")
	}
	id := w.srv.push()
	l := next()
	n, _ := l["notification"].(map[string]any)
	if l["event"] != "notification" || n["id"] != float64(id) {
		t.Fatalf("line = %v, want notification %d", l, id)
	}
	for _, prog := range []string{"zavod", "tmux", "codex"} {
		if got := bin.read(prog); got != "" {
			t.Fatalf("the call without client ran %s: %q", prog, got)
		}
	}
}

// startAgent runs script under a fake agent program named name: a shell
// script whose own process (its name is the program's) stays alive after
// script ends and stands for the agent process; it exports its pid as the
// root process of the Zavod agent.
func startAgent(t *testing.T, bin *namedPathBin, name, script string, env []string) {
	t.Helper()
	wrapper := "#!/bin/sh\nZAVOD_AGENT_ROOT_PID=$$\nexport ZAVOD_AGENT_ROOT_PID\n/bin/sh \"$1\"\nexec sleep 300\n"
	path := filepath.Join(bin.dir, name)
	if err := os.WriteFile(path, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(bin.log, name+".script.sh")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, file)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
}

// lastLine is the last JSON line of a file, or nil.
func lastLine(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var last map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var obj map[string]any
		if json.Unmarshal([]byte(l), &obj) == nil {
			last = obj
		}
	}
	return last
}

// TestMonitorNamedPath_ZavodRecordFromResponseToChannel — row #127: the
// named path of the real Zavod record. client=codex without its own method
// (no CODEX_THREAD_ID) and a Zavod engine that maps to codex: the response
// names the record, its preflight command (run by the agent) prints a
// preflight line on channel zavod, the working command of that line starts
// the detached runner, and the fake zavod gets exactly the event, with the
// argument list and stdin of the CLI form `zavod --json send <ZAVOD_ME>
// --text-file -`. Control of the order: client=claude with the same Zavod
// (engine claude) and its stream available takes stdout and never sends.
func TestMonitorNamedPath_ZavodRecordFromResponseToChannel(t *testing.T) {
	run := func(t *testing.T, client, engine string) (*namedPathWorld, *namedPathBin, string) {
		w := newNamedPathWorld(t)
		body := w.handler(t, map[string]any{"id": "sk-42", "client": client})
		if body["client"] != client {
			t.Fatalf("the response resolved client %q as %v", client, body["client"])
		}
		bin := newNamedPathBin(t)
		bin.zavod(t)
		home := t.TempDir()
		env := cleanEnv("PATH="+bin.dir+string(os.PathListSeparator)+os.Getenv("PATH"), "HOME="+home,
			"XDG_STATE_HOME="+filepath.Join(home, "state"),
			"ZAVOD_ME="+namedPathZavodUUID, "ZAVOD_AGENT_ID="+namedPathZavodUUID, "ZAVOD_AGENT_ENGINE_ID="+engine,
			"ZAVOD_AGENT_ROOT_START=1", "ZAVOD_AGENT_ROOT_BOOT_ID=boot-127")
		prepareToken(t, body, env)
		preflight, _ := body["preflight"].(string)
		pre := filepath.Join(bin.log, "preflight.out")
		started := filepath.Join(bin.log, "run.out")
		script := "{ " + preflight + "; } > '" + pre + "' 2>&1\n" +
			"cmd=$(python3 -c 'import json,sys; print(json.loads(open(sys.argv[1]).read().strip().splitlines()[-1])[\"command\"])' '" + pre + "')\n" +
			"sh -c \"$cmd\" > '" + started + "' 2>&1\n"
		agent := map[string]string{"codex": "codex", "claude": "claude"}[client]
		startAgent(t, bin, agent, script, env)
		waitFor(t, "the preflight line", func() bool {
			l := lastLine(pre)
			return l != nil && (l["event"] == "preflight" || l["reason"] != nil)
		})
		return w, bin, started
	}

	t.Run("codex woken by zavod", func(t *testing.T) {
		w, bin, started := run(t, "codex", "codex")
		pre := lastLine(filepath.Join(bin.log, "preflight.out"))
		if pre["event"] != "preflight" || pre["client"] != "codex" || pre["method"] != "detached" || pre["channel"] != "zavod" {
			t.Fatalf("preflight line = %v, want record codex detached on zavod", pre)
		}
		waitFor(t, "monitor_started", func() bool {
			l := lastLine(started)
			return l != nil && l["event"] == "monitor_started"
		})
		select {
		case <-w.srv.polled:
		case <-time.After(30 * time.Second):
			t.Fatalf("the detached runner never polled")
		}
		id := w.srv.push()
		waitFor(t, "the zavod call", func() bool { return bin.read("zavod.args") != "" })
		time.Sleep(time.Second)
		args := strings.Split(strings.TrimSpace(bin.read("zavod.args")), "\n")
		if len(args) != 1 || args[0] != "--json send "+namedPathZavodUUID+" --text-file -" {
			t.Fatalf("zavod calls = %q, want one `--json send %s --text-file -`", args, namedPathZavodUUID)
		}
		stdin := bin.read("zavod.stdin")
		if !strings.Contains(stdin, "notification=\""+strconv.Itoa(id)+"\"") {
			t.Fatalf("zavod stdin lacks notification %d: %q", id, stdin)
		}
	})

	t.Run("claude keeps its stream before zavod", func(t *testing.T) {
		_, bin, _ := run(t, "claude", "claude")
		pre := lastLine(filepath.Join(bin.log, "preflight.out"))
		if pre["event"] != "preflight" || pre["method"] != "stream" || pre["channel"] != "stdout" {
			t.Fatalf("preflight line = %v, want stream on stdout", pre)
		}
		if got := bin.read("zavod.args"); got != "" {
			t.Fatalf("zavod was called: %q", got)
		}
	})
}
