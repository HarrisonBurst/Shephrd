package wakewatch

import (
	"strings"
	"testing"
	"time"

	"shephrd/internal/driverdelivery"
	extensionhost "shephrd/internal/extension"
	"shephrd/internal/model"
)

func rejected() (driverdelivery.Result, error) {
	return driverdelivery.Result{Outcome: driverdelivery.OutcomeRejected, Detail: "HTTP 403"}, nil
}

func undeliverable() (driverdelivery.Result, error) {
	return driverdelivery.Result{}, &extensionhost.HostError{Kind: extensionhost.ErrorProtocolViolation, Operation: "deliver"}
}

func retryable() (driverdelivery.Result, error) {
	return driverdelivery.Result{Outcome: driverdelivery.OutcomeRetryable, Detail: "HTTP 503"}, nil
}

func (r *recorder) failedClaims() int {
	return r.eventCount(driverdelivery.OutcomeRejected) + r.eventCount(OutcomeUndeliverable)
}

func (r *recorder) eventsExposing(tokens ...string) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var exposed []Event
	for _, event := range r.events {
		for _, token := range tokens {
			if token != "" && strings.Contains(event.Detail, token) {
				exposed = append(exposed, event)
			}
		}
	}
	return exposed
}

func rejectClaims(t *testing.T, f *fixture, r *recorder, id string, from, to int) {
	t.Helper()
	for claim := from; claim <= to; claim++ {
		eventually(t, "rejected or undeliverable claim", func() bool { return r.failedClaims() == claim })
		f.expire(id)
	}
}

