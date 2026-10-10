package cli_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"shephrd/internal/testkit"
)

type action = map[string]any

func withHarness(t *testing.T) (*testkit.Env, string) {
	t.Helper()
	env := initialized(t)
	env.InstallHarnesses()
	env.AppendConfig("[defaults]\nharness = \"codex\"")
	repo := testkit.GitRepo(t, "api")
	env.OK("repo", "add", repo)
	return env, repo
}

func callsFor(env *testkit.Env, task string, run float64) []map[string]any {
	var out []map[string]any
	for _, call := range env.Calls() {
		if call["task"] == task && call["harness"] != nil && call["run"] == run {
			out = append(out, call)
		}
	}
	return out
}

func commandErrors(env *testkit.Env, task string) []string {
	var out []string
	for _, call := range env.Calls() {
		if call["task"] == task && call["command"] != nil && call["error"] != "<nil>" {
			out = append(out, call["stderr"].(string))
		}
	}
	return out
}

func envOf(call map[string]any) map[string]string {
	out := map[string]string{}
	for _, entry := range call["env"].([]any) {
		key, value, _ := strings.Cut(entry.(string), "=")
		out[key] = value
	}
	return out
}

func TestWorkerRunsInItsOwnWorkspaceAndReports(t *testing.T) {
	env, repo := withHarness(t)
	env.Vars["UNRELATED_SECRET"] = "leak"
	env.OK("task", "create", "--repo", "api", "--objective", "Fix the bug")
	started := env.OK("task", "start", "t_1")
	attempt := started["attempt"].(map[string]any)
	if attempt["branch"] != "shephrd/t_1/1" || attempt["workspace_state"] != "held" || started["task"].(map[string]any)["state"] != "running" {
		t.Fatalf("start: %v", started)
	}
	env.WaitState("t_1", "done")
	calls := callsFor(env, "t_1", 0)
	if len(calls) != 1 {
		t.Fatalf("harness calls: %v", env.Calls())
	}
	call := calls[0]
	if call["harness"] != "codex" || call["cwd"] != attempt["workspace"] || !strings.Contains(call["brief"].(string), "Fix the bug") ||
		!strings.Contains(call["brief"].(string), "Run Shephrd as `"+env.Bin+"`") {
		t.Fatalf("harness call: %v", call)
	}
	vars := envOf(call)
	if !strings.HasPrefix(vars["PATH"], filepath.Dir(env.Bin)+":") {
		t.Fatalf("the session's PATH does not start with Shephrd's directory: %s", vars["PATH"])
	}
	for _, key := range []string{"PATH", "HOME", "USER", "SHEPHRD_CONFIG", "SHEPHRD_RUN_TOKEN"} {
		if vars[key] == "" {
			t.Fatalf("harness environment lacks %s: %v", key, vars)
		}
	}
	if _, leaked := vars["UNRELATED_SECRET"]; leaked {
		t.Fatalf("harness environment leaked the caller's variables: %v", vars)
	}
	if head := testkit.Git(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); head != "main" {
		t.Fatalf("registered checkout moved to %s", head)
	}
	log := env.OK("task", "log", "t_1")
	if !strings.Contains(log["log"].(string), "thread.started") {
		t.Fatalf("session log: %v", log)
	}
	if run := log["run"].(map[string]any); run["exit_status"] != float64(0) || run["turn_reported"] != true {
		t.Fatalf("run: %v", run)
	}
}

