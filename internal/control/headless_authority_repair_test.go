package control

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"shephrd/internal/adapter"
	"shephrd/internal/config"
	"shephrd/internal/model"
	workerprocess "shephrd/internal/process"
	"shephrd/internal/store"
)

func TestHeadlessClaudeNestedIncidentRecordHasNoWorkerEventAuthority(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	prefixPath := filepath.Join(root, "prefix.jsonl")
	suffixPath := filepath.Join(root, "suffix.jsonl")
	pausedPath := filepath.Join(root, "paused")
	releasePath := filepath.Join(root, "release")
	reportPath := filepath.Join(dataDir, "report.md")
	var prefix bytes.Buffer
	writeClaudeRecord(t, &prefix, map[string]any{"type": "system", "subtype": "init", "session_id": "claude-session"})
	for index := 0; index < 711; index++ {
		writeClaudeRecord(t, &prefix, map[string]any{"type": "rate_limit_event", "index": index})
	}
	nestedText := "The task description names the backticked bare `<shephrd-event>` marker but does not emit an envelope."
	writeClaudeRecord(t, &prefix, claudeAssistantRecord("claude-session", "toolu_parent", nestedText, map[string]any{"subagent_type": "general-purpose"}))
	checkpoint := headlessClaudeEnvelope(t, model.Event{Type: "checkpoint", Payload: "authoritative checkpoint", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "authoritative checkpoint", Completed: []string{"report written"}, NextSteps: []string{}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}})
	done := headlessClaudeEnvelope(t, model.Event{Type: "done", Payload: "authoritative done", Artifact: "report:" + reportPath})
	var suffix bytes.Buffer
	writeClaudeRecord(t, &suffix, claudeAssistantRecord("claude-session", nil, checkpoint+"\n"+done, nil))
	writeClaudeRecord(t, &suffix, map[string]any{"type": "result", "subtype": "success", "session_id": "claude-session", "is_error": false})
	writeHeadlessClaudeFile(t, prefixPath, prefix.Bytes())
	writeHeadlessClaudeFile(t, suffixPath, suffix.Bytes())
	writeHeadlessClaudeFile(t, reportPath, bytes.Repeat([]byte("R"), 48*1024))
	script := `#!/bin/sh
set -eu
cat "$SHEPHRD_TEST_PREFIX"
: > "$SHEPHRD_TEST_PAUSED"
while [ ! -e "$SHEPHRD_TEST_RELEASE" ]; do sleep 0.01; done
cat "$SHEPHRD_TEST_SUFFIX"
`
	service, state, task, attempt, inputPath := headlessClaudeControlFixture(t, root, dataDir, script)
	defer state.Close()
	t.Setenv("SHEPHRD_TEST_PREFIX", prefixPath)
	t.Setenv("SHEPHRD_TEST_SUFFIX", suffixPath)
	t.Setenv("SHEPHRD_TEST_PAUSED", pausedPath)
	t.Setenv("SHEPHRD_TEST_RELEASE", releasePath)
	runDone := make(chan error, 1)
	go func() { runDone <- service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration) }()
	waitFor(t, func() bool {
		_, err := os.Stat(pausedPath)
		return err == nil
	})
	current, err := state.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := state.Messages(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Type == "blocked" || message.Type == "protocol-repair" || message.Payload == nestedText {
			t.Fatalf("nested record gained protocol authority: %+v", messages)
		}
	}
	if current.Status != model.TaskStatusWorking || !current.ProcessAlive {
		t.Fatalf("task blocked while authoritative worker was still running: %+v", current)
	}
	if err := os.WriteFile(releasePath, []byte("continue"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	current, _ = state.Task(task.ID)
	stored, _ := state.Attempt(attempt.ID)
	messages, _ = state.Messages(task.ID)
	checkpointCursor, doneCursor, doneCount := int64(0), int64(0), 0
	for _, message := range messages {
		if message.Direction == "worker-to-driver" && !message.Stale {
			switch message.Type {
			case "checkpoint":
				checkpointCursor = message.SourceCursor
			case "done":
				doneCursor = message.SourceCursor
				doneCount++
			}
		}
	}
	if current.Status != model.TaskStatusDone || current.ProcessAlive || checkpointCursor != 714 || doneCursor != 715 || doneCount != 1 || stored.Cursor != 716 {
		t.Fatalf("task=%+v attempt=%+v checkpoint=%d done=%d count=%d messages=%+v", current, stored, checkpointCursor, doneCursor, doneCount, messages)
	}
}

func TestHeadlessTopLevelBareTagProseIsProgressNotFraming(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	streamPath := filepath.Join(root, "stream.jsonl")
	reportPath := filepath.Join(dataDir, "report.md")
	prose := "Markdown can name a bare `<shephrd-event>` opening tag without proposing an event."
	checkpoint := headlessClaudeEnvelope(t, model.Event{Type: "checkpoint", Payload: "ready", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "ready", Completed: []string{"report written"}, NextSteps: []string{}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}})
	done := headlessClaudeEnvelope(t, model.Event{Type: "done", Payload: "done", Artifact: "report:" + reportPath})
	var stream bytes.Buffer
	writeClaudeRecord(t, &stream, map[string]any{"type": "system", "subtype": "init", "session_id": "claude-session"})
	writeClaudeRecord(t, &stream, claudeAssistantRecord("claude-session", nil, prose, nil))
	writeClaudeRecord(t, &stream, claudeAssistantRecord("claude-session", nil, checkpoint+done, nil))
	writeClaudeRecord(t, &stream, map[string]any{"type": "result", "subtype": "success", "session_id": "claude-session", "is_error": false})
	writeHeadlessClaudeFile(t, streamPath, stream.Bytes())
	writeHeadlessClaudeFile(t, reportPath, bytes.Repeat([]byte("R"), 48*1024))
	service, state, task, attempt, inputPath := headlessClaudeControlFixture(t, root, dataDir, "#!/bin/sh\ncat \"$SHEPHRD_TEST_STREAM\"\n")
	defer state.Close()
	t.Setenv("SHEPHRD_TEST_STREAM", streamPath)
	if err := service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration); err != nil {
		t.Fatal(err)
	}
	messages, _ := state.Messages(task.ID)
	progress, blocked := 0, 0
	for _, message := range messages {
		progress += boolInt(message.Type == "progress" && message.Payload == prose && !message.Stale)
		blocked += boolInt(message.Type == "blocked" && !message.Stale)
	}
	current, _ := state.Task(task.ID)
	if current.Status != model.TaskStatusDone || progress != 1 || blocked != 0 {
		t.Fatalf("task=%+v progress=%d blocked=%d messages=%+v", current, progress, blocked, messages)
	}
}

