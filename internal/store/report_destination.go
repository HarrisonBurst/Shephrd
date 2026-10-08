package store

import (
	"fmt"

	"shephrd/internal/adapter"
	"shephrd/internal/model"
)

func (s *Store) AssignReportDestination(taskID, attemptID string, generation int, dataDir string) (string, error) {
	path, err := model.RunReportPath(dataDir, taskID, attemptID, generation)
	if err != nil {
		return "", err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var eligible int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM attempts a JOIN tasks t ON t.current_attempt_id=a.id
		WHERE t.id=? AND a.id=? AND a.run_generation=? AND a.status=? AND a.runner_pid=0 AND a.released_at IS NULL AND t.deliverable='report')`,
		taskID, attemptID, generation, model.AttemptStatusStarting).Scan(&eligible); err != nil {
		return "", err
	}
	if eligible == 0 || generation < 1 {
		return "", ErrStaleRun
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM messages WHERE attempt_id=? AND run_generation=? AND direction='system' AND type='report-destination'`, attemptID, generation).Scan(&count); err != nil {
		return "", err
	}
	if count != 0 {
		return "", fmt.Errorf("attempt %s run %d already has a report destination", attemptID, generation)
	}
	if _, err := insertMessage(tx, taskID, attemptID, "system", "report-destination", "Report destination assigned for this run", "report:"+path, false, false, 0, generation, ""); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return path, nil
}

func terminalReportArtifactFence(event model.Event, reportRef string) (string, *adapter.Diagnostic) {
	if reportRef == "" || event.Artifact == "" || event.Artifact == reportRef {
		return "", nil
	}
	return fmt.Sprintf("worker report artifact must be the assigned current-run destination %s", reportRef),
		&adapter.Diagnostic{Code: adapter.DiagnosticArtifactContract, Phase: "store", Field: "artifact", Message: "report artifact does not name the assigned current-run destination"}
}
