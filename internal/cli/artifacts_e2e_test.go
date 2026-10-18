package cli_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shephrd/internal/testkit"
)

func withLanding(t *testing.T, method string) (*testkit.Env, string) {
	env, repo := withHarness(t)
	env.AppendConfig("[repos.api.landing]\nmode = \"direct\"\nmethod = \"" + method + "\"")
	return env, repo
}

func eventNames(env *testkit.Env, task string) []string {
	var names []string
	for _, e := range env.OK("task", "show", task, "--events", "200")["events"].([]any) {
		names = append(names, e.(map[string]any)["name"].(string))
	}
	return names
}

func TestCodeIsSealedLandedAndReleased(t *testing.T) {
	env, repo := withLanding(t, "fast-forward")
	env.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	workspace := env.OK("task", "start", "t_1")["attempt"].(map[string]any)["workspace"].(string)
	env.WaitState("t_1", "done")
	art := env.OK("task", "show", "t_1")["artifact"].(map[string]any)
	if art["kind"] != "code" || art["branch"] != "shephrd/t_1/1" || art["commit"] == "" {
		t.Fatalf("sealed artifact: %v", art)
	}
	r := env.Run("", "task", "accept", "t_1")
	if r.Code != 0 {
		t.Fatalf("accept: %+v", r)
	}
	out := r.JSON(t)
	if task := out["task"].(map[string]any); task["state"] != "closed" || task["reason"] != "accepted" || task["milestone"] != "merged" {
		t.Fatalf("accepted task: %v", out)
	}
	if !strings.Contains(r.Stdout, "checkout_behind") {
		t.Fatalf("no warning about the registered checkout: %s", r.Stdout)
	}
	if head := testkit.Git(t, repo, "rev-parse", "refs/heads/main"); head != art["commit"] {
		t.Fatalf("main is %s, want %s", head, art["commit"])
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace not released: %v", err)
	}
	testkit.Git(t, repo, "rev-parse", "--verify", "shephrd/t_1/1")
	names := strings.Join(eventNames(env, "t_1"), " ")
	for _, want := range []string{"artifact.sealed", "grant.used", "land.attempted", "task.published", "task.delivered", "workspace.state"} {
		if !strings.Contains(names, want) {
			t.Fatalf("missing %s in %s", want, names)
		}
	}
}

func TestAcceptReportAndLegacyClosure(t *testing.T) {
	env, _ := withHarness(t)
	env.OK("task", "create", "--repo", "api", "--objective", "Research", "--deliverable", "report")
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "done")
	art := env.OK("task", "show", "t_1")["artifact"].(map[string]any)["id"]
	accepted := env.OK("task", "accept", "t_1")
	if task := accepted["task"].(map[string]any); task["reason"] != "accepted" || task["milestone"] != "merged" {
		t.Fatalf("report acceptance: %v", task)
	}
	env.Refused("invalid_state", "task", "accept", "t_1")
	env.Refused("invalid_state", "task", "deliver", "t_1")
	db, err := sql.Open("sqlite", "file:"+filepath.Join(env.Home, ".local", "state", "shephrd", "shephrd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE tasks SET reason = 'delivered' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	shown := env.OK("task", "show", "t_1")
	if task := shown["task"].(map[string]any); task["reason"] != "delivered" || task["milestone"] != "merged" || shown["artifact"].(map[string]any)["id"] != art {
		t.Fatalf("historical closure: %v", shown)
	}
	if !strings.Contains(strings.Join(eventNames(env, "t_1"), " "), "task.delivered") {
		t.Fatal("historical acceptance event was lost")
	}
	env.Refused("invalid_state", "task", "accept", "t_1")
}

func TestDirtyCodeResultIsRefusedUntilCommitted(t *testing.T) {
	env, _ := withLanding(t, "fast-forward")
	env.Script(map[string][][]action{"t_1": {{
		{"write": map[string]string{"fix.go": "package fix\n"}},
		{"report": []string{"result", "Fixed"}},
		{"git": []string{"add", "fix.go"}},
		{"git": []string{"commit", "-q", "-m", "Fix"}},
		{"report": []string{"result", "Fixed and committed"}},
	}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "done")
	errs := commandErrors(env, "t_1")
	if len(errs) != 1 || !strings.Contains(errs[0], "uncommitted changes") {
		t.Fatalf("refusals: %v", errs)
	}
}

func TestDeliveryRefusals(t *testing.T) {
	env, _ := withHarness(t)
	env.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	env.Refused("invalid_state", "task", "deliver", "t_1")
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "done")
	env.Refused("landing_not_configured", "task", "deliver", "t_1")
	env.OK("task", "create", "--repo", "api", "--objective", "Theirs", "--as", "driver:other")
	env.OK("task", "start", "t_2", "--as", "driver:other")
	env.WaitState("t_2", "done")
	env.Refused("not_granted", "task", "deliver", "t_2", "--as", "driver:other")
	env.Refused("not_owner", "task", "deliver", "t_2")
}

func TestPRPublicationFlagsRequirePullRequestCode(t *testing.T) {
	env, repo := withLanding(t, "fast-forward")
	env.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "done")
	for _, args := range [][]string{{"--title", "Real title"}, {"--body", "Description"}, {"--draft"}, {"--draft=false"}} {
		env.Refused("usage", append([]string{"task", "accept", "t_1"}, args...)...)
	}
	env.Refused("usage", "task", "accept", "t_1", "--title", "")
	env.Refused("too_large", "task", "accept", "t_1", "--title", strings.Repeat("x", 201))
	if head := testkit.Git(t, repo, "rev-parse", "HEAD"); head != testkit.Git(t, repo, "rev-parse", "main") {
		t.Fatalf("refused flags landed code: %s", head)
	}
	if len(env.OK("task", "show", "t_1")["landings"].([]any)) != 0 {
		t.Fatal("refused controls recorded a landing")
	}
	env.OK("task", "create", "--repo", "api", "--deliverable", "report", "--objective", "Research")
	env.OK("task", "start", "t_2")
	env.WaitState("t_2", "done")
	env.Refused("usage", "task", "deliver", "t_2", "--draft")
}

func TestFastForwardRefusedWhenTheBranchMoved(t *testing.T) {
	env, repo := withLanding(t, "fast-forward")
	env.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "done")
	os.WriteFile(filepath.Join(repo, "other.txt"), []byte("moved"), 0o644)
	testkit.Git(t, repo, "add", "other.txt")
	testkit.Git(t, repo, "commit", "-q", "-m", "Move main")
	env.Refused("landing_failed", "task", "deliver", "t_1")
	if task := env.Task("t_1"); task["state"] != "done" {
		t.Fatalf("after a failed landing: %v", task)
	}
}

