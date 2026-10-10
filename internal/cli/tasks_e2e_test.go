package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/testkit"
)

func withRepo(t *testing.T) *testkit.Env {
	t.Helper()
	env := initialized(t)
	env.AppendConfig("[defaults]\nharness = \"codex\"\nmodel = \"gpt-5\"")
	env.OK("repo", "add", testkit.GitRepo(t, "api"))
	return env
}

func TestTaskCreateResolvesTargetAndDefaults(t *testing.T) {
	env := withRepo(t)
	task := env.OK("task", "create", "--repo", "api", "--objective", "Fix the login bug\nDetails follow.")
	want := map[string]any{"id": "t_1", "role": "worker", "repo": "api", "title": "Fix the login bug", "deliverable": "code", "state": "queued", "driver": "driver:main"}
	for key, value := range want {
		if task[key] != value {
			t.Fatalf("%s = %v, want %v: %v", key, task[key], value, task)
		}
	}
	if target := task["target"].(map[string]any); target["host"] != "workhorse" || target["harness"] != "codex" || target["model"] != "gpt-5" {
		t.Fatalf("target: %v", target)
	}
	env.Refused("usage", "task", "create", "--objective", "No repository")
	env.Refused("usage", "task", "create", "--repo", "api", "--objective", "x", "--deliverable", "answer")
	env.Refused("unknown_harness", "task", "create", "--repo", "api", "--objective", "x", "--harness", "vim")
	env.Refused("unknown_host", "task", "create", "--role", "driver", "--objective", "x", "--host", "laptop")
	env.Refused("not_found", "task", "create", "--repo", "web", "--objective", "x")
}

func TestRoutesPickTheHarnessByRoleAndRepository(t *testing.T) {
	env := withRepo(t)
	env.AppendConfig("[[routes]]\nrole = \"driver\"\nharness = \"claude-code\"\nmodel = \"claude-opus-5-5\"")
	driver := env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Own the api work")
	if target := driver["target"].(map[string]any); target["harness"] != "claude-code" || target["model"] != "claude-opus-5-5" {
		t.Fatalf("driver target: %v", target)
	}
	worker := env.OK("task", "create", "--repo", "api", "--objective", "Work", "--model", "o3")
	if target := worker["target"].(map[string]any); target["harness"] != "codex" || target["model"] != "o3" {
		t.Fatalf("worker target: %v", target)
	}
}

func TestDriverTaskObjectiveIsTheFirstRequest(t *testing.T) {
	env := withRepo(t)
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Own the api work")
	shown := env.OK("task", "show", "t_1")
	requests := shown["requests"].([]any)
	if len(requests) != 1 || requests[0].(map[string]any)["state"] != "open" {
		t.Fatalf("requests: %v", requests)
	}
	sent := env.OK("task", "send", "t_1", "Also add rate limiting")
	requests = env.OK("task", "show", "t_1")["requests"].([]any)
	if len(requests) != 2 || requests[1].(map[string]any)["seq"] != sent["seq"] {
		t.Fatalf("second request: %v", requests)
	}
	if deliverable := env.OK("task", "show", "t_1")["task"].(map[string]any)["deliverable"]; deliverable != "answer" {
		t.Fatalf("driver deliverable: %v", deliverable)
	}
}

func TestDependenciesAndReadiness(t *testing.T) {
	env := withRepo(t)
	env.OK("task", "create", "--repo", "api", "--objective", "Research", "--deliverable", "report")
	env.OK("task", "create", "--repo", "api", "--objective", "Implement", "--after", "t_1")
	shown := env.OK("task", "show", "t_2")
	deps := shown["dependencies"].([]any)
	if len(deps) != 1 || deps[0].(map[string]any)["task"] != "t_1" || deps[0].(map[string]any)["until"] != "published" || shown["ready"] != false {
		t.Fatalf("dependencies: %v", shown)
	}
	if ready := env.OK("task", "show", "t_1")["ready"]; ready != true {
		t.Fatalf("t_1 ready: %v", ready)
	}
	env.OK("task", "create", "--role", "driver", "--objective", "Other tree", "--as", "driver:other")
	env.Refused("invalid_dependency", "task", "create", "--repo", "api", "--objective", "x", "--after", "t_3")
	env.Refused("usage", "task", "create", "--repo", "api", "--objective", "x", "--after", "t_1", "--until", "soon")
}

func TestSendRepliesAndRevisions(t *testing.T) {
	env := withRepo(t)
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.Run("Use the v2 API", "task", "send", "t_1", "-")
	events := env.OK("task", "show", "t_1")["events"].([]any)
	last := events[len(events)-1].(map[string]any)
	if last["name"] != "task.message" || last["data"].(map[string]any)["body"] != "Use the v2 API" {
		t.Fatalf("message event: %v", last)
	}
	env.Refused("invalid_reply", "task", "send", "t_1", "answer", "--reply-to", "1")
	env.Refused("revision_conflict", "task", "send", "t_1", "again", "--if-revision", "99")
}

func TestCancelOnlyBeforeStart(t *testing.T) {
	env := withRepo(t)
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	closed := env.OK("task", "cancel", "t_1")
	if closed["state"] != "closed" || closed["reason"] != "cancelled" {
		t.Fatalf("cancel: %v", closed)
	}
	env.Refused("invalid_state", "task", "cancel", "t_1")
	env.Refused("invalid_state", "task", "send", "t_1", "hello")
}

