package cli_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/testkit"

	_ "modernc.org/sqlite"
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

func TestPublishPullRequestMetadataAndRetries(t *testing.T) {
	f := withGitHub(t, "external", "merge")
	f.OK("task", "create", "--repo", "api", "--title", "Fix API", "--objective", "Fix it")
	f.OK("task", "start", "t_1")
	f.WaitState("t_1", "done")
	first := f.OK("task", "accept", "t_1", "--title", "Actual PR title", "--body", "Details for reviewers", "--draft", "--key", "initial")
	if pr := first["pull_request"].(map[string]any); pr["ref"] != "1" || pr["state"] != "open" {
		t.Fatalf("publication: %v", first)
	}
	p := f.pullRequests(t)[0]
	if p["title"] != "Actual PR title" || p["body"] != "Details for reviewers" || p["isDraft"] != true {
		t.Fatalf("created PR metadata: %v", p)
	}
	f.OK("task", "accept", "t_1", "--title", "Actual PR title", "--body", "Details for reviewers", "--draft", "--key", "initial")
	f.OK("task", "deliver", "t_1", "--title", "Actual PR title", "--body", "Details for reviewers", "--draft", "--key", "repeat")
	f.Refused("landing_failed", "task", "accept", "t_1", "--draft", "--title", "Other title")
	f.Refused("landing_failed", "task", "accept", "t_1", "--body", "Other body")
	f.Refused("landing_failed", "task", "accept", "t_1", "--draft=false")
	f.editPRs(t, func(prs []map[string]any) []map[string]any { prs[0]["isDraft"] = false; return prs })
	f.Refused("landing_failed", "task", "accept", "t_1", "--draft")
	f.OK("task", "accept", "t_1", "--draft=false")
	if prs := f.pullRequests(t); len(prs) != 1 || prs[0]["title"] != "Actual PR title" || prs[0]["body"] != "Details for reviewers" || prs[0]["isDraft"] != false {
		t.Fatalf("retry changed existing PR metadata: %v", prs)
	}
	f.editPRs(t, func(prs []map[string]any) []map[string]any { prs[0]["state"] = "CLOSED"; return prs })
	f.Refused("landing_failed", "task", "accept", "t_1")
	if len(f.pullRequests(t)) != 1 {
		t.Fatal("closed recorded PR was replaced")
	}
}

func TestPublishDefaultsAndDiscoveredPullRequest(t *testing.T) {
	f := withGitHub(t, "external", "merge")
	f.OK("task", "create", "--repo", "api", "--title", "Fix API", "--objective", "Fix it")
	f.OK("task", "start", "t_1")
	f.WaitState("t_1", "done")
	art := f.OK("task", "show", "t_1")["artifact"].(map[string]any)
	f.OK("task", "deliver", "t_1")
	p := f.pullRequests(t)[0]
	if p["title"] != "t_1: Fix API" || p["body"] != art["text"] || p["isDraft"] != false {
		t.Fatalf("default publication metadata: %v", p)
	}

	f.OK("task", "create", "--repo", "api", "--objective", "Preexisting PR")
	f.OK("task", "start", "t_2")
	f.WaitState("t_2", "done")
	commit := f.OK("task", "show", "t_2")["artifact"].(map[string]any)["commit"].(string)
	testkit.Git(t, f.clone, "push", "-q", f.origin, commit+":refs/heads/shephrd/t_2/1")
	f.gh(t, "pr", "create", "--repo", f.origin, "--head", "shephrd/t_2/1", "--base", "main", "--title", "External title", "--body", "External body")
	f.Refused("landing_failed", "task", "accept", "t_2", "--draft")
	f.Refused("landing_failed", "task", "accept", "t_2", "--title", "Different")
	f.OK("task", "accept", "t_2", "--title", "External title", "--body", "External body")
	if prs := f.pullRequests(t); len(prs) != 2 || prs[1]["title"] != "External title" || prs[1]["body"] != "External body" || prs[1]["isDraft"] != false {
		t.Fatalf("discovered PR changed or duplicated: %v", prs)
	}
	f.OK("task", "create", "--repo", "api", "--objective", "Another change")
	f.OK("task", "start", "t_3")
	f.WaitState("t_3", "done")
	out := f.Run("Multiline description\nfrom stdin\n", "task", "accept", "t_3", "--body", "-")
	if out.Code != 0 {
		t.Fatalf("stdin body: %v", out)
	}
	last := f.pullRequests(t)[2]
	if last["title"] != "t_3: Another change" || last["body"] != "Multiline description\nfrom stdin\n" || last["isDraft"] != false {
		t.Fatalf("mixed default and supplied PR fields: %v", last)
	}
}

