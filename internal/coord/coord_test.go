package coord

import (
	"context"
	"path/filepath"
	"testing"

	"shephrd/internal/config"
	"shephrd/internal/fault"
	"shephrd/internal/store"
)

type fixture struct {
	t   *testing.T
	db  *store.Store
	cfg *config.Config
}

var main = Caller{Kind: "driver", Name: "main"}

func newFixture(t *testing.T) *fixture {
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "shephrd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &fixture{t: t, db: db, cfg: &config.Config{Host: "workhorse", MaxDepth: 3}}
	for _, name := range []string{"api", "web"} {
		f.write(func(tx *store.Tx) error {
			_, err := AddRepo(tx, "operator", Repo{Host: "workhorse", Path: "/src/" + name, Name: name, DefaultBranch: "main"})
			return err
		})
	}
	return f
}

func (f *fixture) write(fn func(*store.Tx) error) error {
	return f.db.Write(context.Background(), fn)
}

func (f *fixture) create(c Caller, spec NewTask) (*Task, error) {
	var t *Task
	err := f.write(func(tx *store.Tx) error {
		p, err := Propose(tx, f.cfg, c, spec)
		if err != nil {
			return err
		}
		t, err = Create(tx, c, p, config.Target{Host: "workhorse", Harness: "codex"})
		return err
	})
	return t, err
}

func (f *fixture) must(t *Task, err error) *Task {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
	return t
}

func kind(err error) string {
	if err == nil {
		return ""
	}
	return fault.As(err).Kind
}

func run(t *Task) Caller {
	return Caller{Kind: "run", Task: t.ID, Attempt: 1, Run: 1}
}

func TestSubDriverRunsCreateChildrenForTheirRequest(t *testing.T) {
	f := newFixture(t)
	sub := f.must(f.create(main, NewTask{Role: "driver", Repo: "api", Objective: "Own api"}))
	child := f.must(f.create(run(sub), NewTask{Repo: "api", Objective: "Implement"}))
	if child.Parent != sub.ID || child.Depth != 1 || child.Request == 0 || child.Root != sub.ID {
		t.Fatalf("child: %+v", child)
	}
	if !Owns(run(sub), child) || Owns(main, child) {
		t.Fatal("ownership is not fixed by the tree")
	}
	if _, err := f.create(run(child), NewTask{Repo: "api", Objective: "Nested"}); kind(err) != "not_a_driver" {
		t.Fatalf("worker creating: %v", err)
	}
	if _, err := f.create(run(sub), NewTask{Repo: "web", Objective: "Elsewhere"}); kind(err) != "not_granted" {
		t.Fatalf("other repository: %v", err)
	}
}

func TestRequestsMustBeNamedWhenSeveralAreOpen(t *testing.T) {
	f := newFixture(t)
	sub := f.must(f.create(main, NewTask{Role: "driver", Repo: "api", Objective: "First"}))
	var second int64
	f.write(func(tx *store.Tx) error {
		var err error
		second, err = Send(tx, main, sub, "Second", 0)
		return err
	})
	if _, err := f.create(run(sub), NewTask{Repo: "api", Objective: "Which?"}); kind(err) != "request_required" {
		t.Fatalf("ambiguous request: %v", err)
	}
	child := f.must(f.create(run(sub), NewTask{Repo: "api", Objective: "For the second", Request: second}))
	if child.Request != second {
		t.Fatalf("request: %d, want %d", child.Request, second)
	}
	if _, err := f.create(run(sub), NewTask{Repo: "api", Objective: "Bogus", Request: 999}); kind(err) != "invalid_request" {
		t.Fatalf("unknown request: %v", err)
	}
}

func TestDepthIsBounded(t *testing.T) {
	f := newFixture(t)
	f.cfg.MaxDepth = 1
	sub := f.must(f.create(main, NewTask{Role: "driver", Repo: "api", Objective: "Own api"}))
	nested := f.must(f.create(run(sub), NewTask{Role: "driver", Repo: "api", Objective: "Nested"}))
	if _, err := f.create(run(nested), NewTask{Repo: "api", Objective: "Too deep"}); kind(err) != "depth_exceeded" {
		t.Fatalf("depth: %v", err)
	}
}

func TestReadingFollowsTheTree(t *testing.T) {
	f := newFixture(t)
	sub := f.must(f.create(main, NewTask{Role: "driver", Repo: "api", Objective: "Own api"}))
	worker := f.must(f.create(run(sub), NewTask{Repo: "api", Objective: "Work"}))
	other := f.must(f.create(Caller{Kind: "driver", Name: "other"}, NewTask{Repo: "api", Objective: "Other"}))
	cases := []struct {
		caller Caller
		task   *Task
		want   bool
	}{
		{main, worker, true},
		{main, other, false},
		{run(sub), worker, true},
		{run(worker), sub, false},
		{run(sub), other, false},
		{Caller{Kind: "plugin", Name: "linear"}, other, true},
		{Caller{Kind: "driver", Name: "main", Operator: true}, other, true},
	}
	for _, tc := range cases {
		got, err := CanRead(f.db, tc.caller, tc.task)
		if err != nil || got != tc.want {
			t.Errorf("%s reading %s: %v, %v", tc.caller, tc.task.Ref, got, err)
		}
	}
	if _, err := GetOwned(f.db, main, worker.Ref); kind(err) != "not_owner" {
		t.Fatalf("main acting on a grandchild: %v", err)
	}
	if _, err := Get(f.db, main, other.Ref); kind(err) != "not_found" {
		t.Fatalf("reading another driver's tree: %v", err)
	}
}

