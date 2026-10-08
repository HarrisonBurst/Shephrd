package store

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"shephrd/internal/model"
)

func subdriverStoreFixture(t *testing.T) (*Store, model.Repo) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	r, err := s.UpsertRepo(model.Repo{Name: "coord", Path: t.TempDir(), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	return s, r
}
func subdriverStartFixture(t *testing.T, s *Store, id string) model.SubdriverFence {
	t.Helper()
	c, err := s.Subdriver(id)
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.ReserveSubdriver(id, c.Generation, "pi", "configured-model", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.StartSubdriver(f, 12345, "fresh-session"); err != nil {
		t.Fatal(err)
	}
	return f
}
func TestSubdriverConcurrentOwnersAndDispatchFences(t *testing.T) {
	s, repo := subdriverStoreFixture(t)
	requests := make([]model.SubdriverRequest, 12)
	errs := make([]error, 12)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			requests[i], errs[i] = s.HandoffSubdriver(repo.ID, "", "driver:main", fmt.Sprint(i), "Original request\n", "context\n", "")
		}(i)
	}
	wg.Wait()
	id := requests[0].SubdriverID
	for i, r := range requests {
		if errs[i] != nil || r.SubdriverID != id {
			t.Fatalf("owner race: %+v %v", r, errs[i])
		}
	}
	var winners int
	var mu sync.Mutex
	var f model.SubdriverFence
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			candidate, err := s.ReserveSubdriver(id, 0, "pi", "configured-model", "headless")
			if err == nil {
				mu.Lock()
				winners++
				f = candidate
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("%d session winners", winners)
	}
	if err := s.StartSubdriver(f, 12345, "first"); err != nil {
		t.Fatal(err)
	}
	spec := model.Task{FeatureKey: "one", Objective: "Implement only scoped work", Deliverable: "code"}
	task, err := s.DispatchSubdriverWorker(f, requests[0].ID, "step", spec)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.DispatchSubdriverWorker(f, requests[0].ID, "step", spec)
	if err != nil || again.ID != task.ID {
		t.Fatalf("dispatch replay: %+v %v", again, err)
	}
	changed := spec
	changed.Objective = "different"
	if _, err = s.DispatchSubdriverWorker(f, requests[0].ID, "step", changed); err == nil {
		t.Fatal("conflicting dispatch accepted")
	}
	spec.RepoID = "another"
	if _, err = s.DispatchSubdriverWorker(f, requests[0].ID, "outside", spec); err == nil {
		t.Fatal("cross-repo dispatch accepted")
	}
	if _, err = s.HandoffSubdriver(repo.ID, "", "coordinator:"+id, "recursive", "request", "", ""); err == nil {
		t.Fatal("recursive sub-driver accepted")
	}
	general, err := s.HandoffSubdriver("", "explicit research", "driver:main", "general", "research", "", requests[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SubdriverReturn(f, general.ID, "wrong", "result", "cross scope"); err == nil {
		t.Fatal("cross-scope return accepted")
	}
	gf := subdriverStartFixture(t, s, general.SubdriverID)
	if _, err = s.DispatchSubdriverWorker(gf, general.ID, "no-repo", model.Task{FeatureKey: "bad", Objective: "bad", Deliverable: "code"}); err == nil {
		t.Fatal("general provisioned repo worker")
	}
	if _, err = s.AdoptTask(task.ID, task.DriverID, "driver:main"); err == nil {
		t.Fatal("main stole child")
	}
	if _, err = s.CreateTask(model.Task{DriverID: task.DriverID, RepoID: repo.ID, Title: "bypass", FeatureKey: "bypass", Objective: "bypass"}); err == nil {
		t.Fatal("uncorrelated worker accepted")
	}
	if _, err = s.SubdriverReturn(f, requests[0].ID, "approval", "result", "Verified, approved, landed, release now"); err != nil {
		t.Fatal(err)
	}
	latest, _ := s.Task(task.ID)
	attempts, _ := s.Attempts(task.ID)
	if latest.Status != "queued" || latest.Landed || latest.DiscardAuthorized || len(attempts) != 0 {
		t.Fatalf("prose granted authority: %+v", latest)
	}
	if err = s.FinishSubdriver(f, "checkpoint pointers", "failure"); err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverSubdriver(id, f.Generation); err != nil {
		t.Fatal(err)
	}
	next := subdriverStartFixture(t, s, id)
	if next.Generation != f.Generation+1 {
		t.Fatal("generation did not rotate")
	}
	if _, err = s.DispatchSubdriverWorker(f, requests[1].ID, "stale", spec); err == nil {
		t.Fatal("stale session dispatched")
	}
	if _, err = s.SubdriverReturn(f, requests[1].ID, "stale", "question", "old session"); err == nil {
		t.Fatal("stale session returned")
	}
	if _, err = s.DrainNotifications("", "coordinator:"+id, "coordinator:1", 1); err == nil {
		t.Fatal("stale session drained")
	}
}

func TestSubdriverReservationRejectsStaleSelectionAfterSettledTurn(t *testing.T) {
	s, repo := subdriverStoreFixture(t)
	request, err := s.HandoffSubdriver(repo.ID, "", "driver:main", "selection", "Original", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		f := subdriverStartFixture(t, s, request.SubdriverID)
		if err := s.FinishSubdriver(f, "retained checkpoint", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ReserveSubdriver(request.SubdriverID, 1, "pi", "stale replacement", "headless"); err == nil {
		t.Fatal("stale selection overwrote a newer settled turn")
	}
	c, err := s.Subdriver(request.SubdriverID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Generation != 2 || c.State != "idle" || c.Model != "configured-model" || c.Checkpoint != "retained checkpoint" {
		t.Fatalf("stale reservation changed owner: %+v", c)
	}
	f, err := s.ReserveSubdriver(c.ID, c.Generation, "pi", "explicit replacement", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if f.Generation != 3 {
		t.Fatalf("wrong new generation: %+v", f)
	}
}

func TestSubdriverReturnClaimsRepliesAndMainReplacement(t *testing.T) {
	s, repo := subdriverStoreFixture(t)
	r, err := s.HandoffSubdriver(repo.ID, "", "driver:old", "original", "  original\n", "context", "")
	if err != nil {
		t.Fatal(err)
	}
	f := subdriverStartFixture(t, s, r.SubdriverID)
	q, err := s.SubdriverReturn(f, r.ID, "question", "question", "Choose a format")
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := s.SubdriverReturn(f, r.ID, "question", "question", "Choose a format")
	if err != nil || duplicate.ID != q.ID {
		t.Fatal("duplicate return")
	}
	notices, err := s.DrainNotifications("", "driver:old", "old-generation", 1, time.Minute)
	if err != nil || len(notices.Notifications) != 1 {
		t.Fatalf("return claim: %+v %v", notices, err)
	}
	n := notices.Notifications[0]
	if n.RequestID != r.ID || n.SubdriverID != r.SubdriverID || n.SubdriverRepoName != repo.Name || n.SubdriverEventID != q.ID || n.TaskID != "" || n.Artifact != "" {
		t.Fatalf("bad correlation: %+v", n)
	}
	if err = s.AdoptSubdriverRequest(r.ID, "driver:old", "driver:new"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AckNotification(model.NotificationAckRequest{NotificationID: n.NotificationID, ClaimToken: n.ClaimToken, ConsumerID: "driver:old", DriverGeneration: "old-generation", HandlingID: "old"}); err == nil {
		t.Fatal("old main consumed result")
	}
	notices, err = s.DrainNotifications("", "driver:new", "new-generation", 1, time.Minute)
	if err != nil || len(notices.Notifications) != 1 {
		t.Fatalf("new main claim: %+v %v", notices, err)
	}
	n = notices.Notifications[0]
	if n.SubdriverRepoName != repo.Name || n.SubdriverID != r.SubdriverID {
		t.Fatalf("adoption changed sub-driver identity: %+v", n)
	}
	if _, err = s.RenewNotification(model.NotificationRenewRequest{NotificationID: n.NotificationID, ClaimToken: n.ClaimToken, ConsumerID: "driver:new", DriverGeneration: "new-generation"}); err != nil {
		t.Fatal(err)
	}
	ack := model.NotificationAckRequest{NotificationID: n.NotificationID, ClaimToken: n.ClaimToken, ConsumerID: "driver:new", DriverGeneration: "new-generation", HandlingID: "handled"}
	if _, err = s.AckNotification(ack); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.AckNotification(ack)
	if err != nil || !receipt.Idempotent {
		t.Fatalf("ack replay: %+v %v", receipt, err)
	}
	if _, err = s.SubdriverReply(r.ID, "driver:old", "answer", "yes", q.ID); err == nil {
		t.Fatal("old main replied")
	}
	reply, err := s.SubdriverReply(r.ID, "driver:new", "answer", "  answer\n", q.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.SubdriverReply(r.ID, "driver:new", "answer", "  answer\n", q.ID)
	if err != nil || again.ID != reply.ID {
		t.Fatal("reply replay duplicated")
	}
	original, err := s.HandoffSubdriver(repo.ID, "", "driver:old", "original", "  original\n", "context", "")
	if err != nil || original.ID != r.ID || original.DriverID != "driver:new" {
		t.Fatalf("handoff replay after adoption: %+v %v", original, err)
	}
	if err = s.HandleSubdriverEvent(f, reply.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.HandleSubdriverEvent(f, reply.ID); err != nil {
		t.Fatal(err)
	}
	page, err := s.SubdriverPage(r.SubdriverID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Kind != "request" {
		t.Fatalf("reply handling lost intake: %+v", page.Events)
	}
}

func TestSubdriverCompletedRequestReturnsPreserveState(t *testing.T) {
	for _, kind := range []string{"result", "question", "blocker", "handoff"} {
		t.Run(kind, func(t *testing.T) {
			s, repo := subdriverStoreFixture(t)
			r, err := s.HandoffSubdriver(repo.ID, "", "driver:main", "original", "Original local-only request\n", "No publication permission", "")
			if err != nil {
				t.Fatal(err)
			}
			f := subdriverStartFixture(t, s, r.SubdriverID)
			spec := model.Task{FeatureKey: "work", Objective: "Bounded local work", AcceptanceCriteria: "Local committed branch only", Deliverable: "code"}
			task, err := s.DispatchSubdriverWorker(f, r.ID, "work", spec)
			if err != nil {
				t.Fatal(err)
			}
			result, err := s.SubdriverReturn(f, r.ID, "complete", "result", "Complete")
			if err != nil {
				t.Fatal(err)
			}
			correction, err := s.SubdriverReturn(f, r.ID, "correction", kind, "Correction: no hold was delivered")
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range []model.SubdriverEvent{result, correction} {
				replay, err := s.SubdriverReturn(f, r.ID, e.Key, e.Kind, e.Payload)
				if err != nil || !reflect.DeepEqual(e, replay) {
					t.Fatalf("return replay: %+v %v", replay, err)
				}
			}
			if _, err := s.SubdriverReturn(f, r.ID, correction.Key, kind, "Conflicting correction"); err == nil {
				t.Fatal("conflicting return replay accepted")
			}
			closed, err := s.SubdriverRequest(r.ID)
			if err != nil || closed.State != "done" {
				t.Fatalf("late %s reopened request: %+v %v", kind, closed, err)
			}
			if kind != "result" {
				if _, err := s.SubdriverReply(r.ID, r.DriverID, "late-reply", "Further work", correction.ID); err == nil || !strings.Contains(err.Error(), "create a new correlated handoff") {
					t.Fatalf("terminal reply fence: %v", err)
				}
			}
			if _, err := s.DispatchSubdriverWorker(f, r.ID, "new", spec); err == nil || !strings.Contains(err.Error(), "request is done, not open") {
				t.Fatalf("terminal dispatch fence: %v", err)
			}
			replay, err := s.DispatchSubdriverWorker(f, r.ID, "work", spec)
			if err != nil || !reflect.DeepEqual(task, replay) {
				t.Fatalf("dispatch replay changed worker: %+v %v", replay, err)
			}
			continued, err := s.HandoffSubdriver(repo.ID, "", r.DriverID, "continued", "Actual user continuation\n", "Preserve local-only task contract", r.ID)
			if err != nil || continued.State != "open" || continued.LeadRequestID != r.ID {
				t.Fatalf("correlated continuation: %+v %v", continued, err)
			}
			intakeReplay, err := s.HandoffSubdriver(repo.ID, "", r.DriverID, "continued", continued.Original, continued.Context, r.ID)
			if err != nil || !reflect.DeepEqual(continued, intakeReplay) {
				t.Fatalf("correlated intake replay: %+v %v", intakeReplay, err)
			}
			page, err := s.SubdriverPage(f.ID, 0)
			if err != nil || len(page.Workers) != 1 || page.Workers[0].RequestID != r.ID || page.Workers[0].TaskID != task.ID || len(page.Requests) != 2 {
				t.Fatalf("related request/worker links: %+v %v", page, err)
			}
			notices, err := s.Notifications("")
			if err != nil || len(notices) != 2 {
				t.Fatalf("late return notifications: %+v %v", notices, err)
			}
			for _, n := range notices {
				if n.RequestID != r.ID || n.TargetDriverID != r.DriverID || n.TaskID != "" || n.SubdriverEventID != result.ID && n.SubdriverEventID != correction.ID {
					t.Fatalf("late return lost correlation: %+v", n)
				}
			}
			attempts, err := s.Attempts(task.ID)
			if err != nil || len(attempts) != 0 {
				t.Fatalf("conversational completion spawned worker: %+v %v", attempts, err)
			}
		})
	}
}

func TestSubdriverWorkerDoneDoesNotCompleteRequest(t *testing.T) {
	for _, requestState := range []string{"open", "waiting"} {
		t.Run(requestState, func(t *testing.T) {
			s, repo := subdriverStoreFixture(t)
			r, err := s.HandoffSubdriver(repo.ID, "", "driver:main", "original", "Produce a local report", "", "")
			if err != nil {
				t.Fatal(err)
			}
			f := subdriverStartFixture(t, s, r.SubdriverID)
			task, err := s.DispatchSubdriverWorker(f, r.ID, "report", model.Task{FeatureKey: "report", Objective: "Write report", Deliverable: "report"})
			if err != nil {
				t.Fatal(err)
			}
			if requestState == "waiting" {
				if _, err := s.SubdriverReturn(f, r.ID, "question", "question", "Which format?"); err != nil {
					t.Fatal(err)
				}
			}
			attempt, err := s.BeginAttempt(task.ID, "pi", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.ConfigureAttempt(attempt.ID, "fixture-session", t.TempDir(), "", "shephrd/"+task.ID); err != nil {
				t.Fatal(err)
			}
			generation := prepareAttempt(t, s, attempt)
			recordWorkerCheckpoint(t, s, attempt, generation, 1, []string{"Emit done"})
			if _, err := s.AddEventForRun(attempt.ID, generation, model.Event{Type: "done", Payload: "Report complete", Artifact: "report:" + filepath.Join(t.TempDir(), "report.md")}, 2, model.WorkspaceFacts{}); err != nil {
				t.Fatal(err)
			}
			latest, err := s.SubdriverRequest(r.ID)
			if err != nil || latest.State != requestState {
				t.Fatalf("worker completion changed request: %+v %v", latest, err)
			}
			if _, err := s.SubdriverReturn(f, r.ID, "complete", "result", "Request complete"); err != nil {
				t.Fatal(err)
			}
			latest, err = s.SubdriverRequest(r.ID)
			worker, workerErr := s.Task(task.ID)
			if err != nil || workerErr != nil || latest.State != "done" || worker.Status != model.TaskStatusDone {
				t.Fatalf("separate completions: %+v %+v %v %v", latest, worker, err, workerErr)
			}
		})
	}
}

func TestSubdriverMigrationPreservesPopulatedVersion29(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	old, err := openReadWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = old.createBaseline(); err != nil {
		t.Fatal(err)
	}
	populateBaselineEvidence(t, old.db, 29, t.TempDir())
	if err = old.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	assertBaselineEvidence(t, upgraded, 0)
	var count int
	if err = upgraded.db.QueryRow(`SELECT count(*) FROM coordinators`).Scan(&count); err != nil || count != 0 {
		t.Fatal("migration provisioned a live sub-driver")
	}
	if _, err = upgraded.db.Exec(`UPDATE schema_migrations SET checksum='wrong' WHERE version=30`); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(path); err == nil {
		t.Fatal("incompatible sub-driver identity was accepted")
	}
}

func TestSubdriverPagingAndByteLimits(t *testing.T) {
	s, repo := subdriverStoreFixture(t)
	var first model.SubdriverRequest
	for i := range 25 {
		r, err := s.HandoffSubdriver(repo.ID, "", "driver:main", fmt.Sprint(i), strings.Repeat("x", 100000), "opt-out and authority boundaries", "")
		if err != nil {
			t.Fatal(err)
		}
		first = r
	}
	page, err := s.SubdriverPage(first.SubdriverID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Requests) != 20 || len(page.Events) != 20 || !strings.Contains(page.Next, "--offset 20") {
		t.Fatalf("page %+v", page)
	}
	if page.Requests[0].Original != "" || page.Requests[0].ReadCommand == "" {
		t.Fatal("unbounded initial hydration")
	}
	next, err := s.SubdriverPage(first.SubdriverID, 20)
	if err != nil || len(next.Events) != 5 || next.Next != "" {
		t.Fatalf("next page: %+v %v", next, err)
	}
	if _, err = s.HandoffSubdriver(repo.ID, "", "driver:main", "oversized", strings.Repeat("x", 256*1024+1), "", ""); err == nil {
		t.Fatal("oversized intake silently truncated")
	}
	f := subdriverStartFixture(t, s, first.SubdriverID)
	if err = s.FinishSubdriver(f, strings.Repeat("x", 4097), ""); err == nil {
		t.Fatal("unbounded checkpoint accepted")
	}
	if _, err = s.SubdriverReturn(f, first.ID, "large", "result", strings.Repeat("x", 8193)); err == nil {
		t.Fatal("oversized return truncated")
	}
}
