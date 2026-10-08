package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/model"
)

func TestSubdriverCLISelectionRecoveryE2E(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 fixture runtime unavailable")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "shephrd")
	bin := filepath.Join(root, "bin")
	repo := filepath.Join(root, "repo")
	for _, path := range []string{bin, repo} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/shephrd")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", output, err)
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "--allow-empty", "-m", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	cfg := filepath.Join(root, "config.toml")
	body := fmt.Sprintf("default_harness = \"pi\"\nworker_runtime = \"headless\"\ndatabase_path = %q\ndata_dir = %q\nworktree_root = %q\n", filepath.Join(root, "state.db"), filepath.Join(root, "data"), filepath.Join(root, "worktrees"))
	if err := os.WriteFile(cfg, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/usr/bin/env python3
import os,sys,json
args=sys.argv[1:]
selected=args[args.index('--model')+1] if '--model' in args else 'fixture-native-default'
with open(os.environ['FIXTURE_ROOT']+'/selection.jsonl','a') as f:
 f.write(json.dumps({'model':selected,'harness':os.path.basename(sys.argv[0]),'driver_model':os.environ.get('SHEPHRD_DRIVER_MODEL')})+'\n')
print(json.dumps({'type':'message_end','message':{'role':'assistant','model':selected,'content':[{'type':'text','text':'Fixture stopped before dispatch'}],'stopReason':'stop'}}),flush=True)
sys.exit(7)
`
	if err := os.WriteFile(filepath.Join(bin, "pi"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	environment := compoundCLIEnvironment(os.Environ(), map[string]string{
		"PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH"), "SHEPHRD_CONFIG": cfg,
		"SHEPHRD_EXECUTABLE": binary, "SHEPHRD_WORKER_RUNTIME": "headless", "SHEPHRD_WORKER": "",
		"SHEPHRD_SUBDRIVER_ID": "", "SHEPHRD_SUBDRIVER_GENERATION": "", "SHEPHRD_SUBDRIVER_TOKEN": "",
		"SHEPHRD_DRIVER_HARNESS": "", "SHEPHRD_DRIVER_MODEL": "", "PI_SESSION_ID": "",
		"PI_MODEL": "fixture-requested-model", "FIXTURE_ROOT": root,
	})
	call := func(wantError string, args ...string) []byte {
		t.Helper()
		cmd := exec.Command(binary, append(args, "--json")...)
		cmd.Env = environment
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		if wantError == "" && err != nil || wantError != "" && (err == nil || !strings.Contains(stderr.String(), wantError)) {
			t.Fatalf("%v: %v stdout=%s stderr=%s; want error %q", args, err, out.String(), stderr.String(), wantError)
		}
		return out.Bytes()
	}
	call("", "repo", "add", repo, "--name", "fixture")
	var request model.SubdriverRequest
	original := "Preserve this exact original request.\nNo workers before selection recovery.\n"
	if err := json.Unmarshal(call("", "subdriver", "handoff", original, "--repo", "fixture", "--driver-id", "driver:main", "--key", "selection", "--queue"), &request); err != nil {
		t.Fatal(err)
	}
	inspect := func(generation int, wantModel string) model.SubdriverPage {
		t.Helper()
		var page model.SubdriverPage
		if err := json.Unmarshal(call("", "subdriver", "inspect", request.SubdriverID), &page); err != nil {
			t.Fatal(err)
		}
		c := page.Subdriver
		if c.Generation != generation || c.Model != wantModel || c.Harness != "pi" || c.State != "held" || len(page.Workers) != 0 {
			t.Fatalf("selection/state changed unexpectedly: %+v", page)
		}
		var retained model.SubdriverRequest
		if err := json.Unmarshal(call("", "subdriver", "request", request.ID), &retained); err != nil {
			t.Fatal(err)
		}
		if retained.Original != original || retained.SubdriverID != request.SubdriverID || retained.DriverID != request.DriverID {
			t.Fatalf("request changed: %+v", retained)
		}
		return page
	}
	call("exit 7", "subdriver", "resume", request.SubdriverID, "--foreground")
	inspect(1, "")
	environment = compoundCLIEnvironment(environment, map[string]string{"SHEPHRD_DRIVER_HARNESS": "pi", "SHEPHRD_DRIVER_MODEL": "fixture-requested-model"})
	call("generation changed", "subdriver", "recover", request.SubdriverID, "--generation", "0")
	call("", "subdriver", "recover", request.SubdriverID, "--generation", "1")
	call("exit 7", "subdriver", "resume", request.SubdriverID, "--foreground")
	inspect(2, "")
	selections, err := os.ReadFile(filepath.Join(root, "selection.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(selections, []byte(`"model": "fixture-native-default"`)) != 2 {
		t.Fatalf("native fallback not reproduced: %s", selections)
	}
	t.Log("Reproduced: PI_MODEL alone persists an empty model, invokes the native default, and later driver context does not repair the retained selection")
	call("--model requires --generation", "subdriver", "resume", request.SubdriverID, "--model", "fixture-requested-model", "--foreground")
	call("generation changed", "subdriver", "resume", request.SubdriverID, "--generation", "-1", "--model", "fixture-requested-model", "--foreground")
	call("is held", "subdriver", "resume", request.SubdriverID, "--generation", "2", "--model", "fixture-requested-model", "--foreground")
	inspect(2, "")
	call("", "subdriver", "recover", request.SubdriverID, "--generation", "2")
	call("generation changed", "subdriver", "resume", request.SubdriverID, "--generation", "1", "--model", "fixture-requested-model", "--foreground")
	call("exit 7", "subdriver", "resume", request.SubdriverID, "--generation", "2", "--model", "fixture-requested-model", "--foreground")
	inspect(3, "fixture-requested-model")
	environment = compoundCLIEnvironment(environment, map[string]string{"SHEPHRD_DRIVER_HARNESS": "claude-code", "SHEPHRD_DRIVER_MODEL": "fixture-other-harness-model"})
	call("", "subdriver", "recover", request.SubdriverID, "--generation", "3")
	call("exit 7", "subdriver", "resume", request.SubdriverID, "--foreground")
	inspect(4, "fixture-requested-model")
	call("", "subdriver", "recover", request.SubdriverID, "--generation", "4")
	call("exit 7", "subdriver", "resume", request.SubdriverID, "--generation", "4", "--model=", "--foreground")
	inspect(5, "")
	selections, err = os.ReadFile(filepath.Join(root, "selection.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(selections), []byte("\n"))
	wantModels := []string{"fixture-native-default", "fixture-native-default", "fixture-requested-model", "fixture-requested-model", "fixture-native-default"}
	if len(lines) != len(wantModels) {
		t.Fatalf("unexpected launches: %s", selections)
	}
	for i, line := range lines {
		var selection struct {
			Model       string `json:"model"`
			Harness     string `json:"harness"`
			DriverModel string `json:"driver_model"`
		}
		if err := json.Unmarshal(line, &selection); err != nil {
			t.Fatal(err)
		}
		wantDriver := wantModels[i]
		if wantDriver == "fixture-native-default" {
			wantDriver = ""
		}
		if selection.Model != wantModels[i] || selection.Harness != "pi" || selection.DriverModel != wantDriver {
			t.Fatalf("generation %d invocation/context: %+v", i+1, selection)
		}
	}
	if err := json.Unmarshal(call("", "subdriver", "handoff", original, "--general-context", "Explicit research", "--driver-id", "driver:main", "--key", "initial-selection", "--queue"), &request); err != nil {
		t.Fatal(err)
	}
	call("exit 7", "subdriver", "resume", request.SubdriverID, "--generation", "0", "--model", "fixture-initial-model", "--foreground")
	inspect(1, "fixture-initial-model")
}
