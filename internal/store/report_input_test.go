package store

import (
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/model"
)

func TestVerifiedReportAndTaskInputRowsAreImmutable(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, _ := state.UpsertRepo(model.Repo{Name: "demo", Path: "/repo", DefaultBranch: "main"})
	producer, _ := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:producer", RepoID: repo.ID, FeatureKey: "producer", Objective: "report", Deliverable: "report"})
	attempt, _ := state.BeginAttempt(producer.ID, "pi", "")
	if err := state.ConfigureAttempt(attempt.ID, "session", "/tree", "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	generation := prepareAttempt(t, state, attempt)
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "done", NextSteps: []string{}}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: checkpoint.Summary, Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	artifactRef := "report:/data/" + producer.ID + "/report.md"
	message, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "done", Artifact: artifactRef}, 2, model.WorkspaceFacts{})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := state.SetVerifiedReportDelivery(producer.ID, attempt.ID, message.ID, model.VerifiedArtifact{Kind: "report", OriginalRef: artifactRef,
		SHA256: strings.Repeat("a", 64), SizeBytes: 1, SnapshotPath: "/data/artifacts/sha256/" + strings.Repeat("a", 64) + ".md"}, "verified")
	if err != nil {
		t.Fatal(err)
	}
	target, err := state.CreateTaskWithReportInputs(model.Task{Title: "Test task", DriverID: "driver:target", RepoID: repo.ID, FeatureKey: "target", Objective: "implement"}, []model.ReportArtifactSelection{{ProducerTaskID: producer.ID, ArtifactID: artifact.ID}})
	if err != nil {
		t.Fatal(err)
	}
	var position int
	if err := state.db.QueryRow(`SELECT position FROM task_report_inputs WHERE target_task_id=?`, target.ID).Scan(&position); err != nil {
		t.Fatal(err)
	}
	if position != 1 {
		t.Fatalf("single report input position = %d", position)
	}
	for _, statement := range []string{
		`UPDATE verified_artifacts SET size_bytes=2 WHERE id='` + artifact.ID + `'`,
		`DELETE FROM verified_artifacts WHERE id='` + artifact.ID + `'`,
		`UPDATE task_report_inputs SET position=2 WHERE target_task_id='` + target.ID + `'`,
		`UPDATE task_report_inputs SET attached_by_driver_id='driver:other' WHERE target_task_id='` + target.ID + `'`,
		`DELETE FROM task_report_inputs WHERE target_task_id='` + target.ID + `'`,
	} {
		if _, err := state.db.Exec(statement); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("statement %q error = %v", statement, err)
		}
	}
}
