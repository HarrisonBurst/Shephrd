package wakewatch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/driverdelivery"
	extensionhost "shephrd/internal/extension"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

const owner = "driver:hermes"

type fixture struct {
	t        *testing.T
	state    *store.Store
	database string
	fence    model.SubdriverFence
	keys     int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	database := filepath.Join(t.TempDir(), "state.db")
	state, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	return &fixture{t: t, state: state, database: database}
}

func (f *fixture) subdriverReturn(text string) string {
	f.t.Helper()
	f.keys++
	request, err := f.state.HandoffSubdriver("", "Watch fixture", owner, fmt.Sprintf("goal-%d", f.keys), "Original request", "", "")
	if err != nil {
		f.t.Fatal(err)
	}
	subdriver, err := f.state.Subdriver(request.SubdriverID)
	if err != nil {
		f.t.Fatal(err)
	}
	if subdriver.State != "running" {
		fence, err := f.state.ReserveSubdriver(subdriver.ID, subdriver.Generation, "pi", "fixture", "headless")
		if err != nil {
			f.t.Fatal(err)
		}
		if err := f.state.StartSubdriver(fence, 0, "fixture"); err != nil {
			f.t.Fatal(err)
		}
		f.fence = fence
	}
	event, err := f.state.SubdriverReturn(f.fence, request.ID, "return", "question", text)
	if err != nil {
		f.t.Fatal(err)
	}
	notifications, err := f.state.Notifications("")
	if err != nil {
		f.t.Fatal(err)
	}
	for _, notification := range notifications {
		if notification.SubdriverEventID == event.ID {
			return notification.NotificationID
		}
	}
	f.t.Fatalf("no notification for event %d", event.ID)
	return ""
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	db, err := sql.Open("sqlite", f.database)
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) notification(id string) model.DriverNotification {
	f.t.Helper()
	notification, err := f.state.Notification(id)
	if err != nil {
		f.t.Fatal(err)
	}
	return notification
}

type recorder struct {
	mu       sync.Mutex
	requests []driverdelivery.Request
	outcomes []func() (driverdelivery.Result, error)
	events   []Event
	sweeps   int
	pumps    int
	gate     chan struct{}
	blocked  int
	overlap  bool
}

func (r *recorder) SweepDeaths() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweeps++
	return nil
}

func (r *recorder) PumpSubdrivers(driver string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if driver != owner {
		return fmt.Errorf("pump owner %s", driver)
	}
	r.pumps++
	gate := r.gate
	if gate == nil {
		return nil
	}
	r.overlap = r.overlap || r.blocked > 0
	r.blocked++
	r.mu.Unlock()
	<-gate
	r.mu.Lock()
	r.blocked--
	return nil
}

func (r *recorder) block() chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gate = make(chan struct{})
	return r.gate
}

func (r *recorder) pumpState() (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.blocked, r.overlap
}

func (r *recorder) Deliver(_ context.Context, request driverdelivery.Request) (driverdelivery.Result, error) {
	if err := driverdelivery.ValidateRequest(request); err != nil {
		return driverdelivery.Result{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, request)
	if len(r.outcomes) > 0 {
		outcome := r.outcomes[0]
		r.outcomes = r.outcomes[1:]
		return outcome()
	}
	return driverdelivery.Result{Outcome: driverdelivery.OutcomeDelivered, Detail: "HTTP 200"}, nil
}

func (r *recorder) log(event Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) delivered() []driverdelivery.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]driverdelivery.Request(nil), r.requests...)
}

func (r *recorder) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sweeps, r.pumps
}

func (r *recorder) eventCount(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, event := range r.events {
		if event.Event == name {
			count++
		}
	}
	return count
}

func start(t *testing.T, f *fixture, r *recorder, deliverer Deliverer, horizon time.Duration) (string, func()) {
	t.Helper()
	return startGeneration(t, f, r, deliverer, horizon, "watch:"+strings.ReplaceAll(t.Name(), "/", "-"))
}

