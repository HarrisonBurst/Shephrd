package store

import (
	"encoding/json"
	"fmt"
	"testing"

	"shephrd/internal/model"
)

func TestSubdriverPersistedCompatibility(t *testing.T) {
	s, repo := subdriverStoreFixture(t)
	if got := subdriverChecksum(); got != "63922f9eeee838cac070634ec4617b2646b8bdfd771caa185a7c4fdc58fad31c" {
		t.Fatalf("immutable schema-30 checksum changed: %s", got)
	}
	r, err := s.HandoffSubdriver(repo.ID, "", "driver:main", "intake", "Original coordinator wording is immutable intake", "", "")
	if err != nil {
		t.Fatal(err)
	}
	f := subdriverStartFixture(t, s, r.SubdriverID)
	q, err := s.SubdriverReturn(f, r.ID, "question", "question", "Historical coordinator payload")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE driver_notifications SET kind='coordinator-question' WHERE coordinator_event_id=?`, q.ID); err != nil {
		t.Fatal(err)
	}
	drain, err := s.DrainNotifications("", "driver:main", "main:old", 1)
	if err != nil || len(drain.Notifications) != 1 {
		t.Fatalf("drain: %+v %v", drain, err)
	}
	n := drain.Notifications[0]
	before := databaseFingerprint(t, s)
	reopened, err := Open(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := reopened.Notification(n.NotificationID)
	if err != nil || stored.Kind != "subdriver-question" || stored.Payload != "Historical coordinator payload" || stored.ClaimToken != n.ClaimToken || stored.DriverGeneration != "main:old" || stored.SubdriverEventID != q.ID {
		t.Fatalf("persisted notice: %+v %v", stored, err)
	}
	body, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	var legacy struct {
		ID      string `json:"coordinator_id"`
		EventID int64  `json:"coordinator_event_id"`
		Repo    string `json:"coordinator_repo_name"`
	}
	if err := json.Unmarshal(body, &legacy); err != nil || legacy.ID != f.ID || legacy.EventID != q.ID || legacy.Repo != repo.Name {
		t.Fatalf("legacy JSON: %s %v", body, err)
	}
	if databaseFingerprint(t, reopened) != before {
		t.Fatal("opening or projecting renamed fields changed durable evidence")
	}
	request := model.NotificationAckRequest{NotificationID: n.NotificationID, ClaimToken: n.ClaimToken, ConsumerID: n.ClaimOwner, DriverGeneration: n.DriverGeneration, HandlingID: "once"}
	if _, err := reopened.AckNotification(request); err != nil {
		t.Fatal(err)
	}
	if receipt, err := reopened.AckNotification(request); err != nil || !receipt.Idempotent {
		t.Fatalf("persisted ack replay: %+v %v", receipt, err)
	}
	if err := reopened.FinishSubdriver(f, "checkpoint", "held fixture"); err != nil {
		t.Fatal(err)
	}
	var kind, id string
	if err := reopened.db.QueryRow(`SELECT kind,notification_id FROM driver_notifications WHERE coordinator_event_id IN (SELECT id FROM coordinator_events WHERE event_key='system-held:1')`).Scan(&kind, &id); err != nil {
		t.Fatal(err)
	}
	if kind != "subdriver-blocker" || id == "" {
		t.Fatalf("new held return is not canonical: %s %s", kind, id)
	}
}

func TestSubdriverOwnerAndClaimCompatibilityFences(t *testing.T) {
	s, repo := subdriverStoreFixture(t)
	r, err := s.HandoffSubdriver(repo.ID, "", "driver:main", "intake", "Request", "", "")
	if err != nil {
		t.Fatal(err)
	}
	f := subdriverStartFixture(t, s, r.SubdriverID)
	child, err := s.DispatchSubdriverWorker(f, r.ID, "child", model.Task{FeatureKey: "child", Objective: "Scoped report", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, generation := attentionAttempt(t, s, child)
	attentionTerminalEvent(t, s, attempt, generation, "blocked", "")
	for _, owner := range []string{child.DriverID, "subdriver:" + f.ID} {
		if _, err := s.CreateTask(model.Task{RepoID: repo.ID, DriverID: owner, FeatureKey: "bypass", Objective: "bypass"}); err == nil {
			t.Fatal("renamed owner bypassed correlated dispatch")
		}
		if _, err := s.HandoffSubdriver(repo.ID, "", owner, "bypass", "Recursive", "", ""); err == nil {
			t.Fatal("renamed owner created another supervisor")
		}
		if _, err := s.AdoptTask(child.ID, child.DriverID, owner); err == nil {
			t.Fatal("renamed owner bypassed adoption fence")
		}
		for _, invalid := range []string{"coordinator:0", "coordinator:2", "subdriver:1"} {
			if _, err := s.DrainNotifications("", owner, invalid, 1); err == nil {
				t.Fatalf("invalid owner/generation drained: %s %s", owner, invalid)
			}
		}
	}
	claims, err := s.DrainNotifications("", child.DriverID, fmt.Sprintf("coordinator:%d", f.Generation), 1)
	if err != nil || len(claims.Notifications) != 1 {
		t.Fatalf("durable claim: %+v %v", claims, err)
	}
	n := claims.Notifications[0]
	for _, owner := range []string{"subdriver:" + f.ID, "coordinator:other"} {
		if _, err := s.AckNotification(model.NotificationAckRequest{NotificationID: n.NotificationID, ClaimToken: n.ClaimToken, ConsumerID: owner, DriverGeneration: n.DriverGeneration, HandlingID: "bad"}); err == nil {
			t.Fatal("renamed or cross-owner acknowledgement accepted")
		}
	}
	if err := s.FinishSubdriver(f, "checkpoint", "held"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverSubdriver(f.ID, f.Generation); err != nil {
		t.Fatal(err)
	}
	next := subdriverStartFixture(t, s, f.ID)
	if _, err := s.AckNotification(model.NotificationAckRequest{NotificationID: n.NotificationID, ClaimToken: n.ClaimToken, ConsumerID: n.ClaimOwner, DriverGeneration: n.DriverGeneration, HandlingID: "stale"}); err == nil {
		t.Fatal("old claim acknowledged after generation rotation")
	}
	claims, err = s.DrainNotifications("", child.DriverID, fmt.Sprintf("coordinator:%d", next.Generation), 1)
	if err != nil || len(claims.Notifications) != 1 || claims.Notifications[0].ClaimToken == n.ClaimToken {
		t.Fatalf("recovered claim: %+v %v", claims, err)
	}
}
