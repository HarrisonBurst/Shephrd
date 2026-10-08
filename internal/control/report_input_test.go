package control

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/adapter"
	"shephrd/internal/config"
	"shephrd/internal/delivery"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

type reportInputFixture struct {
	root            string
	dataDir         string
	state           *store.Store
	service         Service
	producerRepo    model.Repo
	targetRepo      model.Repo
	producer        model.Task
	producerAttempt model.Attempt
	artifact        model.VerifiedArtifact
	reportPath      string
	body            string
}

func newReportInputFixture(t *testing.T, body string) reportInputFixture {
	t.Helper()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	producerRoot := filepath.Join(root, "producer")
	targetRoot := filepath.Join(root, "target")
	for _, path := range []string{producerRoot, targetRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	producerRepo, err := state.UpsertRepo(model.Repo{Name: "producer", Path: producerRoot, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	targetRepo, err := state.UpsertRepo(model.Repo{Name: "target", Path: targetRoot, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	producer, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:producer", RepoID: producerRepo.ID, FeatureKey: "investigate", Objective: "investigate", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(producer.ID, "pi", "model")
	if err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, "session", producerRoot, "lease", "producer-branch"); err != nil {
		t.Fatal(err)
	}
	attempt.RunGeneration = prepareReportAttempt(t, state, attempt)
	reportPath := filepath.Join(dataDir, producer.ID, "report.md")
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "report complete", NextSteps: []string{}}
	if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "checkpoint", Payload: checkpoint.Summary, Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "done", Payload: "complete", Artifact: "report:" + reportPath}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	service := New(config.Config{DataDir: dataDir, DefaultHarness: "pi"}, state)
	producer, err = state.Task(producer.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.verifyDelivery(producer, attempt)
	if err != nil || !result.Landed || result.VerifiedArtifact == nil {
		t.Fatalf("verify result=%+v err=%v", result, err)
	}
	return reportInputFixture{root: root, dataDir: dataDir, state: state, service: service, producerRepo: producerRepo,
		targetRepo: targetRepo, producer: producer, producerAttempt: attempt, artifact: *result.VerifiedArtifact, reportPath: reportPath, body: body}
}

func prepareReportAttempt(t *testing.T, state *store.Store, attempt model.Attempt) int {
	t.Helper()
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"finish"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	return generation
}

func createTargetWithReport(t *testing.T, fixture reportInputFixture, feature string) model.Task {
	t.Helper()
	task, err := fixture.service.CreateTaskWithReports(model.Task{Title: "Test task", DriverID: "driver:attacher", RepoID: fixture.targetRepo.ID, FeatureKey: feature,
		Objective: "implement", Deliverable: "code"}, []string{fixture.producer.ID})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func configureTargetAttempt(t *testing.T, fixture reportInputFixture, task model.Task, numberHarness string) model.Attempt {
	t.Helper()
	attempt, err := fixture.state.BeginAttempt(task.ID, numberHarness, "model")
	if err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, fixture.state, attempt.ID, "session", fixture.targetRepo.Path, "target-lease", "target-branch"); err != nil {
		t.Fatal(err)
	}
	attempt.RunGeneration = prepareReportAttempt(t, fixture.state, attempt)
	return attempt
}

func readBrief(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestExplicitCrossRepositoryReportHandoff(t *testing.T) {
	fixture := newReportInputFixture(t, "CROSS_REPO_SENTINEL\nUse strategy ALPHA.\n")
	defer fixture.state.Close()
	if err := os.WriteFile(filepath.Join(fixture.producerRepo.Path, "producer-only.txt"), []byte("not transferred"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := createTargetWithReport(t, fixture, "explicit")
	attempt := configureTargetAttempt(t, fixture, target, "pi")
	path, _, err := fixture.service.renderBrief(target, fixture.targetRepo, attempt)
	if err != nil {
		t.Fatal(err)
	}
	brief := readBrief(t, path)
	for _, want := range []string{"Explicit predecessor report", fixture.body, fixture.producer.ID, fixture.producerAttempt.ID,
		fmt.Sprint(fixture.artifact.DoneMessageID), fixture.artifact.ID, fixture.artifact.SHA256, "driver:attacher", fixture.producerRepo.ID} {
		if !strings.Contains(brief, want) {
			t.Fatalf("explicit brief missing %q:\n%s", want, brief)
		}
	}
	if _, err := os.Stat(filepath.Join(fixture.targetRepo.Path, "producer-only.txt")); !os.IsNotExist(err) {
		t.Fatalf("producer worktree file appeared in target: %v", err)
	}
}

func TestReportInputRemainsPinnedAcrossProducerMutationRetryAndTargetRecovery(t *testing.T) {
	fixture := newReportInputFixture(t, "PINNED_VERSION_ONE\n")
	defer fixture.state.Close()
	target := createTargetWithReport(t, fixture, "pinned")
	inputsBefore, err := fixture.state.ReportInputs(target.ID)
	if err != nil || len(inputsBefore) != 1 {
		t.Fatalf("inputs=%+v err=%v", inputsBefore, err)
	}
	inputBefore := inputsBefore[0]
	if err := os.WriteFile(fixture.reportPath, []byte("MUTATED_SOURCE_VERSION\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.state.PrepareRetry(fixture.producer.ID); err != nil {
		t.Fatal(err)
	}
	producerAttempt2, err := fixture.state.BeginAttempt(fixture.producer.ID, "codex", "other-model")
	if err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, fixture.state, producerAttempt2.ID, "session-2", fixture.producerRepo.Path, "lease-2", "producer-branch-2"); err != nil {
		t.Fatal(err)
	}
	producerAttempt2.RunGeneration = prepareReportAttempt(t, fixture.state, producerAttempt2)
	if err := os.WriteFile(fixture.reportPath, []byte("PRODUCER_VERSION_TWO\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "version two", NextSteps: []string{}}
	if _, err := fixture.state.AddEventForRun(producerAttempt2.ID, producerAttempt2.RunGeneration, model.Event{Type: "checkpoint", Payload: checkpoint.Summary, Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.state.AddEventForRun(producerAttempt2.ID, producerAttempt2.RunGeneration, model.Event{Type: "done", Payload: "version two", Artifact: "report:" + fixture.reportPath}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	producer2, _ := fixture.state.Task(fixture.producer.ID)
	secondResult, err := fixture.service.verifyDelivery(producer2, producerAttempt2)
	if err != nil || secondResult.VerifiedArtifact == nil {
		t.Fatalf("second verification result=%+v err=%v", secondResult, err)
	}
	if secondResult.VerifiedArtifact.ID == inputBefore.ArtifactID {
		t.Fatal("producer retry reused the first artifact identity for different bytes")
	}

	attempt1 := configureTargetAttempt(t, fixture, target, "claude-code")
	initialPath, _, err := fixture.service.renderBrief(target, fixture.targetRepo, attempt1)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "target checkpoint", NextSteps: []string{"recover"}}
	if _, err := fixture.state.AddEventForRun(attempt1.ID, attempt1.RunGeneration, model.Event{Type: "checkpoint", Payload: checkpoint.Summary, Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.state.AddEventForRun(attempt1.ID, attempt1.RunGeneration, model.Event{Type: "blocked", Payload: "recover"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	relaunchPath, _, err := fixture.service.renderRecoveryBrief(target, fixture.targetRepo, attempt1, "same-worktree relaunch", "recover")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.state.PrepareRetry(target.ID); err != nil {
		t.Fatal(err)
	}
	target, _ = fixture.state.Task(target.ID)
	attempt2 := configureTargetAttempt(t, fixture, target, "codex")
	retryPath, _, err := fixture.service.renderRecoveryBrief(target, fixture.targetRepo, attempt2, "clean retry", "recover")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{initialPath, relaunchPath, retryPath} {
		brief := readBrief(t, path)
		for _, want := range []string{"PINNED_VERSION_ONE", inputBefore.ArtifactID, inputBefore.ProducerAttemptID, inputBefore.SHA256} {
			if !strings.Contains(brief, want) {
				t.Fatalf("brief %s missing pinned value %q", path, want)
			}
		}
		for _, forbidden := range []string{"MUTATED_SOURCE_VERSION", "PRODUCER_VERSION_TWO", secondResult.VerifiedArtifact.SHA256} {
			if strings.Contains(brief, forbidden) {
				t.Fatalf("brief %s contains unpinned producer value %q", path, forbidden)
			}
		}
	}
	inputsAfter, err := fixture.state.ReportInputs(target.ID)
	if err != nil || len(inputsAfter) != 1 || inputsAfter[0] != inputBefore {
		t.Fatalf("input changed: before=%+v after=%+v err=%v", inputBefore, inputsAfter, err)
	}
}

func TestCorruptReportInputFailsClosedBeforeInitialSpawnRetryAndRelaunch(t *testing.T) {
	for _, operation := range []string{"spawn", "retry", "relaunch"} {
		t.Run(operation, func(t *testing.T) {
			fixture := newReportInputFixture(t, "TAMPER_TARGET\n")
			defer fixture.state.Close()
			target := createTargetWithReport(t, fixture, operation)
			var currentAttempt model.Attempt
			if operation != "spawn" {
				currentAttempt = configureTargetAttempt(t, fixture, target, "pi")
				checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "recoverable", NextSteps: []string{"recover"}}
				if _, err := fixture.state.AddEventForRun(currentAttempt.ID, currentAttempt.RunGeneration, model.Event{Type: "checkpoint", Payload: checkpoint.Summary, Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.state.AddEventForRun(currentAttempt.ID, currentAttempt.RunGeneration, model.Event{Type: "blocked", Payload: "hold"}, 2, model.WorkspaceFacts{}); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(fixture.artifact.SnapshotPath, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fixture.artifact.SnapshotPath, []byte("CORRUPT\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch operation {
			case "spawn":
				_, err = fixture.service.SpawnWithModel(target.ID, "pi", "")
			case "retry":
				_, err = fixture.service.RetryWithModelSelection(target.ID, "", "", false)
			case "relaunch":
				_, err = fixture.service.Relaunch(target.ID)
			}
			if err == nil || (!strings.Contains(err.Error(), "byte count mismatch") && !strings.Contains(err.Error(), "SHA-256 mismatch")) {
				t.Fatalf("error = %v", err)
			}
			current, _ := fixture.state.Task(target.ID)
			attempts, _ := fixture.state.Attempts(target.ID)
			if operation == "spawn" {
				if current.Status != "queued" || current.CurrentAttemptID != "" || len(attempts) != 0 {
					t.Fatalf("spawn failure mutated task=%+v attempts=%+v", current, attempts)
				}
			} else if current.Status != "blocked" || current.CurrentAttemptID != currentAttempt.ID || len(attempts) != 1 || attempts[0].Status != "blocked" {
				t.Fatalf("%s failure mutated task=%+v attempts=%+v", operation, current, attempts)
			}
		})
	}
}

func TestReportBriefHasHarnessAndRuntimeParity(t *testing.T) {
	fixture := newReportInputFixture(t, "HARNESS_PARITY\n")
	defer fixture.state.Close()
	target := createTargetWithReport(t, fixture, "harness-parity")
	attempt := configureTargetAttempt(t, fixture, target, "pi")
	path, _, err := fixture.service.renderBrief(target, fixture.targetRepo, attempt)
	if err != nil {
		t.Fatal(err)
	}
	prompt := readBrief(t, path)
	binDir := t.TempDir()
	for _, executable := range []string{"claude", "pi", "codex"} {
		if err := os.WriteFile(filepath.Join(binDir, executable), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, harness := range []string{"claude-code", "pi", "codex"} {
		attempt.Harness = harness
		for _, runtime := range []string{"headless", "herdr"} {
			extension := ""
			if runtime == "herdr" && (harness == "pi" || harness == "claude-code") {
				extension = "/private/bridge"
			}
			invocation, err := adapter.BuildNamedForRuntime(harness, attempt, prompt, false, "worker", runtime, extension)
			if err != nil {
				t.Fatalf("%s/%s: %v", harness, runtime, err)
			}
			if !strings.Contains(strings.Join(invocation.Args, "\x00"), "HARNESS_PARITY") || !strings.Contains(strings.Join(invocation.Args, "\x00"), fixture.artifact.SHA256) {
				t.Fatalf("%s/%s invocation dropped report input: %q", harness, runtime, invocation.Args)
			}
		}
	}
}

func TestCreateTaskRejectsIneligibleReportProducersWithoutCreatingTarget(t *testing.T) {
	root := t.TempDir()
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, _ := state.UpsertRepo(model.Repo{Name: "demo", Path: root, DefaultBranch: "main"})
	service := New(config.Config{DataDir: filepath.Join(root, "data")}, state)
	queuedReport, _ := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "queued-report", Objective: "report", Deliverable: "report"})
	code, _ := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "code-producer", Objective: "code", Deliverable: "code"})
	unverified, _ := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "unverified-report", Objective: "report", Deliverable: "report"})
	unverifiedAttempt, _ := state.BeginAttempt(unverified.ID, "pi", "")
	if err := configureAttempt(t, state, unverifiedAttempt.ID, "session", root, "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	unverifiedAttempt.RunGeneration = prepareReportAttempt(t, state, unverifiedAttempt)
	unverifiedPath := filepath.Join(root, "data", unverified.ID, "report.md")
	if err := os.MkdirAll(filepath.Dir(unverifiedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unverifiedPath, []byte("unverified\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "done", NextSteps: []string{}}
	if _, err := state.AddEventForRun(unverifiedAttempt.ID, unverifiedAttempt.RunGeneration, model.Event{Type: "checkpoint", Payload: checkpoint.Summary, Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(unverifiedAttempt.ID, unverifiedAttempt.RunGeneration, model.Event{Type: "done", Payload: "done", Artifact: "report:" + unverifiedPath}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	for index, test := range []struct {
		producer string
		want     string
	}{
		{producer: "task_missing", want: "does not exist"},
		{producer: queuedReport.ID, want: "accepted done"},
		{producer: unverified.ID, want: "no verified report artifact"},
		{producer: code.ID, want: "not report"},
	} {
		_, err := service.CreateTaskWithReports(model.Task{Title: "Test task", DriverID: "driver:attacher", RepoID: repo.ID, FeatureKey: fmt.Sprintf("target-%d", index), Objective: "target"}, []string{test.producer})
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("producer=%s error=%v, want %q", test.producer, err, test.want)
		}
	}
	tasks, err := state.Tasks(store.TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 {
		t.Fatalf("failed attachments created target rows: %+v", tasks)
	}
}

func TestReportInputAttachmentRejectsBoundsAndInvalidContent(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
		want string
	}{
		{name: "limit plus one", body: []byte(strings.Repeat("a", int(delivery.MaxReportInputBytes)+1)), want: "limited"},
		{name: "invalid UTF-8", body: []byte{0xff}, want: "UTF-8"},
		{name: "NUL", body: []byte("a\x00b"), want: "NUL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newReportInputFixture(t, string(test.body))
			defer fixture.state.Close()
			_, err := fixture.service.CreateTaskWithReports(model.Task{Title: "Test task", DriverID: "driver:attacher", RepoID: fixture.targetRepo.ID, FeatureKey: "invalid", Objective: "implement"}, []string{fixture.producer.ID})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
			tasks, listErr := fixture.state.Tasks(store.TaskFilter{RepoID: fixture.targetRepo.ID})
			if listErr != nil || len(tasks) != 0 {
				t.Fatalf("invalid attachment created tasks=%+v err=%v", tasks, listErr)
			}
		})
	}
}

func TestVerifiedReportDigestMatchesBody(t *testing.T) {
	fixture := newReportInputFixture(t, "digest body\n")
	defer fixture.state.Close()
	digest := sha256.Sum256([]byte(fixture.body))
	if fixture.artifact.SHA256 != hex.EncodeToString(digest[:]) || fixture.artifact.SizeBytes != int64(len(fixture.body)) {
		t.Fatalf("artifact=%+v", fixture.artifact)
	}
}
