package control

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"shephrd/internal/model"
)

func TestBusySendDoesNotMutateWorker(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		name := "working"
		if waiting {
			name = "waiting-with-live-runner"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newNativeFixture(t)
			task := fixture.createTask(t, "busy")
			attempt := fixture.spawn(t, task.ID)
			if err := fixture.state.SetRunnerForRun(attempt.ID, attempt.RunGeneration, os.Getpid(), true); err != nil {
				t.Fatal(err)
			}
			if waiting {
				fixture.reachWaiting(t, attempt, model.WorkspaceFacts{})
			}
			beforeTask, err := fixture.state.Task(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeAttempt, err := fixture.state.Attempt(attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeMessages, err := fixture.state.Messages(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := fixture.service.Send(task.ID, "Hold all remote writes")
			if err == nil || !reflect.DeepEqual(receipt, model.Attempt{}) {
				t.Fatalf("busy send returned success: %+v %v", receipt, err)
			}
			for _, want := range []string{"worker is busy", "not queued or delivered", "no hold was applied"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("busy feedback missing %q: %v", want, err)
				}
			}
			afterTask, err := fixture.state.Task(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			afterAttempt, err := fixture.state.Attempt(attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			afterMessages, err := fixture.state.Messages(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeTask, afterTask) || !reflect.DeepEqual(beforeAttempt, afterAttempt) || !reflect.DeepEqual(beforeMessages, afterMessages) {
				t.Fatal("busy send changed task, attempt, run generation or messages")
			}
			files, err := filepath.Glob(filepath.Join(fixture.service.Config.DataDir, task.ID, "follow-up-*.md"))
			if err != nil || len(files) != 0 {
				t.Fatalf("busy send wrote follow-up files: %v %v", files, err)
			}
		})
	}
}