func TestWorkerQuestionWaits(t *testing.T) {
	env, _ := withHarness(t)
	env.Script(map[string][][]action{"t_1": {{{"report": []string{"question", "Which API version?"}}}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "waiting")
	events := env.OK("task", "show", "t_1")["events"].([]any)
	var question map[string]any
	for _, e := range events {
		if e.(map[string]any)["name"] == "task.question" {
			question = e.(map[string]any)
		}
	}
	if question == nil || question["data"].(map[string]any)["body"] != "Which API version?" {
		t.Fatalf("question event: %v", events)
	}
}

func TestRunWithoutReportGetsOneNudgeThenIsHeld(t *testing.T) {
	env, _ := withHarness(t)
	env.Script(map[string][][]action{"t_1": {{}, {}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.OK("task", "start", "t_1")
	task := env.WaitState("t_1", "held")
	if task["reason"] != "no_report" {
		t.Fatalf("held: %v", task)
	}
	nudge := callsFor(env, "t_1", 1)
	if len(nudge) != 1 || !strings.Contains(nudge[0]["brief"].(string), "ended without reporting") {
		t.Fatalf("nudge run: %v", env.Calls())
	}
	if !slices.Contains(toStrings(nudge[0]["args"]), "resume") {
		t.Fatalf("nudge did not resume the session: %v", nudge[0]["args"])
	}
	if len(callsFor(env, "t_1", 2)) != 0 {
		t.Fatal("a second nudge ran")
	}
}

func TestNudgedRunCanStillFinish(t *testing.T) {
	env, _ := withHarness(t)
	env.Script(map[string][][]action{"t_1": {{}, {{"work": "Fix"}, {"report": []string{"result", "Done after the nudge"}}}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "done")
}

func TestStopConfirmsAndStaleRunInputIsRefused(t *testing.T) {
	env, _ := withHarness(t)
	env.Script(map[string][][]action{"t_1": {{{"report": []string{"progress", "starting"}}, {"sleep": "60s"}}, {{"work": "Fix"}, {"report": []string{"result", "resumed"}}}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	first := env.OK("task", "start", "t_1")
	env.Eventually("first progress", func() bool { return len(callsFor(env, "t_1", 0)) == 1 && len(env.Calls()) >= 2 })
	stopped := env.OK("task", "stop", "t_1")
	if len(stopped["stopped"].([]any)) != 1 {
		t.Fatalf("stop: %v", stopped)
	}
	task := env.Task("t_1")
	if task["state"] != "held" || task["reason"] != "stopped" {
		t.Fatalf("after stop: %v", task)
	}
	token := envOf(callsFor(env, "t_1", 0)[0])["SHEPHRD_RUN_TOKEN"]

	resumed := env.OK("task", "resume", "t_1")
	if resumed["attempt"].(map[string]any)["workspace"] != first["attempt"].(map[string]any)["workspace"] {
		t.Fatalf("resume changed workspace: %v", resumed)
	}
	env.WaitState("t_1", "done")
	args := toStrings(callsFor(env, "t_1", 1)[0]["args"])
	if !slices.Contains(args, "resume") || !strings.HasPrefix(args[slices.Index(args, "resume")+1], "codex-t_1-") {
		t.Fatalf("resume args: %v", args)
	}

	env.Vars["SHEPHRD_RUN_TOKEN"] = token
	env.Refused("stale_run", "report", "progress", "late")
	delete(env.Vars, "SHEPHRD_RUN_TOKEN")
	var stale bool
	for _, e := range env.OK("task", "show", "t_1")["events"].([]any) {
		stale = stale || e.(map[string]any)["name"] == "task.stale"
	}
	if !stale {
		t.Fatal("stale input was not recorded")
	}
}

func TestRetryStartsANewAttemptAndKeepsTheOld(t *testing.T) {
	env, _ := withHarness(t)
	env.Script(map[string][][]action{"t_1": {{{"report": []string{"blocker", "Missing credentials"}}}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	first := env.OK("task", "start", "t_1")["attempt"].(map[string]any)
	if task := env.WaitState("t_1", "held"); task["reason"] != "blocked" {
		t.Fatalf("blocker: %v", task)
	}
	retried := env.OK("task", "retry", "t_1", "--harness", "pi", "--model", "local")
	second := retried["attempt"].(map[string]any)
	if second["attempt"] != float64(2) || second["branch"] != "shephrd/t_1/2" || second["harness"] != "pi" || second["workspace"] == first["workspace"] {
		t.Fatalf("retry: %v", retried)
	}
	if _, err := os.Stat(first["workspace"].(string)); err != nil {
		t.Fatalf("old workspace removed: %v", err)
	}
	env.WaitState("t_1", "done")
	if call := callsFor(env, "t_1", 1)[0]; call["harness"] != "pi" || !slices.Contains(toStrings(call["args"]), "local") {
		t.Fatalf("retry harness: %v", call)
	}
}

func TestStartWaitsForDependencies(t *testing.T) {
	env, _ := withHarness(t)
	env.OK("task", "create", "--repo", "api", "--objective", "Research", "--deliverable", "report")
	env.OK("task", "create", "--repo", "api", "--objective", "Build", "--after", "t_1")
	env.Refused("not_ready", "task", "start", "t_2")
	env.Refused("not_owner", "task", "start", "t_1", "--as", "driver:other")
	env.Refused("invalid_state", "task", "resume", "t_1")
}

func TestSubDriverIsReadOnlyAndDelegates(t *testing.T) {
	env, _ := withHarness(t)
	env.Script(map[string][][]action{
		"t_1": {{
			{"write": map[string]string{"edit.txt": "should fail"}},
			{"shephrd": []string{"task", "create", "--repo", "api", "--objective", "Implement it"}},
			{"report": []string{"result", "Planned one worker"}},
			{"report": []string{"note", "Plan: t_2 implements. Waiting on t_2."}},
		}},
	})
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Own the api work", "--harness", "claude-code")
	env.OK("task", "start", "t_1")
	task := env.WaitState("t_1", "done")
	if task["state"] != "done" {
		t.Fatalf("driver: %v", task)
	}
	var wrote map[string]any
	for _, call := range env.Calls() {
		if call["write"] != nil {
			wrote = call
		}
	}
	if wrote == nil || wrote["error"] == "<nil>" {
		t.Fatalf("sub-driver could write its workspace: %v", wrote)
	}
	if errs := commandErrors(env, "t_1"); len(errs) != 0 {
		t.Fatalf("sub-driver commands failed: %v", errs)
	}
	args := toStrings(callsFor(env, "t_1", 0)[0]["args"])
	if !slices.Contains(args, "--disallowedTools") || !slices.Contains(args, "--session-id") {
		t.Fatalf("claude args: %v", args)
	}
	child := env.Task("t_2")
	if child["parent"] != "t_1" || child["request"] == nil {
		t.Fatalf("child: %v", child)
	}
	requests := env.OK("task", "show", "t_1")["requests"].([]any)
	if requests[0].(map[string]any)["state"] != "answered" {
		t.Fatalf("requests: %v", requests)
	}
	env.Refused("not_a_run", "report", "progress", "from a driver")
}

func TestSetupFailureHoldsTheTask(t *testing.T) {
	env := initialized(t)
	env.InstallHarnesses()
	env.AppendConfig("[defaults]\nharness = \"codex\"")
	env.OK("repo", "add", testkit.GitRepo(t, "api"), "--setup", "echo preparing; exit 3")
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.Refused("setup_failed", "task", "start", "t_1")
	task := env.Task("t_1")
	if task["state"] != "held" || task["reason"] != "setup_failed" {
		t.Fatalf("after setup failure: %v", task)
	}
}

func TestDivergedBaseRefusesAndChangesNothing(t *testing.T) {
	env := initialized(t)
	env.InstallHarnesses()
	env.AppendConfig("[defaults]\nharness = \"codex\"")
	origin := testkit.GitRepo(t, "origin")
	clone := filepath.Join(t.TempDir(), "api")
	testkit.Git(t, t.TempDir(), "clone", "-q", origin, clone)
	os.WriteFile(filepath.Join(origin, "upstream.txt"), []byte("upstream"), 0o644)
	testkit.Git(t, origin, "add", ".")
	testkit.Git(t, origin, "commit", "-q", "-m", "Upstream")
	os.WriteFile(filepath.Join(clone, "local.txt"), []byte("local"), 0o644)
	testkit.Git(t, clone, "add", ".")
	testkit.Git(t, clone, "commit", "-q", "-m", "Local")
	clone, _ = filepath.EvalSymlinks(clone)
	env.OK("repo", "add", clone)
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.Refused("base_diverged", "task", "start", "t_1")
	if task := env.Task("t_1"); task["state"] != "queued" || task["attempt"] != float64(0) {
		t.Fatalf("after refusal: %v", task)
	}
}

func TestSkillsPrintGuidanceAsConfigured(t *testing.T) {
	env := testkit.NewEnv(t)
	worker := env.OK("skill", "worker")
	if !strings.Contains(worker["text"].(string), "You are a worker") {
		t.Fatalf("worker guide: %v", worker)
	}
	main := env.OK("skill", "main")["text"].(string)
	if strings.Contains(main, "## Reference: recovery") || !strings.Contains(main, "recovery") {
		t.Fatalf("main skill sections: %s", main)
	}
	if section := env.OK("skill", "main", "--section", "recovery")["text"].(string); !strings.Contains(section, "task resume") {
		t.Fatalf("recovery section: %s", section)
	}
	env.OK("init", "--host", "workhorse")
	extra := filepath.Join(env.Home, "extra.md")
	os.WriteFile(extra, []byte("Always run make check.\n"), 0o644)
	env.AppendConfig("[skills.worker]\nextend = \"" + extra + "\"")
	if text := env.OK("skill", "worker")["text"].(string); !strings.HasSuffix(strings.TrimSpace(text), "Always run make check.") {
		t.Fatalf("extended guide: %s", text)
	}
}

func TestLongSubDriverTurnIsWarnedOnce(t *testing.T) {
	env, _ := withHarness(t)
	env.AppendConfig("[timeouts]\nturn_budget = \"1ms\"")
	env.Script(map[string][][]action{"t_1": {{
		{"sleep": "50ms"},
		{"shephrd": []string{"task", "list"}},
		{"shephrd": []string{"task", "list"}},
		{"report": []string{"result", "done"}},
		{"report": []string{"note", "checkpoint"}},
	}}})
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Own api")
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "done")
	var outputs []string
	for _, call := range env.Calls() {
		if call["command"] != nil && slices.Contains(toStrings(call["command"]), "list") {
			outputs = append(outputs, call["stdout"].(string))
		}
	}
	if len(outputs) != 2 || !strings.Contains(outputs[0], "turn_long") || strings.Contains(outputs[1], "turn_long") {
		t.Fatalf("turn warnings: %v", outputs)
	}
}

func TestReconcileFindsALostRun(t *testing.T) {
	env, _ := withHarness(t)
	env.Script(map[string][][]action{"t_1": {{{"sleep": "60s"}}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	run := env.OK("task", "start", "t_1")["run"].(map[string]any)
	env.Eventually("the harness to start", func() bool { return len(callsFor(env, "t_1", 0)) == 1 })
	syscall.Kill(-int(run["pid"].(float64)), syscall.SIGKILL)
	env.Eventually("the process group to be gone", func() bool { return syscall.Kill(-int(run["pid"].(float64)), 0) != nil })
	reconciled := env.OK("workspace", "reconcile")
	if runs := reconciled["runs"].([]any); len(runs) != 1 || runs[0].(map[string]any)["liveness"] != "exited" {
		t.Fatalf("reconcile: %v", reconciled)
	}
	if task := env.Task("t_1"); task["state"] != "held" || task["reason"] != "lost" {
		t.Fatalf("after reconcile: %v", task)
	}
	env.OK("task", "resume", "t_1")
	env.WaitState("t_1", "done")
}

func TestHostList(t *testing.T) {
	env, _ := withHarness(t)
	hosts := env.OK("host", "list")["hosts"].([]any)
	host := hosts[0].(map[string]any)
	if host["name"] != "workhorse" || len(host["harnesses"].([]any)) != 3 {
		t.Fatalf("hosts: %v", hosts)
	}
}
