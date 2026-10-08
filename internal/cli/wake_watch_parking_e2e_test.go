package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestWakeWatchBuiltCLIParksRejectedClaimsAndUnparksWhileRunning(t *testing.T) {
	root := t.TempDir()
	binary, extension := buildWakeWatchBinaries(t, root)
	receiver := startWakeWatchReceiver(t, root, http.StatusForbidden, http.StatusOK, http.StatusOK)
	database := filepath.Join(root, "state.db")
	config := fmt.Sprintf("database_path = %q\ndata_dir = %q\nworktree_root = %q\n[memory]\nenabled = false\n[wake]\nclaim_ttl = \"30s\"\nclaim_ttl_min = \"30s\"\n[wake_watch]\npoll_min = \"100ms\"\npoll_max = \"200ms\"\nrenew_horizon = \"30m\"\nmax_rejected_claims = 1\n[wake_watch.delivery]\nextension_id = \"shephrd.delivery-webhook\"\ncommand = [%q, \"--url\", %q, \"--secret-file\", %q, \"--allow-http\"]\nsha256 = %q\n",
		database, filepath.Join(root, "data"), filepath.Join(root, "worktrees"), extension, receiver.url+"/hook", receiver.secret, fixtureDigest(t, extension))
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := compoundCLIEnvironment(os.Environ(), map[string]string{
		"SHEPHRD_CONFIG": filepath.Join(root, "config.toml"), "SHEPHRD_WORKER": "", "PI_SESSION_ID": "",
		"SHEPHRD_SUBDRIVER_ID": "", "SHEPHRD_SUBDRIVER_GENERATION": "", "SHEPHRD_SUBDRIVER_TOKEN": "",
		"SHEPHRD_COORDINATOR_ID": "", "SHEPHRD_COORDINATOR_GENERATION": "", "SHEPHRD_COORDINATOR_TOKEN": "",
	})
	run := func(extra map[string]string, args ...string) (string, map[string]string, error) {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Env = compoundCLIEnvironment(environment, extra)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		var diagnostic map[string]string
		_ = json.Unmarshal(stderr.Bytes(), &diagnostic)
		return stdout.String(), diagnostic, err
	}
	const owner = "driver:hermes"
	state, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	var notificationIDs []string
	for _, key := range []string{"first", "second"} {
		request, err := state.HandoffSubdriver("", "Parking fixture", owner, key, "Request "+key, "", "")
		if err != nil {
			t.Fatal(err)
		}
		fence, err := state.ReserveSubdriver(request.SubdriverID, 0, "pi", "fixture", "headless")
		if err != nil {
			t.Fatal(err)
		}
		if err := state.StartSubdriver(fence, 0, "fixture"); err != nil {
			t.Fatal(err)
		}
		event, err := state.SubdriverReturn(fence, request.ID, "return", "question", "Question "+key+"?")
		if err != nil {
			t.Fatal(err)
		}
		notifications, err := state.Notifications("")
		if err != nil {
			t.Fatal(err)
		}
		for _, notification := range notifications {
			if notification.SubdriverEventID == event.ID {
				notificationIDs = append(notificationIDs, notification.NotificationID)
			}
		}
	}

	watch := exec.Command(binary, "wake", "watch", "--driver-id", owner, "--json-log")
	watch.Env = environment
	var watchLog lockedBuffer
	watch.Stdout, watch.Stderr = &watchLog, &watchLog
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- watch.Wait() }()
	t.Cleanup(func() {
		if watch.ProcessState == nil {
			_ = watch.Process.Kill()
			<-exited
		}
	})

	rejected := receiver.next("rejected delivery")
	if rejected.request.Notification.NotificationID != notificationIDs[0] {
		t.Fatalf("rejected delivery = %+v", rejected.request)
	}
	waitFor(t, "rejected outcome recorded", func() bool { return strings.Contains(watchLog.String(), `"event":"rejected"`) })
	db, err := sql.Open("sqlite", database+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE driver_notifications SET claim_until=? WHERE notification_id=?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), notificationIDs[0]); err != nil {
		t.Fatal(err)
	}
	db.Close()
	second := receiver.next("next FIFO notification after parking")
	if second.request.Notification.NotificationID != notificationIDs[1] {
		t.Fatalf("second delivery = %+v", second.request)
	}
	waitFor(t, "parked event", func() bool { return strings.Contains(watchLog.String(), `"event":"parked"`) })

	stdout, diagnostic, err := run(nil, "wake", "parked", "--driver-id", owner, "--json")
	var listed struct {
		DriverID          string                     `json:"driver_id"`
		MaxRejectedClaims int                        `json:"max_rejected_claims"`
		Parked            []model.ParkedNotification `json:"parked"`
	}
	if err != nil || json.Unmarshal([]byte(stdout), &listed) != nil || listed.DriverID != owner || listed.MaxRejectedClaims != 1 || len(listed.Parked) != 1 || listed.Parked[0].NotificationID != notificationIDs[0] || listed.Parked[0].RejectedClaims != 1 || listed.Parked[0].LastResult != "rejected" {
		t.Fatalf("wake parked = %s %+v %v", stdout, diagnostic, err)
	}
	if strings.Contains(stdout, rejected.request.Claim.ClaimToken) {
		t.Fatalf("parked list exposes a claim token: %s", stdout)
	}
	if notice, err := state.Notification(notificationIDs[0]); err != nil || notice.State != model.NotificationPending || notice.AckedAt != nil {
		t.Fatalf("parked notification = %+v %v", notice, err)
	}
	if _, diagnostic, err := run(map[string]string{"SHEPHRD_WORKER": "1"}, "wake", "unpark", notificationIDs[0], "--driver-id", owner, "--json"); err == nil {
		t.Fatalf("worker session unparked: %+v", diagnostic)
	}
	if _, diagnostic, err := run(nil, "wake", "unpark", notificationIDs[1], "--driver-id", owner, "--json"); err == nil || diagnostic["error_kind"] != "notification_not_parked" {
		t.Fatalf("unpark of an unparked notification = %+v %v", diagnostic, err)
	}
	stdout, diagnostic, err = run(nil, "wake", "unpark", notificationIDs[0], "--driver-id", owner, "--json")
	var unparked struct {
		NotificationID string `json:"notification_id"`
		Unparked       bool   `json:"unparked"`
	}
	if err != nil || json.Unmarshal([]byte(stdout), &unparked) != nil || !unparked.Unparked || unparked.NotificationID != notificationIDs[0] {
		t.Fatalf("wake unpark while the watcher runs = %s %+v %v", stdout, diagnostic, err)
	}
	ack := exec.Command(binary, second.request.Commands.Ack[1:]...)
	ack.Env = environment
	if output, err := ack.CombinedOutput(); err != nil {
		t.Fatalf("consumer ack: %s %v", output, err)
	}
	redelivered := receiver.next("unparked notification under a new claim")
	if redelivered.request.Notification.NotificationID != notificationIDs[0] || redelivered.id == rejected.id || redelivered.request.Claim.ClaimToken == rejected.request.Claim.ClaimToken || redelivered.request.Claim.DeliveryAttempt != 1 {
		t.Fatalf("redelivered = %+v", redelivered.request)
	}
	waitFor(t, "redelivery recorded", func() bool { return strings.Count(watchLog.String(), `"event":"delivered"`) == 2 })
	if err := watch.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("watcher exit: %v\n%s", err, watchLog.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watcher did not stop after SIGTERM")
	}
	for _, token := range []string{rejected.request.Claim.ClaimToken, second.request.Claim.ClaimToken, redelivered.request.Claim.ClaimToken} {
		if strings.Contains(watchLog.String(), token) {
			t.Fatalf("watcher log exposes a claim token: %s", watchLog.String())
		}
	}
	if notice, err := state.Notification(notificationIDs[0]); err != nil || notice.State != model.NotificationClaimed || notice.AckedAt != nil || notice.ClaimToken != redelivered.request.Claim.ClaimToken {
		t.Fatalf("unparked notification = %+v %v", notice, err)
	}
}
