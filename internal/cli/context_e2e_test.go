package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/model"
	"shephrd/internal/repository"
)

func TestCLIProjectContextE2E(t *testing.T) {
	fixture := newPlanSurfaceFixture(t, "")
	inspect := func(path string) repository.ProjectContext {
		t.Helper()
		var result repository.ProjectContext
		body := fixture.run(t, nil, "repo", "context", path, "--json")
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "PRIVATE_TOPIC_CONTENT") {
			t.Fatal("context discovery read memory content")
		}
		return result
	}
	result := inspect(fixture.repoRoot)
	if !result.Memory.Enabled || result.OverviewPath != "" {
		t.Fatalf("default context = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(fixture.repoRoot, ".shephrd")); !os.IsNotExist(err) {
		t.Fatalf("discovery created project files: %v", err)
	}
	if _, err := os.Stat(fixture.database); !os.IsNotExist(err) {
		t.Fatalf("context discovery opened the database: %v", err)
	}
	if err := os.WriteFile(fixture.database, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	const agents = "# Existing instructions\nPreserve this file exactly.\n"
	for path, body := range map[string]string{
		"AGENTS.md":           agents,
		".shephrd/context.md": "# Project\nSee [memory](memory.md).\n",
		".shephrd/memory.md":  "PRIVATE_TOPIC_CONTENT\n",
	} {
		writeContextFixtureFile(t, fixture.repoRoot, path, body)
	}
	contextGit(t, fixture.repoRoot, "add", ".")
	contextGit(t, fixture.repoRoot, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "context")
	subdir := filepath.Join(fixture.repoRoot, "src", "nested")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	result = inspect(subdir)
	canonical, err := filepath.EvalSymlinks(fixture.repoRoot)
	if err != nil || result.RepositoryPath != canonical || result.OverviewPath != ".shephrd/context.md" {
		t.Fatalf("nested context = %+v, err = %v", result, err)
	}
	worktree := filepath.Join(filepath.Dir(fixture.repoRoot), "worktree")
	contextGit(t, fixture.repoRoot, "worktree", "add", "--detach", worktree)
	if err := os.Remove(filepath.Join(worktree, ".shephrd", "context.md")); err != nil {
		t.Fatal(err)
	}
	if result := inspect(worktree); result.OverviewPath != "" {
		t.Fatalf("worktree fell back to registered root: %+v", result)
	}
	setContextFixtureMemory(t, fixture, false)
	if result := inspect(fixture.repoRoot); result.Memory.Enabled || result.OverviewPath == "" {
		t.Fatalf("disabled memory lost workflow overview: %+v", result)
	}
	body, err := os.ReadFile(filepath.Join(fixture.repoRoot, "AGENTS.md"))
	if err != nil || string(body) != agents {
		t.Fatalf("AGENTS.md changed: %s, err = %v", body, err)
	}
	body, err = os.ReadFile(filepath.Join(fixture.repoRoot, ".shephrd", "memory.md"))
	if err != nil || string(body) != "PRIVATE_TOPIC_CONTENT\n" {
		t.Fatalf("disabled memory changed notes: %s, err = %v", body, err)
	}
	if output := string(fixture.run(t, nil, "repo", "context", fixture.repoRoot)); !strings.Contains(output, "memory.enabled: false") {
		t.Fatalf("text context = %s", output)
	}
	fixture.runFailure(t, nil, "repo", "context", t.TempDir(), "--json")
}

func TestCLIProjectMemoryWorkerHandoffsE2E(t *testing.T) {
	fixture := newPlanSurfaceFixture(t, contextCapturePiScript())
	owner := map[string]string{"PI_SESSION_ID": "context-e2e"}
	const agents = "# Local instructions\nDo not change this file.\n"
	writeContextFixtureFile(t, fixture.repoRoot, "AGENTS.md", agents)
	contextGit(t, fixture.repoRoot, "add", "AGENTS.md")
	contextGit(t, fixture.repoRoot, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "instructions")
	fixture.run(t, owner, "repo", "add", fixture.repoRoot, "--name", "demo", "--json")
	writeContextFixtureFile(t, fixture.repoRoot, ".shephrd/context.md", "# Uncommitted root overview\n")
	const objective = "Read-only investigation. Do not use project memory for this task."
	const acceptance = "Use only the explicitly selected research scope; no independent review."
	var task model.Task
	if err := json.Unmarshal(fixture.run(t, owner, "task", "create", "--repo", "demo", "--feature", "context", "--deliverable", "report", "--acceptance", acceptance, objective, "--json"), &task); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("task: %s", fixture.run(t, owner, "task", "inspect", task.ID, "--json"))
			body, _ := os.ReadFile(filepath.Join(fixture.dataDir, task.ID, "attempt-1-runner.log"))
			t.Logf("runner: %s", body)
		}
	})
	promptPath := filepath.Join(filepath.Dir(fixture.repoRoot), "prompt.txt")
	extra := map[string]string{"PI_SESSION_ID": "context-e2e", "SHEPHRD_CONTEXT_PROMPT": promptPath}
	var spawned model.SpawnResult
	if err := json.Unmarshal(fixture.run(t, extra, "worker", "spawn", task.ID, "--json"), &spawned); err != nil {
		t.Fatal(err)
	}
	waitForCLITask(t, fixture.database, task.ID, func(task model.Task) bool {
		return task.Status == model.TaskStatusWaiting && !task.ProcessAlive
	})
	prompt := readFile(t, promptPath)
	for _, want := range []string{objective, acceptance, "Automatic project memory is enabled", "AGENTS.md"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("initial prompt missing %q: %s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Read `.shephrd/context.md`") {
		t.Fatal("worker discovered uncommitted root overview")
	}
	writeContextFixtureFile(t, spawned.Attempt.WorktreePath, ".shephrd/context.md", "# Attempt-local overview\n")
	setContextFixtureMemory(t, fixture, false)
	fixture.run(t, extra, "worker", "send", task.ID, "Keep the same read-only scope and memory opt-out.", "--json")
	waitForCLITask(t, fixture.database, task.ID, func(task model.Task) bool {
		return task.Status == model.TaskStatusWaiting && !task.ProcessAlive
	})
	prompt = readFile(t, promptPath)
	for _, want := range []string{"Read `.shephrd/context.md`", "disabled by global configuration", "Current memory setting", "Keep the same read-only scope"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("follow-up missing %q: %s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Follow `.shephrd/memory.md`") {
		t.Fatal("disabled follow-up instructed automatic recall")
	}
	fixture.run(t, extra, "worker", "relaunch", task.ID, "--json")
	waitForCLITask(t, fixture.database, task.ID, func(task model.Task) bool {
		return task.Status == model.TaskStatusWaiting && !task.ProcessAlive
	})
	prompt = readFile(t, promptPath)
	for _, want := range []string{objective, acceptance, "same-worktree relaunch", "Read `.shephrd/context.md`", "disabled by global configuration"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("relaunch missing %q: %s", want, prompt)
		}
	}
	setContextFixtureMemory(t, fixture, true)
	fixture.run(t, extra, "worker", "retry", task.ID, "--json")
	waitForCLITask(t, fixture.database, task.ID, func(task model.Task) bool {
		return task.Status == model.TaskStatusWaiting && !task.ProcessAlive
	})
	prompt = readFile(t, promptPath)
	for _, want := range []string{objective, acceptance, "clean retry", "enabled unless repository guidance or this task opts out"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("retry missing %q: %s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Read `.shephrd/context.md`") {
		t.Fatal("retry copied attempt-local or dirty-root context")
	}
	if got := readFile(t, filepath.Join(fixture.repoRoot, "AGENTS.md")); got != agents {
		t.Fatalf("root AGENTS.md changed: %s", got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func contextGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s: %v", args, output, err)
	}
}

func writeContextFixtureFile(t *testing.T, root, path, body string) {
	t.Helper()
	path = filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setContextFixtureMemory(t *testing.T, fixture *planSurfaceFixture, enabled bool) {
	t.Helper()
	path := filepath.Join(filepath.Dir(fixture.database), "config.toml")
	body := readFile(t, path)
	body, _, _ = strings.Cut(body, "\n[memory]\n")
	value := "false"
	if enabled {
		value = "true"
	}
	if err := os.WriteFile(path, []byte(body+"\n[memory]\nenabled = "+value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func contextCapturePiScript() string {
	return `#!/bin/sh
set -eu
printf '%s\n' "$@" > "$SHEPHRD_CONTEXT_PROMPT"
printf '%s\n' '{"type":"session","id":"context-session"}'
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"<shephrd-event>{\"type\":\"checkpoint\",\"payload\":\"waiting\",\"checkpoint\":{\"schema_version\":1,\"summary\":\"waiting\",\"completed\":[],\"next_steps\":[\"Await the decision\"],\"decisions\":[],\"changed_paths\":[],\"checks\":[],\"blockers\":[]}}</shephrd-event>\n<shephrd-event>{\"type\":\"question\",\"payload\":\"Need a decision\"}</shephrd-event>"}],"stopReason":"stop"}}'
printf '%s\n' '{"type":"agent_end"}'
`
}