func TestMergeLandsOnTheRemote(t *testing.T) {
	env := initialized(t)
	env.InstallHarnesses()
	env.AppendConfig("[defaults]\nharness = \"codex\"\n\n[repos.api.landing]\nmode = \"direct\"\nmethod = \"merge\"")
	origin := filepath.Join(t.TempDir(), "origin.git")
	testkit.Git(t, t.TempDir(), "init", "-q", "--bare", "-b", "main", origin)
	seed := testkit.GitRepo(t, "seed")
	testkit.Git(t, seed, "push", "-q", origin, "main")
	clone := filepath.Join(t.TempDir(), "api")
	testkit.Git(t, t.TempDir(), "clone", "-q", origin, clone)
	clone, _ = filepath.EvalSymlinks(clone)
	env.OK("repo", "add", clone)
	env.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "done")
	os.WriteFile(filepath.Join(seed, "upstream.txt"), []byte("upstream"), 0o644)
	testkit.Git(t, seed, "add", "upstream.txt")
	testkit.Git(t, seed, "commit", "-q", "-m", "Upstream")
	testkit.Git(t, seed, "push", "-q", origin, "main")
	out := env.OK("task", "deliver", "t_1")
	commit := out["artifact"].(map[string]any)["commit"].(string)
	tip := testkit.Git(t, origin, "rev-parse", "main")
	if tip == commit || out["landing"].(map[string]any)["tip"] != tip {
		t.Fatalf("expected a merge commit on origin: %v", out)
	}
	testkit.Git(t, origin, "merge-base", "--is-ancestor", commit, "main")
	if head := testkit.Git(t, clone, "rev-parse", "HEAD"); head == tip {
		t.Fatal("the registered checkout was moved")
	}
}

