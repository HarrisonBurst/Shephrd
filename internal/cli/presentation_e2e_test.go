package cli_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"shephrd/internal/testkit"
)

// withHerdr declares the first-party Herdr presentation with a fake herdr
// that runs each pane's command like a terminal would.
func withHerdr(t *testing.T) *testkit.Env {
	env, _ := withHarness(t)
	installHerdr(t, env)
	return env
}

func installHerdr(t *testing.T, env *testkit.Env) {
	fake, _ := os.ReadFile(testkit.Tool(t, "./internal/testkit/fakeherdr", "fakeherdr"))
	bin := filepath.Join(env.Home, "herdrbin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "herdr"), fake, 0o755)
	env.Path = append([]string{bin}, env.Path...)
	pkg, _ := filepath.EvalSymlinks(t.TempDir())
	plugin := filepath.Join(pkg, "plugins", "herdr")
	os.MkdirAll(filepath.Join(plugin, "bin"), 0o755)
	exe, _ := os.ReadFile(testkit.Tool(t, "./plugins/herdr", "shephrd-herdr"))
	os.WriteFile(filepath.Join(plugin, "bin", "shephrd-herdr"), exe, 0o755)
	manifest, _ := os.ReadFile(filepath.Join("..", "..", "plugins", "herdr", "plugin.toml"))
	os.WriteFile(filepath.Join(plugin, "plugin.toml"), manifest, 0o644)
	config, _ := os.ReadFile(env.ConfigPath())
	os.WriteFile(env.ConfigPath(), append([]byte("presentation = \"herdr\"\n"), config...), 0o600)
	env.AppendConfig(`[packages.firstparty]
path = "` + pkg + `"

[plugins.herdr]
package = "firstparty"

[plugins.herdr.options]
socket = "/tmp/herdr.sock"
workspace = "w1"`)
}

func herdrPanes(env *testkit.Env) []map[string]any {
	cmd := exec.Command(filepath.Join(env.Home, "herdrbin", "herdr"), "pane", "list")
	cmd.Env = append(os.Environ(), "HOME="+env.Home)
	cmd.Run()
	body, _ := os.ReadFile(filepath.Join(env.Home, ".fakeherdr", "panes.json"))
	var panes []map[string]any
	json.Unmarshal(body, &panes)
	return panes
}

func waitStateFromDaemon(t *testing.T, env *testkit.Env, ref, state string) map[string]any {
	var task map[string]any
	env.Eventually(ref+" "+state, func() bool {
		shown := env.Run("", "task", "show", ref)
		if shown.Code != 0 {
			return false
		}
		task = shown.JSON(t)["task"].(map[string]any)
		if task["state"] != state {
			return false
		}
		log := env.Run("", "task", "log", ref)
		if log.Code != 0 {
			return true
		}
		return log.JSON(t)["run"].(map[string]any)["liveness"] == "exited"
	})
	return task
}

func ageRun(t *testing.T, env *testkit.Env, run map[string]any) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(env.Home, ".local", "state", "shephrd", "shephrd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE runs SET started_at = ? WHERE id = ?`, "2020-01-01T00:00:00.000000000Z", run["run"]); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentColdHerdrOpenFailures(t *testing.T) {
	env := withHerdr(t)
	bin := filepath.Join(env.Home, "herdrbin")
	if err := os.Rename(filepath.Join(bin, "herdr"), filepath.Join(bin, "fakeherdr")); err != nil {
		t.Fatal(err)
	}
	wrapper := `#!/bin/sh
if [ "$1 $2" = "tab create" ]; then
  for token in "$HOME"/.local/share/shephrd/runs/*/1/token; do
    [ -f "$token" ] && cp "$token" "$HOME/.fakeherdr/token-$$"
  done
  touch "$HOME/.fakeherdr/cold-$$"
  n=0
  while [ "$(find "$HOME/.fakeherdr" -name 'cold-*' | wc -l | tr -d ' ')" -lt 2 ]; do
    sleep 0.05
    n=$((n+1))
    [ "$n" -lt 200 ] || exit 2
  done
  echo '{"error":{"code":"not_ready","message":"terminal backend still starting"}}'
  exit 1
fi
exec "$(dirname "$0")/fakeherdr" "$@"
`
	if err := os.MkdirAll(filepath.Join(env.Home, ".fakeherdr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "herdr"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		env.OK("task", "create", "--repo", "api", "--objective", "Work")
	}
	var wg sync.WaitGroup
	results := make([]testkit.Result, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = env.Run("", "task", "start", "t_"+strconv.Itoa(i+1))
		}()
	}
	wg.Wait()
	for i, result := range results {
		ref := "t_" + string(rune('1'+i))
		result.Refusal(t, "start_failed")
		run := env.OK("task", "log", ref)["run"].(map[string]any)
		if run["liveness"] != "starting" || run["pid"] != nil || run["endpoint"] != nil {
			t.Fatalf("failed open %s: %v", ref, run)
		}
		dir := filepath.Join(env.Home, ".local", "share", "shephrd", "runs", ref, "1")
		for _, name := range []string{"brief.md", "run.json", "run-not-dispatched.json"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Fatalf("%s %s: %v", ref, name, err)
			}
		}
		for _, name := range []string{"endpoint", "token", "session.log", "exit.json"} {
			if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Fatalf("%s %s: %v", ref, name, err)
			}
		}
		ageRun(t, env, run)
	}
	env.OK("workspace", "reconcile")
	for i := range results {
		ref := "t_" + string(rune('1'+i))
		if task := env.Task(ref); task["state"] != "held" || task["reason"] != "start_failed" {
			t.Fatalf("task %s: %v", ref, task)
		}
		if run := env.OK("task", "log", ref)["run"].(map[string]any); run["liveness"] != "unknown" {
			t.Fatalf("run %s: %v", ref, run)
		}
		env.Refused("liveness_unknown", "task", "retry", ref)
		env.Refused("stop_unconfirmed", "task", "stop", ref)
	}
	if panes := herdrPanes(env); len(panes) != 0 || len(env.Calls()) != 0 {
		t.Fatalf("unexpected launches: panes %v calls %v", panes, env.Calls())
	}
	if matches, err := filepath.Glob(filepath.Join(env.Home, ".fakeherdr", "cold-*")); err != nil || len(matches) != 2 {
		t.Fatalf("concurrent cold opens: %v, %v", matches, err)
	}
	if err := os.Remove(filepath.Join(bin, "herdr")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(bin, "fakeherdr"), filepath.Join(bin, "herdr")); err != nil {
		t.Fatal(err)
	}
	env.OK("task", "create", "--repo", "api", "--objective", "Ready")
	env.OK("task", "start", "t_3")
	env.WaitState("t_3", "done")
	for i := range results {
		ref := "t_" + string(rune('1'+i))
		if run := env.OK("task", "log", ref)["run"].(map[string]any); run["liveness"] != "unknown" {
			t.Fatalf("healthy start changed %s: %v", ref, run)
		}
		proofPath := filepath.Join(env.Home, ".local", "share", "shephrd", "runs", ref, "1", "run-not-dispatched.json")
		proof, err := os.ReadFile(proofPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(proofPath, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		env.Refused("abandon_unconfirmed", "task", "stop", ref, "--abandon")
		if err := os.WriteFile(proofPath, proof, 0o600); err != nil {
			t.Fatal(err)
		}
		env.OK("task", "stop", ref, "--abandon")
		if run := env.OK("task", "log", ref)["run"].(map[string]any); run["liveness"] != "exited" || run["stop_reason"] != "abandoned" {
			t.Fatalf("abandoned %s: %v", ref, run)
		}
		if task := env.Task(ref); task["state"] != "held" {
			t.Fatalf("abandoned task %s: %v", ref, task)
		}
	}
	tokens, err := filepath.Glob(filepath.Join(env.Home, ".fakeherdr", "token-*"))
	if err != nil || len(tokens) == 0 {
		t.Fatalf("no intercepted run token: %v, %v", tokens, err)
	}
	body, err := os.ReadFile(tokens[0])
	if err != nil {
		t.Fatal(err)
	}
	env.Vars["SHEPHRD_RUN_TOKEN"] = strings.TrimSpace(string(body))
	env.Refused("stale_run", "_started", "--pid", "123", "--start", "old")
	delete(env.Vars, "SHEPHRD_RUN_TOKEN")
	if len(callsFor(env, "t_1", 0)) != 0 || len(callsFor(env, "t_2", 0)) != 0 {
		t.Fatalf("abandoned run launched a harness: %v", env.Calls())
	}
}

func TestInterruptedPresentationBeforeSupervisorIdentity(t *testing.T) {
	env := withHerdr(t)
	dir := filepath.Join(env.Home, ".fakeherdr")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pause := filepath.Join(dir, "pause-before-run")
	if err := os.WriteFile(pause, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "exit-before-supervisor"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	cmd := exec.Command(env.Bin, "task", "start", "t_1")
	cmd.Env = []string{"HOME=" + env.Home, "PATH=" + strings.Join(env.Path, ":") + ":" + os.Getenv("PATH"), "USER=" + os.Getenv("USER")}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	env.Eventually("the new pane to wait before supervisor exec", func() bool { return len(herdrPanes(env)) == 1 })
	env.Eventually("the endpoint to be durable before supervisor exec", func() bool {
		body, err := os.ReadFile(filepath.Join(env.Home, ".local", "share", "shephrd", "runs", "t_1", "1", "endpoint"))
		return err == nil && len(body) > 0
	})
	cmd.Process.Kill()
	cmd.Wait()
	os.Remove(pause)
	env.Eventually("the shell to exit without a supervisor", func() bool { return herdrPanes(env)[0]["closed"] == true })
	run := env.OK("task", "log", "t_1")["run"].(map[string]any)
	if run["liveness"] != "starting" || run["pid"] != nil || run["endpoint"] != nil || run["exit_status"] != nil {
		t.Fatalf("interrupted run: %v", run)
	}
	ageRun(t, env, run)
	env.OK("workspace", "reconcile")
	if task := env.Task("t_1"); task["state"] != "held" || task["reason"] != "start_failed" {
		t.Fatalf("task after reconciliation: %v", task)
	}
	if current := env.OK("task", "log", "t_1")["run"].(map[string]any); current["liveness"] != "unknown" {
		t.Fatalf("run after reconciliation: %v", current)
	}
	env.Refused("liveness_unknown", "task", "resume", "t_1")
	env.Refused("liveness_unknown", "task", "retry", "t_1")
	if _, err := os.Stat(filepath.Join(env.Home, ".local", "share", "shephrd", "runs", "t_1", "1", "run-not-dispatched.json")); !os.IsNotExist(err) {
		t.Fatalf("dispatch was attempted: %v", err)
	}
	env.OK("task", "stop", "t_1", "--abandon")
	if current := env.OK("task", "log", "t_1")["run"].(map[string]any); current["liveness"] != "exited" || current["stop_reason"] != "abandoned" {
		t.Fatalf("abandoned run: %v", current)
	}
	if task := env.Task("t_1"); task["state"] != "held" {
		t.Fatalf("abandoned task: %v", task)
	}
}

func TestFailedProofRecordingKeepsRunUnknown(t *testing.T) {
	env := withHerdr(t)
	bin := filepath.Join(env.Home, "herdrbin")
	if err := os.Rename(filepath.Join(bin, "herdr"), filepath.Join(bin, "fakeherdr")); err != nil {
		t.Fatal(err)
	}
	wrapper := `#!/bin/sh
if [ "$1 $2" = "tab create" ]; then
  rm "$HOME/.local/share/shephrd/runs/t_1/1/token"
  echo '{"error":{"code":"not_ready","message":"terminal backend still starting"}}'
  exit 1
fi
exec "$(dirname "$0")/fakeherdr" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "herdr"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.Refused("start_failed", "task", "start", "t_1")
	run := env.OK("task", "log", "t_1")["run"].(map[string]any)
	if _, err := os.Stat(filepath.Join(run["dir"].(string), "run-not-dispatched.json")); !os.IsNotExist(err) {
		t.Fatalf("proof after failed recording: %v", err)
	}
	ageRun(t, env, run)
	env.OK("workspace", "reconcile")
	env.Refused("abandon_unconfirmed", "task", "stop", "t_1", "--abandon")
}

func TestFailedPaneRunHasNoNoDispatchProof(t *testing.T) {
	env := withHerdr(t)
	bin := filepath.Join(env.Home, "herdrbin")
	if err := os.Rename(filepath.Join(bin, "herdr"), filepath.Join(bin, "fakeherdr")); err != nil {
		t.Fatal(err)
	}
	wrapper := `#!/bin/sh
if [ "$1 $2" = "pane run" ]; then
  echo '{"error":{"code":"not_ready","message":"terminal backend still starting"}}'
  exit 1
fi
exec "$(dirname "$0")/fakeherdr" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "herdr"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.Refused("start_failed", "task", "start", "t_1")
	run := env.OK("task", "log", "t_1")["run"].(map[string]any)
	if _, err := os.Stat(filepath.Join(run["dir"].(string), "run-not-dispatched.json")); !os.IsNotExist(err) {
		t.Fatalf("proof after run dispatch: %v", err)
	}
	if panes := herdrPanes(env); len(panes) != 1 || len(env.Calls()) != 0 {
		t.Fatalf("unexpected pane or harness: %v, %v", panes, env.Calls())
	}
	ageRun(t, env, run)
	env.OK("workspace", "reconcile")
	env.Refused("abandon_unconfirmed", "task", "stop", "t_1", "--abandon")
}

func TestLostHostReplyAfterNoDispatchProof(t *testing.T) {
	m := threeMachines(t)
	m.laptop.InstallHarnesses()
	installHerdr(t, m.laptop)
	m.home.Path = append([]string{filepath.Join(m.laptop.Home, "herdrbin")}, m.home.Path...)
	bin := filepath.Join(m.laptop.Home, "herdrbin")
	if err := os.Rename(filepath.Join(bin, "herdr"), filepath.Join(bin, "fakeherdr")); err != nil {
		t.Fatal(err)
	}
	wrapper := `#!/bin/sh
if [ "$1 $2" = "tab create" ]; then
  echo '{"error":{"code":"not_ready","message":"terminal backend still starting"}}'
  exit 1
fi
exec "$(dirname "$0")/fakeherdr" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "herdr"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	m.home.OK("repo", "add", testkit.GitRepo(t, "api"), "--host", "laptop")
	m.home.OK("task", "create", "--repo", "api", "--objective", "Work")
	m.home.SSHDropOperation("me@laptop", "launch")
	m.home.Refused("start_failed", "task", "start", "t_1")
	run := m.home.OK("task", "log", "t_1")["run"].(map[string]any)
	if run["pid"] != nil || run["endpoint"] != nil {
		t.Fatalf("failed remote open: %v", run)
	}
	if _, err := os.Stat(filepath.Join(run["dir"].(string), "run-not-dispatched.json")); err != nil {
		t.Fatal(err)
	}
	ageRun(t, m.home, run)
	m.home.OK("workspace", "reconcile")
	m.home.OK("task", "stop", "t_1", "--abandon")
	if current := m.home.OK("task", "log", "t_1")["run"].(map[string]any); current["liveness"] != "exited" || current["stop_reason"] != "abandoned" {
		t.Fatalf("abandoned remote run: %v", current)
	}
}

func TestInterruptedPIDLessPresentationStartCanBeAbandoned(t *testing.T) {
	m := threeMachines(t)
	m.laptop.InstallHarnesses()
	installHerdr(t, m.laptop)
	m.home.Path = append([]string{filepath.Join(m.laptop.Home, "herdrbin")}, m.home.Path...)
	m.home.OK("repo", "add", testkit.GitRepo(t, "api"), "--host", "laptop")
	marker := filepath.Join(m.laptop.Home, ".fakeherdr", "exit-before-supervisor")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m.home.OK("task", "create", "--repo", "api", "--objective", "Work")
	m.home.SSHDropOperation("me@laptop", "launch")
	m.home.Refused("start_failed", "task", "start", "t_1")
	run := m.home.OK("task", "log", "t_1")["run"].(map[string]any)
	if run["pid"] != nil || run["endpoint"] != nil || run["exit_status"] != nil || len(herdrPanes(m.laptop)) != 1 {
		t.Fatalf("interrupted launch: %v; panes: %v", run, herdrPanes(m.laptop))
	}
	ageRun(t, m.home, run)
	m.home.OK("workspace", "reconcile")
	if task := m.home.Task("t_1"); task["state"] != "held" || task["reason"] != "start_failed" {
		t.Fatalf("task after reconciliation: %v", task)
	}
	if current := m.home.OK("task", "log", "t_1")["run"].(map[string]any); current["liveness"] != "unknown" {
		t.Fatalf("run after reconciliation: %v", current)
	}
	m.home.Refused("liveness_unknown", "task", "resume", "t_1")
	m.home.Refused("liveness_unknown", "task", "retry", "t_1")
	m.home.OK("task", "stop", "t_1", "--abandon")
	if current := m.home.OK("task", "log", "t_1")["run"].(map[string]any); current["liveness"] != "exited" || current["stop_reason"] != "abandoned" {
		t.Fatalf("abandoned run: %v", current)
	}
	if task := m.home.Task("t_1"); task["state"] != "held" {
		t.Fatalf("abandoned task: %v", task)
	}
}

func TestSessionsRunInAPresentation(t *testing.T) {
	env := withHerdr(t)
	env.AppendConfig("[repos.api.landing]\nmode = \"direct\"\nmethod = \"fast-forward\"")
	if hosts := env.OK("host", "list")["hosts"].([]any); !strings.Contains(strings.Join(toStrings(hosts[0].(map[string]any)["presentations"]), " "), "herdr") {
		t.Fatalf("presentations: %v", hosts)
	}
	env.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	run := env.OK("task", "start", "t_1")["run"].(map[string]any)
	env.StartDaemon()
	if run["endpoint"] == nil || run["liveness"] != "starting" {
		t.Fatalf("presented run: %v", run)
	}
	env.WaitState("t_1", "done")
	call := callsFor(env, "t_1", 0)[0]
	if call["interactive"] != true || slices.Contains(toStrings(call["args"]), "-p") || !strings.Contains(call["brief"].(string), "# Shephrd brief: t_1") {
		t.Fatalf("the harness did not run its own UI on the brief: %v", call["args"])
	}
	panes := herdrPanes(env)
	if len(panes) != 1 || panes[0]["label"] != "Fix it" || strings.Contains(panes[0]["command"].(string), envOf(call)["SHEPHRD_RUN_TOKEN"]) {
		t.Fatalf("pane: %v", panes)
	}
	if _, err := os.Stat(filepath.Join(run["dir"].(string), "token")); !os.IsNotExist(err) {
		t.Fatal("the run token file was left behind")
	}
	env.Eventually("the reported turn to close its own terminal", func() bool {
		return herdrPanes(env)[0]["closed"] == true
	})
	env.OK("workspace", "reconcile")
	if run := env.OK("task", "log", "t_1")["run"].(map[string]any); run["liveness"] != "exited" {
		t.Fatalf("the terminated supervisor was not reconciled: %v", run)
	}
	if shown, _ := os.ReadFile(filepath.Join(env.Home, ".fakeherdr", "p1.log")); !strings.Contains(string(shown), "turn finished") || strings.Contains(string(shown), `{"run"`) {
		t.Fatalf("the pane showed: %s", shown)
	}
	env.OK("task", "deliver", "t_1")
	if panes := herdrPanes(env); panes[0]["closed"] != true {
		t.Fatalf("pane after the task closed: %v", panes)
	}
}

func TestAContinuationOpensANewTabAndResumesTheSession(t *testing.T) {
	env := withHerdr(t)
	env.AppendConfig("[timeouts]\nsettle = \"500ms\"")
	env.Script(map[string][][]action{"t_1": {{{"report": []string{"question", "Which API version?"}}}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.OK("task", "start", "t_1")
	env.StartDaemon()
	env.WaitState("t_1", "waiting")
	env.Eventually("the first tab to close", func() bool { return herdrPanes(env)[0]["closed"] == true })
	env.OK("task", "send", "t_1", "Version 2", "--reply-to", questionSeq(env, "t_1"))
	env.WaitState("t_1", "done")
	env.Eventually("the second tab to close", func() bool {
		panes := herdrPanes(env)
		return len(panes) == 2 && panes[1]["closed"] == true
	})
	panes := herdrPanes(env)
	if panes[0]["runs"] != float64(1) || panes[1]["runs"] != float64(1) || panes[0]["pane_id"] == panes[1]["pane_id"] {
		t.Fatalf("panes after two turns: %v", panes)
	}
	if second := callsFor(env, "t_1", 1); len(second) != 1 || !slices.Contains(toStrings(second[0]["args"]), "resume") {
		t.Fatalf("second turn: %v", second)
	}
}

func TestReportedButLiveTurnKeepsItsTab(t *testing.T) {
	env := withHerdr(t)
	env.Script(map[string][][]action{"t_1": {{{"report": []string{"question", "Which version?"}}, {"sleep": "60s"}}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.OK("task", "start", "t_1")
	env.Eventually("the report while the session remains live", func() bool {
		run := env.OK("task", "log", "t_1")["run"].(map[string]any)
		return run["turn_reported"] == true && run["liveness"] == "live"
	})
	env.OK("workspace", "reconcile")
	if panes := herdrPanes(env); panes[0]["closed"] == true {
		t.Fatalf("closed a live run's tab: %v", panes)
	}
}

func TestUnreportedPresentedRunGetsInspectionGraceAndNudge(t *testing.T) {
	env := withHerdr(t)
	env.Script(map[string][][]action{"t_1": {{{"exit": 0}}, {{"exit": 0}}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.OK("task", "start", "t_1")
	env.StartDaemon()
	env.Eventually("a task-log hint in the terminal", func() bool {
		shown, _ := os.ReadFile(filepath.Join(env.Home, ".fakeherdr", "p1.log"))
		return strings.Contains(string(shown), "shephrd task log t_1")
	})
	time.Sleep(6 * time.Second)
	if panes := herdrPanes(env); panes[0]["closed"] == true {
		t.Fatalf("unreported turn closed during inspection grace: %v", panes)
	}
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) && !herdrPanes(env)[0]["closed"].(bool) {
		time.Sleep(100 * time.Millisecond)
	}
	if panes := herdrPanes(env); panes[0]["closed"] != true {
		t.Fatalf("daemon did not supersede the held terminal: %v", panes)
	}
	env.Eventually("the nudge terminal hint", func() bool {
		shown, _ := os.ReadFile(filepath.Join(env.Home, ".fakeherdr", "p2.log"))
		return strings.Contains(string(shown), "shephrd task log t_1")
	})
	time.Sleep(32 * time.Second)
	waitStateFromDaemon(t, env, "t_1", "held")
}

func TestTaskCloseWaitsForTheSupervisorBeforeReleasing(t *testing.T) {
	env := withHerdr(t)
	env.AppendConfig("[repos.api.landing]\nmode = \"direct\"\nmethod = \"fast-forward\"")
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	start := env.OK("task", "start", "t_1")
	workspace := start["attempt"].(map[string]any)["workspace"].(string)
	env.Eventually("a result while its supervisor is still live", func() bool {
		task := env.Task("t_1")
		run := env.OK("task", "log", "t_1")["run"].(map[string]any)
		return task["state"] == "done" && run["liveness"] == "live"
	})
	out := env.OK("task", "deliver", "t_1")
	if len(out["retained"].([]any)) != 1 || herdrPanes(env)[0]["closed"] == true {
		t.Fatalf("closed task released a live process: %v", out)
	}
	env.StartDaemon()
	env.Eventually("the supervisor to exit and its workspace to release", func() bool {
		if herdrPanes(env)[0]["closed"] != true {
			return false
		}
		_, err := os.Stat(workspace)
		return os.IsNotExist(err)
	})
}

func TestFailedHarnessStartKeepsItsTerminalForInspection(t *testing.T) {
	env := withHerdr(t)
	env.Script(map[string][][]action{"t_1": {{{"exit": 0}}}})
	dir := filepath.Join(env.Home, ".fakeherdr")
	os.MkdirAll(dir, 0o700)
	marker := filepath.Join(dir, "pause-before-run")
	os.WriteFile(marker, nil, 0o600)
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	started := make(chan testkit.Result, 1)
	go func() { started <- env.Run("", "task", "start", "t_1") }()
	env.Eventually("the tab before its harness launches", func() bool { return len(herdrPanes(env)) == 1 })
	binary := filepath.Join(env.Home, "fakebin", "codex")
	os.Rename(binary, binary+".off")
	os.Remove(marker)
	if result := <-started; result.Code != 0 {
		t.Fatalf("start: %+v", result)
	}
	env.Eventually("the failed start hint", func() bool {
		shown, _ := os.ReadFile(filepath.Join(dir, "p1.log"))
		return strings.Contains(string(shown), "shephrd task log t_1")
	})
	if panes := herdrPanes(env); panes[0]["closed"] == true {
		t.Fatalf("failed harness start closed before inspection: %v", panes)
	}
	os.Rename(binary+".off", binary)
	input, err := os.OpenFile(filepath.Join(dir, "p1.stdin"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	input.Write([]byte("\n"))
	input.Close()
	env.StartDaemon()
	time.Sleep(32 * time.Second)
	waitStateFromDaemon(t, env, "t_1", "held")
}

func TestStoppingBeforeSupervisorStartRefusesItsHarness(t *testing.T) {
	env := withHerdr(t)
	dir := filepath.Join(env.Home, ".fakeherdr")
	os.MkdirAll(dir, 0o700)
	marker := filepath.Join(dir, "pause-before-run")
	os.WriteFile(marker, nil, 0o600)
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	started := make(chan testkit.Result, 1)
	go func() { started <- env.Run("", "task", "start", "t_1") }()
	env.Eventually("the supervisor launch to wait in its new pane", func() bool {
		return len(herdrPanes(env)) == 1
	})
	env.Refused("stop_unconfirmed", "task", "stop", "t_1")
	os.Remove(marker)
	if result := <-started; result.Code != 0 {
		t.Fatalf("start: %+v", result)
	}
	env.Eventually("the refused supervisor to exit", func() bool {
		return herdrPanes(env)[0]["closed"] == true
	})
	env.OK("workspace", "reconcile")
	if len(callsFor(env, "t_1", 0)) != 0 {
		t.Fatalf("a harness started after stop: %v", env.Calls())
	}
	if run := env.OK("task", "log", "t_1")["run"].(map[string]any); run["liveness"] != "exited" {
		log, _ := os.ReadFile(filepath.Join(env.Home, ".fakeherdr", "p1.log"))
		t.Fatalf("the stopped supervisor was not reconciled: %v; pane log: %s", run, log)
	}
}

func TestStoppingAPresentedRunClosesItsPane(t *testing.T) {
	env := withHerdr(t)
	env.Script(map[string][][]action{"t_1": {{{"sleep": "60s"}}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.OK("task", "start", "t_1")
	env.Eventually("the session in its pane", func() bool { return len(callsFor(env, "t_1", 0)) == 1 })
	env.Eventually("the run to be live", func() bool {
		return env.OK("task", "log", "t_1")["run"].(map[string]any)["liveness"] == "live"
	})
	env.OK("task", "stop", "t_1")
	env.Eventually("the stopped supervisor to close its pane", func() bool {
		return herdrPanes(env)[0]["closed"] == true
	})
}
