package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/testkit"
)

type forgeEnv struct {
	*testkit.Env
	origin, clone string
}

// withGitHub registers a repository whose origin is a bare repository, and
// declares the first-party GitHub forge plugin with a fake gh on PATH.
func withGitHub(t *testing.T, merge, method string) *forgeEnv {
	env := initialized(t)
	env.InstallHarnesses()
	gh, err := os.ReadFile(testkit.Tool(t, "./internal/testkit/fakegh", "fakegh"))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(env.Home, "ghbin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "gh"), gh, 0o755)
	env.Path = append([]string{bin}, env.Path...)

	pkg, _ := filepath.EvalSymlinks(t.TempDir())
	plugin := filepath.Join(pkg, "plugins", "github")
	os.MkdirAll(filepath.Join(plugin, "bin"), 0o755)
	exe, _ := os.ReadFile(testkit.Tool(t, "./plugins/github", "shephrd-github"))
	os.WriteFile(filepath.Join(plugin, "bin", "shephrd-github"), exe, 0o755)
	for _, name := range []string{"plugin.toml", "skill.md"} {
		body, _ := os.ReadFile(filepath.Join("..", "..", "plugins", "github", name))
		os.WriteFile(filepath.Join(plugin, name), body, 0o644)
	}

	f := &forgeEnv{Env: env, origin: filepath.Join(t.TempDir(), "origin.git")}
	testkit.Git(t, t.TempDir(), "init", "-q", "--bare", "-b", "main", f.origin)
	seed := testkit.GitRepo(t, "seed")
	testkit.Git(t, seed, "push", "-q", f.origin, "main")
	f.clone = filepath.Join(t.TempDir(), "api")
	testkit.Git(t, t.TempDir(), "clone", "-q", f.origin, f.clone)
	f.clone, _ = filepath.EvalSymlinks(f.clone)
	env.OK("repo", "add", f.clone)
	env.AppendConfig(`[defaults]
harness = "codex"

[repos.api.landing]
mode = "pull_request"
forge = "github"
merge = "` + merge + `"

[packages.firstparty]
path = "` + pkg + `"

[plugins.github]
package = "firstparty"

[plugins.github.options]
merge_method = "` + method + `"`)
	return f
}

func (f *forgeEnv) pullRequests(t *testing.T) []map[string]any {
	body, _ := os.ReadFile(filepath.Join(f.Home, ".fakegh", "prs.json"))
	var prs []map[string]any
	json.Unmarshal(body, &prs)
	return prs
}

func (f *forgeEnv) gh(t *testing.T, args ...string) {
	cmd := exec.Command(filepath.Join(f.Home, "ghbin", "gh"), args...)
	cmd.Env = []string{"HOME=" + f.Home, "PATH=" + os.Getenv("PATH")}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gh %v: %v\n%s", args, err, out)
	}
}

func TestStackedPullRequestsAndASquashMergeProvenByTheForge(t *testing.T) {
	f := withGitHub(t, "external", "squash")
	f.OK("task", "create", "--repo", "api", "--objective", "Add the API")
	f.OK("task", "create", "--repo", "api", "--objective", "Use the API", "--after", "t_1")
	f.OK("task", "start", "t_1")
	f.WaitState("t_1", "done")
	first := f.OK("task", "deliver", "t_1")
	if pr := first["pull_request"].(map[string]any); pr["ref"] != "1" || pr["target"] != "main" {
		t.Fatalf("first pull request: %v", first)
	}
	if task := f.Task("t_1"); task["state"] != "done" || task["milestone"] != "published" {
		t.Fatalf("published task: %v", task)
	}
	commit := f.OK("task", "show", "t_1")["artifact"].(map[string]any)["commit"].(string)

	second := f.OK("task", "start", "t_2")["attempt"].(map[string]any)
	if second["base"] != commit {
		t.Fatalf("t_2 did not stack on t_1's sealed commit: %v", second)
	}
	f.WaitState("t_2", "done")
	stacked := f.OK("task", "deliver", "t_2")
	if pr := stacked["pull_request"].(map[string]any); pr["ref"] != "2" || pr["target"] != "shephrd/t_1/1" {
		t.Fatalf("stacked pull request: %v", stacked)
	}

	f.Refused("not_proven", "task", "verify", "t_1")
	f.gh(t, "pr", "merge", "1", "--repo", f.origin, "--squash")
	verified := f.OK("task", "verify", "t_1")
	if verified["task"].(map[string]any)["reason"] != "delivered" {
		t.Fatalf("verify: %v", verified)
	}
	landings := f.OK("task", "show", "t_1")["landings"].([]any)
	last := landings[len(landings)-1].(map[string]any)
	if last["proof"] != "recorded_merge" || last["proof_source"] != "forge:github" {
		t.Fatalf("proof: %v", landings)
	}
	if prs := f.pullRequests(t); prs[1]["base"] != "main" {
		t.Fatalf("stacked pull request was not retargeted: %v", prs)
	}
	if !strings.Contains(strings.Join(eventNames(f.Env, "t_2"), " "), "dependency.changed") {
		t.Fatal("the stacked task's owner was not told its dependency was rewritten")
	}
}

func TestShephrdMergesOnceThePullRequestIsMergeable(t *testing.T) {
	f := withGitHub(t, "shephrd", "merge")
	os.MkdirAll(filepath.Join(f.Home, ".fakegh"), 0o700)
	os.WriteFile(filepath.Join(f.Home, ".fakegh", "blocked"), nil, 0o600)
	f.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	f.OK("task", "start", "t_1")
	f.WaitState("t_1", "done")
	pending := f.OK("task", "deliver", "t_1", "--key", "first")
	if pending["pending"] == nil || pending["task"].(map[string]any)["state"] != "done" {
		t.Fatalf("delivery while checks are pending: %v", pending)
	}
	os.Remove(filepath.Join(f.Home, ".fakegh", "blocked"))
	merged := f.OK("task", "deliver", "t_1", "--key", "second")
	if merged["task"].(map[string]any)["reason"] != "delivered" || merged["pull_request"].(map[string]any)["ref"] != "1" {
		t.Fatalf("delivery once mergeable: %v", merged)
	}
	if len(f.pullRequests(t)) != 1 {
		t.Fatalf("a second pull request was opened: %v", f.pullRequests(t))
	}
	commit := merged["artifact"].(map[string]any)["commit"].(string)
	testkit.Git(t, f.origin, "merge-base", "--is-ancestor", commit, "main")
}
