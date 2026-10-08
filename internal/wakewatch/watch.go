package wakewatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"shephrd/internal/driverdelivery"
	extensionhost "shephrd/internal/extension"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

const (
	OutcomeUndeliverable = "undeliverable"
	maxEventDetail       = 512
)

type Activation interface {
	SweepDeaths() error
	PumpSubdrivers(driver string) error
}

type Deliverer interface {
	Deliver(context.Context, driverdelivery.Request) (driverdelivery.Result, error)
}

type Event struct {
	Time            time.Time `json:"time"`
	Event           string    `json:"event"`
	DriverID        string    `json:"driver_id,omitempty"`
	Generation      string    `json:"generation,omitempty"`
	NotificationID  string    `json:"notification_id,omitempty"`
	Kind            string    `json:"kind,omitempty"`
	DeliveryAttempt int       `json:"delivery_attempt,omitempty"`
	Detail          string    `json:"detail,omitempty"`
}

type Watcher struct {
	Store           *store.Store
	Activation      Activation
	Deliverer       Deliverer
	DriverID        string
	Generation      string
	ClaimTTL        time.Duration
	PollMin         time.Duration
	PollMax         time.Duration
	RenewHorizon    time.Duration
	DeliveryTimeout time.Duration
	Log             func(Event)
	pumping         chan error
}

type claim struct {
	notification model.DriverNotification
	request      driverdelivery.Request
	claimedAt    time.Time
	renewedAt    time.Time
	claimUntil   time.Time
	attempts     int
	nextDelivery time.Time
	retryDelay   time.Duration
	finished     bool
	horizon      bool
}

func (w *Watcher) Run(ctx context.Context) {
	w.log(Event{Event: "started", DriverID: w.DriverID, Generation: w.Generation})
	delay := w.PollMin
	var current *claim
	for ctx.Err() == nil {
		if current == nil {
			if current = w.drain(ctx); current == nil {
				if !sleep(ctx, delay) {
					break
				}
				delay = min(delay*2, w.PollMax)
				continue
			}
			delay = w.PollMin
			w.deliver(ctx, current)
		} else if !w.tick(ctx, current) {
			current = nil
			continue
		}
		if !sleep(ctx, w.claimDelay(current)) {
			break
		}
	}
	w.finishPump(true)
	w.log(Event{Event: "stopped", DriverID: w.DriverID, Generation: w.Generation, Detail: "no claim was acknowledged or released; outstanding claims expire for redelivery"})
}

