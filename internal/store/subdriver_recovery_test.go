package store

import (
	"errors"
	"strings"
	"testing"

	"shephrd/internal/model"
)

func heldSubdriverFixture(t *testing.T, s *Store, id string) model.SubdriverFence {
	t.Helper()
	f := subdriverStartFixture(t, s, id)
	if err := s.FinishSubdriver(f, "retained checkpoint", "failed turn"); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestOwnerRecoveryFencesEveryLinkedRequestAndHeldState(t *testing.T) {
	s, repo := subdriverStoreFixture(t)
	const owner = "driver:hermes"
	first, err := s.HandoffSubdriver(repo.ID, "", owner, "first", "First", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.HandoffSubdriver(repo.ID, "", owner, "second", "Second", "", "")
	if err != nil {
		t.Fatal(err)
	}
	f := subdriverStartFixture(t, s, first.SubdriverID)
	if _, err = s.SubdriverReturn(f, first.ID, "done", "result", "Finished"); err != nil {
		t.Fatal(err)
	}
	if done, err := s.SubdriverRequest(first.ID); err != nil || done.State != "done" {
		t.Fatalf("completed request fixture: %+v %v", done, err)
	}
	if err = s.FinishSubdriver(f, "retained checkpoint", ""); err != nil {
		t.Fatal(err)
	}
	selected := "replacement-model"
	recovery := model.SubdriverRecovery{DriverID: owner, Model: &selected}
	if err = s.RecoverSubdriver(f.ID, f.Generation, recovery); errorKind(err) != "subdriver_not_held" {
		t.Fatalf("idle owner recovered: %v", err)
	}
	f = heldSubdriverFixture(t, s, f.ID)
	if err = s.AdoptSubdriverRequest(first.ID, owner, "driver:other"); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		owner string
		kind  string
	}{{owner, "subdriver_owner_refused"}, {"driver:other", "subdriver_owner_refused"}} {
		if err = s.RecoverSubdriver(f.ID, f.Generation, model.SubdriverRecovery{DriverID: check.owner, Model: &selected}); errorKind(err) != check.kind {
			t.Fatalf("%s recovered mixed owners: %v", check.owner, err)
		}
	}
	if err = s.AdoptSubdriverRequest(first.ID, "driver:other", owner); err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverSubdriver(f.ID, f.Generation-1, recovery); err == nil || !strings.Contains(err.Error(), "generation changed") {
		t.Fatalf("stale generation recovered: %v", err)
	}
	c, err := s.Subdriver(f.ID)
	if err != nil || c.State != "held" || c.Model != "configured-model" {
		t.Fatalf("refused recovery changed owner: %+v %v", c, err)
	}
	if err = s.RecoverSubdriver(f.ID, f.Generation, recovery); err != nil {
		t.Fatal(err)
	}
	c, err = s.Subdriver(f.ID)
	if err != nil || c.State != "idle" || c.Model != selected || c.Harness != "pi" || c.Generation != f.Generation {
		t.Fatalf("owner recovery: %+v %v", c, err)
	}
	page, err := s.SubdriverPage(f.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	recoveries := 0
	for _, event := range page.Events {
		if event.Kind == "recovery" {
			recoveries++
			if event.RequestID != second.ID || !strings.Contains(event.Payload, "Recovered by main driver "+owner) || !strings.Contains(event.Payload, `"replacement-model"`) {
				t.Fatalf("recovery event %+v", event)
			}
		}
	}
	if recoveries != 1 {
		t.Fatalf("recovery events = %d: %+v", recoveries, page.Events)
	}
	if err = s.RecoverSubdriver(f.ID, f.Generation, recovery); errorKind(err) != "subdriver_not_held" {
		t.Fatalf("repeated recovery: %v", err)
	}
}

func TestOwnerRecoveryRequiresRetainedHarnessAndBoundedLaunchEvidence(t *testing.T) {
	s, _ := subdriverStoreFixture(t)
	request, err := s.HandoffSubdriver("", "Research", "driver:hermes", "queued", "Original", "", "")
	if err != nil {
		t.Fatal(err)
	}
	selected := "model"
	if err = s.RecoverSubdriver(request.SubdriverID, 0, model.SubdriverRecovery{Model: &selected}); err == nil {
		t.Fatal("model selected without a retained harness")
	}
	f := heldSubdriverFixture(t, s, request.SubdriverID)
	if err = s.RecoverSubdriver(f.ID, f.Generation, model.SubdriverRecovery{DriverID: "driver:hermes", LaunchAbsent: strings.Repeat("e", 513)}); err == nil {
		t.Fatal("unbounded launch-absent evidence accepted")
	}
	if err = s.RecoverSubdriver(f.ID, f.Generation, model.SubdriverRecovery{DriverID: "driver:hermes", LaunchAbsent: "Launcher log and exact source show no harness start"}); err != nil {
		t.Fatal(err)
	}
	page, err := s.SubdriverPage(f.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if last := page.Events[len(page.Events)-1]; last.Kind != "recovery" || !strings.HasPrefix(last.Payload, "Operator asserted absent unrecorded launch effects: Launcher log") {
		t.Fatalf("assertion not recorded: %+v", last)
	}
}

func TestObservedReservationRejectsSelectionChangedByRecovery(t *testing.T) {
	s, repo := subdriverStoreFixture(t)
	request, err := s.HandoffSubdriver(repo.ID, "", "driver:hermes", "race", "Original", "", "")
	if err != nil {
		t.Fatal(err)
	}
	f := subdriverStartFixture(t, s, request.SubdriverID)
	if err = s.FinishSubdriver(f, "", ""); err != nil {
		t.Fatal(err)
	}
	observed, err := s.Subdriver(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.HoldSubdriverObservation(observed, "endpoint uncertain"); err != nil {
		t.Fatal(err)
	}
	selected := "replacement-model"
	if err = s.RecoverSubdriver(f.ID, f.Generation, model.SubdriverRecovery{DriverID: "driver:hermes", Model: &selected}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReserveObservedSubdriver(observed, "", observed.Harness, observed.Model, "headless"); err == nil {
		t.Fatal("reservation read before recovery restored the old model")
	}
	if _, err = s.HandoffSubdriver(repo.ID, "", "driver:other", "foreign", "Foreign", "", ""); err != nil {
		t.Fatal(err)
	}
	current, err := s.Subdriver(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReserveObservedSubdriver(current, "driver:hermes", current.Harness, current.Model, "headless"); errorKind(err) != "subdriver_owner_refused" {
		t.Fatalf("owner reservation with a foreign request: %v", err)
	}
	next, err := s.ReserveObservedSubdriver(current, "", current.Harness, current.Model, "headless")
	if err != nil {
		t.Fatal(err)
	}
	if c, err := s.Subdriver(f.ID); err != nil || c.Model != selected || c.Generation != next.Generation {
		t.Fatalf("reserved selection %+v %v", c, err)
	}
}

func errorKind(err error) string {
	var typed *model.KindError
	if errors.As(err, &typed) {
		return typed.Kind
	}
	return ""
}