func TestHeadlessClaudeRepairsOneRejectedCandidateInSameSession(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	initialPath := filepath.Join(root, "initial.jsonl")
	correctionPath := filepath.Join(root, "correction.jsonl")
	argsPath := filepath.Join(root, "args")
	reportPath := filepath.Join(dataDir, "report.md")
	malformed := `<shephrd-event>{"type":"done","payload":"complete","artifact":"report:` + reportPath + `"}`
	checkpoint := headlessClaudeEnvelope(t, model.Event{Type: "checkpoint", Payload: "corrected checkpoint", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "corrected checkpoint", Completed: []string{"report written"}, NextSteps: []string{}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}})
	done := headlessClaudeEnvelope(t, model.Event{Type: "done", Payload: "corrected done", Artifact: "report:" + reportPath})
	writeHeadlessClaudeFile(t, initialPath, claudeStream(t, "claude-session", malformed))
	writeHeadlessClaudeFile(t, correctionPath, claudeStream(t, "claude-session", checkpoint+done))
	writeHeadlessClaudeFile(t, reportPath, bytes.Repeat([]byte("R"), 48*1024))
	script := `#!/bin/sh
set -eu
printf '%s\n' "$@" >> "$SHEPHRD_TEST_ARGS"
case " $* " in
  *" --resume "*) cat "$SHEPHRD_TEST_CORRECTION" ;;
  *) cat "$SHEPHRD_TEST_INITIAL" ;;
esac
`
	service, state, task, attempt, inputPath := headlessClaudeControlFixture(t, root, dataDir, script)
	defer state.Close()
	t.Setenv("SHEPHRD_TEST_INITIAL", initialPath)
	t.Setenv("SHEPHRD_TEST_CORRECTION", correctionPath)
	t.Setenv("SHEPHRD_TEST_ARGS", argsPath)
	if err := service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration); err != nil {
		t.Fatal(err)
	}
	current, _ := state.Task(task.ID)
	stored, _ := state.Attempt(attempt.ID)
	messages, _ := state.Messages(task.ID)
	repairs, checkpoints, doneCount, rejectedParts, blocked := 0, 0, 0, 0, 0
	digest := sha256.Sum256([]byte(malformed))
	for _, message := range messages {
		switch {
		case message.Type == "protocol-repair":
			repairs++
			if message.SourceCursor != 2 || !strings.Contains(message.Payload, adapter.DiagnosticFramingMissingClose) || !strings.Contains(message.Payload, hex.EncodeToString(digest[:])) {
				t.Fatalf("repair=%+v", message)
			}
		case message.Direction == "worker-to-driver" && message.Type == "checkpoint" && !message.Stale:
			checkpoints++
		case message.Type == "done" && !message.Stale:
			doneCount++
		case message.Direction == "worker-to-driver" && message.Stale:
			rejectedParts++
		case message.Type == "blocked" && !message.Stale:
			blocked++
		}
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"--resume", "SHEPHRD_PROTOCOL_REPAIR", adapter.DiagnosticFramingMissingClose, "exactly two envelopes", hex.EncodeToString(digest[:])} {
		if !strings.Contains(string(args), required) {
			t.Fatalf("repair prompt missing %q: %s", required, args)
		}
	}
	if current.Status != model.TaskStatusDone || current.ProcessAlive || stored.SessionID != "claude-session" || repairs != 1 || checkpoints != 1 || doneCount != 1 || rejectedParts != 0 || blocked != 0 {
		t.Fatalf("task=%+v attempt=%+v repairs=%d checkpoints=%d done=%d rejected=%d blocked=%d messages=%+v", current, stored, repairs, checkpoints, doneCount, rejectedParts, blocked, messages)
	}
}

