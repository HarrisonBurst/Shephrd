package model

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	CheckpointSchemaVersion = 1
	EventMaxPayload         = 16 * 1024
	CheckpointMaxBytes      = 64 * 1024
	CheckpointMaxString     = 1024
	CheckpointMaxSummary    = 4 * 1024
	CheckpointMaxItems      = 20
	CheckpointMaxPaths      = 100
)

func NormalizeCheckpoint(checkpoint Checkpoint) Checkpoint {
	checkpoint.Summary = strings.TrimSpace(checkpoint.Summary)
	checkpoint.Completed = trimStrings(checkpoint.Completed)
	checkpoint.NextSteps = trimStrings(checkpoint.NextSteps)
	checkpoint.ChangedPaths = trimStrings(checkpoint.ChangedPaths)
	checkpoint.Blockers = trimStrings(checkpoint.Blockers)
	for i := range checkpoint.Decisions {
		checkpoint.Decisions[i].Decision = strings.TrimSpace(checkpoint.Decisions[i].Decision)
		checkpoint.Decisions[i].Reason = strings.TrimSpace(checkpoint.Decisions[i].Reason)
	}
	for i := range checkpoint.Checks {
		checkpoint.Checks[i].Command = strings.TrimSpace(checkpoint.Checks[i].Command)
		checkpoint.Checks[i].Result = strings.TrimSpace(checkpoint.Checks[i].Result)
	}
	if checkpoint.Completed == nil {
		checkpoint.Completed = []string{}
	}
	if checkpoint.NextSteps == nil {
		checkpoint.NextSteps = []string{}
	}
	if checkpoint.Decisions == nil {
		checkpoint.Decisions = []Decision{}
	}
	if checkpoint.ChangedPaths == nil {
		checkpoint.ChangedPaths = []string{}
	}
	if checkpoint.Checks == nil {
		checkpoint.Checks = []Check{}
	}
	if checkpoint.Blockers == nil {
		checkpoint.Blockers = []string{}
	}
	return checkpoint
}

func ValidateCheckpoint(checkpoint Checkpoint) error {
	return ValidateCheckpointState(checkpoint, false)
}

func ValidateCheckpointState(checkpoint Checkpoint, allowEmptyNextSteps bool) error {
	nextStepsMissing := checkpoint.NextSteps == nil
	checkpoint = NormalizeCheckpoint(checkpoint)
	if checkpoint.SchemaVersion != CheckpointSchemaVersion {
		return fmt.Errorf("checkpoint schema_version must be %d", CheckpointSchemaVersion)
	}
	if checkpoint.Summary == "" {
		return fmt.Errorf("checkpoint summary must not be empty")
	}
	if strings.ContainsRune(checkpoint.Summary, '\x00') {
		return fmt.Errorf("checkpoint summary contains NUL")
	}
	if len([]byte(checkpoint.Summary)) > CheckpointMaxSummary {
		return fmt.Errorf("checkpoint summary exceeds %d bytes", CheckpointMaxSummary)
	}
	if len(checkpoint.Completed) > CheckpointMaxItems || len(checkpoint.NextSteps) > CheckpointMaxItems || len(checkpoint.Decisions) > CheckpointMaxItems || len(checkpoint.Checks) > CheckpointMaxItems || len(checkpoint.Blockers) > CheckpointMaxItems {
		return fmt.Errorf("checkpoint completed, next_steps, decisions, checks, and blockers allow at most %d entries", CheckpointMaxItems)
	}
	if len(checkpoint.ChangedPaths) > CheckpointMaxPaths {
		return fmt.Errorf("checkpoint changed_paths allows at most %d entries", CheckpointMaxPaths)
	}
	if len(checkpoint.NextSteps) == 0 && (!allowEmptyNextSteps || nextStepsMissing) {
		return fmt.Errorf("checkpoint next_steps must contain at least one entry unless it is immediately followed by done")
	}
	for _, value := range checkpoint.Completed {
		if err := validateCheckpointString("completed item", value); err != nil {
			return err
		}
	}
	for _, value := range checkpoint.NextSteps {
		if err := validateCheckpointString("next step", value); err != nil {
			return err
		}
	}
	for _, value := range checkpoint.ChangedPaths {
		if err := validateCheckpointString("changed path", value); err != nil {
			return err
		}
	}
	for _, value := range checkpoint.Blockers {
		if err := validateCheckpointString("blocker", value); err != nil {
			return err
		}
	}
	for _, decision := range checkpoint.Decisions {
		if err := validateCheckpointString("decision", decision.Decision); err != nil {
			return err
		}
		if err := validateCheckpointString("decision reason", decision.Reason); err != nil {
			return err
		}
	}
	for _, check := range checkpoint.Checks {
		if err := validateCheckpointString("check command", check.Command); err != nil {
			return err
		}
		if err := validateCheckpointString("check result", check.Result); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return fmt.Errorf("encode checkpoint: %w", err)
	}
	if len(encoded) > CheckpointMaxBytes {
		return fmt.Errorf("checkpoint exceeds %d bytes", CheckpointMaxBytes)
	}
	return nil
}

