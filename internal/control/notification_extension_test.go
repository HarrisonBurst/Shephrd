package control

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"shephrd/internal/config"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestConfiguredNotificationCapabilityPresentsWithoutChangingDurableAuthority(t *testing.T) {
	executable := buildControlNotificationExtension(t)
	logPath := filepath.Join(t.TempDir(), "presentation.json")
	_, state, task, attempt := capabilityAttempt(t, "notification-session")
	defer state.Close()
	cfg := config.Config{DataDir: t.TempDir(), Notifications: config.NotificationConfig{
		Enabled: true, Details: true, TaskPerMinute: 2, GlobalPerMinute: 10,
		Extension: &config.ExtensionConfig{Command: []string{executable, logPath, "success"}, SHA256: controlFileDigest(t, executable)},
	}}
	service := New(cfg, state)
	message := presentQuestion(t, service, state, attempt)
	payload, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "task_id") || strings.Contains(string(payload), "attempt") || strings.Contains(string(payload), "artifact") || !strings.Contains(string(payload), "title") || !strings.Contains(string(payload), "body") {
		t.Fatalf("extension payload = %s", payload)
	}
	notice, err := state.Notification("wake:" + fmt.Sprint(message.ID))
	if err != nil || notice.State != model.NotificationPending || notice.TaskID != task.ID {
		t.Fatalf("notification = %+v, err = %v", notice, err)
	}
	logs, err := state.NotificationDeliveryLogs(notice.NotificationID, 10)
	if err != nil || len(logs) != 1 || logs[0].Operation != "notify" || logs[0].Result != "sent" || !strings.Contains(logs[0].Detail, "presented=true") {
		t.Fatalf("logs = %+v, err = %v", logs, err)
	}
	current, err := state.Task(task.ID)
	if err != nil || current.Status != model.TaskStatusWaiting || current.ArtifactRef != "" || current.Landed {
		t.Fatalf("task = %+v, err = %v", current, err)
	}
}

func TestNotificationCapabilityFailureIsVisibleAndNonAuthoritative(t *testing.T) {
	executable := buildControlNotificationExtension(t)
	logPath := filepath.Join(t.TempDir(), "presentation.json")
	_, state, task, attempt := capabilityAttempt(t, "notification-failure-session")
	defer state.Close()
	service := New(config.Config{DataDir: t.TempDir(), Notifications: config.NotificationConfig{
		Enabled:   true,
		Extension: &config.ExtensionConfig{Command: []string{executable, logPath, "unconfirmed"}, SHA256: controlFileDigest(t, executable)},
	}}, state)
	message := presentQuestion(t, service, state, attempt)
	logs, err := state.NotificationDeliveryLogs("wake:"+fmt.Sprint(message.ID), 10)
	if err != nil || len(logs) != 1 || logs[0].Result != "failed" || !strings.Contains(logs[0].Detail, "protocol_violation") {
		t.Fatalf("logs = %+v, err = %v", logs, err)
	}
	current, err := state.Task(task.ID)
	if err != nil || current.Status != model.TaskStatusWaiting || current.ArtifactRef != "" || current.Landed {
		t.Fatalf("task = %+v, err = %v", current, err)
	}
}

func TestDefaultAndDisabledNotificationsLaunchNoExtension(t *testing.T) {
	executable := buildControlNotificationExtension(t)
	for name, notifications := range map[string]config.NotificationConfig{
		"default":  {},
		"disabled": {Extension: &config.ExtensionConfig{Command: []string{executable, filepath.Join(t.TempDir(), "invoked"), "success"}, SHA256: controlFileDigest(t, executable)}},
	} {
		t.Run(name, func(t *testing.T) {
			_, state, _, attempt := capabilityAttempt(t, "notification-disabled-session")
			defer state.Close()
			service := New(config.Config{DataDir: t.TempDir(), Notifications: notifications}, state)
			if service.Notifier != nil {
				t.Fatal("disabled notification capability was initialized")
			}
			message := presentQuestion(t, service, state, attempt)
			logs, err := state.NotificationDeliveryLogs("wake:"+fmt.Sprint(message.ID), 10)
			if err != nil || len(logs) != 0 {
				t.Fatalf("logs = %+v, err = %v", logs, err)
			}
			if notifications.Extension != nil {
				if _, err := os.Stat(notifications.Extension.Command[1]); !os.IsNotExist(err) {
					t.Fatalf("disabled extension was launched: %v", err)
				}
			}
		})
	}
}

func presentQuestion(t *testing.T, service Service, state *store.Store, attempt model.Attempt) model.Message {
	t.Helper()
	checkpoint := model.Checkpoint{SchemaVersion: 1, Summary: "waiting", NextSteps: []string{"answer"}}
	if _, err := state.AddEventForRun(attempt.ID, attempt.RunGeneration, model.Event{Type: "checkpoint", Payload: checkpoint.Summary, Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	message, err := service.ingestRunnerEvent(attempt, attempt.RunGeneration, model.Event{Type: "question", Payload: "choose the safe option"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func buildControlNotificationExtension(t *testing.T) string {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "presentation-extension")
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	command := exec.Command("go", "build", "-o", executable, "./internal/notification/testdata/presentationextension")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build presentation extension: %s: %v", output, err)
	}
	return executable
}

func controlFileDigest(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
