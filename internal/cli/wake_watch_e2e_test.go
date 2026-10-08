package cli

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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

	"shephrd/internal/driverdelivery"
	"shephrd/internal/driverdelivery/webhook"
	extensionhost "shephrd/internal/extension"
	"shephrd/internal/model"
	"shephrd/internal/store"
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
	key := []byte("fixture signing key")
	secret := filepath.Join(root, "secret")
	if err := os.WriteFile(secret, []byte("whsec_"+base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	statuses := []int{http.StatusOK, http.StatusServiceUnavailable}
	deliveries := make(chan webhookDelivery, 16)
	receiver := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var decoded driverdelivery.Request
		if err := extensionhost.StrictDecode(body, &decoded); err != nil {
			t.Errorf("receiver body: %v", err)
		}
		deliveries <- webhookDelivery{id: request.Header.Get("webhook-id"), timestamp: request.Header.Get("webhook-timestamp"), signature: request.Header.Get("webhook-signature"), body: body, request: decoded}
		mu.Lock()
		status := http.StatusOK
		if len(statuses) > 0 {
			status, statuses = statuses[0], statuses[1:]
		}
		mu.Unlock()
		writer.WriteHeader(status)
	}))
	defer receiver.Close()
	next := func(description string) webhookDelivery {
		t.Helper()
		select {
		case delivery := <-deliveries:
			if delivery.signature != webhook.Signature(key, delivery.id, delivery.timestamp, delivery.body) || delivery.id != webhook.WebhookID(delivery.request.Notification.NotificationID, delivery.request.Claim.ClaimToken) {
				t.Fatalf("%s: unsigned or misidentified delivery %+v", description, delivery)
			}
			return delivery
		case <-time.After(15 * time.Second):
			t.Fatalf("timed out waiting for %s", description)
		}
		return webhookDelivery{}
	}

	database := filepath.Join(root, "state.db")
	base := fmt.Sprintf("database_path = %q\ndata_dir = %q\nworktree_root = %q\n[memory]\nenabled = false\n[wake]\nclaim_ttl = \"30s\"\nclaim_ttl_min = \"30s\"\n[wake_watch]\npoll_min = \"100ms\"\npoll_max = \"200ms\"\nrenew_horizon = \"2s\"\n", database, filepath.Join(root, "data"), filepath.Join(root, "worktrees"))
	digest := fixtureDigest(t, extension)
	delivery := fmt.Sprintf("[wake_watch.delivery]\nextension_id = \"shephrd.delivery-webhook\"\ncommand = [%q, \"--url\", %q, \"--secret-file\", %q, \"--allow-http\"]\nsha256 = %q\n", extension, receiver.URL+"/hook", secret, digest)
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
