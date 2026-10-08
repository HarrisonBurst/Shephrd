package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestPiWatcherNotificationTimingE2E(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node fixture runtime unavailable")
	}
	fixture := newPlanSurfaceFixture(t, "#!/bin/sh\ntouch \"$SHEPHRD_TEST_TIMING_ROOT/model-call\"\nexit 97\n")
	state, err := store.Open(fixture.database)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "Registered timing repository", Path: fixture.repoRoot, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprintf("busy=%t", busy), func(t *testing.T) {
			root := t.TempDir()
			repoID, repoName, generalContext := repo.ID, repo.Name, ""
			if busy {
				repoID, repoName, generalContext = "", "", "Isolated timing fixture"
			}
			request, err := state.HandoffSubdriver(repoID, generalContext, "driver:pi:session-owner", root, "Return timing evidence", "", "")
			if err != nil {
				t.Fatal(err)
			}
			fence, err := state.ReserveSubdriver(request.SubdriverID, 0, "pi", "fixture", "headless")
			if err != nil {
				t.Fatal(err)
			}
			if err := state.StartSubdriver(fence, os.Getpid(), "fixture-session"); err != nil {
				t.Fatal(err)
			}
			page, err := state.SubdriverPage(fence.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range page.Events {
				if err := state.HandleSubdriverEvent(fence, event.ID); err != nil {
					t.Fatal(err)
				}
			}
			configPath := filepath.Join(root, "config.toml")
			body := fmt.Sprintf("database_path = %q\ndata_dir = %q\nworktree_root = %q\n[pi_watcher]\nenabled = true\n", fixture.database, fixture.dataDir, filepath.Join(root, "worktrees"))
			if cap := os.Getenv("SHEPHRD_TEST_POLL_MAX"); cap != "" {
				body += fmt.Sprintf("poll_max = %q\n", cap)
			}
			if err := os.WriteFile(configPath, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			fenceJSON, _ := json.Marshal(map[string]string{"SHEPHRD_SUBDRIVER_ID": fence.ID, "SHEPHRD_SUBDRIVER_GENERATION": strconv.Itoa(fence.Generation), "SHEPHRD_SUBDRIVER_TOKEN": fence.Token})
			command := exec.Command(node, "--test", "--test-name-pattern=^built CLI watcher timing$", "tests/pi/shephrd-wake.test.ts")
			command.Dir = "../.."
			command.Env = compoundCLIEnvironment(fixture.environment, map[string]string{
				"SHEPHRD_CONFIG": configPath, "SHEPHRD_WORKER_RUNTIME": "headless", "SHEPHRD_PI_WATCHER_ENABLED": "1",
				"SHEPHRD_WORKER": "", "SHEPHRD_SUBDRIVER_ID": "", "SHEPHRD_SUBDRIVER_GENERATION": "", "SHEPHRD_SUBDRIVER_TOKEN": "",
				"SHEPHRD_TEST_EXECUTABLE": fixture.binary, "SHEPHRD_TEST_TIMING_ROOT": root,
				"SHEPHRD_TEST_REQUEST": request.ID, "SHEPHRD_TEST_FENCE": string(fenceJSON), "SHEPHRD_TEST_BUSY": strconv.FormatBool(busy), "SHEPHRD_TEST_REPO_NAME": repoName,
			})
			output, err := command.CombinedOutput()
			t.Log(string(output))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(root, "model-call")); !os.IsNotExist(err) {
				t.Fatalf("unexpected model invocation: %v", err)
			}
			notifications, err := state.Notifications("")
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, notice := range notifications {
				if notice.RequestID != request.ID {
					continue
				}
				count++
				if notice.State != model.NotificationAcknowledged || notice.DeliveryAttempts != 1 || notice.HandlingID == "" || notice.SubdriverRepoName != repoName {
					t.Fatalf("notification: %+v", notice)
				}
			}
			if count != 2 {
				t.Fatalf("notification count = %d", count)
			}
			if err := state.FinishSubdriver(fence, "fixture complete", ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}