func TestDraftPullRequestWaitsForReviewBeforeShephrdMerge(t *testing.T) {
	f := withGitHub(t, "shephrd", "merge")
	f.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	f.OK("task", "start", "t_1")
	f.WaitState("t_1", "done")
	out := f.OK("task", "accept", "t_1", "--draft")
	if out["pending"] != "the pull request is a draft" || f.Task("t_1")["state"] != "done" {
		t.Fatalf("draft was merged: %v", out)
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
	if verified["task"].(map[string]any)["reason"] != "accepted" {
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

func TestStackedChildSquashProvesOnlyItsSealedDelta(t *testing.T) {
	f := withGitHub(t, "external", "squash")
	f.OK("task", "create", "--repo", "api", "--objective", "Parent")
	f.OK("task", "create", "--repo", "api", "--objective", "Child", "--after", "t_1")
	f.OK("task", "start", "t_1")
	f.WaitState("t_1", "done")
	parent := f.OK("task", "accept", "t_1")["artifact"].(map[string]any)
	f.OK("task", "start", "t_2")
	f.WaitState("t_2", "done")
	child := f.OK("task", "accept", "t_2")["artifact"].(map[string]any)
	if child["base"] != parent["commit"] {
		t.Fatalf("child base: %v; parent: %v", child, parent)
	}
	f.gh(t, "pr", "merge", "1", "--repo", f.origin, "--squash")
	f.OK("task", "verify", "t_1")
	if !strings.Contains(strings.Join(eventNames(f.Env, "t_2"), " "), "dependency.changed") {
		t.Fatal("child owner was not told parent history changed")
	}
	f.checkoutMain(t)
	if err := os.WriteFile(filepath.Join(f.clone, "unrelated.txt"), []byte("before child"), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.Git(t, f.clone, "add", "unrelated.txt")
	testkit.Git(t, f.clone, "commit", "-q", "-m", "unrelated")
	testkit.Git(t, f.clone, "push", "-q", f.origin, "main")
	f.gh(t, "pr", "merge", "2", "--repo", f.origin, "--squash")
	f.OK("task", "verify", "t_2")
	proof := f.OK("task", "show", "t_2")["landings"].([]any)[1].(map[string]any)
	if proof["sealed_base"] != parent["commit"] || proof["sealed_commit"] != child["commit"] || proof["expected_tree"] != proof["carrier_tree"] {
		t.Fatalf("child proof included parent delta: %v", proof)
	}
}

func (f *forgeEnv) editPRs(t *testing.T, change func([]map[string]any) []map[string]any) {
	t.Helper()
	path := filepath.Join(f.Home, ".fakegh", "prs.json")
	body, err := json.Marshal(change(f.pullRequests(t)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *forgeEnv) database(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(f.Home, ".local", "state", "shephrd", "shephrd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func (f *forgeEnv) refusedProof(t *testing.T, task string, prs int) {
	t.Helper()
	f.Refused("not_proven", "task", "verify", task)
	shown := f.OK("task", "show", task)
	if shown["task"].(map[string]any)["state"] != "done" || len(f.pullRequests(t)) != prs {
		t.Fatalf("verification changed task or published another PR: %v", shown)
	}
	for _, item := range shown["landings"].([]any) {
		if item.(map[string]any)["proof"] != nil {
			t.Fatalf("unproven landing recorded: %v", shown)
		}
	}
}

func (f *forgeEnv) checkoutMain(t *testing.T) {
	t.Helper()
	testkit.Git(t, f.clone, "fetch", "-q", f.origin, "main")
	testkit.Git(t, f.clone, "checkout", "-q", "-B", "main", "FETCH_HEAD")
}

func TestExternalSquashWithMovedHeadClosesByArtifactDelta(t *testing.T) {
	f := withGitHub(t, "external", "squash")
	f.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	f.OK("task", "start", "t_1")
	f.WaitState("t_1", "done")
	f.OK("task", "accept", "t_1")
	f.gh(t, "pr", "merge", "1", "--repo", f.origin, "--squash")
	branch := "shephrd/t_1/1"
	testkit.Git(t, f.clone, "fetch", "-q", f.origin, branch)
	testkit.Git(t, f.clone, "checkout", "-q", "-B", "moved", "FETCH_HEAD")
	if err := os.WriteFile(filepath.Join(f.clone, "moved.txt"), []byte("after merge"), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.Git(t, f.clone, "add", "moved.txt")
	testkit.Git(t, f.clone, "commit", "-q", "-m", "move head")
	testkit.Git(t, f.clone, "push", "-q", f.origin, "HEAD:refs/heads/"+branch)
	f.checkoutMain(t)
	if err := os.WriteFile(filepath.Join(f.clone, "later.txt"), []byte("later unrelated commit"), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.Git(t, f.clone, "add", "later.txt")
	testkit.Git(t, f.clone, "commit", "-q", "-m", "later")
	testkit.Git(t, f.clone, "push", "-q", f.origin, "main")
	verified := f.OK("task", "verify", "t_1")
	if verified["task"].(map[string]any)["reason"] != "accepted" {
		t.Fatalf("squashed task not accepted: %v", verified)
	}
	shown := f.OK("task", "show", "t_1")
	proof := shown["landings"].([]any)[1].(map[string]any)
	if proof["sealed_base"] == nil || proof["sealed_commit"] != verified["artifact"].(map[string]any)["commit"] || proof["expected_tree"] != proof["carrier_tree"] || proof["premerge_parent"] == nil {
		t.Fatalf("missing delta evidence: %v", proof)
	}
}

func TestExternalSquashWithDeletedHeadCloses(t *testing.T) {
	f := withGitHub(t, "external", "squash")
	f.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	f.OK("task", "start", "t_1")
	f.WaitState("t_1", "done")
	f.OK("task", "accept", "t_1")
	f.gh(t, "pr", "merge", "1", "--repo", f.origin, "--squash")
	testkit.Git(t, f.clone, "push", "-q", f.origin, ":refs/heads/shephrd/t_1/1")
	f.OK("task", "verify", "t_1")
}

func TestSingleCommitRebaseWithExactDelta(t *testing.T) {
	f := withGitHub(t, "external", "rebase")
	f.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	f.OK("task", "start", "t_1")
	f.WaitState("t_1", "done")
	f.OK("task", "accept", "t_1")
	f.checkoutMain(t)
	if err := os.WriteFile(filepath.Join(f.clone, "unrelated.txt"), []byte("before rebase"), 0o644); err != nil {
		t.Fatal(err)
	}
	testkit.Git(t, f.clone, "add", "unrelated.txt")
	testkit.Git(t, f.clone, "commit", "-q", "-m", "unrelated")
	testkit.Git(t, f.clone, "push", "-q", f.origin, "main")
	f.gh(t, "pr", "merge", "1", "--repo", f.origin, "--rebase")
	if f.OK("task", "verify", "t_1")["task"].(map[string]any)["reason"] != "accepted" {
		t.Fatal("single commit rebase not accepted")
	}
}

func TestMultiCommitRebaseWithoutRangeEvidenceRefused(t *testing.T) {
	f := withGitHub(t, "external", "rebase")
	f.Script(map[string][][]map[string]any{"t_1": {{
		{"work": "first change"}, {"work": "second change"}, {"report": []string{"result", "done"}},
	}}})
	f.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	f.OK("task", "start", "t_1")
	f.WaitState("t_1", "done")
	f.OK("task", "accept", "t_1")
	f.gh(t, "pr", "merge", "1", "--repo", f.origin, "--rebase")
	f.refusedProof(t, "t_1", 1)
}

func TestExternalSquashRefusesUnprovenEvidence(t *testing.T) {
	for _, variant := range []string{"no record", "unreadable", "closed", "wrong artifact", "wrong PR", "wrong URL", "no carrier", "unreachable", "extra", "omitted", "changed"} {
		t.Run(variant, func(t *testing.T) {
			f := withGitHub(t, "external", "squash")
			f.OK("task", "create", "--repo", "api", "--objective", "Fix it")
			f.OK("task", "start", "t_1")
			f.WaitState("t_1", "done")
			art := f.OK("task", "show", "t_1")["artifact"].(map[string]any)
			if variant == "no record" {
				testkit.Git(t, f.clone, "push", "-q", f.origin, art["commit"].(string)+":refs/heads/shephrd/t_1/1")
				f.gh(t, "pr", "create", "--repo", f.origin, "--head", "shephrd/t_1/1", "--base", "main", "--title", "Unrecorded")
			} else {
				f.OK("task", "accept", "t_1")
			}
			f.gh(t, "pr", "merge", "1", "--repo", f.origin, "--squash")
			switch variant {
			case "unreadable":
				f.editPRs(t, func(prs []map[string]any) []map[string]any { prs[0]["repo"] = "unavailable"; return prs })
			case "closed":
				f.editPRs(t, func(prs []map[string]any) []map[string]any { prs[0]["state"] = "CLOSED"; return prs })
			case "wrong artifact", "wrong PR", "wrong URL":
				if variant == "wrong artifact" {
					f.OK("task", "create", "--repo", "api", "--objective", "Different artifact")
					f.OK("task", "start", "t_2")
					f.WaitState("t_2", "done")
				}
				db := f.database(t)
				statement := map[string]string{
					"wrong artifact": `UPDATE landings SET artifact = (SELECT id FROM artifacts WHERE task = 2) WHERE task = 1`,
					"wrong PR":       `UPDATE landings SET pull_request = '999' WHERE task = 1`,
					"wrong URL":      `UPDATE landings SET detail = 'https://example.test/pull/other' WHERE task = 1`,
				}[variant]
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			case "no carrier":
				f.editPRs(t, func(prs []map[string]any) []map[string]any { prs[0]["merge_commit"] = ""; return prs })
			case "unreachable":
				f.checkoutMain(t)
				testkit.Git(t, f.clone, "reset", "--hard", "HEAD^")
				testkit.Git(t, f.clone, "push", "-q", "--force", f.origin, "main")
			case "extra", "omitted", "changed":
				f.checkoutMain(t)
				if variant == "omitted" {
					testkit.Git(t, f.clone, "reset", "--hard", "HEAD^")
				} else {
					testkit.Git(t, f.clone, "reset", "--soft", "HEAD^")
					name := "extra.txt"
					if variant == "changed" {
						name = strings.TrimSpace(testkit.Git(t, f.clone, "diff", "--name-only", art["base"].(string), art["commit"].(string)))
					}
					if err := os.WriteFile(filepath.Join(f.clone, name), []byte("altered"), 0o644); err != nil {
						t.Fatal(err)
					}
					testkit.Git(t, f.clone, "add", name)
				}
				testkit.Git(t, f.clone, "commit", "-q", "--allow-empty", "-m", "alter carrier")
				carrier := testkit.Git(t, f.clone, "rev-parse", "HEAD")
				testkit.Git(t, f.clone, "push", "-q", "--force", f.origin, "main")
				f.editPRs(t, func(prs []map[string]any) []map[string]any { prs[0]["merge_commit"] = carrier; return prs })
			}
			f.refusedProof(t, "t_1", 1)
		})
	}
}

func TestShephrdMergesOnceThePullRequestIsMergeable(t *testing.T) {
	f := withGitHub(t, "shephrd", "merge")
	os.MkdirAll(filepath.Join(f.Home, ".fakegh"), 0o700)
	os.WriteFile(filepath.Join(f.Home, ".fakegh", "blocked"), nil, 0o600)
	f.OK("task", "create", "--repo", "api", "--objective", "Fix it")
	f.OK("task", "start", "t_1")
	f.WaitState("t_1", "done")
	pending := f.OK("task", "accept", "t_1", "--key", "first")
	if pending["pending"] == nil || pending["task"].(map[string]any)["state"] != "done" {
		t.Fatalf("delivery while checks are pending: %v", pending)
	}
	os.Remove(filepath.Join(f.Home, ".fakegh", "blocked"))
	merged := f.OK("task", "deliver", "t_1", "--key", "second")
	if merged["task"].(map[string]any)["reason"] != "accepted" || merged["pull_request"].(map[string]any)["ref"] != "1" {
		t.Fatalf("delivery once mergeable: %v", merged)
	}
	if len(f.pullRequests(t)) != 1 {
		t.Fatalf("a second pull request was opened: %v", f.pullRequests(t))
	}
	commit := merged["artifact"].(map[string]any)["commit"].(string)
	testkit.Git(t, f.origin, "merge-base", "--is-ancestor", commit, "main")
}