func TestHeadlessClaudeAlignsCorrectionAfterMultiEnvelopeRepairCursor(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	initialPath := filepath.Join(root, "initial.jsonl")
	correctionPath := filepath.Join(root, "correction.jsonl")
	reportPath := filepath.Join(dataDir, "report.md")
	progress := headlessClaudeEnvelope(t, model.Event{Type: "progress", Payload: "candidate progress"})
	checkpoint := headlessClaudeEnvelope(t, model.Event{Type: "checkpoint", Payload: "candidate checkpoint", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "candidate checkpoint", Completed: []string{"candidate"}, NextSteps: []string{}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}})
	wrongDone := headlessClaudeEnvelope(t, model.Event{Type: "done", Payload: "wrong terminal", Artifact: "branch:wrong"})
	correctedCheckpoint := headlessClaudeEnvelope(t, model.Event{Type: "checkpoint", Payload: "corrected checkpoint", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "corrected checkpoint", Completed: []string{"report written"}, NextSteps: []string{}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}})
	correctedDone := headlessClaudeEnvelope(t, model.Event{Type: "done", Payload: "corrected done", Artifact: "report:" + reportPath})
	writeHeadlessClaudeFile(t, initialPath, claudeStream(t, "claude-session", progress+checkpoint+wrongDone))
	writeHeadlessClaudeFile(t, correctionPath, claudeStream(t, "claude-session", correctedCheckpoint+correctedDone))
	writeHeadlessClaudeFile(t, reportPath, bytes.Repeat([]byte("R"), 48*1024))
	script := "#!/bin/sh\ncase \" $* \" in *\" --resume \"*) cat \"$SHEPHRD_TEST_CORRECTION\" ;; *) cat \"$SHEPHRD_TEST_INITIAL\" ;; esac\n"
	service, state, task, attempt, inputPath := headlessClaudeControlFixture(t, root, dataDir, script)
	defer state.Close()
	t.Setenv("SHEPHRD_TEST_INITIAL", initialPath)
	t.Setenv("SHEPHRD_TEST_CORRECTION", correctionPath)
	if err := service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration); err != nil {
		t.Fatal(err)
	}
	current, _ := state.Task(task.ID)
	stored, _ := state.Attempt(attempt.ID)
	messages, _ := state.Messages(task.ID)
	repairs, candidateParts, correctedCheckpoints, correctedTerminals := 0, 0, 0, 0
	for _, message := range messages {
		switch {
		case message.Type == "protocol-repair":
			repairs++
			if message.SourceCursor != 4 {
				t.Fatalf("repair=%+v", message)
			}
		case message.Direction == "worker-to-driver" && (message.Payload == "candidate progress" || message.Payload == "candidate checkpoint" || message.Payload == "wrong terminal"):
			candidateParts++
		case message.Direction == "worker-to-driver" && message.Type == "checkpoint" && message.Payload == "corrected checkpoint" && message.SourceCursor == 6:
			correctedCheckpoints++
		case message.Type == "done" && message.Payload == "corrected done" && message.SourceCursor == 7 && !message.Stale:
			correctedTerminals++
		}
	}
	if current.Status != model.TaskStatusDone || current.ProcessAlive || stored.Cursor != 8 || repairs != 1 || candidateParts != 0 || correctedCheckpoints != 1 || correctedTerminals != 1 {
		t.Fatalf("task=%+v attempt=%+v repairs=%d candidate_parts=%d checkpoints=%d terminals=%d messages=%+v", current, stored, repairs, candidateParts, correctedCheckpoints, correctedTerminals, messages)
	}
}

