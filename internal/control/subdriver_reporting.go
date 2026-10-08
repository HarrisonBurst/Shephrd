package control

import (
	"encoding/json"
	"fmt"
	"io"

	"shephrd/internal/adapter"
	"shephrd/internal/model"
	"shephrd/internal/runner"
	"shephrd/internal/store"
)

type subdriverReporting struct {
	store           *store.Store
	fence           model.SubdriverFence
	session         string
	checkpoint      string
	validCheckpoint bool
	finished        bool
	cursor          int64
	repair          *runner.RepairRequest
	log             io.Writer
}

func (r *subdriverReporting) check() error {
	if err := r.store.CheckSubdriverFence(r.fence); err != nil {
		return err
	}
	c, err := r.store.Subdriver(r.fence.ID)
	if err != nil {
		return err
	}
	if c.SessionID != r.session || r.finished {
		return fmt.Errorf("sub-driver reporting session is no longer current")
	}
	return nil
}

func (r *subdriverReporting) reject(cursor int64, request runner.RepairRequest, diagnostic adapter.Diagnostic) (*adapter.Diagnostic, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	if r.repair != nil || cursor <= r.cursor || request.ID == "" || request.CandidateHash == "" || !diagnostic.Repairable() {
		return nil, fmt.Errorf("sub-driver reporting correction exhausted or stale")
	}
	diagnostic = diagnostic.Bounded()
	diagnostic.RequiresCheckpoint = true
	audit := struct {
		Cursor     int64                `json:"cursor"`
		Repair     runner.RepairRequest `json:"repair"`
		Diagnostic adapter.Diagnostic   `json:"diagnostic"`
	}{cursor, request, diagnostic}
	if err := json.NewEncoder(r.log).Encode(audit); err != nil {
		return nil, err
	}
	r.cursor = cursor
	r.repair = &request
	return &diagnostic, nil
}

func (r *subdriverReporting) ingest(events []model.Event, cursor int64, session string, request runner.RepairRequest, correction bool) (runner.CandidateIngestion, error) {
	if err := r.check(); err != nil {
		return runner.CandidateIngestion{}, err
	}
	if session != r.session || cursor <= r.cursor || len(events) == 0 {
		return runner.CandidateIngestion{}, fmt.Errorf("sub-driver reporting session or cursor is stale")
	}
	if correction {
		if r.repair == nil || *r.repair != request || len(events) != 2 || events[0].Type != "checkpoint" || events[1].Type != "done" {
			return runner.CandidateIngestion{}, fmt.Errorf("sub-driver correction requires matching repair identity and exactly checkpoint then done")
		}
	} else if r.repair != nil {
		return runner.CandidateIngestion{}, fmt.Errorf("sub-driver reporting correction is pending")
	}
	checkpoint, valid, finished := r.checkpoint, r.validCheckpoint, false
	for index, event := range events {
		if err := model.ValidateSubdriverEvent(event); err != nil {
			return runner.CandidateIngestion{}, err
		}
		switch event.Type {
		case "checkpoint":
			checkpoint, _ = model.CheckpointJSON(*event.Checkpoint)
			valid = true
		case "done":
			if !valid || index != len(events)-1 {
				return runner.CandidateIngestion{}, fmt.Errorf("session completion requires current checkpoint then done without artifact")
			}
			finished = true
		}
	}
	if err := r.check(); err != nil {
		return runner.CandidateIngestion{}, err
	}
	r.checkpoint, r.validCheckpoint, r.finished = checkpoint, valid, finished
	r.cursor = cursor + int64(len(events)) - 1
	return runner.CandidateIngestion{PersistedCursor: r.cursor}, nil
}