func TestAdoptionMovesRootsBetweenDrivers(t *testing.T) {
	env := withRepo(t)
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	adopted := env.OK("task", "adopt", "t_1", "--to", "driver:hermes")
	if adopted["driver"] != "driver:hermes" {
		t.Fatalf("adopt: %v", adopted)
	}
	env.Refused("not_owner", "task", "send", "t_1", "hello")
	if roots := env.OK("task", "list")["tasks"].([]any); len(roots) != 0 {
		t.Fatalf("main still lists the root: %v", roots)
	}
	if roots := env.OK("task", "list", "--as", "driver:hermes")["tasks"].([]any); len(roots) != 1 {
		t.Fatalf("hermes roots: %v", roots)
	}
	env.OK("task", "send", "t_1", "hello", "--as", "driver:hermes")
	env.Refused("usage", "task", "adopt", "t_1", "--to", "worker:x")
}

func TestNotesAndPluginData(t *testing.T) {
	env := withRepo(t)
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.OK("task", "note", "t_1", "Prefer the smaller change")
	env.OK("task", "data", "set", "t_1", "issue=ENG-12", "points=3", "--plugin", "linear")
	env.Refused("usage", "task", "data", "set", "t_1", "issue=ENG-12")
	env.Run(strings.Repeat("x", 4097), "task", "note", "t_1", "-").Refusal(t, "too_large")
	shown := env.OK("task", "show", "t_1")
	data := shown["data"].(map[string]any)["linear"].(map[string]any)
	if data["issue"] != "ENG-12" || data["points"] != float64(3) {
		t.Fatalf("data: %v", data)
	}
	env.OK("task", "data", "set", "t_1", "--unset", "points", "--plugin", "linear")
	data = env.OK("task", "show", "t_1")["data"].(map[string]any)["linear"].(map[string]any)
	if _, ok := data["points"]; ok || data["issue"] != "ENG-12" {
		t.Fatalf("data after unset: %v", data)
	}
}

func TestTaskListFilters(t *testing.T) {
	env := withRepo(t)
	env.OK("task", "create", "--repo", "api", "--objective", "One")
	env.OK("task", "create", "--role", "driver", "--objective", "General")
	env.OK("task", "cancel", "t_1")
	if tasks := env.OK("task", "list", "--role", "driver")["tasks"].([]any); len(tasks) != 1 || tasks[0].(map[string]any)["id"] != "t_2" {
		t.Fatalf("role filter: %v", tasks)
	}
	if tasks := env.OK("task", "list", "--state", "closed", "--repo", "api")["tasks"].([]any); len(tasks) != 1 {
		t.Fatalf("state filter: %v", tasks)
	}
	if listed := env.OK("task", "list")["tasks"].([]any)[0].(map[string]any); listed["objective"] != nil {
		t.Fatalf("list includes objectives: %v", listed)
	}
}

func TestCreateGateBlocksAndIsRecorded(t *testing.T) {
	env := withRepo(t)
	pkg, _ := filepath.EvalSymlinks(t.TempDir())
	testkit.WritePlugin(t, filepath.Join(pkg, "plugins", "freeze"), "freeze", "[[intercept]]\npoint = \"task.create\"\n")
	env.AppendConfig(`[packages.tools]
path = "` + pkg + `"

[plugins.freeze]
package = "tools"

[plugins.freeze.options]
decision = "block"
reason = "api is frozen"`)
	refusal := env.Refused("plugin_blocked", "task", "create", "--repo", "api", "--objective", "Work")
	if !strings.Contains(refusal["message"].(string), "api is frozen") {
		t.Fatalf("refusal: %v", refusal)
	}
	if tasks := env.OK("task", "list")["tasks"].([]any); len(tasks) != 0 {
		t.Fatalf("blocked task created: %v", tasks)
	}
	events := env.OK("events")["events"].([]any)
	last := events[len(events)-1].(map[string]any)
	if last["name"] != "plugin.blocked" || last["data"].(map[string]any)["plugin"] != "freeze" {
		t.Fatalf("blocked event: %v", last)
	}
}

func TestRouterProviderAnswersTargets(t *testing.T) {
	env := withRepo(t)
	pkg, _ := filepath.EvalSymlinks(t.TempDir())
	testkit.WritePlugin(t, filepath.Join(pkg, "plugins", "router"), "router", "[[provide]]\ntype = \"router\"\n")
	env.AppendConfig(`[providers]
router = "router"

[packages.tools]
path = "` + pkg + `"

[plugins.router]
package = "tools"

[plugins.router.options.provide]
harness = "pi"
model = "local"`)
	task := env.OK("task", "create", "--repo", "api", "--objective", "Work")
	if target := task["target"].(map[string]any); target["harness"] != "pi" || target["model"] != "local" {
		t.Fatalf("routed target: %v", target)
	}
	os.WriteFile(filepath.Join(pkg, "plugins", "router", "plugin.toml"), []byte("broken"), 0o644)
	env.Refused("plugin_failed", "task", "create", "--repo", "api", "--objective", "Work")
}