func TestHeadlessClaudeAlignsCorrectionAfterEarlierRepairEnvelopeIndexes(t *testing.T) {
	for _, test := range []struct {
		name             string
		prefix           func(*testing.T) string
		repairCursor     int64
		checkpointCursor int64
		terminalCursor   int64
		finalCursor      int64
	}{
		{name: "index zero", prefix: func(*testing.T) string { return "" }, repairCursor: 2, checkpointCursor: 4, terminalCursor: 5, finalCursor: 6},
		{name: "index one", prefix: func(t *testing.T) string {
			return headlessClaudeEnvelope(t, model.Event{Type: "progress", Payload: "candidate progress"})
		}, repairCursor: 3, checkpointCursor: 5, terminalCursor: 6, finalCursor: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dataDir := filepath.Join(root, "data")
			initialPath := filepath.Join(root, "initial.jsonl")
			correctionPath := filepath.Join(root, "correction.jsonl")
			reportPath := filepath.Join(dataDir, "report.md")
			wrongDone := headlessClaudeEnvelope(t, model.Event{Type: "done", Payload: "wrong terminal", Artifact: "branch:wrong"})
			checkpoint := headlessClaudeEnvelope(t, model.Event{Type: "checkpoint", Payload: "corrected checkpoint", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "corrected checkpoint", Completed: []string{"report written"}, NextSteps: []string{}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}})
			done := headlessClaudeEnvelope(t, model.Event{Type: "done", Payload: "corrected done", Artifact: "report:" + reportPath})
			writeHeadlessClaudeFile(t, initialPath, claudeStream(t, "claude-session", test.prefix(t)+wrongDone))
			writeHeadlessClaudeFile(t, correctionPath, claudeStream(t, "claude-session", checkpoint+done))
			writeHeadlessClaudeFile(t, reportPath, bytes.Repeat([]byte("R"), 48*1024))
			script := "#!/bin/sh\ncase \" $* \" in *\" --resume \"*) cat \"$SHEPHRD_TEST_CORRECTION\" ;; *) cat \"$SHEPHRD_TEST_INITIAL\" ;; esac\n"
			service, state, task, attempt, inputPath := headlessClaudeControlFixture(t, root, dataDir, script)
			defer state.Close()
			t.Setenv("SHEPHRD_TEST_INITIAL", initialPath)
			t.Setenv("SHEPHRD_TEST_CORRECTION", correctionPath)
			if err := service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration); err != nil {
				t.Fatal(err)
			}
			current, _ := state.Task(task.ID)
			stored, _ := state.Attempt(attempt.ID)
			messages, _ := state.Messages(task.ID)
			repairs, checkpoints, terminals, candidateParts := 0, 0, 0, 0
			for _, message := range messages {
				if message.Type == "protocol-repair" && message.SourceCursor == test.repairCursor {
					repairs++
				}
				if message.Type == "checkpoint" && message.Payload == "corrected checkpoint" && message.SourceCursor == test.checkpointCursor && !message.Stale {
					checkpoints++
				}
				if message.Type == "done" && message.Payload == "corrected done" && message.SourceCursor == test.terminalCursor && !message.Stale {
					terminals++
				}
				if message.Direction == "worker-to-driver" && (message.Payload == "candidate progress" || message.Payload == "wrong terminal") {
					candidateParts++
				}
			}
			if current.Status != model.TaskStatusDone || stored.Cursor != test.finalCursor || repairs != 1 || checkpoints != 1 || terminals != 1 || candidateParts != 0 {
				t.Fatalf("task=%+v attempt=%+v repairs=%d checkpoints=%d terminals=%d candidate_parts=%d messages=%+v", current, stored, repairs, checkpoints, terminals, candidateParts, messages)
			}
		})
	}
}

