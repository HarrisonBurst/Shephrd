package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestCLIConfiguredInitialModelsE2E(t *testing.T) {
	fixture := newPlanSurfaceFixture(t, "#!/bin/sh\nexit 7\n")
	var configPath, binDir string
	for _, entry := range fixture.environment {
		if strings.HasPrefix(entry, "SHEPHRD_CONFIG=") {
			configPath = strings.TrimPrefix(entry, "SHEPHRD_CONFIG=")
		}
		if strings.HasPrefix(entry, "PATH=") {
			binDir = strings.SplitN(strings.TrimPrefix(entry, "PATH="), string(os.PathListSeparator), 2)[0]
		}
	}
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	glidePath := filepath.Join(filepath.Dir(fixture.repoRoot), "glide")
	if err := os.MkdirAll(glidePath, 0700); err != nil {
		t.Fatal(err)
	}
	contextGit(t, glidePath, "init", "-b", "main")
	contextGit(t, glidePath, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "base")
	register := func(path, name string) model.Repo {
		t.Helper()
		var repo model.Repo
		if err := json.Unmarshal(fixture.run(t, nil, "repo", "add", path, "--name", name, "--json"), &repo); err != nil {
			t.Fatal(err)
		}
		return repo
	}
	other := register(fixture.repoRoot, "other")
	glide := register(glidePath, "glide")
	configure := func(global string) {
		t.Helper()
		body := fmt.Sprintf("default_harness = \"pi\"\ndefault_model = %q\nworker_runtime = \"headless\"\ndatabase_path = %q\ndata_dir = %q\nworktree_root = %q\n[repository_models.%s]\nharness = \"claude-code\"\nmodel = \"claude-opus-5-5\"\n", global, fixture.database, fixture.dataDir, filepath.Join(filepath.Dir(fixture.repoRoot), "worktrees"), glide.ID)
		if err := os.WriteFile(configPath, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	configure("openai-codex/gpt-6-sol:xhigh")
	caller := map[string]string{"SHEPHRD_DRIVER_HARNESS": "pi", "SHEPHRD_DRIVER_MODEL": "caller-different", "PI_MODEL": "caller-pi", "SHEPHRD_WORKER": "", "SHEPHRD_SUBDRIVER_ID": "", "SHEPHRD_SUBDRIVER_GENERATION": "", "SHEPHRD_SUBDRIVER_TOKEN": ""}
	spawn := func(repo model.Repo, feature string, flags ...string) model.SpawnResult {
		t.Helper()
		var task model.Task
		if err := json.Unmarshal(fixture.run(t, caller, "task", "create", "--repo", repo.ID, "--feature", feature, "--deliverable", "report", "--driver-id", "driver:main", "exercise model selection", "--json"), &task); err != nil {
			t.Fatal(err)
		}
		args := append([]string{"worker", "spawn", task.ID}, flags...)
		var result model.SpawnResult
		if err := json.Unmarshal(fixture.run(t, caller, append(args, "--json")...), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	check := func(harness, modelID, wantHarness, wantModel string) {
		t.Helper()
		if harness != wantHarness || modelID != wantModel {
			t.Fatalf("selection = %q/%q; want %q/%q", harness, modelID, wantHarness, wantModel)
		}
	}
	global := spawn(other, "global")
	check(global.Attempt.Harness, global.Attempt.Model, "pi", "openai-codex/gpt-6-sol:xhigh")
	repoWorker := spawn(glide, "repo")
	check(repoWorker.Attempt.Harness, repoWorker.Attempt.Model, "claude-code", "claude-opus-5-5")
	explicit := spawn(glide, "explicit", "--harness", "pi", "--model", "manual")
	check(explicit.Attempt.Harness, explicit.Attempt.Model, "pi", "manual")
	globalHarness := spawn(glide, "global-harness", "--harness", "pi")
	check(globalHarness.Attempt.Harness, globalHarness.Attempt.Model, "pi", "openai-codex/gpt-6-sol:xhigh")
	native := spawn(glide, "native", "--model=")
	check(native.Attempt.Harness, native.Attempt.Model, "claude-code", "")
	handoff := func(key string, flags ...string) model.SubdriverRequest {
		t.Helper()
		args := append([]string{"subdriver", "handoff", "Select a model", "--driver-id", "driver:main", "--key", key, "--queue"}, flags...)
		var request model.SubdriverRequest
		if err := json.Unmarshal(fixture.run(t, caller, append(args, "--json")...), &request); err != nil {
			t.Fatal(err)
		}
		return request
	}
	general := handoff("general", "--general-context", "research")
	otherRequest := handoff("other", "--repo", other.ID)
	glideRequest := handoff("glide", "--repo", glide.ID)
	explicitRequest := handoff("explicit", "--general-context", "manual selection")
	for _, test := range []struct {
		request          model.SubdriverRequest
		harness, modelID string
	}{
		{general, "pi", "openai-codex/gpt-6-sol:xhigh"},
		{otherRequest, "pi", "openai-codex/gpt-6-sol:xhigh"},
		{glideRequest, "claude-code", "claude-opus-5-5"},
		{explicitRequest, "pi", "manual-general"},
	} {
		args := []string{"subdriver", "resume", test.request.SubdriverID, "--foreground"}
		if test.request.ID == explicitRequest.ID {
			args = append(args, "--generation", "0", "--model", "manual-general")
		}
		fixture.runFailure(t, caller, append(args, "--json")...)
		var page model.SubdriverPage
		if err := json.Unmarshal(fixture.run(t, caller, "subdriver", "inspect", test.request.SubdriverID, "--json"), &page); err != nil {
			t.Fatal(err)
		}
		check(page.Subdriver.Harness, page.Subdriver.Model, test.harness, test.modelID)
		if page.Subdriver.Generation != 1 || page.Subdriver.State != "held" {
			t.Fatalf("first turn = %+v", page.Subdriver)
		}
	}
	configure("changed-global")
	waitForCLITask(t, fixture.database, global.Task.ID, func(task model.Task) bool {
		return task.Status == model.TaskStatusBlocked && !task.ProcessAlive
	})
	var retry model.SpawnResult
	if err := json.Unmarshal(fixture.run(t, caller, "worker", "retry", global.Task.ID, "--json"), &retry); err != nil {
		t.Fatal(err)
	}
	check(retry.Attempt.Harness, retry.Attempt.Model, "pi", "openai-codex/gpt-6-sol:xhigh")
	fixture.run(t, caller, "subdriver", "recover", glideRequest.SubdriverID, "--generation", "1", "--json")
	fixture.runFailure(t, caller, "subdriver", "resume", glideRequest.SubdriverID, "--foreground", "--json")
	var retained model.SubdriverPage
	if err := json.Unmarshal(fixture.run(t, caller, "subdriver", "inspect", glideRequest.SubdriverID, "--json"), &retained); err != nil {
		t.Fatal(err)
	}
	check(retained.Subdriver.Harness, retained.Subdriver.Model, "claude-code", "claude-opus-5-5")
	if retained.Subdriver.Generation != 2 {
		t.Fatalf("retained generation = %d", retained.Subdriver.Generation)
	}
	state, err := store.Open(fixture.database)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	stored, err := state.Attempt(global.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	check(stored.Harness, stored.Model, "pi", "openai-codex/gpt-6-sol:xhigh")
}