func TestReportPassesToItsDependentByReference(t *testing.T) {
	env, _ := withHarness(t)
	env.OK("task", "create", "--repo", "api", "--objective", "Research", "--deliverable", "report")
	env.OK("task", "create", "--repo", "api", "--objective", "Build on it", "--after", "t_1")
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "done")
	art := env.OK("task", "show", "t_1")["artifact"].(map[string]any)
	if read := env.OK("artifact", "read", art["id"].(string)); read["content"] != "# Findings\n" {
		t.Fatalf("artifact read: %v", read)
	}
	delivered := env.OK("task", "deliver", "t_1")
	if len(delivered["released"].([]any)) != 1 {
		t.Fatalf("a workspace holding only its sealed report was kept: %v", delivered)
	}
	if !strings.Contains(strings.Join(eventNames(env, "t_2"), " "), "task.ready") {
		t.Fatal("no task.ready for the dependent")
	}
	workspace := env.OK("task", "start", "t_2")["attempt"].(map[string]any)["workspace"].(string)
	env.WaitState("t_2", "done")
	brief := callsFor(env, "t_2", 0)[0]["brief"].(string)
	input := filepath.Join(workspace+".inputs", "t_1", "report.md")
	if !strings.Contains(brief, workspace+".inputs") {
		t.Fatalf("brief does not list the input:\n%s", brief)
	}
	if body, err := os.ReadFile(input); err != nil || string(body) != "# Findings\n" {
		t.Fatalf("pinned input: %q, %v", body, err)
	}
	if info, _ := os.Stat(input); info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("pinned input is writable: %v", info.Mode())
	}
}

func TestSubDriverLandsWithADelegatedGrant(t *testing.T) {
	env, _ := withLanding(t, "fast-forward")
	env.AppendConfig("[timeouts]\nsettle = \"300ms\"")
	env.Script(map[string][][]action{
		"t_1": {
			{
				{"shephrd": []string{"task", "create", "--repo", "api", "--objective", "Implement"}},
				{"shephrd": []string{"task", "start", "t_2"}},
				{"report": []string{"note", "Waiting on t_2"}},
			},
			{
				{"shephrd": []string{"task", "deliver", "t_2"}},
				{"report": []string{"result", "Landed t_2"}},
				{"report": []string{"note", "Done"}},
			},
		},
	})
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Own api")
	env.Refused("usage", "grant", "land", "--to", "t_1")
	grant := env.OK("grant", "land", "--to", "t_1", "--reason", "The user approved landing api work")
	if grant["id"] != "g_1" || grant["to"] != "t_1" {
		t.Fatalf("grant: %v", grant)
	}
	env.OK("task", "start", "t_1")
	env.WaitState("t_2", "done")
	env.StartDaemon()
	env.WaitState("t_1", "done")
	if errs := commandErrors(env, "t_1"); len(errs) != 0 {
		t.Fatalf("sub-driver commands failed: %v", errs)
	}
	if task := env.Task("t_2"); task["reason"] != "accepted" {
		t.Fatalf("worker: %v", task)
	}
	if revoked := env.OK("grant", "revoke", "g_1"); revoked["revoked"] == "" {
		t.Fatalf("revoke: %v", revoked)
	}
}

func TestSubDriverAcceptsChildrenBeforeParent(t *testing.T) {
	env, repo := withHarness(t)
	env.AppendConfig("[timeouts]\nsettle = \"300ms\"")
	observe := subDriverDiagnostic(t, env, 300*time.Millisecond)
	env.Script(map[string][][]action{"t_1": {
		{
			{"shephrd": []string{"task", "create", "--repo", "api", "--objective", "Research", "--deliverable", "report"}},
			{"shephrd": []string{"task", "create", "--repo", "api", "--objective", "Implement"}},
			{"shephrd": []string{"task", "start", "t_2"}},
			{"shephrd": []string{"task", "start", "t_3"}},
			{"report": []string{"result", "Children produced results"}},
		},
		{{"report": []string{"note", "Children remain open pending review"}}},
		{
			{"shephrd": []string{"task", "accept", "t_2"}},
			{"shephrd": []string{"task", "accept", "t_2"}},
			{"shephrd": []string{"task", "verify", "t_3"}},
			{"report": []string{"result", "Reviewed the report and proved code on main"}},
		},
		{{"report": []string{"note", "All children closed"}}},
	}})
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Supervise")
	env.OK("task", "start", "t_1")
	env.WaitState("t_2", "done", observe)
	env.WaitState("t_3", "done", observe)
	env.StartDaemon()
	env.WaitState("t_1", "done", observe)
	env.Refused("open_children", "task", "accept", "t_1")
	env.Refused("not_owner", "task", "accept", "t_2")
	env.Refused("not_owner", "task", "accept", "t_3")
	if task := env.Task("t_3"); task["milestone"] != nil || task["state"] != "done" {
		t.Fatalf("unmerged child was implicitly accepted: %v", task)
	}
	art := env.OK("task", "show", "t_3")["artifact"].(map[string]any)
	testkit.Git(t, repo, "merge", "-q", "--no-ff", "-m", "External merge", "shephrd/t_3/1")
	env.OK("task", "send", "t_1", "Review and close your children")
	env.Eventually("sub-driver to accept both children", func() bool {
		return env.Task("t_2")["state"] == "closed" && env.Task("t_3")["state"] == "closed"
	}, observe)
	env.WaitState("t_1", "done", observe)
	if errs := commandErrors(env, "t_1"); len(errs) != 1 || !strings.Contains(errs[0], `"kind":"invalid_state"`) {
		t.Fatalf("unexpected repeat acceptance response: %v", errs)
	}
	for _, ref := range []string{"t_2", "t_3"} {
		if task := env.Task(ref); task["reason"] != "accepted" {
			t.Fatalf("child %s: %v", ref, task)
		}
	}
	if shown := env.OK("task", "show", "t_3"); shown["artifact"].(map[string]any)["id"] != art["id"] {
		t.Fatalf("proof changed the sealed artifact: %v", shown)
	}
	out := env.OK("task", "accept", "t_1")
	if task := out["task"].(map[string]any); task["reason"] != "accepted" || task["state"] != "closed" {
		t.Fatalf("parent acceptance: %v", task)
	}
	env.Refused("invalid_state", "task", "accept", "t_1")
}

