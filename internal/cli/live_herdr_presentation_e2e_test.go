package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"shephrd/internal/control"
	"shephrd/internal/model"
	"shephrd/internal/store"
	"shephrd/internal/terminal"
)

var liveANSISequence = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

type liveSubdriverFixture struct {
	request model.SubdriverRequest
	fence   model.SubdriverFence
	label   string
}

func liveSubdriver(t *testing.T, state *store.Store, repoID, generalContext, driverID, key, label string) liveSubdriverFixture {
	t.Helper()
	request, err := state.HandoffSubdriver(repoID, generalContext, driverID, key, "Return live presentation evidence", "", "")
	if err != nil {
		t.Fatal(err)
	}
	fence, err := state.ReserveSubdriver(request.SubdriverID, 0, "pi", "fixture", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.StartSubdriver(fence, os.Getpid(), "live-fixture-session"); err != nil {
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
	t.Cleanup(func() { _ = state.FinishSubdriver(fence, "live fixture complete", "") })
	return liveSubdriverFixture{request: request, fence: fence, label: label}
}

func TestLiveHerdrSubDriverPresentationIsOptIn(t *testing.T) {
	if os.Getenv("SHEPHRD_REAL_HERDR_E2E") != "1" {
		t.Skip("set SHEPHRD_REAL_HERDR_E2E=1 to run against the live Herdr server")
	}
	client := terminal.New(os.Getenv("HERDR_SOCKET_PATH"))
	parent, err := client.ValidateParent()
	if err != nil {
		t.Fatal(err)
	}
	runtimeClient := terminal.RuntimeClient(client)
	dir := t.TempDir()
	for _, repoName := range []string{"Shephrd", ""} {
		label := control.SubdriverLabel(repoName)
		endpoint, err := runtimeClient.CreateWorkspace(terminal.WorkspaceSpec{WorkspaceID: parent.WorkspaceID, CWD: dir, Label: label, Harness: "pi", Source: "shephrd-live-subdriver-label"})
		if err != nil {
			t.Fatal(err)
		}
		state, err := runtimeClient.Inspect(endpoint)
		closeErr := runtimeClient.Close(endpoint)
		if err != nil || closeErr != nil {
			t.Fatalf("inspect=%v close=%v", err, closeErr)
		}
		if state.Label != label {
			t.Fatalf("live Herdr tab label = %q, want %q", state.Label, label)
		}
		t.Logf("live Herdr tab %s carried label %q and was closed", endpoint.TabID, state.Label)
	}
	if os.Getenv("SHEPHRD_REAL_HERDR_PI_E2E") != "1" {
		t.Log("set SHEPHRD_REAL_HERDR_PI_E2E=1 to also observe receipt feedback in a real Pi TUI")
		return
	}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi is not installed")
	}
	fixture := newPlanSurfaceFixture(t, "")
	state, err := store.Open(fixture.database)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "Live fixture", Path: fixture.repoRoot, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := fmt.Sprintf("shephrd-live-%d", time.Now().UnixNano())
	driverID := "driver:pi:" + sessionID
	repoOwner := liveSubdriver(t, state, repo.ID, "", driverID, "live-repo", control.SubdriverLabel(repo.Name))
	generalOwner := liveSubdriver(t, state, "", "Isolated live presentation fixture", driverID, "live-general", control.SubdriverLabel(""))
	root := t.TempDir()
	configPath := filepath.Join(root, "config.toml")
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf("database_path = %q\ndata_dir = %q\nworktree_root = %q\n[pi_watcher]\nenabled = true\n", fixture.database, fixture.dataDir, filepath.Join(root, "worktrees"))), 0600); err != nil {
		t.Fatal(err)
	}
	_, file, _, _ := runtime.Caller(0)
	extension := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".pi", "extensions", "shephrd-wake.ts"))
	modelName := os.Getenv("SHEPHRD_REAL_HERDR_PI_MODEL")
	if modelName == "" {
		modelName = "openai-codex/gpt-5.6-luna"
	}
	endpoint, err := client.CreateTab(terminal.TabSpec{WorkspaceID: parent.WorkspaceID, CWD: root, Label: "shephrd-live-receipt-fixture", Harness: "pi", Source: "shephrd-live-subdriver-receipt", Environment: []string{
		"SHEPHRD_CONFIG=" + configPath, "SHEPHRD_EXECUTABLE=" + fixture.binary, "SHEPHRD_PI_WATCHER_ENABLED=1", "PI_SKIP_VERSION_CHECK=1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.CloseExactPane(endpoint) }()
	command := "exec pi --session-id " + shellQuoteLive(sessionID) + " --no-extensions --no-skills --no-prompt-templates --no-context-files --no-builtin-tools --model " + shellQuoteLive(modelName) + " -e " + shellQuoteLive(extension) +
		" --append-system-prompt " + shellQuoteLive("This is an isolated Shephrd presentation fixture. Reply to every message with exactly one short sentence and never run commands.")
	if err := client.Run(endpoint, command); err != nil {
		t.Fatal(err)
	}
	screen := func() string {
		output, err := client.ReadPane(endpoint, 200)
		if err != nil {
			return ""
		}
		return liveANSISequence.ReplaceAllString(output, "")
	}
	waitFor := func(what string, timeout time.Duration, predicate func() bool) time.Time {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if predicate() {
				return time.Now()
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s; screen:\n%s", what, screen())
		return time.Time{}
	}
	env := compoundCLIEnvironment(fixture.environment, map[string]string{"SHEPHRD_CONFIG": configPath, "SHEPHRD_WORKER": "", "SHEPHRD_ATTEMPT_ID": "", "SHEPHRD_PI_WATCHER_ENABLED": "0"})
	publish := func(owner liveSubdriverFixture, key string) (model.SubdriverEvent, time.Time) {
		command := exec.Command(fixture.binary, "subdriver", "return", owner.request.ID, "Live presentation return "+key, "--kind", "question", "--key", key, "--json")
		command.Env = compoundCLIEnvironment(env, map[string]string{"SHEPHRD_SUBDRIVER_ID": owner.fence.ID, "SHEPHRD_SUBDRIVER_GENERATION": strconv.Itoa(owner.fence.Generation), "SHEPHRD_SUBDRIVER_TOKEN": owner.fence.Token})
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("publish return: %s: %v", output, err)
		}
		var event model.SubdriverEvent
		if err := json.Unmarshal(output, &event); err != nil {
			t.Fatalf("decode return: %s: %v", output, err)
		}
		return event, time.Now()
	}
	acknowledged := func(eventID int64) bool {
		notifications, err := state.Notifications("")
		if err != nil {
			return false
		}
		for _, notice := range notifications {
			if notice.SubdriverEventID == eventID {
				return notice.State == model.NotificationAcknowledged
			}
		}
		return false
	}
	waitFor("Pi TUI ready", 90*time.Second, func() bool {
		body := screen()
		return strings.Contains(body, modelName) || strings.Contains(body, "pi ") && strings.Contains(body, root)
	})
	time.Sleep(3 * time.Second)
	if strings.Contains(screen(), "Sub-driver") {
		t.Fatalf("premature presentation:\n%s", screen())
	}
	type phase struct {
		owner    liveSubdriverFixture
		key      string
		busyText string
	}
	results := make([]map[string]any, 0, 2)
	for _, p := range []phase{{repoOwner, "live-repo-return", ""}, {generalOwner, "live-general-return", "Write one paragraph of about one hundred and fifty words describing terminal multiplexers, then stop."}} {
		if p.busyText != "" {
			if _, stderr, err := (terminal.ExecRunner{}).Run(endpoint.SocketPath, "pane", "send-text", endpoint.PaneID, p.busyText); err != nil {
				t.Fatalf("busy prompt: %s: %v", stderr, err)
			}
			if _, stderr, err := (terminal.ExecRunner{}).Run(endpoint.SocketPath, "pane", "send-keys", endpoint.PaneID, "Enter"); err != nil {
				t.Fatalf("busy Enter: %s: %v", stderr, err)
			}
			time.Sleep(500 * time.Millisecond)
		}
		event, published := publish(p.owner, p.key)
		receiptPrefix := "Shephrd received " + p.owner.label + " subdriver-question ("
		indicated := waitFor("receipt indicator "+p.owner.label, 60*time.Second, func() bool { return strings.Contains(screen(), receiptPrefix) })
		body := screen()
		line := ""
		for _, candidate := range strings.Split(body, "\n") {
			if strings.Contains(candidate, receiptPrefix) {
				line = strings.TrimSpace(candidate)
			}
		}
		footerSeen := strings.Contains(body, "● "+p.owner.label+" subdriver-question")
		injected := waitFor("injected return "+p.owner.label, 120*time.Second, func() bool {
			return strings.Contains(screen(), "Shephrd "+p.owner.label+" return wake")
		})
		settled := waitFor("acknowledgement "+p.owner.label, 180*time.Second, func() bool { return acknowledged(event.ID) })
		if strings.Contains(screen(), "● "+p.owner.label) {
			t.Fatalf("footer status remained after acknowledgement:\n%s", screen())
		}
		results = append(results, map[string]any{
			"label": p.owner.label, "busy_prompt": p.busyText != "", "indicator_line": line, "footer_status_seen": footerSeen,
			"receipt_visible_ms": indicated.Sub(published).Milliseconds(), "return_text_visible_ms": injected.Sub(published).Milliseconds(), "acknowledged_ms": settled.Sub(published).Milliseconds(),
		})
	}
	evidence, _ := json.MarshalIndent(results, "", "  ")
	t.Logf("live Herdr Pi TUI receipt evidence (real provider turns, isolated state):\n%s", evidence)
	if !strings.Contains(results[0]["indicator_line"].(string), "Sub-driver: Live fixture") || !strings.Contains(results[1]["indicator_line"].(string), "Sub-driver: General") {
		t.Fatalf("indicator lines: %s", evidence)
	}
}

func shellQuoteLive(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
