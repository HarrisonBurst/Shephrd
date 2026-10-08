package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"shephrd/internal/model"
	"shephrd/internal/process"
	"shephrd/internal/store"
)

func TestSubdriverStaleHoldWatcherE2E(t *testing.T) {
	for _, name := range []string{"python3", "node"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip(name + " fixture runtime unavailable")
		}
	}
	fixture := newPlanSurfaceFixture(t, subdriverTerminalHarness)
	fixture.environment = compoundCLIEnvironment(fixture.environment, map[string]string{
		"SHEPHRD_WORKER": "", "SHEPHRD_SUBDRIVER_ID": "", "SHEPHRD_SUBDRIVER_GENERATION": "", "SHEPHRD_SUBDRIVER_TOKEN": "", "SHEPHRD_COORDINATOR_ID": "", "SHEPHRD_COORDINATOR_GENERATION": "", "SHEPHRD_COORDINATOR_TOKEN": "",
	})
	root := filepath.Dir(fixture.binary)
	source, err := filepath.Abs("../control/subdriver.go")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	needle := "for _, c := range candidates {"
	if strings.Count(string(body), needle) != 1 {
		t.Fatal("pump scheduling point changed")
	}
	gate := `
		if gate := os.Getenv("SHEPHRD_TEST_PUMP_GATE"); gate != "" && c.State == "running" {
			body, err := json.Marshal(c)
			if err != nil { return err }
			if err := os.WriteFile(filepath.Join(gate, "observed.json"), body, 0600); err != nil { return err }
			deadline := time.Now().Add(8*time.Second)
			for {
				if _, err := os.Stat(filepath.Join(gate, "probe")); err == nil { break }
				if time.Now().After(deadline) { return fmt.Errorf("fixture probe gate timed out") }
				time.Sleep(time.Millisecond)
			}
		}
`
	overlaySource := filepath.Join(root, "subdriver.go")
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write(overlaySource, strings.Replace(string(body), needle, needle+gate, 1))
	overlay, _ := json.Marshal(map[string]any{"Replace": map[string]string{source: overlaySource}})
	overlayPath := filepath.Join(root, "overlay.json")
	write(overlayPath, string(overlay))
	pumpBinary := filepath.Join(root, "pump-shephrd")
	build := exec.Command("go", "build", "-overlay", overlayPath, "-o", pumpBinary, "./cmd/shephrd")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build scheduled CLI: %s %v", output, err)
	}
	extension := filepath.Join(root, "herdr")
	build = exec.Command("go", "build", "-o", extension, "./internal/terminal/testdata/herdrextension")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build terminal fixture: %s %v", output, err)
	}
	body, err = os.ReadFile(extension)
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(body))
	for _, runtime := range []string{"headless", "herdr"} {
		for _, mode := range []string{"pump-complete", "pump-runner-loss"} {
			t.Run(runtime+"/"+mode, func(t *testing.T) {
				dir := t.TempDir()
				database := filepath.Join(dir, "state.db")
				config := filepath.Join(dir, "config.toml")
				operations := filepath.Join(dir, "operations")
				text := fmt.Sprintf("database_path = %q\ndata_dir = %q\n[memory]\nenabled = false\n[notifications]\nenabled = false\n", database, filepath.Join(dir, "data"))
				if runtime == "herdr" {
					text += fmt.Sprintf("[terminal_extensions.herdr]\ncommand = [%q, %q, %q]\nsha256 = %q\n", extension, operations, fixture.binary, digest)
				}
				write(config, text)
				state, err := store.Open(database)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { state.Close() })
				repo, err := state.UpsertRepo(model.Repo{Name: "hold-fixture", Path: fixture.repoRoot, DefaultBranch: "main"})
				if err != nil {
					t.Fatal(err)
				}
				driver := "driver:" + mode
				request, err := state.HandoffSubdriver(repo.ID, "", driver, mode, "Keep existing work owned", "", "")
				if err != nil {
					t.Fatal(err)
				}
				c, err := state.Subdriver(request.SubdriverID)
				if err != nil {
					t.Fatal(err)
				}
				fence, err := state.ReserveSubdriver(c.ID, c.Generation, "pi", "retained-model", runtime)
				if err != nil {
					t.Fatal(err)
				}
				if runtime == "herdr" {
					endpoint := model.TerminalEndpoint{Backend: runtime, SocketPath: "/socket", WorkspaceID: "w7", TabID: "w7:t2", PaneID: "w7:p2"}
					if err := state.SetSubdriverEndpoint(fence, endpoint); err != nil {
						t.Fatal(err)
					}
				}
				env := compoundCLIEnvironment(fixture.environment, map[string]string{"SHEPHRD_CONFIG": config, "FIXTURE_MODE": mode, "FIXTURE_DIR": dir})
				runner := exec.Command(fixture.binary, "subdriver", "_run", fence.ID, "--generation", fmt.Sprint(fence.Generation), "--token", fence.Token)
				runner.Env = env
				var output bytes.Buffer
				runner.Stdout, runner.Stderr = &output, &output
				if err := runner.Start(); err != nil {
					t.Fatal(err)
				}
				runnerDone := make(chan error, 1)
				go func() { runnerDone <- runner.Wait() }()
				t.Cleanup(func() {
					_ = runner.Process.Kill()
					c, _ := state.Subdriver(fence.ID)
					if process.Alive(c.HarnessPID) {
						_ = process.Stop(c.HarnessPID)
					}
				})
				wait := func(label string, ready func() bool) {
					t.Helper()
					deadline := time.Now().Add(8 * time.Second)
					for !ready() {
						if time.Now().After(deadline) {
							t.Fatal("timed out: " + label)
						}
						time.Sleep(5 * time.Millisecond)
					}
				}
				exists := func(name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }
				wait("harness ready", func() bool { return exists("ready") })
				wrapper := filepath.Join(dir, "watcher-cli")
				write(wrapper, fmt.Sprintf("#!/bin/sh\n%q \"$@\"\nstatus=$?\nprintf '%%s %%s %%s\\n' \"$1\" \"$2\" \"$status\" >> %q\nexit \"$status\"\n", pumpBinary, filepath.Join(dir, "calls")))
				args, _ := json.Marshal([]string{"wake", "drain", "--driver-id", driver, "--driver-generation", "watcher-fixture", "--json"})
				watcher := exec.Command("node", "--input-type=module", "-e", `import { createRequire } from "node:module"; import { watcherChild } from "./.pi/extensions/shephrd-wake.ts"; const require = createRequire(import.meta.url); eval(watcherChild);`)
				watcher.Dir = "../.."
				watcher.Env = compoundCLIEnvironment(env, map[string]string{"SHEPHRD_EXECUTABLE": wrapper, "SHEPHRD_WAKE_ARGS": string(args), "SHEPHRD_PI_POLL_MIN": "50", "SHEPHRD_PI_POLL_MAX": "50", "SHEPHRD_TEST_PUMP_GATE": dir})
				input, err := watcher.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				var watcherOutput bytes.Buffer
				watcher.Stdout, watcher.Stderr = &watcherOutput, &watcherOutput
				if err := watcher.Start(); err != nil {
					t.Fatal(err)
				}
				stopped := false
				stop := func() {
					if !stopped {
						stopped = true
						_, _ = input.Write([]byte("{\"op\":\"stop\"}\n"))
						_ = input.Close()
						if err := watcher.Wait(); err != nil {
							t.Errorf("watcher: %v %s", err, watcherOutput.String())
						}
					}
				}
				defer stop()
				wait("running snapshot", func() bool { return exists("observed.json") })
				var observed model.Subdriver
				body, err := os.ReadFile(filepath.Join(dir, "observed.json"))
				if err != nil || json.Unmarshal(body, &observed) != nil || observed.State != "running" || observed.RunnerPID != runner.Process.Pid || !process.Alive(observed.RunnerPID) {
					t.Fatalf("snapshot: %s %v", body, err)
				}
				write(filepath.Join(dir, "finish-turn"), "")
				select {
				case err := <-runnerDone:
					if (err == nil) != (mode == "pump-complete") {
						t.Fatalf("runner: %v %s", err, output.String())
					}
				case <-time.After(8 * time.Second):
					t.Fatal("runner did not exit")
				}
				wait("harness exit", func() bool { return !process.Alive(observed.HarnessPID) })
				settled, err := state.Subdriver(fence.ID)
				if err != nil || process.Alive(settled.RunnerPID) {
					t.Fatalf("runner absence: %+v %v", settled, err)
				}
				if mode == "pump-complete" && (settled.State != "idle" || !strings.Contains(settled.Checkpoint, "bounded checkpoint")) || mode == "pump-runner-loss" && settled.State != "running" {
					t.Fatalf("before probe: %+v", settled)
				}
				t.Logf("snapshot %s generation %d -> runner exited with durable state %s; releasing missing-runner probe", observed.State, observed.Generation, settled.State)
				write(operations+".absent", "")
				write(filepath.Join(dir, "probe"), "")
				wait("watcher drain", func() bool {
					calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
					return strings.Count(string(calls), "wake drain 0") >= 1
				})
				latest, err := state.Subdriver(fence.ID)
				if err != nil {
					t.Fatal(err)
				}
				notices, err := state.Notifications("")
				if err != nil {
					t.Fatal(err)
				}
				if mode == "pump-complete" {
					if !reflect.DeepEqual(settled, latest) || len(notices) != 0 {
						t.Fatalf("successful bounded turn reclassified: before=%+v after=%+v notifications=%+v", settled, latest, notices)
					}
					wait("three quiet watcher passes", func() bool {
						calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
						return strings.Count(string(calls), "wake drain 0") >= 3
					})
				} else {
					if latest.State != "held" || !strings.Contains(latest.Failure, "Recorded sub-driver runner exited") || len(notices) != 1 {
						t.Fatalf("real loss not held and notified for unfinished requests: %+v %+v", latest, notices)
					}
					wait("watcher delivery", func() bool {
						calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
						return strings.Contains(string(calls), "task obligations 0")
					})
				}
				stop()
				page, err := state.SubdriverPage(fence.ID, 0)
				if err != nil || len(page.Events) != 0 || len(page.Workers) != 1 || page.Subdriver.Generation != fence.Generation || page.Subdriver.SessionID != observed.SessionID || page.Subdriver.RunnerPID != observed.RunnerPID || page.Subdriver.HarnessPID != observed.HarnessPID || !reflect.DeepEqual(page.Subdriver.Endpoint, observed.Endpoint) {
					t.Fatalf("session identity or pending input changed: %+v %v", page, err)
				}
				notices, err = state.Notifications("")
				if err != nil {
					t.Fatal(err)
				}
				if mode == "pump-complete" {
					if !reflect.DeepEqual(page.Subdriver, settled) || len(notices) != 0 || watcherOutput.Len() != 0 {
						t.Fatalf("quiet passes changed settled turn: %+v %+v %s", page, notices, watcherOutput.String())
					}
				} else if len(notices) != 1 || notices[0].State != model.NotificationClaimed || notices[0].AckedAt != nil || !strings.Contains(watcherOutput.String(), `"type":"notification"`) {
					t.Fatalf("missing watcher blocker or implicit acknowledgement: %+v %s", notices, watcherOutput.String())
				}
				for _, link := range page.Workers {
					worker, err := state.Task(link.TaskID)
					if err != nil || worker.DriverID != observed.DriverID() || worker.Status != model.TaskStatusQueued {
						t.Fatalf("worker ownership changed: %+v %v", worker, err)
					}
				}
				if err := state.CheckSubdriverFence(fence); err == nil {
					t.Fatal("settled or held session retained authority")
				}
			})
		}
	}
}
