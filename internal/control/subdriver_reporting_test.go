package control

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/adapter"
	"shephrd/internal/model"
	"shephrd/internal/runner"
	"shephrd/internal/store"
)

func TestSubdriverReportingCorrectionFencesAndAtomicity(t *testing.T) {
	for _, mode := range []string{"valid", "repair-id", "hash", "cursor", "session", "generation", "token", "held", "checkpoint-only", "artifact", "duplicate", "exhausted", "replay"} {
		t.Run(mode, func(t *testing.T) {
			state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer state.Close()
			request, err := state.HandoffSubdriver("", "Research", "driver:test", "request", "Original", "", "")
			if err != nil {
				t.Fatal(err)
			}
			fence, err := state.ReserveSubdriver(request.SubdriverID, 0, "pi", "selected", "headless")
			if err != nil {
				t.Fatal(err)
			}
			if err := state.StartSubdriver(fence, 123, "session"); err != nil {
				t.Fatal(err)
			}
			var log bytes.Buffer
			r := subdriverReporting{store: state, fence: fence, session: "session", checkpoint: "retained checkpoint", log: &log}
			repair := runner.RepairRequest{ID: "repair_exact", CandidateHash: strings.Repeat("a", 64)}
			diagnostic := adapter.Diagnostic{Code: adapter.DiagnosticSchemaTypeMismatch, Phase: "adapter", Field: "checkpoint.decisions[]", Message: "expected object"}
			if got, err := r.reject(2, repair, diagnostic); err != nil || got == nil || !got.RequiresCheckpoint {
				t.Fatalf("rejection=%+v %v", got, err)
			}
			if r.checkpoint != "retained checkpoint" || r.finished || !strings.Contains(log.String(), repair.CandidateHash) {
				t.Fatalf("rejection changed report: %+v", r)
			}
			events := []model.Event{
				{Type: "checkpoint", Payload: "corrected", Checkpoint: &model.Checkpoint{SchemaVersion: 1, Summary: "corrected", NextSteps: []string{}}},
				{Type: "done", Payload: "turn handled"},
			}
			cursor, session := int64(3), "session"
			switch mode {
			case "repair-id":
				repair.ID = "repair_other"
			case "hash":
				repair.CandidateHash = strings.Repeat("b", 64)
			case "cursor":
				cursor = 2
			case "session":
				session = "other"
			case "generation":
				r.fence.Generation++
			case "token":
				r.fence.Token = "other"
			case "held":
				if err := state.FinishSubdriver(fence, "retained checkpoint", "held elsewhere"); err != nil {
					t.Fatal(err)
				}
			case "checkpoint-only":
				events = events[:1]
			case "artifact":
				events[1].Artifact = "branch:main"
			case "duplicate":
				events = append(events, events[1])
			case "exhausted":
				if _, err := r.reject(3, repair, diagnostic); err == nil {
					t.Fatal("second repair allowed")
				}
				events[1].Payload = ""
			}
			_, err = r.ingest(events, cursor, session, repair, true)
			if mode == "valid" || mode == "replay" {
				if err != nil || !r.finished || !strings.Contains(r.checkpoint, "corrected") {
					t.Fatalf("correction=%+v %v", r, err)
				}
				if _, err := r.ingest(events, 5, session, repair, true); err == nil {
					t.Fatal("post-terminal correction accepted")
				}
			} else if err == nil || r.finished || r.checkpoint != "retained checkpoint" {
				t.Fatalf("invalid correction partially accepted: %+v %v", r, err)
			}
		})
	}
}