func TestDependenciesStayBetweenSiblings(t *testing.T) {
	f := newFixture(t)
	sub := f.must(f.create(main, NewTask{Role: "driver", Repo: "api", Objective: "Own api"}))
	first := f.must(f.create(run(sub), NewTask{Repo: "api", Objective: "First"}))
	second := f.must(f.create(run(sub), NewTask{Repo: "api", Objective: "Second", After: []string{first.Ref}, Until: "merged"}))
	deps, ready, err := Dependencies(f.db, second.ID)
	if err != nil || ready || len(deps) != 1 || deps[0].Until != "merged" {
		t.Fatalf("dependencies: %+v, %v, %v", deps, ready, err)
	}
	if _, err := f.create(main, NewTask{Repo: "api", Objective: "Cross", After: []string{first.Ref}}); kind(err) != "invalid_dependency" {
		t.Fatalf("cross-owner dependency: %v", err)
	}
	f.write(func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE tasks SET milestone = 'published' WHERE id = ?`, first.ID)
		return err
	})
	if _, ready, _ := Dependencies(f.db, second.ID); ready {
		t.Fatal("published satisfied an --until merged dependency")
	}
	f.write(func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE tasks SET milestone = 'merged' WHERE id = ?`, first.ID)
		return err
	})
	if _, ready, _ := Dependencies(f.db, second.ID); !ready {
		t.Fatal("merged did not satisfy the dependency")
	}
}

func TestClosingNeverOrphansChildren(t *testing.T) {
	f := newFixture(t)
	sub := f.must(f.create(main, NewTask{Role: "driver", Repo: "api", Objective: "Own api"}))
	f.must(f.create(run(sub), NewTask{Repo: "api", Objective: "Work"}))
	err := f.write(func(tx *store.Tx) error {
		return Cancel(tx, main, sub)
	})
	if kind(err) != "open_children" {
		t.Fatalf("cancel with open children: %v", err)
	}
}

func TestUnknownLivenessRefusesEveryReplacement(t *testing.T) {
	f := newFixture(t)
	task := f.must(f.create(main, NewTask{Repo: "api", Objective: "Work"}))
	f.write(func(tx *store.Tx) error {
		a, err := AllocateAttempt(tx, task, task.Target, func(int64) (string, string) { return "/w", "b" }, 0)
		if err != nil {
			return err
		}
		if err := AdoptAttempt(tx, "test", task, a); err != nil {
			return err
		}
		r, _, err := BeginRun(tx, "test", task, "start", func(int64) string { return "/r" })
		if err != nil {
			return err
		}
		if err := SetLiveness(tx, r, "unknown"); err != nil {
			return err
		}
		return Hold(tx, task, "lost")
	})
	task, _ = Load(f.db, task.ID)
	for _, purpose := range []string{"resume", "retry"} {
		if err := CheckRunnable(f.db, task, purpose); kind(err) != "liveness_unknown" {
			t.Fatalf("%s with unknown liveness: %v", purpose, err)
		}
	}
}

func TestClosingATaskAcknowledgesItsInboxItems(t *testing.T) {
	f := newFixture(t)
	task := f.must(f.create(main, NewTask{Repo: "api", Objective: "Work"}))
	f.write(func(tx *store.Tx) error {
		seq, err := Note(tx, main, task, "needs a look")
		if err != nil {
			return err
		}
		return AddItem(tx, main.String(), task, seq)
	})
	if items, _ := Items(f.db, `i.state = 'pending'`); len(items) != 1 {
		t.Fatalf("pending items: %v", items)
	}
	if err := f.write(func(tx *store.Tx) error { return Cancel(tx, main, task) }); err != nil {
		t.Fatal(err)
	}
	items, _ := Items(f.db, `1 = 1`)
	if len(items) != 1 || items[0].State != "acked" || items[0].AckedBy != "system" {
		t.Fatalf("after close: %+v", items)
	}
}

func TestStateTransitionsFollowTheLifecycle(t *testing.T) {
	f := newFixture(t)
	task := f.must(f.create(main, NewTask{Repo: "api", Objective: "Work"}))
	err := f.write(func(tx *store.Tx) error { return SetState(tx, "test", task, "done", "") })
	if kind(err) != "invalid_state" {
		t.Fatalf("queued to done: %v", err)
	}
	for _, step := range []string{"running", "waiting", "running", "done", "closed"} {
		if err := f.write(func(tx *store.Tx) error { return SetState(tx, "test", task, step, "") }); err != nil {
			t.Fatalf("to %s: %v", step, err)
		}
	}
	if reloaded, _ := Load(f.db, task.ID); reloaded.State != "closed" || reloaded.Revision != task.Revision {
		t.Fatalf("reloaded: %+v", reloaded)
	}
}