func ValidateDeliverableArtifact(deliverable, artifact string) error {
	trimmed := strings.TrimSpace(artifact)
	if artifact != trimmed {
		return fmt.Errorf("%s task artifact must not have surrounding whitespace", deliverable)
	}
	switch deliverable {
	case "code":
		if strings.HasPrefix(artifact, "report:") {
			return fmt.Errorf("code task artifact must be branch:<name> or a GitHub PR URL, not %q", artifact)
		}
	case "report":
		if !strings.HasPrefix(artifact, "report:") {
			return fmt.Errorf("report task artifact must be report:<path>, not %q", artifact)
		}
	default:
		return fmt.Errorf("task deliverable must be code or report, got %q", deliverable)
	}
	return nil
}

func ValidateEvent(event Event) error {
	event.Payload = strings.TrimSpace(event.Payload)
	if event.Payload == "" {
		return fmt.Errorf("worker event %q requires a non-empty payload", event.Type)
	}
	if strings.ContainsRune(event.Payload, '\x00') {
		return fmt.Errorf("worker event payload contains NUL")
	}
	if len([]byte(event.Payload)) > EventMaxPayload {
		return fmt.Errorf("worker event payload exceeds %d bytes", EventMaxPayload)
	}
	switch event.Type {
	case "progress", "question", "done", "blocked", "failed", "checkpoint":
	default:
		return fmt.Errorf("worker event type must be progress, question, done, blocked, failed, or checkpoint, got %q", event.Type)
	}
	if event.Type == "checkpoint" {
		if event.Checkpoint == nil {
			return fmt.Errorf("checkpoint event requires a checkpoint object")
		}
		if err := ValidateCheckpointState(*event.Checkpoint, true); err != nil {
			return err
		}
	} else if event.Checkpoint != nil {
		return fmt.Errorf("checkpoint object is only valid on checkpoint events")
	}
	if event.Type == "done" && strings.TrimSpace(event.Artifact) == "" {
		return fmt.Errorf("worker done event requires an artifact ref; send branch:<name>, a PR URL, or report:<path>")
	}
	return nil
}

const SubdriverCheckpointMaxBytes = 4096

func ValidateSubdriverEvent(event Event) error {
	if event.Type != "checkpoint" && event.Type != "progress" && event.Type != "done" {
		return fmt.Errorf("sub-driver session accepts only progress, checkpoint and done")
	}
	if event.Artifact != "" {
		return fmt.Errorf("sub-driver session events require no artifact; use subdriver return for outcomes")
	}
	if event.Type == "done" {
		event.Type = "progress"
	}
	if err := ValidateEvent(event); err != nil {
		return err
	}
	if event.Checkpoint != nil {
		body, err := CheckpointJSON(*event.Checkpoint)
		if err != nil {
			return err
		}
		if len(body) > SubdriverCheckpointMaxBytes {
			return fmt.Errorf("sub-driver checkpoint must fit %d bytes", SubdriverCheckpointMaxBytes)
		}
	}
	return nil
}

func CheckpointJSON(checkpoint Checkpoint) (string, error) {
	if err := ValidateCheckpointState(checkpoint, true); err != nil {
		return "", err
	}
	checkpoint = NormalizeCheckpoint(checkpoint)
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func trimStrings(values []string) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = strings.TrimSpace(value)
	}
	return result
}

func validateCheckpointString(label, value string) error {
	if value == "" {
		return fmt.Errorf("checkpoint %s must not be empty", label)
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("checkpoint %s contains NUL", label)
	}
	if len([]byte(value)) > CheckpointMaxString {
		return fmt.Errorf("checkpoint %s exceeds %d bytes", label, CheckpointMaxString)
	}
	return nil
}