func TestWaitingChildDoesNotCloseWithParent(t *testing.T) {
	env, _ := withHarness(t)
	env.AppendConfig("[timeouts]\nsettle = \"300ms\"")
	env.Script(map[string][][]action{
		"t_1": {
			{
				{"shephrd": []string{"task", "create", "--repo", "api", "--objective", "Investigate", "--deliverable", "report"}},
				{"shephrd": []string{"task", "start", "t_2"}},
				{"report": []string{"result", "Investigation needs an answer"}},
				{"report": []string{"note", "Waiting for the child's question to be resolved"}},
			},
			{{"report": []string{"note", "Child still needs an answer"}}},
		},
		"t_2": {{{"report": []string{"question", "Which behavior?"}}}},
	})
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Supervise")
	env.OK("task", "start", "t_1")
	env.WaitState("t_2", "waiting")
	env.StartDaemon()
	env.WaitState("t_1", "done")
	env.Refused("open_children", "task", "accept", "t_1")
	if task := env.Task("t_2"); task["state"] != "waiting" {
		t.Fatalf("waiting child was closed: %v", task)
	}
}

func TestDiscardRemovesEvenDirtyWorkspaces(t *testing.T) {
	env, repo := withLanding(t, "fast-forward")
	env.Script(map[string][][]action{"t_1": {{
		{"write": map[string]string{"half.txt": "unfinished"}},
		{"report": []string{"blocker", "Stuck"}},
	}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	workspace := env.OK("task", "start", "t_1")["attempt"].(map[string]any)["workspace"].(string)
	env.WaitState("t_1", "held")
	env.Refused("invalid_state", "task", "cancel", "t_1")
	out := env.OK("task", "discard", "t_1")
	if out["task"].(map[string]any)["reason"] != "discarded" || len(out["released"].([]any)) != 1 {
		t.Fatalf("discard: %v", out)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatal("discarded workspace kept")
	}
	testkit.Git(t, repo, "rev-parse", "--verify", "shephrd/t_1/1")
}

func TestDirtyWorkspaceIsRetainedOnDelivery(t *testing.T) {
	env, _ := withLanding(t, "fast-forward")
	env.Script(map[string][][]action{"t_1": {{
		{"work": "Fix"},
		{"report": []string{"result", "Fixed"}},
		{"write": map[string]string{"scratch.txt": "notes"}},
	}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	workspace := env.OK("task", "start", "t_1")["attempt"].(map[string]any)["workspace"].(string)
	env.WaitState("t_1", "done")
	out := env.OK("task", "deliver", "t_1")
	if len(out["retained"].([]any)) != 1 {
		t.Fatalf("dirty workspace released: %v", out)
	}
	var retained bool
	for _, item := range env.OK("inbox")["items"].([]any) {
		retained = retained || item.(map[string]any)["event"].(map[string]any)["name"] == "workspace.retained"
	}
	if !retained {
		t.Fatal("the owner was not told about the retained workspace")
	}
	env.OK("task", "discard", "t_1", "--attempt", "1")
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatal("workspace kept after discard --attempt")
	}
}

func TestVerifyProvesAMergeDoneOutsideShephrd(t *testing.T) {
	env, repo := withHarness(t)
	env.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "done")
	env.Refused("not_proven", "task", "verify", "t_1")
	testkit.Git(t, repo, "merge", "-q", "--no-ff", "-m", "Merge by hand", "shephrd/t_1/1")
	out := env.OK("task", "verify", "t_1")
	if out["task"].(map[string]any)["reason"] != "accepted" {
		t.Fatalf("verify: %v", out)
	}
}
