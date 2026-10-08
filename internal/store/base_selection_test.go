package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"shephrd/internal/model"
)

func TestAttemptBaseSelectionIsDurable(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "base", Path: filepath.Join(t.TempDir(), "repo"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "base", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "base", Objective: "base", Deliverable: "code"})
	if err != nil {
		t.Fatal(err)
	}
	want := model.BaseSelection{Strategy: model.BaseStrategyCommit, Ref: "0123456789012345678901234567890123456789"}
	attempt, err := state.BeginAttemptWithBaseSelection(task.ID, "pi", "", want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseStrategy != want.Strategy || got.BaseRef != want.Ref {
		t.Fatalf("base selection = %q/%q, want %q/%q", got.BaseStrategy, got.BaseRef, want.Strategy, want.Ref)
	}
}

func TestV28BaselineGainsBaseSelectionColumnsAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	body, err := os.ReadFile("baseline_v28.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES(28, '028_consolidated_baseline', 'e9686cf907fa2c051bf53c8d6eeeee49e846c7c53c7dbed9569b6fd22b9ee552', '2026-08-18T15:35:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	var strategy, ref string
	if err := state.db.QueryRow(`SELECT base_strategy, base_ref FROM attempts LIMIT 1`).Scan(&strategy, &ref); err != sql.ErrNoRows {
		t.Fatalf("empty attempt selection query = %v", err)
	}
	if _, err := state.db.Exec(`INSERT INTO repos(id, name, path, default_branch, created_at, updated_at) VALUES('repo', 'repo', '/tmp/repo', 'main', 'now', 'now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := state.db.Exec(`INSERT INTO tasks(id, repo_id, feature_key, title, driver_id, objective, status, created_at, updated_at) VALUES('task', 'repo', 'task', 'task', 'driver', 'task', 'queued', 'now', 'now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := state.db.Exec(`INSERT INTO attempts(id, task_id, number, harness, status, created_at, updated_at) VALUES('attempt', 'task', 1, 'pi', 'starting', 'now', 'now')`); err != nil {
		t.Fatal(err)
	}
	if err := state.db.QueryRow(`SELECT base_strategy, base_ref FROM attempts WHERE id='attempt'`).Scan(&strategy, &ref); err != nil {
		t.Fatal(err)
	}
	if strategy != model.BaseStrategyDefaultBranch || ref != "" {
		t.Fatalf("migrated base selection = %q/%q", strategy, ref)
	}
}
