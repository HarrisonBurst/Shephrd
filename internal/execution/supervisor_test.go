package execution

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestSupervisorRecordsExitWhenStartRegistrationFails(t *testing.T) {
	dir := t.TempDir()
	shephrd := filepath.Join(dir, "shephrd")
	if err := os.WriteFile(shephrd, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := RunSpec{Shephrd: shephrd, Path: os.Getenv("PATH"), Home: dir, Config: filepath.Join(dir, "config"), Command: []string{"true"}, Workspace: dir}
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHEPHRD_RUN_TOKEN", "token")
	if status := Supervise(dir); status == 0 {
		t.Fatal("supervisor succeeded after start registration failed")
	}
	if _, ok := ReadExit(dir); !ok {
		t.Fatal("supervisor did not write an exit record")
	}
}

func TestTurnStatusRetriesTransientError(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	shephrd := filepath.Join(dir, "shephrd")
	script := "#!/bin/sh\ncount=0\nif [ -f \"$SHEPHRD_TEST_COUNT\" ]; then count=$(cat \"$SHEPHRD_TEST_COUNT\"); fi\ncount=$((count + 1))\nprintf '%s' \"$count\" > \"$SHEPHRD_TEST_COUNT\"\nif [ \"$count\" = 1 ]; then exit 1; fi\nprintf '{\"ended\":true}\\n'\n"
	if err := os.WriteFile(shephrd, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := RunSpec{Shephrd: shephrd, Path: os.Getenv("PATH"), Home: dir, Config: filepath.Join(dir, "config"), Account: []string{"SHEPHRD_TEST_COUNT=" + count}}
	ended, err := spec.turnEnded("token")
	if err != nil || !ended {
		t.Fatalf("turn status: ended=%v err=%v", ended, err)
	}
	body, err := os.ReadFile(count)
	if err != nil {
		t.Fatal(err)
	}
	calls, err := strconv.Atoi(string(body))
	if err != nil || calls != 2 {
		t.Fatalf("turn calls: %q, %v", body, err)
	}
}
