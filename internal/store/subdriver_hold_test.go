package store

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"shephrd/internal/model"
)

func TestSubdriverObservationHoldRejectsChangedSnapshot(t *testing.T) {
	for _, change := range []string{"finished", "held", "started", "session", "harness", "endpoint", "recovered", "generation"} {
		t.Run(change, func(t *testing.T) {
			s, repo := subdriverStoreFixture(t)
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			r, err := s.HandoffSubdriver(repo.ID, "", "driver:main", "request", "Original", "", "")
			must(err)
			f, err := s.ReserveSubdriver(r.SubdriverID, 0, "pi", "retained", "herdr")
			must(err)
			if change != "started" && change != "endpoint" {
				must(s.StartSubdriver(f, 123, "session"))
				must(s.SubdriverHarnessPID(f, 456))
			}
			observed, err := s.Subdriver(f.ID)
			must(err)
			switch change {
			case "finished":
				must(s.FinishSubdriver(f, "accepted checkpoint", ""))
			case "held":
				must(s.FinishSubdriver(f, "accepted checkpoint", "original failure"))
			case "started":
				must(s.StartSubdriver(f, 123, "session"))
			case "session":
				must(s.SubdriverSession(f, "different session"))
			case "harness":
				must(s.SubdriverHarnessPID(f, 789))
			case "endpoint":
				must(s.SetSubdriverEndpoint(f, model.TerminalEndpoint{Backend: "herdr", SocketPath: "/fixture", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"}))
			case "recovered":
				must(s.RecoverSubdriver(f.ID, f.Generation))
			case "generation":
				must(s.FinishSubdriver(f, "accepted checkpoint", ""))
				_, err = s.ReserveSubdriver(f.ID, f.Generation, "pi", "retained", "herdr")
				must(err)
			}
			before, err := s.SubdriverPage(f.ID, 0)
			must(err)
			notices, err := s.Notifications("")
			must(err)
			must(s.HoldSubdriverObservation(observed, "stale probe"))
			after, err := s.SubdriverPage(f.ID, 0)
			must(err)
			latestNotices, err := s.Notifications("")
			must(err)
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(notices, latestNotices) {
				t.Fatalf("stale hold changed state or notices: before=%+v after=%+v notices=%+v", before, after, latestNotices)
			}
		})
	}
}

func TestSubdriverObservationHoldRetainsCurrentFailures(t *testing.T) {
	for _, state := range []string{"starting", "running", "idle"} {
		t.Run(state, func(t *testing.T) {
			s, repo := subdriverStoreFixture(t)
			r, err := s.HandoffSubdriver(repo.ID, "", "driver:main", "request", "Original", "", "")
			if err != nil {
				t.Fatal(err)
			}
			f, err := s.ReserveSubdriver(r.SubdriverID, 0, "pi", "retained", "headless")
			if err != nil {
				t.Fatal(err)
			}
			if state != "starting" {
				if err := s.StartSubdriver(f, 123, "session"); err != nil {
					t.Fatal(err)
				}
			}
			if state == "idle" {
				if err := s.FinishSubdriver(f, "accepted checkpoint", ""); err != nil {
					t.Fatal(err)
				}
			}
			observed, err := s.Subdriver(f.ID)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := s.HoldSubdriverObservation(observed, "current failure"); err != nil {
					t.Fatal(err)
				}
			}
			held, err := s.Subdriver(f.ID)
			if err != nil {
				t.Fatal(err)
			}
			if held.State != "held" || held.Failure != "current failure" || held.Checkpoint != observed.Checkpoint || held.Generation != observed.Generation || held.SessionID != observed.SessionID || held.RunnerPID != observed.RunnerPID {
				t.Fatalf("current failure not preserved: %+v", held)
			}
			notices, err := s.Notifications("")
			if err != nil || len(notices) != 1 || notices[0].Kind != "subdriver-blocker" || notices[0].State != model.NotificationPending || notices[0].TargetDriverID != "driver:main" || notices[0].RequestID != r.ID {
				t.Fatalf("failure fanout: %+v %v", notices, err)
			}
			if err := s.CheckSubdriverFence(f); err == nil {
				t.Fatal("held session retained authority")
			}
		})
	}
}

func TestSubdriverObservationHoldRetainsWorkerClaimsAndOwnership(t *testing.T) {
	s, repo := subdriverStoreFixture(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.HandoffSubdriver(repo.ID, "", "driver:main", "request", "Original", "", "")
	must(err)
	f := subdriverStartFixture(t, s, r.SubdriverID)
	worker, err := s.DispatchSubdriverWorker(f, r.ID, "worker", model.Task{FeatureKey: "work", Objective: "Keep ownership", Deliverable: "code"})
	must(err)
	attempt, err := s.BeginAttempt(worker.ID, "pi", "")
	must(err)
	must(s.ConfigureAttempt(attempt.ID, "worker-session", t.TempDir(), "lease", "branch"))
	generation := prepareAttempt(t, s, attempt)
	recordWorkerCheckpoint(t, s, attempt, generation, 1, []string{"ask"})
	_, err = s.AddEventForRun(attempt.ID, generation, model.Event{Type: "question", Payload: "Existing worker question"}, 2, model.WorkspaceFacts{})
	must(err)
	drain, err := s.DrainNotifications("", "coordinator:"+f.ID, fmt.Sprintf("coordinator:%d", f.Generation), 1, time.Minute)
	must(err)
	if len(drain.Notifications) != 1 {
		t.Fatalf("claim: %+v", drain)
	}
	claimed, err := s.Notification(drain.Notifications[0].NotificationID)
	must(err)
	before, err := s.Task(worker.ID)
	must(err)
	observed, err := s.Subdriver(f.ID)
	must(err)
	must(s.HoldSubdriverObservation(observed, "runner absent"))
	after, err := s.Task(worker.ID)
	must(err)
	latest, err := s.Notification(claimed.NotificationID)
	must(err)
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(claimed, latest) {
		t.Fatalf("hold changed owned work or claim: %+v %+v", after, latest)
	}
	_, err = s.AckNotification(model.NotificationAckRequest{NotificationID: claimed.NotificationID, ClaimToken: claimed.ClaimToken, ConsumerID: claimed.ClaimOwner, DriverGeneration: claimed.DriverGeneration, HandlingID: "stale-session"})
	if err == nil {
		t.Fatal("held session acknowledged worker input")
	}
}
