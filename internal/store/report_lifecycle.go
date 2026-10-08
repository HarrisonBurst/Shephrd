package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"shephrd/internal/model"
)

const reportLifecycleInvocationSelect = `SELECT id, event_id, event_name, event_version, artifact_id, task_id, attempt_id,
	handler_name, extension_id, configuration_hash, position, state, attempts, claim_token, annotation, receipt_system,
	receipt_id, failure_kind, failure_message, effect, started_at, deadline_at, completed_at, created_at, updated_at
	FROM report_lifecycle_invocations`

func prepareReportAcceptedArtifact(artifact *model.VerifiedArtifact) {
	artifact.AcceptedEventID = NewID("report_event")
	artifact.AcceptedEventName = model.ReportAcceptedEventName
	artifact.AcceptedEventVersion = model.ReportAcceptedEventVersion
}

func insertReportLifecycleInvocationsTx(tx *sql.Tx, artifact model.VerifiedArtifact, handlers []model.ReportAcceptedHandlerBinding) error {
	seen := make(map[string]bool, len(handlers))
	created := now()
	for index, handler := range handlers {
		if handler.Name == "" || handler.ExtensionID == "" || seen[handler.Name] || len(handler.ConfigurationHash) != 64 || strings.ToLower(handler.ConfigurationHash) != handler.ConfigurationHash {
			return fmt.Errorf("report.accepted handler binding is invalid")
		}
		if _, err := hex.DecodeString(handler.ConfigurationHash); err != nil {
			return fmt.Errorf("report.accepted handler binding is invalid")
		}
		seen[handler.Name] = true
		if _, err := tx.Exec(`INSERT INTO report_lifecycle_invocations(id, event_id, event_name, event_version, artifact_id,
			task_id, attempt_id, handler_name, extension_id, configuration_hash, position, state, created_at, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`, NewID("lifecycle_invocation"), artifact.AcceptedEventID,
			artifact.AcceptedEventName, artifact.AcceptedEventVersion, artifact.ID, artifact.ProducerTaskID, artifact.ProducerAttemptID,
			handler.Name, handler.ExtensionID, handler.ConfigurationHash, index+1, created, created); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ReportLifecycleInvocations(taskID string) ([]model.ReportLifecycleInvocation, error) {
	var exists int
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='report_lifecycle_invocations')`).Scan(&exists); err != nil || exists == 0 {
		return []model.ReportLifecycleInvocation{}, err
	}
	rows, err := s.db.Query(reportLifecycleInvocationSelect+` WHERE task_id=? ORDER BY created_at, position, id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReportLifecycleInvocations(rows)
}

func (s *Store) ReportLifecycleInvocationsForEvent(eventID string) ([]model.ReportLifecycleInvocation, error) {
	rows, err := s.db.Query(reportLifecycleInvocationSelect+` WHERE event_id=? ORDER BY position, id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReportLifecycleInvocations(rows)
}

func reportLifecycleInvocationsForMessage(queryer interface {
	Query(string, ...any) (*sql.Rows, error)
}, messageID int64) ([]model.ReportLifecycleInvocation, error) {
	rows, err := queryer.Query(reportLifecycleInvocationSelect+` WHERE artifact_id=(SELECT id FROM verified_artifacts WHERE done_message_id=?) ORDER BY position, id`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	invocations, err := scanReportLifecycleInvocations(rows)
	if len(invocations) == 0 {
		return nil, err
	}
	return invocations, err
}

func scanReportLifecycleInvocations(rows *sql.Rows) ([]model.ReportLifecycleInvocation, error) {
	result := make([]model.ReportLifecycleInvocation, 0)
	for rows.Next() {
		invocation, err := scanReportLifecycleInvocation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, invocation)
	}
	return result, rows.Err()
}

func scanReportLifecycleInvocation(scanner interface{ Scan(...any) error }) (model.ReportLifecycleInvocation, error) {
	var invocation model.ReportLifecycleInvocation
	var started, deadline, completed sql.NullString
	var created, updated string
	if err := scanner.Scan(&invocation.ID, &invocation.EventID, &invocation.EventName, &invocation.EventVersion,
		&invocation.ArtifactID, &invocation.TaskID, &invocation.AttemptID, &invocation.HandlerName, &invocation.ExtensionID,
		&invocation.ConfigurationHash, &invocation.Position, &invocation.State, &invocation.Attempts, &invocation.InvocationClaimToken,
		&invocation.Annotation, &invocation.ReceiptSystem, &invocation.ReceiptID, &invocation.FailureKind,
		&invocation.FailureMessage, &invocation.Effect, &started, &deadline, &completed, &created, &updated); err != nil {
		return model.ReportLifecycleInvocation{}, err
	}
	invocation.StartedAt = nullableTimePointer(started)
	invocation.DeadlineAt = nullableTimePointer(deadline)
	invocation.CompletedAt = nullableTimePointer(completed)
	invocation.CreatedAt = parseTime(created)
	invocation.UpdatedAt = parseTime(updated)
	invocation.Recoverable = invocation.State != "succeeded"
	if invocation.Recoverable {
		invocation.RetryCommand = "shephrd task verify-delivery " + model.ShellQuote(invocation.TaskID) + " --json"
	}
	return invocation, nil
}

func (s *Store) ClaimReportLifecycleInvocation(id, configurationHash string, at, deadline time.Time) (model.ReportLifecycleInvocation, bool, error) {
	if id == "" || len(configurationHash) != 64 || !deadline.After(at) {
		return model.ReportLifecycleInvocation{}, false, fmt.Errorf("report lifecycle invocation claim is invalid")
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.ReportLifecycleInvocation{}, false, err
	}
	defer tx.Rollback()
	invocation, err := scanReportLifecycleInvocation(tx.QueryRow(reportLifecycleInvocationSelect+` WHERE id=?`, id))
	if err != nil {
		return model.ReportLifecycleInvocation{}, false, err
	}
	if invocation.ConfigurationHash != configurationHash {
		return invocation, false, fmt.Errorf("handler %s configuration no longer matches accepted event %s", invocation.HandlerName, invocation.EventID)
	}
	if invocation.State == "succeeded" || invocation.State == "invoking" && invocation.DeadlineAt != nil && at.Before(*invocation.DeadlineAt) {
		return invocation, false, tx.Commit()
	}
	claim := NewID("lifecycle_claim")
	result, err := tx.Exec(`UPDATE report_lifecycle_invocations SET state='invoking', attempts=attempts+1, claim_token=?,
		annotation='', receipt_system='', receipt_id='', failure_kind='', failure_message='', effect='', started_at=?, deadline_at=?,
		completed_at=NULL, updated_at=? WHERE id=? AND configuration_hash=?`, claim, stamp(at), stamp(deadline), stamp(at), id, configurationHash)
	if err != nil {
		return model.ReportLifecycleInvocation{}, false, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return model.ReportLifecycleInvocation{}, false, fmt.Errorf("report lifecycle invocation %s changed before claim", id)
	}
	invocation, err = scanReportLifecycleInvocation(tx.QueryRow(reportLifecycleInvocationSelect+` WHERE id=?`, id))
	if err != nil {
		return model.ReportLifecycleInvocation{}, false, err
	}
	return invocation, true, tx.Commit()
}

func (s *Store) CompleteReportLifecycleInvocation(id, claimToken, state, annotation, receiptSystem, receiptID, failureKind, failureMessage, effect string, at time.Time) (model.ReportLifecycleInvocation, error) {
	if state != "succeeded" && state != "failed" && state != "unknown" {
		return model.ReportLifecycleInvocation{}, fmt.Errorf("invalid report lifecycle invocation outcome %q", state)
	}
	if claimToken == "" || invalidLifecycleText(annotation, 4096, true) || invalidLifecycleText(receiptSystem, 128, true) || invalidLifecycleText(receiptID, 512, true) || invalidLifecycleText(failureKind, 128, true) || invalidLifecycleText(failureMessage, 2048, true) {
		return model.ReportLifecycleInvocation{}, fmt.Errorf("report lifecycle invocation outcome is invalid or oversized")
	}
	if effect != "" && effect != "none" && effect != "known" && effect != "unknown" {
		return model.ReportLifecycleInvocation{}, fmt.Errorf("report lifecycle invocation effect is invalid")
	}
	if state == "succeeded" {
		if annotation == "" && receiptID == "" || receiptSystem == "" && receiptID != "" || receiptSystem != "" && receiptID == "" || failureKind != "" || failureMessage != "" || effect != "" {
			return model.ReportLifecycleInvocation{}, fmt.Errorf("successful report lifecycle outcome is incomplete")
		}
	} else if failureKind == "" || failureMessage == "" || effect == "" || annotation != "" || receiptSystem != "" || receiptID != "" {
		return model.ReportLifecycleInvocation{}, fmt.Errorf("failed report lifecycle outcome is incomplete")
	}
	result, err := s.db.Exec(`UPDATE report_lifecycle_invocations SET state=?, claim_token='', annotation=?, receipt_system=?, receipt_id=?,
		failure_kind=?, failure_message=?, effect=?, completed_at=?, updated_at=? WHERE id=? AND state='invoking' AND claim_token=?`,
		state, annotation, receiptSystem, receiptID, failureKind, failureMessage, effect, stamp(at), stamp(at), id, claimToken)
	if err != nil {
		return model.ReportLifecycleInvocation{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return model.ReportLifecycleInvocation{}, fmt.Errorf("report lifecycle invocation %s claim is stale", id)
	}
	return scanReportLifecycleInvocation(s.db.QueryRow(reportLifecycleInvocationSelect+` WHERE id=?`, id))
}

func (s *Store) RecordReportVerificationFailure(taskID, attemptID, failureKind string) (model.Message, error) {
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(attemptID) == "" || invalidLifecycleText(failureKind, 128, false) {
		return model.Message{}, fmt.Errorf("report verification failure identity is invalid")
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.Message{}, err
	}
	defer tx.Rollback()
	if err := acquireImmediateTransactionLock(tx); err != nil {
		return model.Message{}, err
	}
	var driverID, deliverable, currentAttemptID, taskStatus string
	var runGeneration int
	var acceptedDone, verified int
	if err := tx.QueryRow(`SELECT t.driver_id, t.deliverable, COALESCE(t.current_attempt_id,''), t.status, a.run_generation,
		EXISTS(SELECT 1 FROM messages m WHERE m.task_id=t.id AND m.attempt_id=a.id AND m.run_generation=a.run_generation
			AND m.direction='worker-to-driver' AND m.type='done' AND m.stale=0),
		EXISTS(SELECT 1 FROM verified_artifacts artifact WHERE artifact.producer_task_id=t.id AND artifact.producer_attempt_id=a.id AND artifact.kind='report')
		FROM tasks t JOIN attempts a ON a.id=? AND a.task_id=t.id WHERE t.id=?`, attemptID, taskID).
		Scan(&driverID, &deliverable, &currentAttemptID, &taskStatus, &runGeneration, &acceptedDone, &verified); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Message{}, fmt.Errorf("report verification failure does not match a stored task attempt")
		}
		return model.Message{}, err
	}
	if deliverable != "report" || currentAttemptID != attemptID || taskStatus != model.TaskStatusDone || acceptedDone != 1 || verified != 0 {
		return model.Message{}, fmt.Errorf("report verification failure is not current unverified report completion state")
	}
	existing, err := scanMessage(tx.QueryRow(`SELECT id, task_id, attempt_id, direction, type, payload, artifact_ref, stale, wake,
		source_cursor, run_generation, checkpoint_json, created_at FROM messages
		WHERE task_id=? AND attempt_id=? AND run_generation=? AND direction='system' AND type=? AND stale=0
		ORDER BY id LIMIT 1`, taskID, attemptID, runGeneration, model.ReportVerificationFailedMessageType))
	if err == nil {
		return existing, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.Message{}, err
	}
	var sourceCursor int64
	if err := tx.QueryRow(`SELECT source_cursor FROM messages WHERE task_id=? AND attempt_id=? AND run_generation=?
		AND direction='worker-to-driver' AND type='done' AND stale=0 ORDER BY id DESC LIMIT 1`, taskID, attemptID, runGeneration).Scan(&sourceCursor); err != nil {
		return model.Message{}, err
	}
	payload := fmt.Sprintf("Report verification failed (%s). The report remains unverified and its bytes have not been presented. Inspect with: shephrd task inspect %s --json. Retry explicitly with: shephrd task verify-delivery %s --attempt %s --driver-id %s --json.",
		failureKind, model.ShellQuote(taskID), model.ShellQuote(taskID), model.ShellQuote(attemptID), model.ShellQuote(driverID))
	message, err := insertMessage(tx, taskID, attemptID, "system", model.ReportVerificationFailedMessageType, payload, "", false, true, sourceCursor, runGeneration, "")
	if err != nil {
		return model.Message{}, err
	}
	if err := publishNotificationTx(tx, message); err != nil {
		return model.Message{}, err
	}
	return message, tx.Commit()
}

func invalidLifecycleText(value string, limit int, optional bool) bool {
	if value == "" {
		return !optional
	}
	return len(value) > limit || !utf8.ValidString(value) || strings.IndexFunc(value, func(char rune) bool {
		return char < 0x20 || char == 0x7f
	}) >= 0
}

func (s *Store) VerifiedReportForAttempt(taskID, attemptID string) (*model.VerifiedArtifact, error) {
	rows, err := s.db.Query(s.verifiedArtifactSelect()+` WHERE producer_task_id=? AND producer_attempt_id=? AND kind='report' ORDER BY verified_at, id`, taskID, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var artifacts []model.VerifiedArtifact
	for rows.Next() {
		artifact, err := scanVerifiedArtifact(rows)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(artifacts) == 0 {
		return nil, nil
	}
	if len(artifacts) != 1 {
		return nil, errors.New("report attempt has multiple immutable verified artifacts")
	}
	return &artifacts[0], nil
}
