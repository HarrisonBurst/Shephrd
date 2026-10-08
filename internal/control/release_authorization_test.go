package control

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

func newReleaseFixture(t *testing.T) (Service, *store.Store, model.Task, string) {
	t.Helper()
	databasePath := filepath.Join(t.TempDir(), "state.db")
	state, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: t.TempDir(), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "feature", Objective: "work"})
	if err != nil {
		t.Fatal(err)
	}
	service := New(config.Config{DataDir: t.TempDir(), WorktreeRoot: t.TempDir()}, state)
	return service, state, task, databasePath
}

func TestSpawnAcquireFailureRecordsNoWorkspace(t *testing.T) {
	installFakeHarnesses(t, "claude")
	service, state, task, _ := newReleaseFixture(t)
	if _, err := service.SpawnWithModel(task.ID, "claude-code", "", "headless"); err == nil {
		t.Fatal("spawn succeeded with an exhausted pool")
	}
	current, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.Attempt(current.CurrentAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.WorktreePath != "" || attempt.LeaseID != "" {
		t.Fatalf("failed acquisition recorded a workspace: %+v", attempt)
	}
	if attempt.ReleaseState != "no_workspace" || attempt.ReleasedAt != nil {
		t.Fatalf("failed acquisition is not classified no_workspace: %+v", attempt)
	}
	if !strings.Contains(attempt.ReleaseReason, "before any worktree existed") {
		t.Fatalf("no-workspace reason = %q", attempt.ReleaseReason)
	}
	if current.Status != "blocked" {
		t.Fatalf("task status = %s", current.Status)
	}
	// Releasing a no-workspace attempt is a safe no-op: there is nothing to
	// return and nothing to discard.
	if err := service.Release(task.ID, attempt.ID, false); err != nil {
		t.Fatalf("no-workspace release = %v", err)
	}
	if err := service.Release(task.ID, attempt.ID, true); err != nil {
		t.Fatalf("no-workspace discard release = %v", err)
	}
	refreshed, err := state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.ReleaseState != "no_workspace" || refreshed.DiscardAuthorized {
		t.Fatalf("no-workspace attempt mutated by release: %+v", refreshed)
	}
}

func TestReleaseRefusesAttestedLandingProofWithoutEvidence(t *testing.T) {
	for _, test := range []struct {
		kind string
		want string
	}{
		{kind: "github_pr_attested_ancestry", want: "external delivery attestation evidence"},
		{kind: model.LandingKindLocalAttestedAncestry, want: "local delivery recovery evidence"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			service, state, task, databasePath := newReleaseFixture(t)
			attempt, err := state.BeginAttempt(task.ID, "pi", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := configureAttempt(t, state, attempt.ID, "session", "/tree", "lease-1", "branch"); err != nil {
				t.Fatal(err)
			}
			legacy, err := sql.Open("sqlite", databasePath+"?_busy_timeout=5000")
			if err != nil {
				t.Fatal(err)
			}
			defer legacy.Close()
			verifiedAt := time.Now().UTC().Format(time.RFC3339Nano)
			if _, err := legacy.Exec(`UPDATE attempts SET landed_proven=1, landing_kind=?, landed_source_commit='source',
				landed_target_ref='refs/heads/main', landed_target_commit='target', landed_checkpoint_revision=1,
				landed_verified_at=?, updated_at=? WHERE id=?`, test.kind, verifiedAt, verifiedAt, attempt.ID); err != nil {
				t.Fatal(err)
			}
			projected, err := state.Task(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if projected.Landed {
				t.Fatalf("unsupported attested proof projected onto task: %+v", projected)
			}
			if err := service.Release(task.ID, attempt.ID, false); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("release without attestation evidence = %v", err)
			}
			held, err := state.Attempt(attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if held.ReleaseState != "held" || held.ReleasedAt != nil {
				t.Fatalf("unproven attested attempt was released: %+v", held)
			}
		})
	}
}

func TestReleaseRefusesUnvalidatedLandingProof(t *testing.T) {
	service, state, _, databasePath := newReleaseFixture(t)
	repo, err := state.Repo("demo")
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "legacy-report", Objective: "report", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, "session", "/tree", "lease-1", "branch"); err != nil {
		t.Fatal(err)
	}
	if err := service.Release(task.ID, attempt.ID, false); err == nil || !strings.Contains(err.Error(), "no validated landing/report proof") {
		t.Fatalf("release without proof = %v", err)
	}
	legacy, err := sql.Open("sqlite", databasePath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	verifiedAt := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := legacy.Exec(`UPDATE attempts SET landed_proven=1, landing_kind='report_artifact', landed_verified_at=?, updated_at=? WHERE id=?`, verifiedAt, verifiedAt, attempt.ID); err != nil {
		t.Fatal(err)
	}
	projected, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projected.Landed || projected.LandedReason != "" {
		t.Fatalf("incomplete attempt proof projected onto task: %+v", projected)
	}
	if err := service.Release(task.ID, attempt.ID, false); err == nil || !strings.Contains(err.Error(), "verified report artifact") {
		t.Fatalf("release with incomplete report proof = %v", err)
	}
}