func TestWatcherParksAfterConsecutiveRejectedClaimsAndUnparksUnderANewClaim(t *testing.T) {
	f := newFixture(t)
	first := f.subdriverReturn("first question")
	second := f.subdriverReturn("second question")
	r := &recorder{outcomes: []func() (driverdelivery.Result, error){rejected, retryable, undeliverable, rejected}}
	generation, _ := startWatcher(t, f, r, r, "watch:park", func(w *Watcher) { w.MaxRejectedClaims = 3 })
	rejectClaims(t, f, r, first, 1, 3)
	eventually(t, "next FIFO notification after parking", func() bool { return len(r.delivered()) == 5 })
	requests := r.delivered()
	tokens := map[string]bool{}
	for _, request := range requests[:4] {
		if request.Notification.NotificationID != first {
			t.Fatalf("parked notification interleaved: %+v", request)
		}
		tokens[request.Claim.ClaimToken] = true
	}
	if len(tokens) != 3 || requests[4].Notification.NotificationID != second || r.eventCount("parked") != 1 || r.eventCount(driverdelivery.OutcomeRetryable) != 1 {
		t.Fatalf("claims %d, fifth request %+v, events %+v", len(tokens), requests[4].Notification, r.events)
	}
	if notice := f.notification(first); notice.State != model.NotificationPending || notice.AckedAt != nil || notice.DeliveryAttempts != 3 || notice.ClaimToken != "" {
		t.Fatalf("parked notification = %+v", notice)
	}
	parked, err := f.state.ParkedNotifications(owner, 3)
	if err != nil || len(parked) != 1 || parked[0].NotificationID != first || parked[0].RejectedClaims != 3 || parked[0].LastResult != driverdelivery.OutcomeRejected || parked[0].RequestID == "" {
		t.Fatalf("parked list = %+v %v", parked, err)
	}

	claimed := f.notification(second)
	if _, err := f.state.AckNotification(model.NotificationAckRequest{NotificationID: second, ClaimToken: claimed.ClaimToken, ConsumerID: owner, DriverGeneration: generation, HandlingID: "handling:external"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "acknowledgement observed", func() bool { return r.eventCount("acknowledged") == 1 })
	sweeps, _ := r.counts()
	eventually(t, "idle drains skip the parked notification", func() bool { current, _ := r.counts(); return current >= sweeps+5 })
	if len(r.delivered()) != 5 || r.eventCount("parked") != 1 {
		t.Fatalf("parked notification redelivered: deliveries %d, events %+v", len(r.delivered()), r.events)
	}
	if _, err := f.state.UnparkNotification("driver:other", first, 3); err == nil {
		t.Fatal("another owner unparked the notification")
	}
	if _, err := f.state.UnparkNotification(owner, second, 3); err == nil {
		t.Fatal("unparked a notification that was never parked")
	}
	unparked, err := f.state.UnparkNotification(owner, first, 3)
	if err != nil || unparked.NotificationID != first || unparked.RejectedClaims != 3 {
		t.Fatalf("unpark = %+v %v", unparked, err)
	}
	eventually(t, "redelivery after unpark", func() bool { return len(r.delivered()) == 6 })
	redelivered := r.delivered()[5]
	if redelivered.Notification.NotificationID != first || tokens[redelivered.Claim.ClaimToken] || redelivered.Claim.DeliveryAttempt != 1 {
		t.Fatalf("redelivery = %+v", redelivered)
	}
	if notice := f.notification(first); notice.State != model.NotificationClaimed || notice.AckedAt != nil {
		t.Fatalf("redelivered notification = %+v", notice)
	}
	all := []string{redelivered.Claim.ClaimToken, claimed.ClaimToken}
	for token := range tokens {
		all = append(all, token)
	}
	if exposed := r.eventsExposing(all...); len(exposed) != 0 {
		t.Fatalf("events expose claim tokens: %+v", exposed)
	}
}

func TestWatcherParkingSurvivesRestart(t *testing.T) {
	f := newFixture(t)
	first := f.subdriverReturn("first question")
	second := f.subdriverReturn("second question")
	r := &recorder{outcomes: []func() (driverdelivery.Result, error){rejected, undeliverable}}
	_, stop := startWatcher(t, f, r, r, "watch:one", func(w *Watcher) { w.MaxRejectedClaims = 3 })
	rejectClaims(t, f, r, first, 1, 1)
	eventually(t, "second rejected claim", func() bool { return r.failedClaims() == 2 })
	stop()
	if notice := f.notification(first); notice.State != model.NotificationClaimed || notice.DriverGeneration != "watch:one" {
		t.Fatalf("crashed watcher claim = %+v", notice)
	}

	restarted := &recorder{outcomes: []func() (driverdelivery.Result, error){rejected}}
	startWatcher(t, f, restarted, restarted, "watch:two", func(w *Watcher) { w.MaxRejectedClaims = 3 })
	sweeps, _ := restarted.counts()
	eventually(t, "restarted drain passes", func() bool { current, _ := restarted.counts(); return current >= sweeps+3 })
	if len(restarted.delivered()) != 0 {
		t.Fatalf("restarted watcher delivered past the outstanding claim: %+v", restarted.delivered())
	}
	f.expire(first)
	rejectClaims(t, f, restarted, first, 1, 1)
	eventually(t, "next FIFO notification after parking across restart", func() bool { return len(restarted.delivered()) == 2 })
	if request := restarted.delivered()[1]; request.Notification.NotificationID != second || request.Driver.Generation != "watch:two" || restarted.eventCount("parked") != 1 {
		t.Fatalf("second request = %+v, events %+v", request, restarted.events)
	}
	if notice := f.notification(first); notice.State != model.NotificationPending || notice.DeliveryAttempts != 3 {
		t.Fatalf("parked notification = %+v", notice)
	}
}

func TestParkingCountsOnlyConsecutiveRejectedOrUndeliverableWatcherClaims(t *testing.T) {
	f := newFixture(t)
	first := f.subdriverReturn("first question")
	second := f.subdriverReturn("second question")
	claim := func(generation string) model.DriverNotification {
		t.Helper()
		result, err := f.state.DrainNotificationExclusive(owner, generation, time.Minute, 2)
		if err != nil || len(result.Notifications) != 1 {
			t.Fatalf("drain = %+v %v", result, err)
		}
		return result.Notifications[0]
	}
	record := func(notification model.DriverNotification, results ...string) {
		t.Helper()
		for _, result := range results {
			if err := f.state.RecordNotificationDelivery(model.NotificationDeliveryLog{NotificationID: notification.NotificationID, Operation: "notify", ConsumerID: owner, DriverGeneration: notification.DriverGeneration, ClaimToken: notification.ClaimToken, Result: result, Detail: "fixture " + result}); err != nil {
				t.Fatal(err)
			}
		}
		f.expire(notification.NotificationID)
	}
	parkedCount := func() int {
		t.Helper()
		parked, err := f.state.ParkedNotifications(owner, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, notification := range parked {
			if notification.NotificationID == first {
				return notification.RejectedClaims
			}
		}
		return 0
	}
	expect := func(description string, want int) {
		t.Helper()
		if count := parkedCount(); count != want {
			t.Fatalf("%s: consecutive rejected claims = %d, want %d", description, count, want)
		}
	}
	record(claim("watch:one"), driverdelivery.OutcomeRetryable, driverdelivery.OutcomeRetryable)
	expect("retryable-only claim", 0)
	record(claim("watch:one"), driverdelivery.OutcomeRejected)
	expect("rejected claim", 1)
	if err := f.state.RecordNotificationDelivery(model.NotificationDeliveryLog{NotificationID: first, Operation: "notify", Result: "failed", Detail: "desktop presentation"}); err != nil {
		t.Fatal(err)
	}
	manual, err := f.state.DrainNotifications("", owner, "generation:manual", 1, time.Minute)
	if err != nil || len(manual.Notifications) != 1 || manual.Notifications[0].NotificationID != first || len(manual.Parked) != 0 {
		t.Fatalf("manual drain = %+v %v", manual, err)
	}
	record(manual.Notifications[0])
	expect("desktop presentation failure and manual claim", 1)
	record(claim("watch:two"), driverdelivery.OutcomeRetryable, driverdelivery.OutcomeDelivered)
	expect("delivered claim", 0)
	record(claim("watch:two"), OutcomeUndeliverable)
	expect("undeliverable claim after restart generation", 1)
	record(claim("watch:two"), driverdelivery.OutcomeRejected)
	expect("second consecutive failed claim", 2)
	next := claim("watch:two")
	if next.NotificationID != second {
		t.Fatalf("watcher drain after parking = %+v", next)
	}
	parked, err := f.state.ParkedNotifications(owner, 2)
	if err != nil || len(parked) != 1 || parked[0].NotificationID != first || parked[0].RejectedClaims != 2 || parked[0].LastResult != driverdelivery.OutcomeRejected {
		t.Fatalf("parked = %+v %v", parked, err)
	}
	if more, err := f.state.ParkedNotifications(owner, 3); err != nil || len(more) != 0 {
		t.Fatalf("higher threshold parked = %+v %v", more, err)
	}
	ordinary, err := f.state.DrainNotifications("", owner, "generation:ordinary", 10, time.Minute)
	if err != nil || len(ordinary.Notifications) != 1 || ordinary.Notifications[0].NotificationID != first || len(ordinary.Parked) != 0 {
		t.Fatalf("ordinary drain skipped the parked notification: %+v %v", ordinary, err)
	}
	if notice := f.notification(first); notice.AckedAt != nil || notice.State != model.NotificationClaimed {
		t.Fatalf("notification = %+v", notice)
	}
}
