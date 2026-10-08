package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCheckpointValidationAndEncoding(t *testing.T) {
	checkpoint := Checkpoint{SchemaVersion: CheckpointSchemaVersion, Summary: " ready ", NextSteps: []string{" finish "}}
	normalized := NormalizeCheckpoint(checkpoint)
	if normalized.Summary != "ready" || normalized.NextSteps[0] != "finish" || normalized.Completed == nil || normalized.Decisions == nil || normalized.ChangedPaths == nil || normalized.Checks == nil || normalized.Blockers == nil {
		t.Fatalf("normalized checkpoint = %+v", normalized)
	}
	if err := ValidateCheckpoint(normalized); err != nil {
		t.Fatal(err)
	}
	encoded, err := CheckpointJSON(normalized)
	if err != nil {
		t.Fatal(err)
	}
	var shape map[string]any
	if err := json.Unmarshal([]byte(encoded), &shape); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"completed", "next_steps", "decisions", "changed_paths", "checks", "blockers"} {
		if _, ok := shape[field].([]any); !ok {
			t.Fatalf("%s JSON value = %#v", field, shape[field])
		}
	}
}

func TestCheckpointValidationTerminalAllowance(t *testing.T) {
	empty := Checkpoint{SchemaVersion: CheckpointSchemaVersion, Summary: "done", NextSteps: []string{}}
	if err := ValidateCheckpoint(empty); err == nil || !strings.Contains(err.Error(), "next_steps") {
		t.Fatalf("ordinary validation error = %v", err)
	}
	if err := ValidateCheckpointState(empty, true); err != nil {
		t.Fatal(err)
	}
	empty.NextSteps = nil
	if err := ValidateCheckpointState(empty, true); err == nil || !strings.Contains(err.Error(), "next_steps") {
		t.Fatalf("missing next_steps error = %v", err)
	}
}

func TestKindErrorContract(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", Failure("pr_not_merged", "not merged"))
	var classified interface{ ErrorKind() string }
	if !errors.As(err, &classified) || classified.ErrorKind() != "pr_not_merged" || err.Error() != "wrapped: not merged" {
		t.Fatalf("kinded error = %v kind=%q", err, classified.ErrorKind())
	}
}
