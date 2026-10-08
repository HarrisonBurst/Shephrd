package control

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/model"
)

func TestInteractiveWorkerTypedCheckpointRepair(t *testing.T) {
	for _, mode := range []string{"repair-decisions", "repair-checks", "repair-exhausted", "repair-duplicate"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			data := filepath.Join(root, "data")
			service, state, task, attempt := interactivePiFixture(t, root, data, mode, filepath.Join(root, "args"), "")
			defer state.Close()
			input := writeInput(t, data, task.ID, "brief.md", "prompt")
			err := service.RunAttempt(attempt.ID, input, false, nil, attempt.RunGeneration)
			failed := mode == "repair-exhausted" || mode == "repair-duplicate"
			if (err != nil) != failed {
				t.Fatalf("error=%v", err)
			}
			current, _ := state.Task(task.ID)
			messages, _ := state.Messages(task.ID)
			repairs, checkpoints, terminals := 0, 0, 0
			for _, message := range messages {
				if message.Type == "protocol-repair" {
					repairs++
				}
				if message.Direction != "worker-to-driver" || message.Stale {
					continue
				}
				if message.Type == "checkpoint" {
					checkpoints++
				}
				if message.Type == "question" {
					terminals++
				}
			}
			if repairs != 1 || current.ArtifactRef != "" || current.ProcessAlive {
				t.Fatalf("state=%+v messages=%+v", current, messages)
			}
			if failed {
				if current.Status != model.TaskStatusBlocked || checkpoints != 1 || terminals != 0 {
					t.Fatalf("partial correction: %+v %+v", current, messages)
				}
			} else if current.Status != model.TaskStatusWaiting || checkpoints != 2 || terminals != 1 {
				t.Fatalf("correction not accepted: %+v %+v", current, messages)
			}
		})
	}
}

func TestHeadlessWorkerTypedCheckpointRepair(t *testing.T) {
	for _, mode := range []string{"decisions", "checks", "exhausted"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			data := filepath.Join(root, "data")
			report := filepath.Join(data, "report.md")
			cp := headlessClaudeEnvelope(t, model.Event{Type: "checkpoint", Payload: "reported", Checkpoint: &model.Checkpoint{
				SchemaVersion: 1, Summary: "reported", NextSteps: []string{},
				Decisions: []model.Decision{{Decision: "Retain work", Reason: "Already complete"}},
				Checks:    []model.Check{{Command: "inspect", Result: "complete"}},
			}})
			done := headlessClaudeEnvelope(t, model.Event{Type: "done", Payload: "complete", Artifact: "report:" + report})
			bad := strings.Replace(cp, `[{"decision":"Retain work","reason":"Already complete"}]`, `["Retain work"]`, 1)
			if mode == "checks" {
				bad = strings.Replace(cp, `[{"command":"inspect","result":"complete"}]`, `["inspect passed"]`, 1)
			}
			initial := filepath.Join(root, "initial.jsonl")
			correction := filepath.Join(root, "correction.jsonl")
			actions := filepath.Join(root, "actions")
			writeHeadlessClaudeFile(t, initial, claudeStream(t, "claude-session", bad+done))
			corrected := cp + done
			if mode == "exhausted" {
				corrected = cp + strings.Replace(done, `"type":`, `"extra":true,"type":`, 1)
			}
			writeHeadlessClaudeFile(t, correction, claudeStream(t, "claude-session", corrected))
			writeHeadlessClaudeFile(t, report, []byte("Already written report\n"))
			script := "#!/bin/sh\ncase \" $* \" in *\" --resume \"*) echo repair >> \"$SHEPHRD_TEST_ACTIONS\"; cat \"$SHEPHRD_TEST_CORRECTION\" ;; *) echo work >> \"$SHEPHRD_TEST_ACTIONS\"; cat \"$SHEPHRD_TEST_INITIAL\" ;; esac\n"
			service, state, task, attempt, input := headlessClaudeControlFixture(t, root, data, script)
			defer state.Close()
			t.Setenv("SHEPHRD_TEST_INITIAL", initial)
			t.Setenv("SHEPHRD_TEST_CORRECTION", correction)
			t.Setenv("SHEPHRD_TEST_ACTIONS", actions)
			if err := service.RunAttempt(attempt.ID, input, false, nil, attempt.RunGeneration); err != nil {
				t.Fatal(err)
			}
			messages, _ := state.Messages(task.ID)
			current, _ := state.Task(task.ID)
			repairs, accepted := 0, 0
			for _, message := range messages {
				if message.Type == "protocol-repair" {
					repairs++
				}
				if message.Direction == "worker-to-driver" && (message.Type == "checkpoint" || message.Type == "done") {
					accepted++
				}
			}
			calls, _ := os.ReadFile(actions)
			if repairs != 1 || bytes.Count(calls, []byte("work")) != 1 || bytes.Count(calls, []byte("repair")) != 1 {
				t.Fatalf("calls=%q repairs=%d", calls, repairs)
			}
			if mode == "exhausted" {
				if accepted != 0 || current.Status != model.TaskStatusBlocked || current.ArtifactRef != "" {
					t.Fatalf("partial results: %+v %+v", current, messages)
				}
			} else if accepted != 2 || current.Status != model.TaskStatusDone || current.ArtifactRef != "report:"+report {
				t.Fatalf("worker contract changed: %+v %+v", current, messages)
			}
		})
	}
}
