package cli

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"shephrd/internal/driverdelivery"
	"shephrd/internal/driverdelivery/webhook"
	extensionhost "shephrd/internal/extension"
	"shephrd/internal/model"
	"shephrd/internal/store"
	"shephrd/internal/wakewatch"
)

type webhookDelivery struct {
	id        string
	timestamp string
	signature string
	body      []byte
	request   driverdelivery.Request
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func TestWakeWatchBuiltCLIDeliversSignedClaimsWithOwnerExclusivity(t *testing.T) {
	root := t.TempDir()
	binary, extension := buildWakeWatchBinaries(t, root)
	receiver := startWakeWatchReceiver(t, root, http.StatusOK, http.StatusServiceUnavailable)
	next, secret := receiver.next, receiver.secret

	database := filepath.Join(root, "state.db")
	base := fmt.Sprintf("database_path = %q\ndata_dir = %q\nworktree_root = %q\n[memory]\nenabled = false\n[wake]\nclaim_ttl = \"30s\"\nclaim_ttl_min = \"30s\"\n[wake_watch]\npoll_min = \"100ms\"\npoll_max = \"200ms\"\nrenew_horizon = \"2s\"\n", database, filepath.Join(root, "data"), filepath.Join(root, "worktrees"))
	digest := fixtureDigest(t, extension)
	delivery := fmt.Sprintf("[wake_watch.delivery]\nextension_id = \"shephrd.delivery-webhook\"\ncommand = [%q, \"--url\", %q, \"--secret-file\", %q, \"--allow-http\"]\nsha256 = %q\n", extension, receiver.url+"/hook", secret, digest)
	configs := map[string]string{"config": base + delivery, "unconfigured": base, "unpinned": base + strings.Replace(delivery, digest, strings.Repeat("0", 64), 1), "disabled": strings.Replace(base, "[wake]\n", "[wake]\nenabled = false\n", 1) + delivery}
	for name, body := range configs {
		if err := os.WriteFile(filepath.Join(root, name+".toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
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
	refuse := func(kind string, extra map[string]string, args ...string) map[string]string {
		t.Helper()
		_, diagnostic, err := run(extra, append(args, "--json")...)
		if err == nil || (kind != "" && diagnostic["error_kind"] != kind) {
			t.Fatalf("%v: err = %v, diagnostic = %+v", args, err, diagnostic)
		}
		return diagnostic
	}

	const owner = "driver:hermes"
	state, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	general, err := state.HandoffSubdriver("", "Watch fixture", owner, "general", "General request", "", "")
	if err != nil {
		t.Fatal(err)
	}
	fence, err := state.ReserveSubdriver(general.SubdriverID, 0, "pi", "fixture", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.StartSubdriver(fence, 0, "fixture"); err != nil {
		t.Fatal(err)
	}
	var notificationIDs []string
	for index := range 2 {
		request, returnFence := general, fence
		if index > 0 {
			if request, err = state.HandoffSubdriver("", "Watch fixture", owner, "second", "Second request", "", ""); err != nil {
				t.Fatal(err)
			}
			if returnFence, err = state.ReserveSubdriver(request.SubdriverID, 0, "pi", "fixture", "headless"); err != nil {
				t.Fatal(err)
			}
			if err := state.StartSubdriver(returnFence, 0, "fixture"); err != nil {
				t.Fatal(err)
			}
		}
		event, err := state.SubdriverReturn(returnFence, request.ID, "return", "question", fmt.Sprintf("Question %d?", index+1))
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
	if len(notificationIDs) != 2 {
		t.Fatalf("fixture notifications = %v", notificationIDs)
	}

	refuse("driver_context_required", nil, "wake", "watch")
	refuse("wake_watch_owner_refused", nil, "wake", "watch", "--driver-id", "driver:pi:session")
	refuse("", nil, "wake", "watch", "--driver-id", "coordinator:"+general.SubdriverID)
	refuse("", map[string]string{"SHEPHRD_WORKER": "1"}, "wake", "watch", "--driver-id", owner)
	session := map[string]string{"SHEPHRD_SUBDRIVER_ID": fence.ID, "SHEPHRD_SUBDRIVER_GENERATION": strconv.Itoa(fence.Generation), "SHEPHRD_SUBDRIVER_TOKEN": fence.Token}
	if diagnostic := refuse("", session, "wake", "watch", "--driver-id", owner); !strings.Contains(diagnostic["error"], "not permitted in a sub-driver session") {
		t.Fatalf("sub-driver session watcher = %+v", diagnostic)
	}
	refuse("wake_watch_unconfigured", map[string]string{"SHEPHRD_CONFIG": filepath.Join(root, "unconfigured.toml")}, "wake", "watch", "--driver-id", owner)
	refuse("", map[string]string{"SHEPHRD_CONFIG": filepath.Join(root, "disabled.toml")}, "wake", "watch", "--driver-id", owner)
	if diagnostic := refuse("", map[string]string{"SHEPHRD_CONFIG": filepath.Join(root, "unpinned.toml")}, "wake", "watch", "--driver-id", owner); !strings.Contains(diagnostic["error"], "SHA-256") {
		t.Fatalf("unpinned extension = %+v", diagnostic)
	}
	for _, id := range notificationIDs {
		if notice, err := state.Notification(id); err != nil || notice.State != model.NotificationPending {
			t.Fatalf("refused watcher touched %s: %+v %v", id, notice, err)
		}
	}
	pi := map[string]string{"PI_SESSION_ID": "session"}
	for _, args := range [][]string{{"wake", "drain", "--json"}, {"wake", "pump", "--json"}} {
		if _, diagnostic, err := run(pi, args...); err != nil {
			t.Fatalf("Pi %v: %v %+v", args, err, diagnostic)
		}
	}
	refuse("", session, "wake", "drain")
	refuse("", session, "wake", "pump")
	for _, ineligible := range []string{"driver:pi:session", "coordinator:" + general.SubdriverID} {
		if _, err := os.Stat(wakewatch.LockPath(filepath.Join(root, "data"), ineligible)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("manual command created a watcher lock for %s: %v", ineligible, err)
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

	first := next("first delivery")
	if first.request.Notification.NotificationID != notificationIDs[0] || first.request.Driver.ID != owner || !strings.HasPrefix(first.request.Driver.Generation, "watch:") ||
		first.request.Notification.Kind != "subdriver-question" || first.request.Notification.Payload != "Question 1?" || first.request.Claim.DeliveryAttempt != 1 {
		t.Fatalf("first delivery = %+v", first.request)
	}
	for _, args := range [][]string{{"wake", "drain", "--driver-id", owner}, {"wake", "pump", "--driver-id", owner}, {"wake", "watch", "--driver-id", owner}} {
		if diagnostic := refuse("wake_watch_active", nil, args...); diagnostic["watcher_generation"] != first.request.Driver.Generation && args[1] != "watch" {
			t.Fatalf("%v diagnostic = %+v", args, diagnostic)
		}
	}
	if _, _, err := run(nil, "wake", "drain", "--driver-id", "driver:other", "--json"); err != nil {
		t.Fatalf("other owner drain: %v", err)
	}
	if _, err := os.Stat(wakewatch.LockPath(filepath.Join(root, "data"), "driver:other")); err != nil {
		t.Fatalf("eligible owner drain did not take the watcher lock: %v", err)
	}

	repo, err := state.UpsertRepo(model.Repo{Name: "activation", Path: filepath.Join(root, "activation"), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := state.HandoffSubdriver(repo.ID, "", owner, "activation", "Activation request", "", "")
	if err != nil {
		t.Fatal(err)
	}
	dead := exec.Command("/usr/bin/true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	activationFence, err := state.ReserveSubdriver(activation.SubdriverID, 0, "pi", "fixture", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.StartSubdriver(activationFence, dead.Process.Pid, "fixture"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pump-only activation pass", func() bool {
		subdriver, err := state.Subdriver(activation.SubdriverID)
		return err == nil && subdriver.State == "held"
	})
	if notice, err := state.Notification(notificationIDs[1]); err != nil || notice.State != model.NotificationPending {
		t.Fatalf("activation pass drained main notification: %+v %v", notice, err)
	}
	if notice, err := state.Notification(notificationIDs[0]); err != nil || notice.State != model.NotificationClaimed || notice.ClaimToken != first.request.Claim.ClaimToken {
		t.Fatalf("first claim changed during activation: %+v %v", notice, err)
	}

	ack := exec.Command(binary, first.request.Commands.Ack[1:]...)
	ack.Env = environment
	if output, err := ack.CombinedOutput(); err != nil {
		t.Fatalf("consumer ack: %s %v", output, err)
	}
	second := next("second delivery")
	retried := next("second delivery retry")
	if second.request.Notification.NotificationID != notificationIDs[1] || retried.id != second.id || second.request.Claim.DeliveryAttempt != 1 || retried.request.Claim.DeliveryAttempt != 2 {
		t.Fatalf("second = %+v, retried = %+v", second.request, retried.request)
	}
	waitFor(t, "renewal horizon", func() bool { return strings.Contains(watchLog.String(), `"event":"horizon"`) })
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE driver_notifications SET claim_until=? WHERE notification_id=?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), notificationIDs[1]); err != nil {
		t.Fatal(err)
	}
	db.Close()
	redelivered := next("redelivery under a new claim")
	if redelivered.request.Notification.NotificationID != notificationIDs[1] || redelivered.id == second.id || redelivered.request.Claim.ClaimToken == second.request.Claim.ClaimToken || redelivered.request.Claim.DeliveryAttempt != 1 {
		t.Fatalf("redelivered = %+v", redelivered.request)
	}

	waitFor(t, "recorded redelivery outcome", func() bool { return strings.Count(watchLog.String(), `"event":"delivered"`) == 3 })
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
	acknowledged, err := state.Notification(notificationIDs[0])
	if err != nil || acknowledged.State != model.NotificationAcknowledged || acknowledged.AckOwner != owner || acknowledged.AckDriverGeneration != first.request.Driver.Generation {
		t.Fatalf("consumer acknowledgement = %+v %v", acknowledged, err)
	}
	outstanding, err := state.Notification(notificationIDs[1])
	if err != nil || outstanding.State != model.NotificationClaimed || outstanding.AckedAt != nil || outstanding.ClaimToken != redelivered.request.Claim.ClaimToken {
		t.Fatalf("SIGTERM changed the outstanding claim: %+v %v", outstanding, err)
	}
	logs, err := state.NotificationDeliveryLogs(notificationIDs[1], 100)
	if err != nil {
		t.Fatal(err)
	}
	var operations []string
	for _, log := range logs {
		operations = append(operations, log.Operation+":"+log.Result)
	}
	if strings.Join(operations, ",") != "claim:claimed,notify:retryable,notify:delivered,reclaim:reclaimed,claim:claimed,notify:delivered" {
		t.Fatalf("delivery log = %v", operations)
	}
	for _, line := range strings.Split(strings.TrimSpace(watchLog.String()), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("watcher log line %q: %v", line, err)
		}
		if strings.Contains(line, first.request.Claim.ClaimToken) || strings.Contains(line, second.request.Claim.ClaimToken) {
			t.Fatalf("watcher log exposes a claim token: %s", line)
		}
	}
	if !strings.Contains(watchLog.String(), `"event":"stopped"`) || !strings.Contains(watchLog.String(), `"event":"acknowledged"`) {
		t.Fatalf("watcher log = %s", watchLog.String())
	}
}

func TestWakeWatchBuiltCLIRestartWaitsForOutstandingClaimAndDeliversLongTitles(t *testing.T) {
	root := t.TempDir()
	binary, extension := buildWakeWatchBinaries(t, root)
	receiver := startWakeWatchReceiver(t, root)
	database := filepath.Join(root, "state.db")
	repoRoot := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	contextGit(t, repoRoot, "init", "-b", "main")
	contextGit(t, repoRoot, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial")
	config := fmt.Sprintf("database_path = %q\ndata_dir = %q\nworktree_root = %q\n[memory]\nenabled = false\n[wake]\nclaim_ttl = \"30s\"\nclaim_ttl_min = \"30s\"\n[wake_watch]\npoll_min = \"100ms\"\npoll_max = \"200ms\"\nrenew_horizon = \"30m\"\n[wake_watch.delivery]\nextension_id = \"shephrd.delivery-webhook\"\ncommand = [%q, \"--url\", %q, \"--secret-file\", %q, \"--allow-http\"]\nsha256 = %q\n",
		database, filepath.Join(root, "data"), filepath.Join(root, "worktrees"), extension, receiver.url+"/hook", receiver.secret, fixtureDigest(t, extension))
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := compoundCLIEnvironment(os.Environ(), map[string]string{
		"SHEPHRD_CONFIG": filepath.Join(root, "config.toml"), "SHEPHRD_WORKER": "", "PI_SESSION_ID": "",
		"SHEPHRD_SUBDRIVER_ID": "", "SHEPHRD_SUBDRIVER_GENERATION": "", "SHEPHRD_SUBDRIVER_TOKEN": "",
		"SHEPHRD_COORDINATOR_ID": "", "SHEPHRD_COORDINATOR_GENERATION": "", "SHEPHRD_COORDINATOR_TOKEN": "",
	})
	run := func(args ...string) []byte {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Env = environment
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("shephrd %v: %s %v", args, stderr.String(), err)
		}
		return stdout.Bytes()
	}

	const owner = "driver:hermes"
	state, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	var returns []string
	for _, key := range []string{"first", "second"} {
		request, err := state.HandoffSubdriver("", "Restart fixture", owner, key, "Request "+key, "", "")
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
				returns = append(returns, notification.NotificationID)
			}
		}
	}
	if len(returns) != 2 {
		t.Fatalf("fixture returns = %v", returns)
	}
	run("repo", "add", repoRoot, "--name", "titles", "--json")
	titles := []string{strings.Repeat("Long explicit title ", 60), strings.Repeat("長いタイトル", 70)}
	var tasks []model.Task
	var questions []string
	for index, title := range titles {
		var task model.Task
		if err := json.Unmarshal(run("task", "create", "--repo", "titles", "--feature", fmt.Sprintf("long-%d", index), "--title", title, "--deliverable", "code", "--driver-id", owner, "Exercise long titles", "--json"), &task); err != nil {
			t.Fatal(err)
		}
		if task.Title != strings.TrimSpace(title) || len(task.Title) <= 1024 {
			t.Fatalf("task title = %d bytes, want explicit title over 1024 bytes", len(task.Title))
		}
		attempt, err := state.BeginAttempt(task.ID, "pi", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := configureAttempt(t, state, attempt.ID, "session", filepath.Join(root, "tree-"+attempt.ID), "lease-"+attempt.ID, "branch-"+attempt.ID); err != nil {
			t.Fatal(err)
		}
		generation, err := state.ReserveRunGeneration(attempt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"answer"}, model.WorkspaceFacts{}); err != nil {
			t.Fatal(err)
		}
		checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: "waiting", NextSteps: []string{"answer"}}
		if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "checkpoint", Payload: "waiting", Checkpoint: &checkpoint}, 1, model.WorkspaceFacts{}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.AddEventForRun(attempt.ID, generation, model.Event{Type: "question", Payload: "Which option?"}, 2, model.WorkspaceFacts{}); err != nil {
			t.Fatal(err)
		}
		notifications, err := state.Notifications(task.ID)
		if err != nil || len(notifications) != 1 {
			t.Fatalf("task %s notifications = %+v %v", task.ID, notifications, err)
		}
		tasks = append(tasks, task)
		questions = append(questions, notifications[0].NotificationID)
	}

	start := func() (*exec.Cmd, *lockedBuffer, chan error) {
		t.Helper()
		watch := exec.Command(binary, "wake", "watch", "--driver-id", owner, "--json-log")
		watch.Env = environment
		log := &lockedBuffer{}
		watch.Stdout, watch.Stderr = log, log
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
		waitFor(t, "watcher start", func() bool { return strings.Contains(log.String(), `"event":"started"`) })
		return watch, log, exited
	}
	stop := func(watch *exec.Cmd, log *lockedBuffer, exited chan error) {
		t.Helper()
		if err := watch.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-exited:
			if err != nil {
				t.Fatalf("watcher exit: %v\n%s", err, log.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("watcher did not stop after SIGTERM")
		}
	}
	ack := func(delivery webhookDelivery) {
		t.Helper()
		command := exec.Command(binary, delivery.request.Commands.Ack[1:]...)
		command.Env = environment
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("consumer ack: %s %v", output, err)
		}
	}
	pending := func(id string) {
		t.Helper()
		if notice, err := state.Notification(id); err != nil || notice.State != model.NotificationPending {
			t.Fatalf("notification %s = %+v %v, want pending", id, notice, err)
		}
	}

	watch, log, exited := start()
	first := receiver.next("first return")
	if first.request.Notification.NotificationID != returns[0] {
		t.Fatalf("first delivery = %+v", first.request)
	}
	stop(watch, log, exited)

	watch, log, exited = start()
	receiver.none("delivery while the previous generation's claim is outstanding", 1500*time.Millisecond)
	pending(returns[1])
	if outstanding, err := state.Notification(returns[0]); err != nil || outstanding.State != model.NotificationClaimed || outstanding.ClaimToken != first.request.Claim.ClaimToken || outstanding.DriverGeneration != first.request.Driver.Generation {
		t.Fatalf("restarted watcher changed the outstanding claim: %+v %v", outstanding, err)
	}
	ack(first)
	second := receiver.next("second return after the outstanding claim is acknowledged")
	if second.request.Notification.NotificationID != returns[1] || second.request.Driver.Generation == first.request.Driver.Generation {
		t.Fatalf("second delivery = %+v", second.request)
	}
	stop(watch, log, exited)

	watch, log, exited = start()
	receiver.none("delivery while the previous generation's claim is unexpired", 1500*time.Millisecond)
	pending(questions[0])
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE driver_notifications SET claim_until=? WHERE notification_id=?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), returns[1]); err != nil {
		t.Fatal(err)
	}
	db.Close()
	redelivered := receiver.next("expired return redelivered first")
	if redelivered.request.Notification.NotificationID != returns[1] || redelivered.request.Claim.ClaimToken == second.request.Claim.ClaimToken {
		t.Fatalf("redelivered = %+v", redelivered.request)
	}
	ack(redelivered)
	previous := redelivered
	for index, task := range tasks {
		delivery := receiver.next(fmt.Sprintf("long title %d", index))
		title := delivery.request.Notification.TaskTitle
		if delivery.request.Notification.TaskID != task.ID || len(title) > 1024 || len(title) < 1000 || !utf8.ValidString(title) || !strings.HasPrefix(task.Title, title) {
			t.Fatalf("long title delivery %d = %q (%d bytes)", index, title, len(title))
		}
		if delivery.request.Claim.ClaimToken == previous.request.Claim.ClaimToken {
			t.Fatalf("long title delivery %d reused a claim", index)
		}
		ack(delivery)
		previous = delivery
	}
	waitFor(t, "final acknowledgement observed", func() bool { return strings.Count(log.String(), `"event":"acknowledged"`) == 3 })
	stop(watch, log, exited)
	for _, id := range append(returns, questions...) {
		if notice, err := state.Notification(id); err != nil || notice.State != model.NotificationAcknowledged {
			t.Fatalf("notification %s = %+v %v", id, notice, err)
		}
	}
	if strings.Contains(log.String(), `"event":"undeliverable"`) {
		t.Fatalf("watcher log = %s", log.String())
	}
}

func TestWakeWatchBuiltCLIKeepsClaimRenewedDuringSlowActivation(t *testing.T) {
	root := t.TempDir()
	binary, extension := buildWakeWatchBinaries(t, root)
	terminalExtension := filepath.Join(root, "herdr")
	build := exec.Command("go", "build", "-o", terminalExtension, "./internal/terminal/testdata/herdrextension")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build terminal fixture: %s %v", output, err)
	}
	receiver := startWakeWatchReceiver(t, root)
	database := filepath.Join(root, "state.db")
	operations := filepath.Join(root, "operations")
	config := fmt.Sprintf("database_path = %q\ndata_dir = %q\nworktree_root = %q\n[memory]\nenabled = false\n[wake]\nclaim_ttl = \"30s\"\nclaim_ttl_min = \"30s\"\n[wake_watch]\npoll_min = \"100ms\"\npoll_max = \"200ms\"\nrenew_horizon = \"30m\"\n[wake_watch.delivery]\nextension_id = \"shephrd.delivery-webhook\"\ncommand = [%q, \"--url\", %q, \"--secret-file\", %q, \"--allow-http\"]\nsha256 = %q\n[terminal_extensions.herdr]\ncommand = [%q, %q, %q]\nsha256 = %q\n",
		database, filepath.Join(root, "data"), filepath.Join(root, "worktrees"), extension, receiver.url+"/hook", receiver.secret, fixtureDigest(t, extension), terminalExtension, operations, binary, fixtureDigest(t, terminalExtension))
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := compoundCLIEnvironment(os.Environ(), map[string]string{
		"SHEPHRD_CONFIG": filepath.Join(root, "config.toml"), "SHEPHRD_WORKER": "", "PI_SESSION_ID": "",
		"SHEPHRD_SUBDRIVER_ID": "", "SHEPHRD_SUBDRIVER_GENERATION": "", "SHEPHRD_SUBDRIVER_TOKEN": "",
		"SHEPHRD_COORDINATOR_ID": "", "SHEPHRD_COORDINATOR_GENERATION": "", "SHEPHRD_COORDINATOR_TOKEN": "",
	})

	const owner = "driver:hermes"
	state, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	request, err := state.HandoffSubdriver("", "Slow activation fixture", owner, "question", "Question request", "", "")
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
	event, err := state.SubdriverReturn(fence, request.ID, "return", "question", "Question?")
	if err != nil {
		t.Fatal(err)
	}
	notifications, err := state.Notifications("")
	if err != nil {
		t.Fatal(err)
	}
	var notificationID string
	for _, notification := range notifications {
		if notification.SubdriverEventID == event.ID {
			notificationID = notification.NotificationID
		}
	}
	const idleOwners = 8
	for index := range idleOwners {
		idle, err := state.HandoffSubdriver("", "Slow activation fixture", owner, fmt.Sprintf("idle-%d", index), "Idle request", "", "")
		if err != nil {
			t.Fatal(err)
		}
		idleFence, err := state.ReserveSubdriver(idle.SubdriverID, 0, "pi", "fixture", "herdr")
		if err != nil {
			t.Fatal(err)
		}
		if err := state.SetSubdriverEndpoint(idleFence, model.TerminalEndpoint{Backend: "herdr", SocketPath: "/socket", WorkspaceID: "w7", TabID: "w7:t2", PaneID: "w7:p2"}); err != nil {
			t.Fatal(err)
		}
		if err := state.StartSubdriver(idleFence, 0, "fixture"); err != nil {
			t.Fatal(err)
		}
		page, err := state.SubdriverPage(idleFence.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range page.Events {
			if err := state.HandleSubdriverEvent(idleFence, event.ID); err != nil {
				t.Fatal(err)
			}
		}
		if err := state.FinishSubdriver(idleFence, "retained", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(operations+".absent", nil, 0o600); err != nil {
		t.Fatal(err)
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

	delivered := receiver.next("claim delivery")
	if delivered.request.Notification.NotificationID != notificationID {
		t.Fatalf("delivery = %+v", delivered.request)
	}
	probes := func() int {
		t.Helper()
		body, err := os.ReadFile(operations)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(body), "process_info\n")
	}
	if err := os.WriteFile(operations+".delay", []byte("4s"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := probes()
	receiver.none("redelivery during slow activation", time.Until(delivered.request.Claim.ClaimUntil)+2*time.Second)
	if slow := probes() - before; slow == 0 || slow > idleOwners+1 {
		t.Fatalf("activation probes across the original lease = %d, want one slow pass", slow)
	}
	if strings.Count(watchLog.String(), `"event":"pump_failed"`) != 0 {
		t.Fatalf("slow activation failed instead of overlapping the lease: %s", watchLog.String())
	}
	outstanding, err := state.Notification(notificationID)
	if err != nil || outstanding.State != model.NotificationClaimed || outstanding.ClaimToken != delivered.request.Claim.ClaimToken || outstanding.ClaimUntil == nil || !outstanding.ClaimUntil.After(time.Now()) {
		t.Fatalf("claim during slow activation = %+v %v\n%s", outstanding, err, watchLog.String())
	}
	ack := exec.Command(binary, delivered.request.Commands.Ack[1:]...)
	ack.Env = environment
	if output, err := ack.CombinedOutput(); err != nil {
		t.Fatalf("consumer ack after the original lease: %s %v\n%s", output, err, watchLog.String())
	}
	if err := os.Remove(operations + ".delay"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "acknowledgement observed", func() bool { return strings.Contains(watchLog.String(), `"event":"acknowledged"`) })
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
	log := watchLog.String()
	if !strings.Contains(log, `"event":"renewed"`) || strings.Contains(log, `"event":"expired"`) || strings.Count(log, `"event":"delivered"`) != 1 || !strings.Contains(log, `"event":"stopped"`) {
		t.Fatalf("watcher log = %s", log)
	}
	owners, err := state.SubdriverCandidates(owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range owners {
		if candidate.ID != request.SubdriverID && candidate.State != "idle" {
			t.Fatalf("idle owner changed during slow activation: %+v", candidate)
		}
	}
}

type wakeWatchReceiver struct {
	t          *testing.T
	url        string
	secret     string
	key        []byte
	deliveries chan webhookDelivery
}

func startWakeWatchReceiver(t *testing.T, root string, statuses ...int) *wakeWatchReceiver {
	t.Helper()
	receiver := &wakeWatchReceiver{t: t, secret: filepath.Join(root, "secret"), key: []byte("fixture signing key"), deliveries: make(chan webhookDelivery, 16)}
	if err := os.WriteFile(receiver.secret, []byte("whsec_"+base64.StdEncoding.EncodeToString(receiver.key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var decoded driverdelivery.Request
		if err := extensionhost.StrictDecode(body, &decoded); err != nil {
			t.Errorf("receiver body: %v", err)
		}
		receiver.deliveries <- webhookDelivery{id: request.Header.Get("webhook-id"), timestamp: request.Header.Get("webhook-timestamp"), signature: request.Header.Get("webhook-signature"), body: body, request: decoded}
		mu.Lock()
		status := http.StatusOK
		if len(statuses) > 0 {
			status, statuses = statuses[0], statuses[1:]
		}
		mu.Unlock()
		writer.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	receiver.url = server.URL
	return receiver
}

func (r *wakeWatchReceiver) next(description string) webhookDelivery {
	r.t.Helper()
	select {
	case delivery := <-r.deliveries:
		if delivery.signature != webhook.Signature(r.key, delivery.id, delivery.timestamp, delivery.body) || delivery.id != webhook.WebhookID(delivery.request.Notification.NotificationID, delivery.request.Claim.ClaimToken) {
			r.t.Fatalf("%s: unsigned or misidentified delivery %+v", description, delivery)
		}
		return delivery
	case <-time.After(15 * time.Second):
		r.t.Fatalf("timed out waiting for %s", description)
	}
	return webhookDelivery{}
}

func (r *wakeWatchReceiver) none(description string, wait time.Duration) {
	r.t.Helper()
	select {
	case delivery := <-r.deliveries:
		r.t.Fatalf("unexpected %s: %+v", description, delivery.request)
	case <-time.After(wait):
	}
}

func buildWakeWatchBinaries(t *testing.T, root string) (string, string) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	binary := filepath.Join(root, "shephrd")
	extension := filepath.Join(root, "shephrd-delivery-webhook")
	for output, pkg := range map[string]string{binary: "./cmd/shephrd", extension: "./cmd/shephrd-delivery-webhook"} {
		build := exec.Command("go", "build", "-o", output, pkg)
		build.Dir = projectRoot
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %s %v", pkg, out, err)
		}
	}
	return binary, extension
}

func waitFor(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func fixtureDigest(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