func TestHeadlessCorrectionControlsFailClosedWithoutPartialIngestion(t *testing.T) {
	checkpoint := headlessClaudeEnvelope(t, model.Event{Type: "checkpoint", Payload: "repair", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "repair", Completed: []string{}, NextSteps: []string{}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}})
	done := `<shephrd-event>{"type":"done","payload":"done","artifact":"report:/tmp/report.md"}</shephrd-event>`
	failed := `<shephrd-event>{"type":"failed","payload":"duplicate"}</shephrd-event>`
	for _, test := range []struct {
		name       string
		correction func(*testing.T, string) []byte
	}{
		{name: "second invalid", correction: func(t *testing.T, session string) []byte {
			return claudeStream(t, session, `<shephrd-event>{"type":"done","payload":}`)
		}},
		{name: "duplicate terminal", correction: func(t *testing.T, session string) []byte { return claudeStream(t, session, checkpoint+done+failed) }},
		{name: "oversized", correction: func(t *testing.T, session string) []byte {
			return claudeStream(t, session, `<shephrd-event>{`+strings.Repeat("x", 100*1024)+`</shephrd-event>`)
		}},
		{name: "non authoritative", correction: func(t *testing.T, session string) []byte {
			var stream bytes.Buffer
			writeClaudeRecord(t, &stream, map[string]any{"type": "system", "subtype": "init", "session_id": session})
			writeClaudeRecord(t, &stream, claudeAssistantRecord(session, "toolu_child", checkpoint+done, map[string]any{"subagent_type": "general-purpose"}))
			writeClaudeRecord(t, &stream, map[string]any{"type": "result", "subtype": "success", "session_id": session, "is_error": false})
			return stream.Bytes()
		}},
		{name: "multiple authoritative records", correction: func(t *testing.T, session string) []byte {
			var stream bytes.Buffer
			writeClaudeRecord(t, &stream, map[string]any{"type": "system", "subtype": "init", "session_id": session})
			writeClaudeRecord(t, &stream, claudeAssistantRecord(session, nil, checkpoint+done, nil))
			writeClaudeRecord(t, &stream, claudeAssistantRecord(session, nil, "extra", nil))
			writeClaudeRecord(t, &stream, map[string]any{"type": "result", "subtype": "success", "session_id": session, "is_error": false})
			return stream.Bytes()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dataDir := filepath.Join(root, "data")
			initialPath := filepath.Join(root, "initial.jsonl")
			correctionPath := filepath.Join(root, "correction.jsonl")
			malformed := `<shephrd-event>{"type":"question","payload":"choose","question":{}}</shephrd-event>`
			writeHeadlessClaudeFile(t, initialPath, claudeStream(t, "claude-session", malformed))
			writeHeadlessClaudeFile(t, correctionPath, test.correction(t, "claude-session"))
			script := "#!/bin/sh\ncase \" $* \" in *\" --resume \"*) cat \"$SHEPHRD_TEST_CORRECTION\" ;; *) cat \"$SHEPHRD_TEST_INITIAL\" ;; esac\n"
			service, state, task, attempt, inputPath := headlessClaudeControlFixture(t, root, dataDir, script)
			defer state.Close()
			t.Setenv("SHEPHRD_TEST_INITIAL", initialPath)
			t.Setenv("SHEPHRD_TEST_CORRECTION", correctionPath)
			if err := service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration); err != nil {
				t.Fatal(err)
			}
			current, _ := state.Task(task.ID)
			messages, _ := state.Messages(task.ID)
			repairs, accepted, blocked := 0, 0, 0
			for _, message := range messages {
				repairs += boolInt(message.Type == "protocol-repair")
				accepted += boolInt(message.Direction == "worker-to-driver" && (message.Type == "checkpoint" || message.Type == "done" || message.Type == "failed"))
				blocked += boolInt(message.Type == "blocked" && !message.Stale)
			}
			if current.Status != model.TaskStatusBlocked || current.ProcessAlive || repairs != 1 || accepted != 0 || blocked != 1 {
				t.Fatalf("task=%+v repairs=%d accepted=%d blocked=%d messages=%+v", current, repairs, accepted, blocked, messages)
			}
		})
	}
}

