package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"shephrd/internal/model"
	"shephrd/internal/store"
)

// TestCLITaskAttentionE2E runs the built binary against a seeded control
// database with a poisoned PATH: any git, gh, treehouse, or worker-runtime
// invocation writes a marker and the test fails. It also proves the JSON
// contract, owner scoping, portfolio sections, and repeat-read determinism.
func TestCLITaskAttentionE2E(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "shephrd")
	binDir := filepath.Join(root, "bin")
	databasePath := filepath.Join(root, "state.db")
	configPath := filepath.Join(root, "config.toml")
	markerDir := filepath.Join(root, "markers")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(markerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	build := exec.Command("go", "build", "-o", binary, "./cmd/shephrd")
	build.Dir = projectRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Shephrd E2E binary: %s: %v", output, err)
	}
	for _, poisoned := range []string{"git", "gh", "treehouse", "claude", "codex"} {
		script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %s > %q\nexit 97\n", poisoned, filepath.Join(markerDir, poisoned))
		if err := os.WriteFile(filepath.Join(binDir, poisoned), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configBody := fmt.Sprintf("database_path = %q\ndata_dir = %q\n", databasePath, filepath.Join(root, "data"))
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}

	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: filepath.Join(root, "repo"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	driverID := "driver:e2e-attention"
	for _, owner := range []string{driverID, "driver:e2e-previous"} {
		request, err := state.HandoffSubdriver(repo.ID, "", owner, "mixed-notifications", "Isolated sub-driver request", "", "")
		if err != nil {
			t.Fatal(err)
		}
		subdriver, err := state.Subdriver(request.SubdriverID)
		if err != nil {
			t.Fatal(err)
		}
		fence, err := state.ReserveSubdriver(subdriver.ID, subdriver.Generation, "pi", "fixture", "headless")
		if err != nil {
			t.Fatal(err)
		}
		if err := state.StartSubdriver(fence, 12345, "fixture-session"); err != nil {
			t.Fatal(err)
		}
		if _, err := state.SubdriverReturn(fence, request.ID, "result", "result", "SECRET-SUBDRIVER-RETURN"); err != nil {
			t.Fatal(err)
		}
		if err := state.FinishSubdriver(fence, "fixture complete", ""); err != nil {
			t.Fatal(err)
		}
	}
	subdriverDrain, err := state.DrainNotifications("", driverID, "generation:subdriver", 1)
	if err != nil || len(subdriverDrain.Notifications) != 1 || subdriverDrain.Notifications[0].SubdriverEventID == 0 {
		t.Fatalf("sub-driver notification drain: %+v %v", subdriverDrain, err)
	}
	makeAttempt := func(task model.Task) (model.Attempt, int) {
		attempt, err := state.BeginAttempt(task.ID, "claude-code", "model")
		if err != nil {
			t.Fatal(err)
		}
		if err := configureAttempt(t, state, attempt.ID, "session-"+task.ID, filepath.Join(root, "worktrees", task.ID), "lease-"+task.ID, "shephrd/"+task.ID); err != nil {
			t.Fatal(err)
		}
		generation, err := state.ReserveRunGeneration(attempt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"finish"}, model.WorkspaceFacts{}); err != nil {
			t.Fatal(err)
		}
		checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "checkpoint", NextSteps: []string{"finish"}}
		if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "checkpoint", Checkpoint: &checkpoint}, 2, model.WorkspaceFacts{}); err != nil {
			t.Fatal(err)
		}
		return attempt, generation
	}
	blocked, err := state.CreateTask(model.Task{Title: "Blocked release", DriverID: driverID, RepoID: repo.ID, FeatureKey: "blocked-held",
		Objective: "blocked objective", AcceptanceCriteria: "SECRET-E2E-ACCEPTANCE", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	blockedAttempt, blockedGeneration := makeAttempt(blocked)
	if _, err := state.AddEventForRun(blockedAttempt.ID, blockedGeneration, model.Event{Type: "blocked", Payload: "SECRET-E2E-PAYLOAD"}, 3, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	landed, err := state.CreateTask(model.Task{Title: "Landed release", DriverID: driverID, RepoID: repo.ID, FeatureKey: "landed-released",
		Objective: "landed objective", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	landedAttempt, landedGeneration := makeAttempt(landed)
	landedMessage, err := state.AddEventForRun(landedAttempt.ID, landedGeneration, model.Event{Type: "done", Payload: "done", Artifact: "report:landed.md"}, 3, model.WorkspaceFacts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.SetVerifiedReportDelivery(landed.ID, landedAttempt.ID, landedMessage.ID, model.VerifiedArtifact{
		Kind: "report", OriginalRef: landedMessage.ArtifactRef, SHA256: strings.Repeat("a", 64), SizeBytes: 1,
		SnapshotPath: filepath.Join(root, "artifacts", "landed.md"),
	}, "verified report artifact"); err != nil {
		t.Fatal(err)
	}
	if claimed, _, err := state.ClaimRelease(landedAttempt.ID); err != nil || !claimed {
		t.Fatalf("claim release: %t %v", claimed, err)
	}
	if err := state.MarkReleasedWithReason(landedAttempt.ID, "native test workspace released"); err != nil {
		t.Fatal(err)
	}
	landedDrain, err := state.DrainNotifications(landed.ID, driverID, "generation:landed", 1)
	if err != nil || len(landedDrain.Notifications) != 1 {
		t.Fatalf("landed notification drain: %+v %v", landedDrain, err)
	}
	landedNotification := landedDrain.Notifications[0]
	if _, err := state.AckNotification(model.NotificationAckRequest{NotificationID: landedNotification.NotificationID, ClaimToken: landedNotification.ClaimToken,
		ConsumerID: driverID, DriverGeneration: "generation:landed", HandlingID: "handling:landed"}); err != nil {
		t.Fatal(err)
	}
	otherOwner, err := state.CreateTask(model.Task{Title: "Previous owner work", DriverID: "driver:e2e-previous", RepoID: repo.ID, FeatureKey: "other-owner",
		Objective: "other owner obligation", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	otherAttempt, otherGeneration := makeAttempt(otherOwner)
	if _, err := state.AddEventForRun(otherAttempt.ID, otherGeneration, model.Event{Type: "blocked", Payload: "prior owner blocked"}, 3, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	notificationsBefore, err := state.Notifications("")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) (string, error) {
		command := exec.Command(binary, args...)
		command.Env = append(os.Environ(),
			"PATH="+binDir,
			"SHEPHRD_CONFIG="+configPath,
			"PI_SESSION_ID=",
		)
		output, err := command.CombinedOutput()
		return string(output), err
	}

	for _, args := range [][]string{
		{"task", "create", "--group", "removed", "objective"},
		{"task", "obligations", "--group", "removed"},
	} {
		if output, err := run(args...); err == nil || !strings.Contains(output, "unknown flag: --group") {
			t.Fatalf("removed group flag %v output=%s err=%v", args, output, err)
		}
	}
	if output, err := run("task", "obligations"); err == nil {
		t.Fatalf("attention without owner scope succeeded: %s", output)
	} else if !strings.Contains(output, "--driver-id") || !strings.Contains(output, "--all-drivers") {
		t.Fatalf("scope error lacks guidance: %s", output)
	}

	output, err := run("--json", "task", "obligations", "--driver-id", driverID)
	if err != nil {
		t.Fatalf("task obligations: %v\n%s", err, output)
	}
	var snapshot model.AttentionSnapshot
	if strings.Contains(output, `"group"`) {
		t.Fatalf("attention JSON retains group: %s", output)
	}
	if err := json.Unmarshal([]byte(output), &snapshot); err != nil {
		t.Fatalf("attention JSON: %v\n%s", err, output)
	}
	if snapshot.SchemaVersion != model.AttentionSchemaVersion || snapshot.DatabaseSchemaVersion < 9 {
		t.Fatalf("schema versions = %d/%d", snapshot.SchemaVersion, snapshot.DatabaseSchemaVersion)
	}
	if snapshot.Counts != (model.AttentionCounts{NeedsDisposition: 1, Closed: 1, OtherDriverAttention: 1}) {
		t.Fatalf("counts = %+v", snapshot.Counts)
	}
	if len(snapshot.Items) != 1 || snapshot.Items[0].Task == nil || snapshot.Items[0].Task.ID != blocked.ID ||
		snapshot.Items[0].Task.Title != blocked.Title || snapshot.Items[0].Task.FeatureKey != blocked.FeatureKey {
		t.Fatalf("default inbox = %+v", snapshot.Items)
	}
	if snapshot.Items[0].Kind != "terminal_held" || len(snapshot.Items[0].DriverChoices) == 0 {
		t.Fatalf("blocked row = %+v", snapshot.Items[0])
	}
	if snapshot.Items[0].Notifications == nil || *snapshot.Items[0].Notifications != (model.AttentionNotificationSummary{ActiveCount: 1, LatestKind: "blocked", LatestState: "pending"}) {
		t.Fatalf("task notification summary = %+v", snapshot.Items[0].Notifications)
	}
	for _, secret := range []string{"SECRET-E2E-ACCEPTANCE", "SECRET-E2E-PAYLOAD", "SECRET-SUBDRIVER-RETURN", filepath.Join(root, "worktrees")} {
		if strings.Contains(output, secret) {
			t.Fatalf("attention JSON leaked %q", secret)
		}
	}
	closedOmitted := false
	for _, omission := range snapshot.Omitted {
		if omission.Section == model.BucketClosed && omission.Count == 1 && strings.Contains(omission.Reveal, "--portfolio") {
			closedOmitted = true
		}
	}
	if !closedOmitted {
		t.Fatalf("closed omission missing: %+v", snapshot.Omitted)
	}

	portfolioOutput, err := run("--json", "task", "obligations", "--all-drivers", "--portfolio")
	if err != nil {
		t.Fatalf("portfolio attention: %v\n%s", err, portfolioOutput)
	}
	var portfolio model.AttentionSnapshot
	if err := json.Unmarshal([]byte(portfolioOutput), &portfolio); err != nil {
		t.Fatal(err)
	}
	if portfolio.Counts.NeedsDisposition != 2 || portfolio.Counts.Closed != 1 || len(portfolio.Items) != 3 {
		t.Fatalf("portfolio counts=%+v items=%d", portfolio.Counts, len(portfolio.Items))
	}

	// Repeat reads are deterministic apart from generated_at and ages.
	repeatOutput, err := run("--json", "task", "obligations", "--driver-id", driverID, "--limit", "1")
	if err != nil {
		t.Fatal(err)
	}
	var repeat model.AttentionSnapshot
	if err := json.Unmarshal([]byte(repeatOutput), &repeat); err != nil {
		t.Fatal(err)
	}
	if len(repeat.Items) != len(snapshot.Items) || repeat.Items[0].AttentionKey != snapshot.Items[0].AttentionKey ||
		!repeat.Items[0].AttentionSince.Equal(snapshot.Items[0].AttentionSince) || repeat.Counts != snapshot.Counts {
		t.Fatalf("repeat read diverged:\n%+v\n%+v", snapshot.Items, repeat.Items)
	}

	textOutput, err := run("task", "obligations", "--driver-id", driverID, "--details")
	if err != nil {
		t.Fatalf("text attention: %v\n%s", err, textOutput)
	}
	for _, expected := range []string{"NEEDS DISPOSITION", "demo/Blocked release [blocked-held]", "decide:", "closed rows omitted", "blocked objective"} {
		if !strings.Contains(textOutput, expected) {
			t.Fatalf("text output misses %q:\n%s", expected, textOutput)
		}
	}
	if strings.Contains(textOutput, "SECRET-E2E-ACCEPTANCE") || strings.Contains(textOutput, "SECRET-E2E-PAYLOAD") {
		t.Fatalf("text output leaked sensitive content:\n%s", textOutput)
	}

	entries, err := os.ReadDir(markerDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("attention invoked external tools: %v", names)
	}

	before, err := run("--json", "task", "list", "--include-archived")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run("--json", "task", "obligations", "--all-drivers", "--portfolio", "--include-archived-closed", "--details"); err != nil {
		t.Fatal(err)
	}
	after, err := run("--json", "task", "list", "--include-archived")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("attention mutated task state:\n%s\n%s", before, after)
	}
	state, err = store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	notificationsAfter, err := state.Notifications("")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(notificationsBefore, notificationsAfter) {
		t.Fatalf("obligations changed notification delivery state:\n%+v\n%+v", notificationsBefore, notificationsAfter)
	}
}
