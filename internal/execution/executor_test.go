package execution

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/store"
)

func TestBriefDeliversUnreadMidRunMessageAfterResult(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "shephrd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var task *coord.Task
	var attempt *coord.Attempt
	var continuation *coord.Run
	err = db.Write(ctx, func(tx *store.Tx) error {
		if _, err := coord.AddRepo(tx, "operator", coord.Repo{Name: "api", Host: "workhorse", Path: "/src/api", DefaultBranch: "main"}); err != nil {
			return err
		}
		proposal, err := coord.Propose(tx, &config.Config{Host: "workhorse", MaxDepth: 3}, coord.Caller{Kind: "driver", Name: "main"}, coord.NewTask{Repo: "api", Objective: "Build"})
		if err != nil {
			return err
		}
		task, err = coord.Create(tx, coord.Caller{Kind: "driver", Name: "main"}, proposal, config.Target{Host: "workhorse", Harness: "codex"})
		if err != nil {
			return err
		}
		attempt, err = coord.AllocateAttempt(tx, task, task.Target, func(int64) (string, string) { return t.TempDir(), "shephrd/t_1/1" }, 0)
		if err != nil {
			return err
		}
		if err := coord.SetWorkspaceState(tx, "system", attempt, "held", "base"); err != nil {
			return err
		}
		if err := coord.AdoptAttempt(tx, "system", task, attempt); err != nil {
			return err
		}
		first, _, err := coord.BeginRun(tx, "system", task, "start", func(int64) string { return t.TempDir() })
		if err != nil {
			return err
		}
		if _, err := coord.Send(tx, coord.Caller{Kind: "driver", Name: "main"}, task, "Update after first brief", 0); err != nil {
			return err
		}
		caller := coord.Caller{Kind: "run", Task: task.ID, Attempt: 1, Run: first.ID}
		if _, err := coord.RecordReport(tx, caller, task, &coord.Report{Kind: "result", Body: "Initial result"}); err != nil {
			return err
		}
		if err := coord.RecordExit(tx, first, 0, "session"); err != nil {
			return err
		}
		continuation, _, err = coord.BeginRun(tx, "system", task, "continue", func(int64) string { return t.TempDir() })
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	x := &Executor{DB: db, Guide: func(string) (string, error) { return "worker guidance", nil }}
	brief, err := x.brief(ctx, "shephrd", task, attempt, continuation)
	if err != nil || !strings.Contains(brief, "task.message on "+task.Ref+" from driver:main: Update after first brief") {
		t.Fatalf("continuation brief omitted unread message: %v\n%s", err, brief)
	}
}

func TestTabLabelUsesTitleForDuplicateTasks(t *testing.T) {
	first := &coord.Task{Ref: "t_1", Role: "worker", Title: "Same title"}
	second := &coord.Task{Ref: "t_2", Role: "driver", Title: "Same title"}
	if got := TabLabel(first); got != "Same title" || TabLabel(second) != got {
		t.Fatalf("duplicate title labels: %q, %q", got, TabLabel(second))
	}
}

func TestTabLabelFallsBackToTaskIDForEmptyTitle(t *testing.T) {
	for _, title := range []string{"", "   "} {
		if got := TabLabel(&coord.Task{Ref: "t_3", Title: title}); got != "t_3" {
			t.Fatalf("title %q label: %q", title, got)
		}
	}
}