func TestHeadlessMissingCorrectionTimesOutAndReapsBeforeBlocking(t *testing.T) {
	original := headlessRepairTimeout
	headlessRepairTimeout = 30 * time.Millisecond
	t.Cleanup(func() { headlessRepairTimeout = original })
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	initialPath := filepath.Join(root, "initial.jsonl")
	malformed := `<shephrd-event>{"type":"question","payload":"choose","question":{}}</shephrd-event>`
	writeHeadlessClaudeFile(t, initialPath, claudeStream(t, "claude-session", malformed))
	script := "#!/bin/sh\ncase \" $* \" in *\" --resume \"*) while :; do sleep 1; done ;; *) cat \"$SHEPHRD_TEST_INITIAL\" ;; esac\n"
	service, state, task, attempt, inputPath := headlessClaudeControlFixture(t, root, dataDir, script)
	defer state.Close()
	t.Setenv("SHEPHRD_TEST_INITIAL", initialPath)
	if err := service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration); err != nil {
		t.Fatal(err)
	}
	current, _ := state.Task(task.ID)
	stored, _ := state.Attempt(attempt.ID)
	messages, _ := state.Messages(task.ID)
	if current.Status != model.TaskStatusBlocked || current.ProcessAlive || !strings.Contains(stored.FailureReason, "timed out") {
		t.Fatalf("task=%+v attempt=%+v messages=%+v", current, stored, messages)
	}
	if conflicts := service.Native.LiveProcessPIDs(root); len(conflicts) != 0 {
		t.Fatalf("timed-out repair left process conflicts: %v", conflicts)
	}
}

