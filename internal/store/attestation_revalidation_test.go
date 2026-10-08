package store

import (
	"strings"
	"testing"

	"shephrd/internal/model"
)

func TestExternalAttestationRecordRevalidatesAttemptAndCheckpointIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Store, model.Attempt) error
	}{
		{name: "attempt", mutate: func(state *Store, attempt model.Attempt) error {
			_, err := state.db.Exec(`UPDATE attempts SET session_id='changed' WHERE id=?`, attempt.ID)
			return err
		}},
		{name: "checkpoint", mutate: func(state *Store, attempt model.Attempt) error {
			_, err := state.db.Exec(`UPDATE attempt_checkpoints SET worktree_dirty=1 WHERE attempt_id=?`, attempt.ID)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, task, attempt := externalAttestationAttempt(t)
			defer state.Close()
			candidate, err := state.DeliveryAttestationCandidate(task.ID, "", task.DriverID, storeSealed, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := test.mutate(state, attempt); err != nil {
				t.Fatal(err)
			}
			if _, _, err := state.RecordExternalDeliveryAttestation(candidate, storeEvidence()); err == nil || attestationErrorKind(err) != "attempt_selection_changed" || !strings.Contains(err.Error(), "during attestation") {
				t.Fatalf("error = %v", err)
			}
			if stored, err := state.ExternalDeliveryAttestationForAttempt(attempt.ID); err != nil || stored != nil {
				t.Fatalf("stored=%+v err=%v", stored, err)
			}
		})
	}
}
