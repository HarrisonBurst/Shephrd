package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shephrd/internal/model"
)

func TestPiWorkerTransportCompletion(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node fixture runtime unavailable for the Pi bridge transport fixture")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 fixture runtime unavailable")
	}
	grace := interactivePiShutdownGrace
	interactivePiShutdownGrace = 100 * time.Millisecond
	t.Cleanup(func() { interactivePiShutdownGrace = grace })
	fixture, err := filepath.Abs("../pibridge/testdata/transport-harness.mjs")
	if err != nil {
		t.Fatal(err)
	}
	for _, runtime := range []string{"headless", "herdr"} {
		for _, mode := range []string{"transport-empty", "transport-partial", "transport-complete", "transport-exhausted", "transport-cancel", "transport-abort", "transport-auth", "transport-schema", "transport-normal", "repair-transport", "repair-transport-exhausted", "repair-transport-invalid"} {
			t.Run(runtime+"/"+mode, func(t *testing.T) {
				root := t.TempDir()
				data := filepath.Join(root, "data")
				service, state, task, attempt := interactivePiFixture(t, root, data, "", "", "")
				defer state.Close()
				if err := state.SetRuntimeBackend(attempt.ID, runtime); err != nil {
					t.Fatal(err)
				}
				report := filepath.Join(data, task.ID, "report.md")
				checkpoint := headlessEventEnvelope(t, model.Event{Type: "checkpoint", Payload: "recovered", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "recovered", Completed: []string{"report written once"}, NextSteps: []string{}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}})
				done := headlessEventEnvelope(t, model.Event{Type: "done", Payload: "recovered", Artifact: "report:" + report})
				corrected := checkpoint + "\n" + done
				envelopes := corrected
				if strings.HasPrefix(mode, "repair-") {
					envelopes = strings.ReplaceAll(corrected, `"decisions":[]`, `"decisions":["incorrect"]`)
				}
				if mode == "repair-transport-invalid" {
					corrected = strings.ReplaceAll(corrected, `"checks":[]`, `"checks":["still wrong"]`)
				}
				input, err := json.Marshal(map[string]any{"session": attempt.SessionID, "mode": mode, "headless": runtime == "headless", "envelopes": envelopes, "corrected": corrected})
				if err != nil {
					t.Fatal(err)
				}
				config := filepath.Join(root, "fixture.json")
				if err := os.WriteFile(config, input, 0600); err != nil {
					t.Fatal(err)
				}
				script := fmt.Sprintf(`#!%s
import sys,json,subprocess,os
args=sys.argv[1:]
payload=json.load(open(%q))
correction='--session' in args
payload['correction']=correction
if correction:
 assert 'correction attempt 1 of 1' in args[-1]
else:
 with open(%q,'a') as f:f.write('effect\n')
 os.makedirs(os.path.dirname(%q),exist_ok=True)
 with open(%q,'w') as f:f.write('report written once\n')
bridge=args[args.index('--extension')+1] if '--extension' in args else ''
p=subprocess.run([%q,%q,bridge],input=json.dumps(payload),text=True)
sys.exit(p.returncode)
`, python, config, filepath.Join(root, "effects"), report, report, node, fixture)
				if err := os.WriteFile(filepath.Join(root, "bin", "pi"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				inputPath := writeInput(t, data, task.ID, "brief.md", "Write the fixture report exactly once")
				var output bytes.Buffer
				runErr := service.RunAttempt(attempt.ID, inputPath, false, &output, attempt.RunGeneration)
				current, _ := state.Task(task.ID)
				stored, _ := state.Attempt(attempt.ID)
				messages, _ := state.Messages(task.ID)
				success := mode == "transport-empty" || mode == "transport-partial" || mode == "transport-complete" || mode == "transport-normal" || mode == "repair-transport"
				want := model.TaskStatusBlocked
				if success {
					want = model.TaskStatusDone
				}
				if current.Status != want || current.ProcessAlive || success && (runErr != nil || strings.Contains(stored.FailureReason, "WebSocket") || strings.Contains(stored.FailureReason, "protocol correction")) {
					t.Fatalf("run=%v task=%+v attempt=%+v output=%s", runErr, current, stored, output.String())
				}
				accepted, repairs, progress := 0, 0, 0
				for _, message := range messages {
					if message.Type == "progress" && message.Payload == "Completed fixture action" {
						progress++
					}
					if message.Type == "protocol-repair" {
						repairs++
					}
					if message.Direction == "worker-to-driver" && !message.Stale && (message.Type == "checkpoint" || message.Type == "done") {
						accepted++
						if strings.Contains(message.Payload, "FAILED TRANSPORT") {
							t.Fatalf("failed text persisted: %+v", message)
						}
					}
				}
				if accepted != 2*boolInt(success) || repairs != boolInt(strings.HasPrefix(mode, "repair-")) || progress != boolInt(!strings.HasPrefix(mode, "repair-")) {
					t.Fatalf("accepted=%d repairs=%d progress=%d messages=%+v", accepted, repairs, progress, messages)
				}
				effects, err := os.ReadFile(filepath.Join(root, "effects"))
				if err != nil || string(effects) != "effect\n" {
					t.Fatalf("effects=%q err=%v", effects, err)
				}
				body, err := os.ReadFile(report)
				if err != nil || string(body) != "report written once\n" {
					t.Fatalf("report lost: %q %v", body, err)
				}
			})
		}
	}
}
