package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"shephrd/internal/testkit"
)

// withHerdr declares the first-party Herdr presentation with a fake herdr
// that runs each pane's command like a terminal would.
func withHerdr(t *testing.T) *testkit.Env {
	env, _ := withHarness(t)
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
	return env
}

func herdrPanes(env *testkit.Env) []map[string]any {
	body, _ := os.ReadFile(filepath.Join(env.Home, ".fakeherdr", "panes.json"))
	var panes []map[string]any
	json.Unmarshal(body, &panes)
	return panes
}

func TestSessionsRunInAPresentation(t *testing.T) {
	env := withHerdr(t)
	env.AppendConfig("[repos.api.landing]\nmode = \"direct\"\nmethod = \"fast-forward\"")
	if hosts := env.OK("host", "list")["hosts"].([]any); !strings.Contains(strings.Join(toStrings(hosts[0].(map[string]any)["presentations"]), " "), "herdr") {
		t.Fatalf("presentations: %v", hosts)
	}
	env.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	run := env.OK("task", "start", "t_1")["run"].(map[string]any)
	if run["endpoint"] == nil || run["liveness"] != "starting" {
		t.Fatalf("presented run: %v", run)
	}
	env.WaitState("t_1", "done")
	call := callsFor(env, "t_1", 0)[0]
	if call["interactive"] != true || slices.Contains(toStrings(call["args"]), "-p") || !strings.Contains(call["brief"].(string), "# Shephrd brief: t_1") {
		t.Fatalf("the harness did not run its own UI on the brief: %v", call["args"])
	}
	panes := herdrPanes(env)
	if len(panes) != 1 || panes[0]["label"] != "t_1 worker · Fix it" || strings.Contains(panes[0]["command"].(string), envOf(call)["SHEPHRD_RUN_TOKEN"]) {
		t.Fatalf("pane: %v", panes)
	}
	if _, err := os.Stat(filepath.Join(run["dir"].(string), "token")); !os.IsNotExist(err) {
		t.Fatal("the run token file was left behind")
	}
	env.Eventually("the turn to be ended", func() bool {
		return env.OK("task", "log", "t_1")["run"].(map[string]any)["liveness"] == "exited"
	})
	if panes := herdrPanes(env); panes[0]["closed"] == true {
		t.Fatalf("the terminal closed before the process was proven gone: %v", panes)
	}
	env.Eventually("the exited turn's terminal to close", func() bool {
		env.OK("workspace", "reconcile")
		return herdrPanes(env)[0]["closed"] == true
	})
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
	env.WaitState("t_1", "waiting")
	env.Eventually("the first run to exit", func() bool {
		return env.OK("task", "log", "t_1")["run"].(map[string]any)["liveness"] == "exited"
	})
	env.StartDaemon()
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

func TestUncertainEndpointIsNotClosed(t *testing.T) {
	env := withHerdr(t)
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "done")
	env.Eventually("the run to exit", func() bool {
		return env.OK("task", "log", "t_1")["run"].(map[string]any)["liveness"] == "exited"
	})
	binary := filepath.Join(env.Home, "herdrbin", "herdr")
	os.Rename(binary, binary+".off")
	env.OK("workspace", "reconcile")
	if panes := herdrPanes(env); panes[0]["closed"] == true {
		t.Fatalf("closed a tab without proving its endpoint: %v", panes)
	}
	os.Rename(binary+".off", binary)
	path := filepath.Join(env.Home, ".fakeherdr", "panes.json")
	panes := herdrPanes(env)
	panes[0]["tab_id"] = "other"
	body, _ := json.Marshal(panes)
	os.WriteFile(path, body, 0o600)
	env.OK("workspace", "reconcile")
	if panes := herdrPanes(env); panes[0]["closed"] == true {
		t.Fatalf("closed a tab with mismatched endpoint ownership: %v", panes)
	}
	panes[0]["tab_id"] = "t1"
	body, _ = json.Marshal(panes)
	os.WriteFile(path, body, 0o600)
	env.Eventually("the terminal to close after the endpoint can be probed", func() bool {
		env.OK("workspace", "reconcile")
		return herdrPanes(env)[0]["closed"] == true
	})
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
	if panes := herdrPanes(env); panes[0]["closed"] != true {
		t.Fatalf("pane after stop: %v", panes)
	}
}
