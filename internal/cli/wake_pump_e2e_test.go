package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestWakePumpCLILeavesIneligibleOwnersAndNotificationsUntouched(t *testing.T) {
	fixture := newPlanSurfaceFixture(t, "#!/bin/sh\nexit 97\n")
	fixture.environment = compoundCLIEnvironment(fixture.environment, map[string]string{
		"SHEPHRD_WORKER": "", "SHEPHRD_SUBDRIVER_ID": "", "SHEPHRD_SUBDRIVER_GENERATION": "", "SHEPHRD_SUBDRIVER_TOKEN": "",
		"SHEPHRD_COORDINATOR_ID": "", "SHEPHRD_COORDINATOR_GENERATION": "", "SHEPHRD_COORDINATOR_TOKEN": "",
	})
	state, err := store.Open(fixture.database)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	var owners []model.Subdriver
	for _, kind := range []string{"empty", "held", "starting", "running", "idle-live-runner", "idle-live-harness", "cross-owner"} {
		driver := "driver:main"
		if kind == "cross-owner" {
			driver = "driver:other"
		}
		r, err := state.HandoffSubdriver("", "Pump fixture", driver, kind, kind, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if kind != "cross-owner" {
			f, err := state.ReserveSubdriver(r.SubdriverID, 0, "pi", "fixture", "headless")
			if err != nil {
				t.Fatal(err)
			}
			if kind != "starting" {
				pid := 0
				if kind == "running" || kind == "idle-live-runner" {
					pid = os.Getpid()
				}
				if err := state.StartSubdriver(f, pid, "fixture"); err != nil {
					t.Fatal(err)
				}
				if kind == "idle-live-harness" {
					if err := state.SubdriverHarnessPID(f, os.Getpid()); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "empty" {
					page, err := state.SubdriverPage(f.ID, 0)
					if err != nil {
						t.Fatal(err)
					}
					for _, event := range page.Events {
						if err := state.HandleSubdriverEvent(f, event.ID); err != nil {
							t.Fatal(err)
						}
					}
				}
				if kind != "running" {
					failure := ""
					if kind == "held" {
						failure = "fixture hold"
					}
					if err := state.FinishSubdriver(f, "retained", failure); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		owner, err := state.Subdriver(r.SubdriverID)
		if err != nil {
			t.Fatal(err)
		}
		owners = append(owners, owner)
	}
	notifications, err := state.Notifications("")
	if err != nil || len(notifications) != 1 {
		t.Fatalf("pending held notification: %+v %v", notifications, err)
	}
	for range 2 {
		var result struct {
			Pumped   bool   `json:"pumped"`
			DriverID string `json:"driver_id"`
		}
		if err := json.Unmarshal(fixture.run(t, map[string]string{"PI_SESSION_ID": "fixture"}, "wake", "pump", "--driver-id", "driver:main", "--json"), &result); err != nil || !result.Pumped || result.DriverID != "driver:main" {
			t.Fatalf("pump: %+v %v", result, err)
		}
	}
	for _, before := range owners {
		after, err := state.Subdriver(before.ID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("ineligible owner changed: %+v -> %+v %v", before, after, err)
		}
	}
	after, err := state.Notifications("")
	if err != nil || !reflect.DeepEqual(notifications, after) {
		t.Fatalf("pump changed notification delivery: %+v -> %+v %v", notifications, after, err)
	}
	fixture.runFailure(t, nil, "wake", "pump", "--json")
	fixture.runFailure(t, nil, "wake", "pump", "--driver-id", "", "--json")
	fixture.run(t, map[string]string{"PI_SESSION_ID": "fixture"}, "wake", "pump", "--json")
	path := filepath.Join(t.TempDir(), "disabled.toml")
	if err := os.WriteFile(path, []byte("[wake]\nenabled = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fixture.runFailure(t, map[string]string{"SHEPHRD_CONFIG": path, "SHEPHRD_STATE_DIR": t.TempDir()}, "wake", "pump", "--driver-id", "driver:main", "--json")
}
