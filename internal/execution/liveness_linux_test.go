package execution

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"shephrd/internal/coord"
	"shephrd/internal/proc"
)

func TestReconcileRecordedExitWithZombieOnlyGroup(t *testing.T) {
	cmd := exec.Command("sh", "-c", "read line")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Wait() })
	id, err := proc.Of(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	input.Close()
	deadline := time.Now().Add(5 * time.Second)
	for id.Probe() != "exited" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if id.Probe() != "exited" || syscall.Kill(-id.PID, 0) != nil {
		t.Fatalf("supervisor %d did not become an unreaped zombie", id.PID)
	}

	db, x, _, run := livenessFixture(t, nil, true, "running")
	if _, err := db.Exec(`UPDATE runs SET pid = ?, start_time = ? WHERE id = ?`, id.PID, id.Start, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := writeExitRecord(run.Dir, ExitRecord{Status: 0}); err != nil {
		t.Fatal(err)
	}
	x.Host = func(string) (Host, error) { return &Local{Env: HostEnv{Host: "workhorse"}}, nil }
	out, err := x.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Runs) != 1 || out.Runs[0]["liveness"] != "exited" {
		t.Fatalf("reconciliation: %+v", out)
	}
	current, err := coord.LoadRun(db, run.ID)
	if err != nil || current.Liveness != "exited" || current.ExitStatus == nil || *current.ExitStatus != 0 {
		t.Fatalf("run: %+v, %v", current, err)
	}
}