func TestHeadlessNonrepairableNestedAndOversizedFramingFailClosed(t *testing.T) {
	for _, test := range []struct {
		name, candidate string
	}{
		{name: "nested", candidate: `<shephrd-event>{"type":"done","payload":"<shephrd-event>{}","artifact":"report:/tmp/report.md"}</shephrd-event>`},
		{name: "oversized", candidate: `<shephrd-event>{` + strings.Repeat("x", 100*1024) + `</shephrd-event>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dataDir := filepath.Join(root, "data")
			streamPath := filepath.Join(root, "stream.jsonl")
			writeHeadlessClaudeFile(t, streamPath, claudeStream(t, "claude-session", test.candidate))
			service, state, task, attempt, inputPath := headlessClaudeControlFixture(t, root, dataDir, "#!/bin/sh\ncat \"$SHEPHRD_TEST_STREAM\"\n")
			defer state.Close()
			t.Setenv("SHEPHRD_TEST_STREAM", streamPath)
			if err := service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration); err != nil {
				t.Fatal(err)
			}
			current, _ := state.Task(task.ID)
			messages, _ := state.Messages(task.ID)
			repairs, workerEvents, blocked := 0, 0, 0
			for _, message := range messages {
				repairs += boolInt(message.Type == "protocol-repair")
				workerEvents += boolInt(message.Direction == "worker-to-driver" && (message.Type == "checkpoint" || message.Type == "done"))
				blocked += boolInt(message.Type == "blocked" && !message.Stale)
			}
			if current.Status != model.TaskStatusBlocked || current.ProcessAlive || repairs != 0 || workerEvents != 0 || blocked != 1 {
				t.Fatalf("task=%+v repairs=%d events=%d blocked=%d messages=%+v", current, repairs, workerEvents, blocked, messages)
			}
		})
	}
}

func TestHeadlessInvalidCorrectionReapsProcessTreeBeforeBlocking(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	initialPath := filepath.Join(root, "initial.jsonl")
	correctionPath := filepath.Join(root, "correction.jsonl")
	childPIDPath := filepath.Join(root, "child-pid")
	releasePath := filepath.Join(root, "child-release")
	lateWritePath := filepath.Join(root, "late-write")
	reportPath := filepath.Join(dataDir, "preserved-report.md")
	reportBody := []byte("recoverable report state\n")
	writeHeadlessClaudeFile(t, reportPath, reportBody)
	malformed := `<shephrd-event>{"type":"question","payload":"choose","question":{}}</shephrd-event>`
	checkpoint := headlessClaudeEnvelope(t, model.Event{Type: "checkpoint", Payload: "repair", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "repair", Completed: []string{}, NextSteps: []string{"finish"}, Decisions: []model.Decision{}, ChangedPaths: []string{}, Checks: []model.Check{}, Blockers: []string{}}})
	done := `<shephrd-event>{"type":"done","payload":"done","artifact":"report:/tmp/report.md"}</shephrd-event>`
	writeHeadlessClaudeFile(t, initialPath, claudeStream(t, "claude-session", malformed))
	writeHeadlessClaudeFile(t, correctionPath, claudeStream(t, "claude-session", checkpoint+done+done))
	script := `#!/bin/sh
set -eu
case " $* " in
  *" --resume "*)
    (while [ ! -e "$SHEPHRD_TEST_CHILD_RELEASE" ]; do sleep 0.01; done; printf late > "$SHEPHRD_TEST_LATE_WRITE") &
    child=$!
    printf '%s' "$child" > "$SHEPHRD_TEST_CHILD_PID"
    cat "$SHEPHRD_TEST_CORRECTION"
    wait "$child"
    ;;
  *) cat "$SHEPHRD_TEST_INITIAL" ;;
esac
`
	service, state, task, attempt, inputPath := headlessClaudeControlFixture(t, root, dataDir, script)
	defer state.Close()
	t.Setenv("SHEPHRD_TEST_INITIAL", initialPath)
	t.Setenv("SHEPHRD_TEST_CORRECTION", correctionPath)
	t.Setenv("SHEPHRD_TEST_CHILD_PID", childPIDPath)
	t.Setenv("SHEPHRD_TEST_CHILD_RELEASE", releasePath)
	t.Setenv("SHEPHRD_TEST_LATE_WRITE", lateWritePath)
	if err := service.RunAttempt(attempt.ID, inputPath, false, nil, attempt.RunGeneration); err != nil {
		t.Fatal(err)
	}
	pidBytes, err := os.ReadFile(childPIDPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !workerprocess.Alive(pid) })
	current, _ := state.Task(task.ID)
	stored, _ := state.Attempt(attempt.ID)
	messages, _ := state.Messages(task.ID)
	repairs, workerEvents, blocked := 0, 0, 0
	for _, message := range messages {
		repairs += boolInt(message.Type == "protocol-repair")
		workerEvents += boolInt(message.Direction == "worker-to-driver" && (message.Type == "checkpoint" || message.Type == "done"))
		blocked += boolInt(message.Type == "blocked" && !message.Stale)
	}
	if current.Status != model.TaskStatusBlocked || current.ProcessAlive || current.ClaimedDone || current.ArtifactRef != "" || stored.Status != model.AttemptStatusBlocked || stored.RunnerPID != 0 || repairs != 1 || workerEvents != 0 || blocked != 1 {
		t.Fatalf("task=%+v attempt=%+v repairs=%d events=%d blocked=%d messages=%+v", current, stored, repairs, workerEvents, blocked, messages)
	}
	if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lateWritePath); !os.IsNotExist(err) {
		t.Fatalf("post-block write occurred: %v", err)
	}
	preserved, err := os.ReadFile(reportPath)
	if err != nil || !bytes.Equal(preserved, reportBody) {
		t.Fatalf("report state was not preserved: bytes=%q err=%v", preserved, err)
	}
	if conflicts := service.Native.LiveProcessPIDs(root); len(conflicts) != 0 {
		t.Fatalf("headless process conflict remained after block: %v", conflicts)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil || generation != attempt.RunGeneration+1 {
		t.Fatalf("immediate recovery generation=%d err=%v", generation, err)
	}
}

func TestRealHeadlessClaudeAuthorityIsOptIn(t *testing.T) {
	if os.Getenv("SHEPHRD_REAL_HEADLESS_CLAUDE_E2E") != "1" {
		t.Skip("set SHEPHRD_REAL_HEADLESS_CLAUDE_E2E=1 to run a real headless Claude worker")
	}
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo, err := state.UpsertRepo(model.Repo{Name: "real-headless-claude", Path: root, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Real headless Claude authority", DriverID: "driver:e2e", RepoID: repo.ID, FeatureKey: "real-headless-claude", Objective: "validate authoritative stream records", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "claude-code", os.Getenv("SHEPHRD_REAL_HEADLESS_CLAUDE_MODEL"))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetRuntimeBackend(attempt.ID, "headless"); err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, adapter.NewSessionID("claude-code"), root, "real-headless-lease", "branch"); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"emit the protocol fixture"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	attempt, err = state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := `<shephrd-event>{"type":"checkpoint","payload":"Real headless authority checkpoint","checkpoint":{"schema_version":1,"summary":"Real headless authority checkpoint","completed":["No project work required"],"next_steps":["Ask the fixture question"],"decisions":[],"changed_paths":[],"checks":[],"blockers":[]}}</shephrd-event>`
	question := `<shephrd-event>{"type":"question","payload":"Real headless Claude authority check completed. No answer is required."}</shephrd-event>`
	inputPath := writeInput(t, dataDir, task.ID, "brief.md", "Do not use tools or modify files. Respond with exactly these two envelopes and no prose: "+checkpoint+question)
	service := New(config.Config{DataDir: dataDir}, state)
	if err := service.RunAttempt(attempt.ID, inputPath, false, nil, generation); err != nil {
		t.Fatal(err)
	}
	current, _ := state.Task(task.ID)
	messages, _ := state.Messages(task.ID)
	checkpoints, questions, blocked := 0, 0, 0
	for _, message := range messages {
		checkpoints += boolInt(message.Direction == "worker-to-driver" && message.Type == "checkpoint" && !message.Stale)
		questions += boolInt(message.Type == "question" && !message.Stale)
		blocked += boolInt(message.Type == "blocked" && !message.Stale)
	}
	if current.Status != model.TaskStatusWaiting || current.ProcessAlive || checkpoints != 1 || questions != 1 || blocked != 0 {
		t.Fatalf("task=%+v checkpoints=%d questions=%d blocked=%d messages=%+v", current, checkpoints, questions, blocked, messages)
	}
}

func headlessClaudeControlFixture(t *testing.T, root, dataDir, script string) (Service, *store.Store, model.Task, model.Attempt, string) {
	t.Helper()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := state.UpsertRepo(model.Repo{Name: "demo", Path: root, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := state.CreateTask(model.Task{Title: "Test task", DriverID: "driver:test", RepoID: repo.ID, FeatureKey: "headless-claude-" + store.NewID("fixture"), Objective: "headless authority", Deliverable: "report"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := state.BeginAttempt(task.ID, "claude-code", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetRuntimeBackend(attempt.ID, "headless"); err != nil {
		t.Fatal(err)
	}
	if err := configureAttempt(t, state, attempt.ID, "claude-session", root, "lease", "branch"); err != nil {
		t.Fatal(err)
	}
	generation, err := state.ReserveRunGeneration(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordSystemCheckpoint(attempt.ID, generation, "assigned", []string{"work"}, model.WorkspaceFacts{}); err != nil {
		t.Fatal(err)
	}
	attempt, err = state.Attempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	service := New(config.Config{DataDir: dataDir}, state)
	inputPath := writeInput(t, dataDir, task.ID, "brief.md", "prompt")
	return service, state, task, attempt, inputPath
}

func claudeAssistantRecord(sessionID string, parent any, text string, fields map[string]any) map[string]any {
	record := map[string]any{"type": "assistant", "session_id": sessionID, "parent_tool_use_id": parent, "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}}
	for key, value := range fields {
		record[key] = value
	}
	return record
}

func claudeStream(t *testing.T, sessionID, text string) []byte {
	t.Helper()
	var stream bytes.Buffer
	writeClaudeRecord(t, &stream, map[string]any{"type": "system", "subtype": "init", "session_id": sessionID})
	writeClaudeRecord(t, &stream, claudeAssistantRecord(sessionID, nil, text, nil))
	writeClaudeRecord(t, &stream, map[string]any{"type": "result", "subtype": "success", "session_id": sessionID, "is_error": false})
	return stream.Bytes()
}

func writeClaudeRecord(t *testing.T, target *bytes.Buffer, record map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	target.Write(encoded)
	target.WriteByte('\n')
}

func headlessClaudeEnvelope(t *testing.T, event model.Event) string {
	t.Helper()
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return "<shephrd-event>" + string(encoded) + "</shephrd-event>"
}

func writeHeadlessClaudeFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