func (w *Watcher) drain(ctx context.Context) *claim {
	w.finishPump(true)
	if ctx.Err() != nil {
		return nil
	}
	if err := w.Activation.SweepDeaths(); err != nil {
		w.log(Event{Event: "drain_failed", Detail: err.Error()})
		return nil
	}
	if err := w.Activation.PumpSubdrivers(w.DriverID); err != nil {
		w.log(Event{Event: "drain_failed", Detail: err.Error()})
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	result, err := w.Store.DrainNotificationExclusive(w.DriverID, w.Generation, w.ClaimTTL)
	if err != nil {
		w.log(Event{Event: "drain_failed", Detail: err.Error()})
		return nil
	}
	if len(result.Notifications) == 0 {
		return nil
	}
	notification := result.Notifications[0]
	now := time.Now()
	current := &claim{notification: notification, claimedAt: now, renewedAt: now, claimUntil: *notification.ClaimUntil, retryDelay: w.PollMin}
	current.request = driverdelivery.NewRequest(notification, w.Generation, 1, w.obligations())
	w.log(Event{Event: "claimed", NotificationID: notification.NotificationID, Kind: notification.Kind})
	return current
}

func (w *Watcher) tick(ctx context.Context, current *claim) bool {
	now := time.Now()
	stored, err := w.Store.Notification(current.notification.NotificationID)
	if err != nil {
		w.log(Event{Event: "observe_failed", NotificationID: current.notification.NotificationID, Detail: err.Error()})
		return true
	}
	if settled, detail := settlement(stored, current, now); settled != "" {
		w.log(Event{Event: settled, NotificationID: stored.NotificationID, Kind: stored.Kind, Detail: detail})
		return false
	}
	current.claimUntil = *stored.ClaimUntil
	if !current.horizon && now.Sub(current.claimedAt) >= w.RenewHorizon {
		current.horizon = true
		w.log(Event{Event: "horizon", NotificationID: current.notification.NotificationID, Detail: "renewal stopped; the claim expires and is redelivered under a new claim"})
	}
	deliver := !current.finished && !current.horizon && !now.Before(current.nextDelivery)
	if !current.horizon && !now.Before(current.renewalDue()) {
		request := model.NotificationRenewRequest{NotificationID: current.notification.NotificationID, ClaimToken: current.notification.ClaimToken, ConsumerID: w.DriverID, DriverGeneration: w.Generation}
		renewed, err := w.Store.RenewNotification(request, w.ClaimTTL)
		switch {
		case err == nil:
			current.renewedAt = now
			current.claimUntil = *renewed.ClaimUntil
			w.log(Event{Event: "renewed", NotificationID: current.notification.NotificationID})
		case errors.Is(err, store.ErrNotificationStale):
			w.log(Event{Event: "stale", NotificationID: current.notification.NotificationID, Detail: err.Error()})
			return false
		default:
			w.log(Event{Event: "renew_failed", NotificationID: current.notification.NotificationID, Detail: err.Error()})
			deliver = false
		}
	}
	if deliver {
		w.deliver(ctx, current)
	}
	if ctx.Err() == nil && w.finishPump(false) {
		pumping := make(chan error, 1)
		w.pumping = pumping
		go func() { pumping <- w.Activation.PumpSubdrivers(w.DriverID) }()
	}
	return true
}

func (w *Watcher) finishPump(wait bool) bool {
	if w.pumping == nil {
		return true
	}
	var err error
	if wait {
		err = <-w.pumping
	} else {
		select {
		case err = <-w.pumping:
		default:
			return false
		}
	}
	w.pumping = nil
	if err != nil {
		w.log(Event{Event: "pump_failed", Detail: err.Error()})
	}
	return true
}

func (c *claim) renewalDue() time.Time {
	return c.renewedAt.Add(RenewalInterval(c.renewedAt, c.claimUntil))
}

func (w *Watcher) claimDelay(current *claim) time.Duration {
	if current.horizon {
		return w.PollMin
	}
	return min(w.PollMin, max(time.Until(current.renewalDue()), time.Second))
}

func settlement(stored model.DriverNotification, current *claim, now time.Time) (string, string) {
	ours := stored.ClaimToken == current.notification.ClaimToken && stored.ClaimOwner == current.notification.ClaimOwner && stored.DriverGeneration == current.notification.DriverGeneration
	switch {
	case stored.State == model.NotificationAcknowledged && ours:
		return "acknowledged", "handling ID " + stored.HandlingID
	case stored.State == model.NotificationSuperseded:
		return "superseded", stored.SupersedeReason
	case stored.State == model.NotificationPending:
		return "reclaimed", "the claim was released or reclaimed"
	case stored.State != model.NotificationClaimed || !ours:
		return "lost", "the stored claim identity no longer matches this watcher"
	case stored.ClaimUntil == nil || !now.Before(*stored.ClaimUntil):
		return "expired", "the claim expired and is redelivered by the next drain"
	}
	return "", ""
}

func (w *Watcher) deliver(ctx context.Context, current *claim) {
	current.attempts++
	request := current.request
	request.Claim.ClaimUntil = current.claimUntil.UTC()
	request.Claim.DeliveryAttempt = current.attempts
	deliverContext, cancel := context.WithTimeout(ctx, w.DeliveryTimeout)
	result, err := w.Deliverer.Deliver(deliverContext, request)
	cancel()
	if ctx.Err() != nil {
		return
	}
	outcome, detail := result.Outcome, result.Detail
	if err != nil {
		outcome, detail = OutcomeUndeliverable, err.Error()
		var hostErr *extensionhost.HostError
		if errors.As(err, &hostErr) && (hostErr.Kind == extensionhost.ErrorDeadlineExceeded || hostErr.Kind == extensionhost.ErrorExtensionExit) {
			outcome = driverdelivery.OutcomeRetryable
		}
	}
	if outcome == driverdelivery.OutcomeRetryable {
		current.nextDelivery = time.Now().Add(current.retryDelay)
		current.retryDelay = min(current.retryDelay*2, w.PollMax)
	} else {
		current.finished = true
	}
	detail = bounded(detail)
	log := model.NotificationDeliveryLog{NotificationID: current.notification.NotificationID, Operation: "notify", ConsumerID: w.DriverID, DriverGeneration: w.Generation, ClaimToken: current.notification.ClaimToken, Result: outcome, Detail: fmt.Sprintf("%s attempt %d: %s", driverdelivery.CapabilityName, current.attempts, detail)}
	if err := w.Store.RecordNotificationDelivery(log); err != nil {
		w.log(Event{Event: "record_failed", NotificationID: current.notification.NotificationID, Detail: err.Error()})
	}
	w.log(Event{Event: outcome, NotificationID: current.notification.NotificationID, Kind: current.notification.Kind, DeliveryAttempt: current.attempts, Detail: detail})
}

func (w *Watcher) obligations() *driverdelivery.Obligations {
	snapshot, err := w.Store.AttentionSnapshot(model.AttentionFilter{DriverID: w.DriverID, Limit: 1})
	if err != nil {
		return nil
	}
	counts := snapshot.Counts
	return &driverdelivery.Obligations{SchemaVersion: snapshot.SchemaVersion, Counts: driverdelivery.ObligationCounts{ActNow: counts.ActNow, NeedsDisposition: counts.NeedsDisposition, ResultReady: counts.ResultReady, PlannedReady: counts.PlannedReady}}
}

func (w *Watcher) log(event Event) {
	event.Time = time.Now().UTC()
	event.Detail = bounded(event.Detail)
	w.Log(event)
}

func RenewalInterval(renewedAt, claimUntil time.Time) time.Duration {
	return min(max(claimUntil.Sub(renewedAt)/2, time.Second), time.Minute)
}

func bounded(value string) string {
	if len(value) <= maxEventDetail {
		return value
	}
	return value[:maxEventDetail]
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