func startGeneration(t *testing.T, f *fixture, r *recorder, deliverer Deliverer, horizon time.Duration, generation string) (string, func()) {
	t.Helper()
	watcher := Watcher{Store: f.state, Activation: r, Deliverer: deliverer, DriverID: owner, Generation: generation,
		ClaimTTL: time.Minute, PollMin: 20 * time.Millisecond, PollMax: 40 * time.Millisecond, RenewHorizon: horizon,
		DeliveryTimeout: 500 * time.Millisecond, Log: r.log}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watcher.Run(ctx)
		close(done)
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("watcher did not stop")
		}
	}
	t.Cleanup(stop)
	return generation, stop
}

func eventually(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestWatcherDeliversOneClaimAtATimeInOrderAndPumpsOnlyWhileInFlight(t *testing.T) {
	f := newFixture(t)
	first := f.subdriverReturn("first question")
	second := f.subdriverReturn("second question")
	r := &recorder{}
	generation, _ := start(t, f, r, r, time.Hour)
	eventually(t, "first delivery", func() bool { return len(r.delivered()) == 1 })
	request := r.delivered()[0]
	if request.Notification.NotificationID != first || request.Driver != (driverdelivery.Driver{ID: owner, Generation: generation}) || request.Claim.DeliveryAttempt != 1 || request.Obligations == nil {
		t.Fatalf("first request = %+v", request)
	}
	sweeps, pumps := r.counts()
	eventually(t, "pump-only passes", func() bool { _, current := r.counts(); return current >= pumps+5 })
	if current, _ := r.counts(); current != sweeps {
		t.Fatalf("main drain ran while a claim was outstanding: sweeps %d -> %d", sweeps, current)
	}
	if notice := f.notification(second); notice.State != model.NotificationPending || len(r.delivered()) != 1 {
		t.Fatalf("second notification consumed while first in flight: %+v, deliveries %d", notice, len(r.delivered()))
	}
	claimed := f.notification(first)
	if _, err := f.state.AckNotification(model.NotificationAckRequest{NotificationID: first, ClaimToken: claimed.ClaimToken, ConsumerID: owner, DriverGeneration: generation, HandlingID: "handling:external"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "second delivery after external acknowledgement", func() bool { return len(r.delivered()) == 2 })
	if r.delivered()[1].Notification.NotificationID != second || r.eventCount("acknowledged") != 1 {
		t.Fatalf("second request = %+v, events = %+v", r.delivered()[1], r.events)
	}
	if notice := f.notification(second); notice.State != model.NotificationClaimed || notice.AckedAt != nil {
		t.Fatalf("watcher acknowledged or released: %+v", notice)
	}
}

func TestRestartedWatcherWaitsForThePreviousGenerationsClaim(t *testing.T) {
	f := newFixture(t)
	first := f.subdriverReturn("first question")
	second := f.subdriverReturn("second question")
	r := &recorder{}
	_, stop := startGeneration(t, f, r, r, time.Hour, "watch:one")
	eventually(t, "first delivery", func() bool { return len(r.delivered()) == 1 })
	stop()
	claimed := f.notification(first)

	restarted := &recorder{}
	startGeneration(t, f, restarted, restarted, time.Hour, "watch:two")
	sweeps, _ := restarted.counts()
	eventually(t, "restarted drain passes", func() bool { current, _ := restarted.counts(); return current >= sweeps+5 })
	if len(restarted.delivered()) != 0 || f.notification(second).State != model.NotificationPending {
		t.Fatalf("restarted watcher delivered past an outstanding claim: %+v", restarted.delivered())
	}
	if notice := f.notification(first); notice.State != model.NotificationClaimed || notice.ClaimToken != claimed.ClaimToken || notice.DriverGeneration != "watch:one" || notice.ClaimUntil == nil || !notice.ClaimUntil.Equal(*claimed.ClaimUntil) {
		t.Fatalf("restarted watcher changed the outstanding claim: %+v", notice)
	}
	if _, err := f.state.AckNotification(model.NotificationAckRequest{NotificationID: first, ClaimToken: claimed.ClaimToken, ConsumerID: owner, DriverGeneration: "watch:one", HandlingID: "handling:external"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "second delivery after acknowledgement", func() bool { return len(restarted.delivered()) == 1 })
	if request := restarted.delivered()[0]; request.Notification.NotificationID != second || request.Driver.Generation != "watch:two" {
		t.Fatalf("second request = %+v", request)
	}
}

func TestWatcherRetriesRetryableAndStopsOnRejected(t *testing.T) {
	f := newFixture(t)
	id := f.subdriverReturn("question")
	r := &recorder{outcomes: []func() (driverdelivery.Result, error){
		func() (driverdelivery.Result, error) {
			return driverdelivery.Result{Outcome: driverdelivery.OutcomeRetryable, Detail: "HTTP 503"}, nil
		},
		func() (driverdelivery.Result, error) {
			return driverdelivery.Result{}, &extensionhost.HostError{Kind: extensionhost.ErrorDeadlineExceeded, Operation: "deliver"}
		},
		func() (driverdelivery.Result, error) {
			return driverdelivery.Result{Outcome: driverdelivery.OutcomeRejected, Detail: "HTTP 403"}, nil
		},
	}}
	start(t, f, r, r, time.Hour)
	eventually(t, "three delivery attempts", func() bool { return len(r.delivered()) == 3 })
	time.Sleep(200 * time.Millisecond)
	requests := r.delivered()
	if len(requests) != 3 {
		t.Fatalf("delivery continued after rejection: %d", len(requests))
	}
	for index, request := range requests {
		if request.Claim.DeliveryAttempt != index+1 || request.Claim.ClaimToken != requests[0].Claim.ClaimToken {
			t.Fatalf("attempt %d = %+v", index, request.Claim)
		}
	}
	logs, err := f.state.NotificationDeliveryLogs(id, 100)
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, log := range logs {
		if log.Operation == "notify" {
			results = append(results, log.Result)
		}
	}
	if strings.Join(results, ",") != "retryable,retryable,rejected" {
		t.Fatalf("delivery log results = %v", results)
	}
	if notice := f.notification(id); notice.State != model.NotificationClaimed || notice.AckedAt != nil {
		t.Fatalf("rejected delivery changed notification state: %+v", notice)
	}
	if presented, err := f.state.NotificationPresentationRecorded(id); err != nil || presented {
		t.Fatalf("driver delivery recorded as desktop presentation: %v %v", presented, err)
	}
}

func TestWatcherTreatsProtocolFailuresAsUndeliverable(t *testing.T) {
	f := newFixture(t)
	f.subdriverReturn("question")
	r := &recorder{outcomes: []func() (driverdelivery.Result, error){
		func() (driverdelivery.Result, error) {
			return driverdelivery.Result{}, &extensionhost.HostError{Kind: extensionhost.ErrorProtocolViolation, Operation: "deliver"}
		},
	}}
	start(t, f, r, r, time.Hour)
	eventually(t, "undeliverable", func() bool { return r.eventCount(OutcomeUndeliverable) == 1 })
	time.Sleep(150 * time.Millisecond)
	if len(r.delivered()) != 1 {
		t.Fatalf("undeliverable claim was retried: %d", len(r.delivered()))
	}
}

func TestWatcherClearsSupersededAndLostClaimsWithoutAcknowledging(t *testing.T) {
	f := newFixture(t)
	first := f.subdriverReturn("first")
	second := f.subdriverReturn("second")
	third := f.subdriverReturn("third")
	r := &recorder{}
	start(t, f, r, r, time.Hour)
	eventually(t, "first delivery", func() bool { return len(r.delivered()) == 1 })
	f.exec(`UPDATE driver_notifications SET state='superseded', superseded_at=?, supersede_reason='fixture supersession' WHERE notification_id=?`, time.Now().UTC().Format(time.RFC3339Nano), first)
	eventually(t, "second delivery", func() bool { return len(r.delivered()) == 2 })
	f.exec(`UPDATE driver_notifications SET claim_token='foreign-token', driver_generation='watch:other' WHERE notification_id=?`, second)
	eventually(t, "lost claim", func() bool { return r.eventCount("lost") == 1 })
	time.Sleep(150 * time.Millisecond)
	if len(r.delivered()) != 2 || f.notification(third).State != model.NotificationPending {
		t.Fatalf("watcher delivered past the foreign generation's claim: %d", len(r.delivered()))
	}
	f.exec(`UPDATE driver_notifications SET state='acknowledged', acked_at=?, ack_owner=?, ack_driver_generation='watch:other', handling_id='handling:foreign' WHERE notification_id=?`, time.Now().UTC().Format(time.RFC3339Nano), owner, second)
	eventually(t, "third delivery", func() bool { return len(r.delivered()) == 3 })
	if r.delivered()[2].Notification.NotificationID != third || r.eventCount("superseded") != 1 || r.eventCount("lost") != 1 {
		t.Fatalf("events = %+v", r.events)
	}
	if notice := f.notification(first); notice.AckedAt != nil || notice.State == model.NotificationAcknowledged {
		t.Fatalf("watcher acknowledged %s: %+v", first, notice)
	}
	if notice := f.notification(second); notice.ClaimToken != "foreign-token" || notice.AckDriverGeneration != "watch:other" {
		t.Fatalf("watcher changed the foreign claim: %+v", notice)
	}
}

func TestWatcherRenewsBeforeExpiryThenStopsAtHorizonAndRedelivers(t *testing.T) {
	f := newFixture(t)
	id := f.subdriverReturn("question")
	r := &recorder{}
	start(t, f, r, r, 1500*time.Millisecond)
	eventually(t, "first delivery", func() bool { return len(r.delivered()) == 1 })
	original := r.delivered()[0].Claim
	f.exec(`UPDATE driver_notifications SET claim_until=? WHERE notification_id=?`, time.Now().UTC().Add(1500*time.Millisecond).Format(time.RFC3339Nano), id)
	eventually(t, "renewal", func() bool { return r.eventCount("renewed") >= 1 })
	if renewed := f.notification(id); renewed.ClaimToken != original.ClaimToken || renewed.ClaimUntil == nil || time.Until(*renewed.ClaimUntil) < 30*time.Second {
		t.Fatalf("renewed claim = %+v", renewed)
	}
	eventually(t, "renewal horizon", func() bool { return r.eventCount("horizon") == 1 })
	renewals := r.eventCount("renewed")
	f.exec(`UPDATE driver_notifications SET claim_until=? WHERE notification_id=?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), id)
	eventually(t, "redelivery under a new claim", func() bool { return len(r.delivered()) == 2 })
	redelivered := r.delivered()[1]
	if redelivered.Notification.NotificationID != id || redelivered.Claim.ClaimToken == original.ClaimToken || redelivered.Claim.DeliveryAttempt != 1 || r.eventCount("expired") != 1 {
		t.Fatalf("redelivery = %+v, events = %+v", redelivered, r.events)
	}
	if r.eventCount("renewed") != renewals {
		t.Fatal("renewal continued past the horizon")
	}
	if notice := f.notification(id); notice.DeliveryAttempts != 2 || notice.AckedAt != nil {
		t.Fatalf("notification = %+v", notice)
	}
}

func TestWatcherRenewsBeforeExpiryWhenPollMinExceedsTheRenewalInterval(t *testing.T) {
	f := newFixture(t)
	id := f.subdriverReturn("question")
	r := &recorder{}
	watcher := Watcher{Store: f.state, Activation: r, Deliverer: r, DriverID: owner, Generation: "watch:slow-poll",
		ClaimTTL: store.MinNotificationClaimTTL, PollMin: time.Minute, PollMax: time.Minute, RenewHorizon: time.Hour,
		DeliveryTimeout: time.Second, Log: r.log}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watcher.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	eventually(t, "first delivery", func() bool { return len(r.delivered()) == 1 })
	original := r.delivered()[0].Claim
	deadline := original.ClaimUntil.Add(-5 * time.Second)
	for r.eventCount("renewed") == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no renewal before the lease neared expiry: events %+v", r.events)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if renewed := f.notification(id); renewed.State != model.NotificationClaimed || renewed.ClaimToken != original.ClaimToken || renewed.ClaimUntil == nil || !renewed.ClaimUntil.After(original.ClaimUntil) {
		t.Fatalf("renewed claim = %+v", renewed)
	}
	if len(r.delivered()) != 1 || r.eventCount("expired") != 0 {
		t.Fatalf("claim was redelivered instead of renewed: deliveries %d, events %+v", len(r.delivered()), r.events)
	}
}

func TestWatcherRenewsAndSettlesDuringSlowActivationWithoutOverlappingPumps(t *testing.T) {
	f := newFixture(t)
	first := f.subdriverReturn("first question")
	second := f.subdriverReturn("second question")
	r := &recorder{}
	generation, _ := start(t, f, r, r, time.Hour)
	eventually(t, "first delivery", func() bool { return len(r.delivered()) == 1 })
	original := r.delivered()[0].Claim
	gate := r.block()
	eventually(t, "slow activation pass", func() bool { blocked, _ := r.pumpState(); return blocked == 1 })
	sweeps, pumps := r.counts()
	f.exec(`UPDATE driver_notifications SET claim_until=? WHERE notification_id=?`, time.Now().UTC().Add(1500*time.Millisecond).Format(time.RFC3339Nano), first)
	eventually(t, "renewal during slow activation", func() bool { return r.eventCount("renewed") >= 1 })
	if renewed := f.notification(first); renewed.State != model.NotificationClaimed || renewed.ClaimToken != original.ClaimToken || renewed.ClaimUntil == nil || time.Until(*renewed.ClaimUntil) < 30*time.Second {
		t.Fatalf("renewed claim = %+v", renewed)
	}
	if _, err := f.state.AckNotification(model.NotificationAckRequest{NotificationID: first, ClaimToken: original.ClaimToken, ConsumerID: owner, DriverGeneration: generation, HandlingID: "handling:external"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "acknowledgement during slow activation", func() bool { return r.eventCount("acknowledged") == 1 })
	time.Sleep(150 * time.Millisecond)
	if current, currentPumps := r.counts(); current != sweeps || currentPumps != pumps || len(r.delivered()) != 1 || f.notification(second).State != model.NotificationPending {
		t.Fatalf("watcher drained or pumped past the in-flight activation: sweeps %d -> %d, pumps %d -> %d, deliveries %d", sweeps, current, pumps, currentPumps, len(r.delivered()))
	}
	close(gate)
	eventually(t, "second delivery after the activation pass", func() bool { return len(r.delivered()) == 2 })
	if _, overlap := r.pumpState(); overlap || r.delivered()[1].Notification.NotificationID != second || r.eventCount("expired") != 0 {
		t.Fatalf("overlap %v, second request = %+v, events = %+v", overlap, r.delivered()[1], r.events)
	}
}

func TestWatcherCancellationWaitsForTheInFlightActivationPass(t *testing.T) {
	f := newFixture(t)
	id := f.subdriverReturn("question")
	r := &recorder{}
	watcher := Watcher{Store: f.state, Activation: r, Deliverer: r, DriverID: owner, Generation: "watch:cancel",
		ClaimTTL: time.Minute, PollMin: 20 * time.Millisecond, PollMax: 40 * time.Millisecond, RenewHorizon: time.Hour,
		DeliveryTimeout: 500 * time.Millisecond, Log: r.log}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watcher.Run(ctx)
		close(done)
	}()
	eventually(t, "delivery", func() bool { return len(r.delivered()) == 1 })
	gate := r.block()
	eventually(t, "slow activation pass", func() bool { blocked, _ := r.pumpState(); return blocked == 1 })
	cancel()
	select {
	case <-done:
		t.Fatal("watcher stopped while an activation pass was in flight")
	case <-time.After(150 * time.Millisecond):
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not stop after the activation pass finished")
	}
	if blocked, overlap := r.pumpState(); blocked != 0 || overlap || r.eventCount("stopped") != 1 {
		t.Fatalf("blocked %d, overlap %v, events %+v", blocked, overlap, r.events)
	}
	if notice := f.notification(id); notice.State != model.NotificationClaimed || notice.AckedAt != nil {
		t.Fatalf("notification = %+v", notice)
	}
}

func TestWatcherWithholdsDeliveryWhenRenewalFails(t *testing.T) {
	for _, test := range []struct {
		name  string
		fault func(*fixture, string)
	}{
		{name: "conflict", fault: func(f *fixture, id string) {
			f.exec(`UPDATE driver_notifications SET target_driver_id='driver:adopter' WHERE notification_id=?`, id)
		}},
		{name: "error", fault: func(f *fixture, _ string) {
			f.exec(`CREATE TRIGGER renewal_fault BEFORE UPDATE OF claim_until ON driver_notifications BEGIN SELECT RAISE(ABORT, 'fixture renewal fault'); END`)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			id := f.subdriverReturn("question")
			r := &recorder{outcomes: []func() (driverdelivery.Result, error){
				func() (driverdelivery.Result, error) {
					time.Sleep(1100 * time.Millisecond)
					f.exec(`UPDATE driver_notifications SET claim_until=? WHERE notification_id=?`, time.Now().UTC().Add(600*time.Millisecond).Format(time.RFC3339Nano), id)
					test.fault(f, id)
					return driverdelivery.Result{Outcome: driverdelivery.OutcomeRetryable, Detail: "HTTP 503"}, nil
				},
			}}
			start(t, f, r, r, time.Hour)
			eventually(t, "failed renewal", func() bool { return r.eventCount("renew_failed") >= 1 })
			_, pumps := r.counts()
			eventually(t, "expiry observed from stored state", func() bool { return r.eventCount("expired") == 1 })
			if _, current := r.counts(); len(r.delivered()) != 1 || r.eventCount("renewed") != 0 || current <= pumps {
				t.Fatalf("delivered after a failed renewal: deliveries %d, pumps %d -> %d, events %+v", len(r.delivered()), pumps, current, r.events)
			}
		})
	}
}

func TestWatcherHoldsUnobservableClaimWithoutRenewingOrDelivering(t *testing.T) {
	f := newFixture(t)
	id := f.subdriverReturn("question")
	r := &recorder{outcomes: []func() (driverdelivery.Result, error){
		func() (driverdelivery.Result, error) {
			f.exec(`ALTER TABLE driver_notifications RENAME TO driver_notifications_hidden`)
			return driverdelivery.Result{Outcome: driverdelivery.OutcomeRetryable, Detail: "HTTP 503"}, nil
		},
	}}
	start(t, f, r, r, time.Hour)
	eventually(t, "observation failures", func() bool { return r.eventCount("observe_failed") >= 3 })
	sweeps, pumps := r.counts()
	failures := r.eventCount("observe_failed")
	eventually(t, "continued observation failures", func() bool { return r.eventCount("observe_failed") >= failures+5 })
	if current, currentPumps := r.counts(); current != sweeps || currentPumps != pumps || len(r.delivered()) != 1 || r.eventCount("renewed")+r.eventCount("renew_failed") != 0 {
		t.Fatalf("watcher acted on an unobserved claim: sweeps %d -> %d, pumps %d -> %d, deliveries %d, events %+v", sweeps, current, pumps, currentPumps, len(r.delivered()), r.events)
	}
	f.exec(`ALTER TABLE driver_notifications_hidden RENAME TO driver_notifications`)
	eventually(t, "retry after observation recovers", func() bool { return len(r.delivered()) == 2 })
	first, retried := r.delivered()[0].Claim, r.delivered()[1].Claim
	if retried.ClaimToken != first.ClaimToken || retried.DeliveryAttempt != 2 {
		t.Fatalf("retry claim = %+v, first = %+v", retried, first)
	}
	if current, _ := r.counts(); current != sweeps {
		t.Fatalf("main drain ran while the claim was outstanding: sweeps %d -> %d", sweeps, current)
	}
	if notice := f.notification(id); notice.State != model.NotificationClaimed || notice.ClaimToken != first.ClaimToken || notice.AckedAt != nil {
		t.Fatalf("notification = %+v", notice)
	}
}

func TestWatcherCancellationLeavesClaimUnacknowledged(t *testing.T) {
	f := newFixture(t)
	id := f.subdriverReturn("question")
	r := &recorder{}
	_, stop := start(t, f, r, r, time.Hour)
	eventually(t, "delivery", func() bool { return len(r.delivered()) == 1 })
	stop()
	if notice := f.notification(id); notice.State != model.NotificationClaimed || notice.AckedAt != nil || r.eventCount("stopped") != 1 {
		t.Fatalf("notification = %+v", notice)
	}
}

func TestWatcherRetriesExtensionTimeoutAndCrash(t *testing.T) {
	dir := t.TempDir()
	manifest := `{"wire":{"major":1,"minor_min":0,"minor_max":0},"extension":{"id":"fixture.delivery","version":"1.0.0"},"capabilities":[{"name":"driver.delivery","version":1,"operations":["deliver"]}]}`
	for name, invoke := range map[string]string{"timeout": "read -r line; exec sleep 30", "crash": "read -r line; exit 3"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			id := f.subdriverReturn("question")
			path := filepath.Join(dir, name)
			body := "#!/bin/sh\ncase \"$1\" in\ndescribe) printf '%s\\n' '" + manifest + "' ;;\ninvoke) " + invoke + " ;;\nesac\n"
			if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte(body))
			deliverer := driverdelivery.NewExtensionDeliverer(config.DeliveryExtensionConfig{ExtensionID: "fixture.delivery", Command: []string{path}, SHA256: hex.EncodeToString(sum[:])}, nil)
			r := &recorder{}
			start(t, f, r, deliverer, time.Hour)
			eventually(t, "two retryable attempts", func() bool { return r.eventCount(driverdelivery.OutcomeRetryable) >= 2 })
			if notice := f.notification(id); notice.State != model.NotificationClaimed || notice.AckedAt != nil {
				t.Fatalf("notification = %+v", notice)
			}
		})
	}
}

func TestGuardSkipsOwnersNoWatcherServes(t *testing.T) {
	dataDir := t.TempDir()
	for _, ineligible := range []string{"driver:pi:session", "coordinator:subdriver_1", "subdriver:subdriver_1"} {
		release, err := Guard(dataDir, ineligible)
		if err != nil {
			t.Fatalf("%s: %v", ineligible, err)
		}
		release()
		if Eligible(ineligible) {
			t.Fatalf("%s is eligible for a watcher", ineligible)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "watch")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("guard created watcher lock state for ineligible owners: %v", err)
	}
	if !Eligible(owner) {
		t.Fatalf("%s is not eligible for a watcher", owner)
	}
}

func TestLockExcludesSecondWatcherAndManualCommands(t *testing.T) {
	dataDir := t.TempDir()
	release, err := Guard(dataDir, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(LockPath(dataDir, owner)); err != nil {
		t.Fatalf("guard did not create the owner lock: %v", err)
	}
	_, err = Acquire(dataDir, Holder{DriverID: owner, Generation: "watch:zero"})
	active, ok := err.(*model.KindError)
	if !ok || active.Kind != ErrorWatchActive || active.Evidence["driver_id"] != owner || active.Evidence["watcher_generation"] != "" {
		t.Fatalf("watcher started during a manual command on an absent lock path: %v", err)
	}
	second, err := Guard(dataDir, owner)
	if err != nil {
		t.Fatalf("concurrent manual command: %v", err)
	}
	second()
	release()
	lock, err := Acquire(dataDir, Holder{DriverID: owner, Generation: "watch:one", PID: os.Getpid(), StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dataDir, Holder{DriverID: owner, Generation: "watch:two", PID: os.Getpid()}); err == nil || !strings.Contains(err.Error(), "watch:one") {
		t.Fatalf("second watcher = %v", err)
	}
	_, err = Guard(dataDir, owner)
	kind, ok := err.(interface{ ErrorKind() string })
	if !ok || kind.ErrorKind() != ErrorWatchActive || !strings.Contains(err.Error(), "watch:one") {
		t.Fatalf("manual command guard = %v", err)
	}
	other, err := Guard(dataDir, "driver:other")
	if err != nil {
		t.Fatalf("other owner guarded: %v", err)
	}
	other()
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	release, err = Guard(dataDir, owner)
	if err != nil {
		t.Fatalf("guard after watcher exit: %v", err)
	}
	_, err = Acquire(dataDir, Holder{DriverID: owner, Generation: "watch:three"})
	if active, ok := err.(*model.KindError); !ok || active.Kind != ErrorWatchActive || active.Evidence["watcher_generation"] != "" || strings.Contains(err.Error(), "watch:one") {
		t.Fatalf("watcher during a manual command = %v", err)
	}
	release()
	lock, err = Acquire(dataDir, Holder{DriverID: owner, Generation: "watch:four"})
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}
