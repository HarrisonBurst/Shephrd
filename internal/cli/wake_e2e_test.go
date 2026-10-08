package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestWakeClaimShorthandHumanJSONOverridesAndRecovery(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SHEPHRD_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("SHEPHRD_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("SHEPHRD_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("PI_SESSION_ID", "wake-cli")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "wake-cli", Path: filepath.Join(dir, "repo"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	const owner = "driver:pi:wake-cli"
	task, err := state.CreateTask(model.Task{Title: "Wake shorthand", DriverID: owner, RepoID: repo.ID, FeatureKey: "wake-shorthand", Objective: "exercise wake shorthand"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(dir, "worktree")
	if err := state.BeginNativeAllocation(attempt.ID, worktree); err != nil {
		t.Fatal(err)
	}
	if err := state.ConfigureNativeWorkspace(attempt.ID, model.AttemptWorkspace{Backend: model.WorkspaceBackendNative, SessionID: "session", Path: worktree, GitDir: filepath.Join(dir, "git-dir"), CommonDir: filepath.Join(dir, "git-common"), Branch: "branch"}); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"ask"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "ready", NextSteps: []string{"ask"}}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: checkpoint.Summary, Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "question", Payload: "choose"}, 2, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	claimed, err := state.DrainNotifications(task.ID, owner, "generation:wake-cli", 1, 30*time.Second)
	if err != nil || len(claimed.Notifications) != 1 {
		t.Fatalf("claim = %+v, err = %v", claimed, err)
	}
	notification := claimed.Notifications[0]
	run := func(args ...string) (string, error) {
		t.Helper()
		root := New()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs(args)
		err := root.Execute()
		return output.String(), err
	}

	_, err = run("wake", "ack", notification.NotificationID, "--json")
	if err == nil || ErrorKind(err) != "claim_identity_missing" || !strings.Contains(err.Error(), "claim token is missing") || !strings.Contains(ErrorEvidence(err)["recovery_command"], notification.ClaimToken) {
		t.Fatalf("missing token error = %v, evidence = %+v", err, ErrorEvidence(err))
	}
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "notification ID", args: []string{"wake", "renew", "wake:wrong", "--claim-token", notification.ClaimToken, "--json"}, want: "notification ID"},
		{name: "driver generation", args: []string{"wake", "renew", "--claim-token", notification.ClaimToken, "--driver-generation", "generation:wrong", "--json"}, want: "driver generation"},
		{name: "inferred driver", args: []string{"wake", "renew", "--claim-token", notification.ClaimToken, "--driver-id", "driver:other", "--json"}, want: "current inferred driver"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := run(test.args...)
			if err == nil || ErrorKind(err) != "claim_conflict" || !strings.Contains(err.Error(), test.want) || !strings.HasPrefix(ErrorEvidence(err)["recovery_command"], "shephrd ") {
				t.Fatalf("error = %v, evidence = %+v", err, ErrorEvidence(err))
			}
		})
	}

	var renewed model.DriverNotification
	renewJSON, err := run("wake", "renew", "--claim-token", notification.ClaimToken, "--json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(renewJSON), &renewed); err != nil {
		t.Fatal(err)
	}
	if renewed.NotificationID != notification.NotificationID || renewed.ClaimOwner != owner || renewed.DriverGeneration != notification.DriverGeneration || renewed.ClaimUntil == nil {
		t.Fatalf("renewed = %+v", renewed)
	}
	if _, err := run("wake", "renew", notification.NotificationID, "--claim-token", notification.ClaimToken, "--driver-id", owner, "--driver-generation", notification.DriverGeneration, "--json"); err != nil {
		t.Fatalf("explicit renew override: %v", err)
	}
	_, err = run("wake", "ack", "--claim-token", notification.ClaimToken, "--handling-id=", "--json")
	if err == nil || ErrorKind(err) != "claim_identity_missing" || !strings.Contains(err.Error(), "handling identity is missing") || !strings.Contains(ErrorEvidence(err)["recovery_command"], notification.ClaimToken) {
		t.Fatalf("missing handling override = %v, evidence = %+v", err, ErrorEvidence(err))
	}

	human, err := run("wake", "ack", "--claim-token", notification.ClaimToken)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human, "acknowledged "+notification.NotificationID+" with handling ID handling_") {
		t.Fatalf("human ack = %q", human)
	}
	stored, err := state.Notification(notification.NotificationID)
	if err != nil || stored.State != model.NotificationAcknowledged || stored.HandlingID == "" {
		t.Fatalf("stored = %+v, err = %v", stored, err)
	}
	var repeated model.NotificationAckReceipt
	repeatJSON, err := run("wake", "ack", "--claim-token", notification.ClaimToken, "--json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(repeatJSON), &repeated); err != nil {
		t.Fatal(err)
	}
	if repeated.SchemaVersion != model.NotificationAckSchemaVersion || repeated.NotificationID != notification.NotificationID || repeated.HandlingID != stored.HandlingID || !repeated.Idempotent {
		t.Fatalf("repeated receipt = %+v", repeated)
	}
	var explicit model.NotificationAckReceipt
	explicitJSON, err := run("wake", "ack", notification.NotificationID, "--claim-token", notification.ClaimToken, "--driver-id", owner, "--driver-generation", notification.DriverGeneration, "--handling-id", stored.HandlingID, "--json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(explicitJSON), &explicit); err != nil || !explicit.Idempotent || explicit.HandlingID != stored.HandlingID {
		t.Fatalf("explicit receipt = %+v, err = %v", explicit, err)
	}
	_, err = run("wake", "ack", "--claim-token", notification.ClaimToken, "--handling-id", "handling:wrong", "--json")
	if err == nil || ErrorKind(err) != "claim_conflict" || !strings.Contains(err.Error(), "handling ID") || !strings.Contains(ErrorEvidence(err)["recovery_command"], notification.ClaimToken) {
		t.Fatalf("handling conflict = %v, evidence = %+v", err, ErrorEvidence(err))
	}

	t.Setenv("PI_SESSION_ID", "replacement")
	_, err = run("wake", "renew", "--claim-token", notification.ClaimToken, "--json")
	if err == nil || ErrorKind(err) != "claim_conflict" || !strings.Contains(err.Error(), "stored claim owner") || !strings.Contains(ErrorEvidence(err)["recovery_command"], "task adopt") || !strings.Contains(ErrorEvidence(err)["recovery_command"], "driver:pi:replacement") {
		t.Fatalf("replacement driver conflict = %v, evidence = %+v", err, ErrorEvidence(err))
	}
	t.Setenv("PI_SESSION_ID", "wake-cli")

	checkpoint.Summary = "again"
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: checkpoint.Summary, Checkpoint: &checkpoint}, 3, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "question", Payload: "again"}, 4, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	expiring, err := state.DrainNotifications(task.ID, owner, "generation:wake-cli", 1, 30*time.Second)
	if err != nil || len(expiring.Notifications) != 1 {
		t.Fatalf("expiring claim = %+v, err = %v", expiring, err)
	}
	expiringNotification := expiring.Notifications[0]
	db, err := sql.Open("sqlite", cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE driver_notifications SET claim_until=? WHERE notification_id=?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), expiringNotification.NotificationID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"ack", "renew"} {
		_, err = run("wake", operation, "--claim-token", expiringNotification.ClaimToken, "--json")
		if err == nil || ErrorKind(err) != "claim_expired" || !strings.Contains(err.Error(), "claim expired") || !strings.Contains(ErrorEvidence(err)["recovery_command"], "wake drain") || !strings.Contains(ErrorEvidence(err)["recovery_command"], task.ID) {
			t.Fatalf("expired %s error = %v, evidence = %+v", operation, err, ErrorEvidence(err))
		}
	}
	expiredStored, storedErr := state.Notification(expiringNotification.NotificationID)
	if storedErr != nil || expiredStored.State != model.NotificationClaimed || expiredStored.AckedAt != nil {
		t.Fatalf("expired stored = %+v, err = %v", expiredStored, storedErr)
	}
}
