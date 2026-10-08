package cli

import (
	"bytes"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"shephrd/internal/model"
	"shephrd/internal/store"
)

func TestCoordinationCommandHelpExplainsReceipts(t *testing.T) {
	for _, test := range []struct {
		args []string
		want []string
	}{
		{[]string{"worker", "send", "--help"}, []string{"waiting worker", "not queued or delivered", "no hold was applied", "successful receipt", "annotations and narrative plans are not delivered instructions", "Terminal tasks cannot be resumed with send"}},
		{[]string{"subdriver", "return", "--help"}, []string{"immediately completes the conversational request", "not assignment or progress", "confirm successful worker spawn", "do not reopen a completed request", "subdriver handoff --lead-request", "nonterminal", "subdriver reply"}},
		{[]string{"subdriver", "handoff", "--help"}, []string{"sibling handoff or continuation"}},
	} {
		t.Run(strings.Join(test.args[:2], "-"), func(t *testing.T) {
			root := New()
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			root.SetArgs(test.args)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			for _, want := range test.want {
				if !strings.Contains(output.String(), want) {
					t.Errorf("%v help missing %q: %s", test.args, want, output.String())
				}
			}
		})
	}
}

func TestSubdriverCommandScopeAndLifecycleBoundaries(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(model.Repo{Name: "fixture", Path: t.TempDir(), DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.HandoffSubdriver(repo.ID, "", "driver:main", "original", "Read-only investigation. No discard authority.", "", "")
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.ReserveSubdriver(r.SubdriverID, 0, "pi", "fixture", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.StartSubdriver(f, 1, "fixture"); err != nil {
		t.Fatal(err)
	}
	task, err := s.DispatchSubdriverWorker(f, r.ID, "investigate", model.Task{FeatureKey: "investigate", Objective: "Read only", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHEPHRD_WORKER", "")
	t.Setenv("SHEPHRD_SUBDRIVER_ID", f.ID)
	t.Setenv("SHEPHRD_SUBDRIVER_GENERATION", strconv.Itoa(f.Generation))
	t.Setenv("SHEPHRD_SUBDRIVER_TOKEN", f.Token)
	app := &application{store: s}
	for _, path := range []string{"worker status", "worker peek", "worker stop", "task attest-delivery", "task attest-report-recovery", "task verify-delivery", "workspace release"} {
		cmd, _, err := New().Find(strings.Fields(path))
		if err != nil {
			t.Fatal(err)
		}
		if err := subdriverCommandFence(app, cmd, []string{task.ID}); err != nil {
			t.Fatalf("existing scoped operation %s: %v", path, err)
		}
		if err := subdriverCommandFence(app, cmd, []string{"unowned-task"}); err == nil {
			t.Fatalf("cross-scope operation %s accepted", path)
		}
		if cmd.Flags().Lookup("discard") != nil {
			if err := cmd.Flags().Set("discard", "true"); err != nil {
				t.Fatal(err)
			}
			if err := subdriverCommandFence(app, cmd, []string{task.ID}); err == nil {
				t.Fatalf("%s accepted new discard authority", path)
			}
		}
	}
	t.Setenv("SHEPHRD_SUBDRIVER_ID", "")
	t.Setenv("SHEPHRD_SUBDRIVER_GENERATION", "")
	t.Setenv("SHEPHRD_SUBDRIVER_TOKEN", "")
	cmd, _, err := New().Find([]string{"wake", "drain"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("driver-id", " coordinator:"+f.ID+" "); err != nil {
		t.Fatal(err)
	}
	if err := subdriverCommandFence(app, cmd, nil); err == nil {
		t.Fatal("manual drain bypassed the fenced session")
	}
}
