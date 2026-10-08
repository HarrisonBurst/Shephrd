package control

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shephrd/internal/adapter"
	"shephrd/internal/model"
	"shephrd/internal/store"
	"shephrd/internal/terminal"
)

func TestRealInteractiveClaudeHerdrWorkerIsOptIn(t *testing.T) {
	if os.Getenv("SHEPHRD_REAL_HERDR_CLAUDE_E2E") != "1" {
		t.Skip("set SHEPHRD_REAL_HERDR_CLAUDE_E2E=1 to run a real interactive Claude Code worker in Herdr")
	}
	client := terminal.New(os.Getenv("HERDR_SOCKET_PATH"))
	parent, err := client.ValidateParent()
	if err != nil {
		t.Fatal(err)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	sandbox := t.TempDir()
	dataDir := filepath.Join(sandbox, "data")
	databasePath := filepath.Join(sandbox, "state.db")
	configPath := filepath.Join(sandbox, "config.toml")
	binary := filepath.Join(sandbox, "shephrd")
	build := exec.Command("go", "build", "-o", binary, "./cmd/shephrd")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build E2E Shephrd: %s: %v", output, err)
	}
	configBody := fmt.Sprintf(`default_harness = "claude-code"
worker_runtime = "herdr"
database_path = %q
data_dir = %q

[wake]
enabled = true
default_batch = 10
max_batch = 20
claim_ttl = "5m"
claim_ttl_min = "30s"
claim_ttl_max = "30m"
driver_id = "driver:e2e"

[notifications]
enabled = false
details = false
task_per_minute = 2
global_per_minute = 10

[pi_watcher]
enabled = false
poll_min = "1s"
poll_max = "15s"
`, databasePath, dataDir)
	configBody += liveHerdrExtensionConfig(t, root, sandbox)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, _ := state.UpsertRepo(model.Repo{Name: "herdr-e2e", Path: root, DefaultBranch: "main"})
	task, _ := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:e2e", RepoID: repo.ID, FeatureKey: "interactive-claude", Objective: "HERDR_INTERACTIVE_CLAUDE_E2E prove the real TUI", Deliverable: "report"})
	modelName := os.Getenv("SHEPHRD_REAL_HERDR_CLAUDE_MODEL")
	if modelName == "" {
		modelName = "claude-fable-5"
	}
	attempt, _ := state.BeginAttempt(task.ID, "claude-code", modelName)
	if err := state.SetRuntimeBackend(attempt.ID, "herdr"); err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, adapter.NewSessionID("claude-code"), root, "e2e-lease", "e2e-branch"); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"complete the E2E"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(dataDir, task.ID, "report.md")
	inputPath := writeInput(t, dataDir, task.ID, "brief.md", fmt.Sprintf("HERDR_INTERACTIVE_CLAUDE_E2E. First use the Bash tool to run sleep 3 so the active native Claude Code TUI can be observed. Then write a short report to %s. In your final response emit exactly this checkpoint with truthful content: <shephrd-event>{\"type\":\"checkpoint\",\"payload\":\"E2E checkpoint\",\"checkpoint\":{\"schema_version\":1,\"summary\":\"E2E checkpoint\",\"completed\":[\"Real Claude Code TUI ran\"],\"next_steps\":[\"Finish with done\"],\"decisions\":[],\"changed_paths\":[],\"checks\":[],\"blockers\":[]}}</shephrd-event>. Immediately follow it with exactly <shephrd-event>{\"type\":\"done\",\"payload\":\"E2E complete\",\"artifact\":\"report:%s\"}</shephrd-event> and no later event.", reportPath, reportPath))
	endpoint, err := client.CreateTab(terminal.TabSpec{WorkspaceID: parent.WorkspaceID, CWD: root, Label: "shephrd-real-claude-e2e", Harness: "claude-code", Source: "shephrd:e2e:display", Environment: []string{"SHEPHRD_CONFIG=" + configPath, "SHEPHRD_HERDR_LIFECYCLE_SEQ=1", "SHEPHRD_DRIVER_HARNESS=", "SHEPHRD_DRIVER_MODEL="}})
	if err != nil {
		t.Fatal(err)
	}
	paneOwned := true
	defer func() {
		if paneOwned {
			_ = client.CloseExactPane(endpoint)
		}
	}()
	if _, err := state.SetTerminalEndpointForRun(attempt.ID, generation, model.TerminalEndpoint{Backend: "herdr", SocketPath: endpoint.SocketPath, WorkspaceID: endpoint.WorkspaceID, TabID: endpoint.TabID, PaneID: endpoint.PaneID}); err != nil {
		t.Fatal(err)
	}
	if err := client.ReportAgent(endpoint, terminal.AgentReport{Source: terminalLifecycleSource(attempt.ID, generation), Agent: "claude-code", State: "working", Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	command := "exec " + shellQuote(binary) + " _run " + shellQuote(attempt.ID) + " --input " + shellQuote(inputPath) + " --run-generation " + fmt.Sprint(generation)
	if err := client.Run(endpoint, command); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(sandbox, "queued-input-reached-shell")
	paneWorking, tabWorking, visible, queued, focused := false, false, false, false, false
	lastOutput := ""
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		pane, paneErr := client.GetPane(endpoint)
		if terminal.IsNotFound(paneErr) {
			paneOwned = false
			break
		}
		if paneErr != nil {
			t.Fatal(paneErr)
		}
		paneWorking = paneWorking || pane.AgentStatus == "working"
		focused = focused || pane.Focused
		tab, tabErr := client.GetTab(endpoint)
		if tabErr == nil {
			tabWorking = tabWorking || tab.AgentStatus == "working"
			focused = focused || tab.Focused
		}
		output, readErr := client.ReadPane(endpoint, 200)
		if readErr == nil {
			lastOutput = output
			if strings.Contains(output, "HERDR_INTERACTIVE_CLAUDE_E2E") && !strings.Contains(output, `{"type":"system","subtype":"init"`) {
				visible = true
			}
		}
		if !queued && visible && paneWorking && tabWorking {
			if _, stderr, sendErr := (terminal.ExecRunner{}).Run(endpoint.SocketPath, "pane", "send-text", endpoint.PaneID, "touch "+marker); sendErr != nil {
				t.Fatalf("queue input: %s: %v", stderr, sendErr)
			}
			queued = true
		}
		time.Sleep(100 * time.Millisecond)
	}
	if paneOwned {
		t.Fatal("real interactive Claude pane did not disappear")
	}
	if !paneWorking || !tabWorking || !visible || !queued || focused {
		current, _ := state.Task(task.ID)
		logBody, _ := os.ReadFile(filepath.Join(dataDir, task.ID, "attempt-1-runner.log"))
		t.Fatalf("paneWorking=%t tabWorking=%t visible=%t queued=%t focused=%t output=%q task=%+v log=%q", paneWorking, tabWorking, visible, queued, focused, lastOutput, current, logBody)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("queued input reached a shell: %v", err)
	}
	current, _ := state.Task(task.ID)
	messages, _ := state.Messages(task.ID)
	checkpointSeen, doneSeen := false, false
	for _, message := range messages {
		checkpointSeen = checkpointSeen || message.Type == "checkpoint" && message.Direction == "worker-to-driver"
		doneSeen = doneSeen || message.Type == "done" && !message.Stale
	}
	if current.Status != "done" || !current.ClaimedDone || !current.Landed || !checkpointSeen || !doneSeen {
		t.Fatalf("task=%+v checkpoint=%t done=%t messages=%+v", current, checkpointSeen, doneSeen, messages)
	}
}
