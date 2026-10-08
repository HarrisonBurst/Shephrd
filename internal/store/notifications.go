package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"shephrd/internal/model"
)

const (
	DefaultNotificationBatchLimit = 10
	MaxNotificationBatchLimit     = 20
	DefaultNotificationClaimTTL   = 5 * time.Minute
	MinNotificationClaimTTL       = 30 * time.Second
	MaxNotificationClaimTTL       = 30 * time.Minute
	MaxNotificationHandlingID     = 256
	MaxNotificationConsumerID     = 256
	maxSettledNotificationPayload = 1024
)

var (
	ErrNotificationConflict = errors.New("notification claim conflict")
	ErrNotificationStale    = errors.New("notification is no longer current")
	ErrNotificationExpired  = errors.New("notification claim expired")
)

const notificationSelect = `SELECT d.notification_id, COALESCE(d.message_id,0), COALESCE(d.task_id,''), COALESCE(t.title,'Sub-driver return'), COALESCE(t.feature_key,''),
	COALESCE(r.name || '/' || t.title || ' [' || t.feature_key || ']', t.title || ' [' || t.feature_key || ']', cr.id), COALESCE(d.attempt_id,''), d.target_driver_id,
	d.worker_run_generation, d.source_cursor, d.kind, d.state, COALESCE(m.payload,ce.payload), COALESCE(m.artifact_ref,''),
	d.created_at, d.updated_at, d.claim_owner, d.driver_generation, d.claim_token,
	d.claimed_at, d.claim_until, d.delivery_attempts, d.acked_at, d.ack_owner,
	d.ack_driver_generation, d.handling_id, d.superseded_at, d.supersede_reason,
	COALESCE(ce.id,0),COALESCE(cr.id,''),COALESCE(cr.coordinator_id,''),COALESCE(subdriver_repo.name,'')
	FROM driver_notifications d
	LEFT JOIN messages m ON m.id=d.message_id
	LEFT JOIN tasks t ON t.id=d.task_id
	LEFT JOIN repos r ON r.id=t.repo_id
	LEFT JOIN coordinator_events ce ON ce.id=d.coordinator_event_id
	LEFT JOIN coordinator_requests cr ON cr.id=ce.request_id
	LEFT JOIN coordinators c ON c.id=cr.coordinator_id
	LEFT JOIN repos subdriver_repo ON subdriver_repo.id=c.repo_id`

const reportDoneNotificationPresentable = `(d.kind<>'done' OR NOT EXISTS (
	SELECT 1 FROM tasks presentable_task WHERE presentable_task.id=d.task_id AND presentable_task.deliverable='report'
) OR EXISTS (
	SELECT 1 FROM verified_artifacts presentable_artifact
	WHERE presentable_artifact.done_message_id=d.message_id AND presentable_artifact.producer_task_id=d.task_id
		AND presentable_artifact.producer_attempt_id=d.attempt_id AND presentable_artifact.kind='report'
		AND NOT EXISTS (
			SELECT 1 FROM report_lifecycle_invocations presentable_invocation
			WHERE presentable_invocation.artifact_id=presentable_artifact.id AND presentable_invocation.state IN ('pending','invoking')
		)
))`

const currentReleasedReportDoneNotification = `d.kind='done' AND EXISTS (
	SELECT 1 FROM tasks notification_task
	JOIN attempts notification_attempt ON notification_attempt.id=d.attempt_id AND notification_attempt.task_id=notification_task.id
	JOIN attempt_landing_projections notification_landing ON notification_landing.attempt_id=notification_attempt.id AND notification_landing.task_id=notification_task.id
	JOIN messages notification_message ON notification_message.id=d.message_id AND notification_message.task_id=notification_task.id AND notification_message.attempt_id=notification_attempt.id
	JOIN verified_artifacts notification_artifact ON notification_artifact.done_message_id=notification_message.id
		AND notification_artifact.producer_task_id=notification_task.id AND notification_artifact.producer_attempt_id=notification_attempt.id
	WHERE notification_task.id=d.task_id AND notification_task.deliverable='report' AND notification_task.status='done'
		AND notification_task.claimed_done=1 AND COALESCE(notification_task.current_attempt_id,'')=d.attempt_id
		AND notification_attempt.status='done' AND notification_attempt.run_generation=d.worker_run_generation
		AND notification_attempt.landing_kind='report_artifact' AND notification_landing.landed=1
		AND notification_message.run_generation=d.worker_run_generation AND notification_message.direction='worker-to-driver'
		AND notification_message.type='done' AND notification_message.stale=0 AND notification_message.wake=1
		AND notification_message.artifact_ref=notification_task.artifact_ref
		AND notification_artifact.kind='report' AND notification_artifact.original_ref=notification_message.artifact_ref
)`

func acquireImmediateTransactionLock(tx *sql.Tx) error {
	// Keep read-first transactions serialized even if a future SQLite driver defers write-lock acquisition.
	_, err := tx.Exec(`UPDATE tasks SET updated_at=updated_at WHERE 0`)
	return err
}

func (s *Store) Notification(notificationID string) (model.DriverNotification, error) {
	notification, err := scanDriverNotification(s.db.QueryRow(notificationSelect+` WHERE d.notification_id=?`, notificationID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.DriverNotification{}, fmt.Errorf("notification %q does not exist", notificationID)
	}
	if err != nil {
		return model.DriverNotification{}, err
	}
	notification.ReportLifecycle, err = reportLifecycleInvocationsForMessage(s.db, notification.MessageID)
	return notification, err
}

func (s *Store) NotificationPresentable(notificationID string) (bool, error) {
	var presentable int
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM driver_notifications d WHERE d.notification_id=? AND `+reportDoneNotificationPresentable+`)`, notificationID).Scan(&presentable)
	return presentable != 0, err
}

func (s *Store) NotificationPresentationRecorded(notificationID string) (bool, error) {
	var recorded int
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM notification_delivery_log WHERE notification_id=? AND operation IN ('notify','dedupe') AND result IN ('sent','suppressed'))`, notificationID).Scan(&recorded)
	return recorded != 0, err
}

func (s *Store) NotificationByClaimToken(claimToken string) (model.DriverNotification, error) {
	if strings.TrimSpace(claimToken) == "" {
		return model.DriverNotification{}, fmt.Errorf("%w: claim token must not be empty", ErrNotificationConflict)
	}
	rows, err := s.db.Query(notificationSelect+` WHERE d.claim_token=? ORDER BY d.notification_id LIMIT 2`, claimToken)
	if err != nil {
		return model.DriverNotification{}, err
	}
	matches := make([]model.DriverNotification, 0, 2)
	for rows.Next() {
		notification, err := scanDriverNotification(rows)
		if err != nil {
			return model.DriverNotification{}, err
		}
		matches = append(matches, notification)
	}
	if err := rows.Close(); err != nil {
		return model.DriverNotification{}, err
	}
	if err := rows.Err(); err != nil {
		return model.DriverNotification{}, err
	}
	if len(matches) == 0 {
		return model.DriverNotification{}, fmt.Errorf("%w: claim token does not identify a stored notification claim", ErrNotificationConflict)
	}
	if len(matches) > 1 {
		return model.DriverNotification{}, fmt.Errorf("%w: claim token identifies more than one stored notification claim", ErrNotificationConflict)
	}
	matches[0].ReportLifecycle, err = reportLifecycleInvocationsForMessage(s.db, matches[0].MessageID)
	return matches[0], err
}

func (s *Store) Notifications(taskID string) ([]model.DriverNotification, error) {
	query := notificationSelect
	args := []any{}
	if taskID != "" {
		query += ` WHERE d.task_id=?`
		args = append(args, taskID)
	}
	query += ` ORDER BY d.created_at, d.message_id`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	result := make([]model.DriverNotification, 0)
	for rows.Next() {
		notification, err := scanDriverNotification(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, notification)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range result {
		result[index].ReportLifecycle, err = reportLifecycleInvocationsForMessage(s.db, result[index].MessageID)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *Store) AdoptTask(taskID, currentDriverID, newDriverID string) (model.TaskAdoption, error) {
	currentDriverID = strings.TrimSpace(currentDriverID)
	newDriverID = strings.TrimSpace(newDriverID)
	result := model.TaskAdoption{TaskID: taskID, DriverID: newDriverID}
	if err := validateDriverID(currentDriverID); err != nil {
		return result, err
	}
	if err := validateDriverID(newDriverID); err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err := acquireImmediateTransactionLock(tx); err != nil {
		return result, err
	}
	if err := tx.QueryRow(`SELECT driver_id FROM tasks WHERE id=?`, taskID).Scan(&result.PreviousDriverID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return result, fmt.Errorf("task %q does not exist", taskID)
		}
		return result, err
	}
	if model.IsSubdriverOwner(result.PreviousDriverID) || model.IsSubdriverOwner(newDriverID) {
		return result, fmt.Errorf("sub-driver worker ownership cannot be adopted; adopt the request return route instead")
	}
	if result.PreviousDriverID != currentDriverID {
		return result, fmt.Errorf("task %s is owned by %s, not current driver %s", taskID, result.PreviousDriverID, currentDriverID)
	}
	if result.PreviousDriverID == newDriverID {
		return result, tx.Commit()
	}
	rows, err := tx.Query(`SELECT notification_id, state, claim_owner, driver_generation, claim_token FROM driver_notifications WHERE task_id=? AND state IN ('pending','claimed') ORDER BY created_at, message_id`, taskID)
	if err != nil {
		return result, err
	}
	type retargetedNotification struct {
		id, state, owner, generation, token string
	}
	items := make([]retargetedNotification, 0)
	for rows.Next() {
		var item retargetedNotification
		if err := rows.Scan(&item.id, &item.state, &item.owner, &item.generation, &item.token); err != nil {
			rows.Close()
			return result, err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	timestamp := now()
	if _, err := tx.Exec(`UPDATE tasks SET driver_id=?, updated_at=? WHERE id=?`, newDriverID, timestamp, taskID); err != nil {
		return result, err
	}
	for _, item := range items {
		if _, err := tx.Exec(`UPDATE driver_notifications SET target_driver_id=?, state='pending', updated_at=?, claim_owner='', driver_generation='', claim_token='', claimed_at=NULL, claim_until=NULL WHERE notification_id=? AND state IN ('pending','claimed')`, newDriverID, timestamp, item.id); err != nil {
			return result, err
		}
		if item.state == string(model.NotificationClaimed) {
			result.ReleasedClaims++
		}
		if err := appendNotificationLogTx(tx, item.id, "adopt", item.owner, item.generation, item.token, "", "retargeted", "task adopted from "+result.PreviousDriverID); err != nil {
			return result, err
		}
	}
	result.Retargeted = len(items)
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func (s *Store) DrainNotifications(taskID, consumerID, driverGeneration string, limit int, requestedTTL ...time.Duration) (model.NotificationDrain, error) {
	result := model.NotificationDrain{Notifications: make([]model.DriverNotification, 0), ConsumerID: consumerID, DriverGeneration: driverGeneration}
	if err := validateConsumer(consumerID, driverGeneration); err != nil {
		return result, err
	}
	if limit == 0 {
		limit = DefaultNotificationBatchLimit
	}
	if limit < 1 || limit > MaxNotificationBatchLimit {
		return result, fmt.Errorf("notification limit must be between 1 and %d", MaxNotificationBatchLimit)
	}
	ttl, err := notificationTTL(requestedTTL...)
	if err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err := acquireImmediateTransactionLock(tx); err != nil {
		return result, err
	}
	if err := subdriverConsumerTx(tx, consumerID, driverGeneration); err != nil {
		return result, err
	}
	nowTime := time.Now().UTC()
	nowStamp := stamp(nowTime)
	result.Reclaimed, err = reclaimExpiredNotificationsTx(tx, nowStamp, consumerID)
	if err != nil {
		return result, err
	}
	result.Superseded, err = supersedeStaleNotificationsTx(tx, nowStamp)
	if err != nil {
		return result, err
	}

	query := notificationSelect + ` WHERE d.state='pending' AND d.target_driver_id=? AND ` + reportDoneNotificationPresentable
	args := []any{consumerID}
	if taskID != "" {
		query += ` AND d.task_id=?`
		args = append(args, taskID)
	}
	query += ` ORDER BY d.created_at, d.message_id`
	rows, err := tx.Query(query, args...)
	if err != nil {
		return result, err
	}
	pending := make([]model.DriverNotification, 0)
	for rows.Next() {
		notification, scanErr := scanDriverNotification(rows)
		if scanErr != nil {
			rows.Close()
			return result, scanErr
		}
		pending = append(pending, notification)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	for index := range pending {
		pending[index].ReportLifecycle, err = reportLifecycleInvocationsForMessage(tx, pending[index].MessageID)
		if err != nil {
			return result, err
		}
	}
	groups := make(map[string][]model.DriverNotification)
	order := make([]string, 0)
	for _, notification := range pending {
		key := notification.TaskID
		if notification.RequestID != "" {
			key = "request:" + notification.RequestID
		}
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], notification)
	}
	selected := make([]model.DriverNotification, 0, limit)
	for _, groupTaskID := range order {
		blocked, blockErr := taskHasActiveClaimTx(tx, groupTaskID, nowStamp)
		if blockErr != nil {
			return result, blockErr
		}
		if blocked {
			continue
		}
		for i, notification := range groups[groupTaskID] {
			if i >= 2 || len(selected) >= limit {
				break
			}
			selected = append(selected, notification)
		}
		if len(selected) >= limit {
			break
		}
	}
	claimUntil := nowTime.Add(ttl)
	for i := range selected {
		token, tokenErr := notificationToken()
		if tokenErr != nil {
			return result, tokenErr
		}
		untilStamp := stamp(claimUntil)
		if _, err := tx.Exec(`UPDATE driver_notifications SET state='claimed', updated_at=?, claim_owner=?, driver_generation=?, claim_token=?, claimed_at=?, claim_until=?, delivery_attempts=delivery_attempts+1 WHERE notification_id=? AND state='pending'`,
			nowStamp, consumerID, driverGeneration, token, nowStamp, untilStamp, selected[i].NotificationID); err != nil {
			return result, err
		}
		if err := appendNotificationLogTx(tx, selected[i].NotificationID, "claim", consumerID, driverGeneration, token, "", "claimed", ""); err != nil {
			return result, err
		}
		selected[i].State = model.NotificationClaimed
		selected[i].UpdatedAt = nowTime
		selected[i].ClaimOwner = consumerID
		selected[i].DriverGeneration = driverGeneration
		selected[i].ClaimToken = token
		selected[i].ClaimedAt = timePointer(nowStamp)
		selected[i].ClaimUntil = timePointer(untilStamp)
		selected[i].DeliveryAttempts++
		selected[i].Claim = &model.NotificationClaim{ConsumerID: consumerID, DriverGeneration: driverGeneration, ClaimToken: token, ClaimUntil: claimUntil}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	result.Notifications = selected
	return result, nil
}

func (s *Store) AckNotification(request model.NotificationAckRequest) (model.NotificationAckReceipt, error) {
	if err := validateAckRequest(request); err != nil {
		return model.NotificationAckReceipt{}, err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.NotificationAckReceipt{}, err
	}
	defer tx.Rollback()
	notification, err := scanDriverNotification(tx.QueryRow(notificationSelect+` WHERE d.notification_id=?`, request.NotificationID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.NotificationAckReceipt{}, fmt.Errorf("notification %q does not exist", request.NotificationID)
	}
	if err != nil {
		return model.NotificationAckReceipt{}, err
	}
	if err := subdriverConsumerTx(tx, request.ConsumerID, request.DriverGeneration); err != nil {
		return model.NotificationAckReceipt{}, err
	}
	if notification.TargetDriverID != request.ConsumerID {
		return model.NotificationAckReceipt{}, fmt.Errorf("%w: notification %s belongs to driver %s", ErrNotificationConflict, request.NotificationID, notification.TargetDriverID)
	}
	if notification.State == model.NotificationAcknowledged {
		if notification.ClaimToken != request.ClaimToken || notification.ClaimOwner != request.ConsumerID || notification.DriverGeneration != request.DriverGeneration || notification.HandlingID != request.HandlingID {
			return model.NotificationAckReceipt{}, fmt.Errorf("%w: notification %s is already acknowledged by another handling operation", ErrNotificationConflict, request.NotificationID)
		}
		return model.NotificationAckReceipt{SchemaVersion: model.NotificationAckSchemaVersion, NotificationID: notification.NotificationID, MessageID: notification.MessageID, HandlingID: notification.HandlingID, AckedAt: valueTime(notification.AckedAt), ConsumerID: notification.AckOwner, DriverGeneration: notification.AckDriverGeneration, Idempotent: true}, tx.Commit()
	}
	if notification.State != model.NotificationClaimed {
		return model.NotificationAckReceipt{}, fmt.Errorf("%w: notification %s is %s", ErrNotificationConflict, request.NotificationID, notification.State)
	}
	if notification.ClaimToken != request.ClaimToken || notification.ClaimOwner != request.ConsumerID || notification.DriverGeneration != request.DriverGeneration {
		return model.NotificationAckReceipt{}, fmt.Errorf("%w: notification %s claim identity does not match", ErrNotificationConflict, request.NotificationID)
	}
	nowTime := time.Now().UTC()
	if notification.ClaimUntil == nil || !nowTime.Before(*notification.ClaimUntil) {
		return model.NotificationAckReceipt{}, fmt.Errorf("%w: notification %s claim expired", ErrNotificationExpired, request.NotificationID)
	}
	current, err := currentNotificationIdentityTx(tx, notification.TaskID, notification.AttemptID, notification.SubdriverEventID)
	if err != nil {
		return model.NotificationAckReceipt{}, err
	}
	if current.currentAttemptID != notification.AttemptID || current.runGeneration != notification.WorkerRunGeneration {
		return model.NotificationAckReceipt{}, fmt.Errorf("%w: notification %s no longer matches the current worker identity", ErrNotificationStale, request.NotificationID)
	}
	if current.released {
		preserved, err := currentAfterReleaseTx(tx, notification.NotificationID)
		if err != nil {
			return model.NotificationAckReceipt{}, err
		}
		if !preserved {
			return model.NotificationAckReceipt{}, fmt.Errorf("%w: notification %s no longer matches the current worker identity", ErrNotificationStale, request.NotificationID)
		}
	}
	ackedAt := nowTime
	result, err := tx.Exec(`UPDATE driver_notifications SET state='acknowledged', updated_at=?, acked_at=?, ack_owner=?, ack_driver_generation=?, handling_id=? WHERE notification_id=? AND state='claimed' AND claim_owner=? AND driver_generation=? AND claim_token=? AND claim_until>?`,
		stamp(ackedAt), stamp(ackedAt), request.ConsumerID, request.DriverGeneration, request.HandlingID, request.NotificationID, request.ConsumerID, request.DriverGeneration, request.ClaimToken, stamp(ackedAt))
	if err != nil {
		return model.NotificationAckReceipt{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return model.NotificationAckReceipt{}, fmt.Errorf("%w: notification %s changed before acknowledgement", ErrNotificationConflict, request.NotificationID)
	}
	if err := appendNotificationLogTx(tx, request.NotificationID, "ack", request.ConsumerID, request.DriverGeneration, request.ClaimToken, request.HandlingID, "acknowledged", ""); err != nil {
		return model.NotificationAckReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.NotificationAckReceipt{}, err
	}
	return model.NotificationAckReceipt{SchemaVersion: model.NotificationAckSchemaVersion, NotificationID: request.NotificationID, MessageID: notification.MessageID, HandlingID: request.HandlingID, AckedAt: ackedAt, ConsumerID: request.ConsumerID, DriverGeneration: request.DriverGeneration}, nil
}

func (s *Store) RenewNotification(request model.NotificationRenewRequest, requestedTTL ...time.Duration) (model.DriverNotification, error) {
	if err := validateRenewRequest(request); err != nil {
		return model.DriverNotification{}, err
	}
	ttl, err := notificationTTL(requestedTTL...)
	if err != nil {
		return model.DriverNotification{}, err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.DriverNotification{}, err
	}
	defer tx.Rollback()
	notification, err := scanDriverNotification(tx.QueryRow(notificationSelect+` WHERE d.notification_id=?`, request.NotificationID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.DriverNotification{}, fmt.Errorf("notification %q does not exist", request.NotificationID)
	}
	if err != nil {
		return model.DriverNotification{}, err
	}
	if err := subdriverConsumerTx(tx, request.ConsumerID, request.DriverGeneration); err != nil {
		return model.DriverNotification{}, err
	}
	if notification.TargetDriverID != request.ConsumerID {
		return model.DriverNotification{}, fmt.Errorf("%w: notification %s belongs to driver %s", ErrNotificationConflict, request.NotificationID, notification.TargetDriverID)
	}
	if notification.State != model.NotificationClaimed || notification.ClaimOwner != request.ConsumerID || notification.DriverGeneration != request.DriverGeneration || notification.ClaimToken != request.ClaimToken {
		return model.DriverNotification{}, fmt.Errorf("%w: notification %s claim identity does not match", ErrNotificationConflict, request.NotificationID)
	}
	nowTime := time.Now().UTC()
	if notification.ClaimUntil == nil || !nowTime.Before(*notification.ClaimUntil) {
		return model.DriverNotification{}, fmt.Errorf("%w: notification %s claim expired", ErrNotificationExpired, request.NotificationID)
	}
	current, err := currentNotificationIdentityTx(tx, notification.TaskID, notification.AttemptID, notification.SubdriverEventID)
	if err != nil {
		return model.DriverNotification{}, err
	}
	if current.currentAttemptID != notification.AttemptID || current.runGeneration != notification.WorkerRunGeneration {
		return model.DriverNotification{}, fmt.Errorf("%w: notification %s no longer matches the current worker identity", ErrNotificationStale, request.NotificationID)
	}
	if current.released {
		preserved, err := currentAfterReleaseTx(tx, notification.NotificationID)
		if err != nil {
			return model.DriverNotification{}, err
		}
		if !preserved {
			return model.DriverNotification{}, fmt.Errorf("%w: notification %s no longer matches the current worker identity", ErrNotificationStale, request.NotificationID)
		}
	}
	until := nowTime.Add(ttl)
	if _, err := tx.Exec(`UPDATE driver_notifications SET updated_at=?, claim_until=? WHERE notification_id=? AND state='claimed' AND claim_owner=? AND driver_generation=? AND claim_token=? AND claim_until>?`, stamp(nowTime), stamp(until), request.NotificationID, request.ConsumerID, request.DriverGeneration, request.ClaimToken, stamp(nowTime)); err != nil {
		return model.DriverNotification{}, err
	}
	if err := appendNotificationLogTx(tx, request.NotificationID, "renew", request.ConsumerID, request.DriverGeneration, request.ClaimToken, "", "renewed", ""); err != nil {
		return model.DriverNotification{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.DriverNotification{}, err
	}
	notification.UpdatedAt = nowTime
	notification.ClaimUntil = timePointer(stamp(until))
	notification.Claim = &model.NotificationClaim{ConsumerID: request.ConsumerID, DriverGeneration: request.DriverGeneration, ClaimToken: request.ClaimToken, ClaimUntil: until}
	notification.ReportLifecycle, err = reportLifecycleInvocationsForMessage(s.db, notification.MessageID)
	return notification, err
}

func (s *Store) SupersedeNotificationsForAttempt(attemptID, reason string) (int, error) {
	if strings.TrimSpace(attemptID) == "" {
		return 0, fmt.Errorf("attempt ID must not be empty")
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	count, err := supersedeNotificationsTx(tx, `d.attempt_id=?`, []any{attemptID}, reason)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) SupersedeNotificationsForRun(attemptID string, generation int, reason string) (int, error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	count, err := supersedeNotificationsTx(tx, `d.attempt_id=? AND d.worker_run_generation=?`, []any{attemptID, generation}, reason)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) RecordNotificationDelivery(log model.NotificationDeliveryLog) error {
	if log.NotificationID == "" {
		return fmt.Errorf("notification ID must not be empty")
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := appendNotificationLogTx(tx, log.NotificationID, log.Operation, log.ConsumerID, log.DriverGeneration, log.ClaimToken, log.HandlingID, log.Result, log.Detail); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) NotificationDeliveryLogs(notificationID string, limit int) ([]model.NotificationDeliveryLog, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	query := `SELECT id, notification_id, target_driver_id, operation, consumer_id, driver_generation, claim_token, handling_id, result, detail, created_at FROM notification_delivery_log`
	args := []any{}
	if notificationID != "" {
		query += ` WHERE notification_id=?`
		args = append(args, notificationID)
	}
	query += ` ORDER BY id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs := make([]model.NotificationDeliveryLog, 0)
	for rows.Next() {
		var log model.NotificationDeliveryLog
		var created string
		if err := rows.Scan(&log.ID, &log.NotificationID, &log.TargetDriverID, &log.Operation, &log.ConsumerID, &log.DriverGeneration, &log.ClaimToken, &log.HandlingID, &log.Result, &log.Detail, &created); err != nil {
			return nil, err
		}
		log.CreatedAt = parseTime(created)
		logs = append(logs, log)
	}
	return logs, rows.Err()
}

func (s *Store) NotificationCounts() (model.NotificationCounts, error) {
	rows, err := s.db.Query(`SELECT state, COUNT(*) FROM driver_notifications GROUP BY state`)
	if err != nil {
		return model.NotificationCounts{}, err
	}
	defer rows.Close()
	var counts model.NotificationCounts
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			return counts, err
		}
		switch model.NotificationState(state) {
		case model.NotificationPending:
			counts.Pending = count
		case model.NotificationClaimed:
			counts.Claimed = count
		case model.NotificationAcknowledged:
			counts.Acknowledged = count
		case model.NotificationSuperseded:
			counts.Superseded = count
		}
	}
	if err := rows.Err(); err != nil {
		return counts, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM driver_notifications WHERE state='claimed' AND claim_until<=?`, now()).Scan(&counts.Expired); err != nil {
		return counts, err
	}
	return counts, nil
}

func publishNotificationTx(tx *sql.Tx, message model.Message) error {
	if !message.Wake {
		return nil
	}
	result, err := tx.Exec(`INSERT INTO driver_notifications(notification_id, message_id, task_id, attempt_id, target_driver_id, worker_run_generation, source_cursor, kind, state, created_at, updated_at)
		SELECT ?, ?, ?, ?, driver_id, ?, ?, ?, 'pending', ?, ? FROM tasks WHERE id=?`, "wake:"+fmt.Sprint(message.ID), message.ID, message.TaskID, message.AttemptID, message.RunGeneration, message.SourceCursor, message.Type, stamp(message.CreatedAt), stamp(message.CreatedAt), message.TaskID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("task %s has no notification owner", message.TaskID)
	}
	return nil
}

func publishSettledNotificationTx(tx *sql.Tx, attemptID string, generation int, settlementReason string) error {
	var exists int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM driver_notifications WHERE attempt_id=? AND worker_run_generation=? AND kind=?)`, attemptID, generation, model.SettledMessageType).Scan(&exists); err != nil || exists != 0 {
		return err
	}
	var taskID, terminalKind, terminalState string
	var sourceCursor int64
	err := tx.QueryRow(`SELECT m.task_id, m.type, m.source_cursor, d.state
		FROM messages m JOIN driver_notifications d ON d.message_id=m.id
		WHERE m.attempt_id=? AND m.run_generation=? AND m.type IN ('question','done','blocked','failed')
			AND m.stale=0 AND m.wake=1
		ORDER BY m.id DESC LIMIT 1`, attemptID, generation).Scan(&taskID, &terminalKind, &sourceCursor, &terminalState)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if terminalState != string(model.NotificationAcknowledged) && terminalState != string(model.NotificationSuperseded) {
		return nil
	}
	attention, err := classifyTaskAttentionTx(tx, taskID, "", time.Now().UTC())
	if err != nil {
		return err
	}
	if !actionableTaskAttention(attention) {
		return nil
	}
	payload := boundedNotificationText(fmt.Sprintf("Runner settlement recorded for task %s attempt %s run generation %d after accepted %s (%s). Actionable residue remains (%s/%s). This wake is evidence, not authority. Inspect before disposition: shephrd task inspect %s --json; shephrd task obligations --all-drivers --json.",
		model.ShellQuote(taskID), model.ShellQuote(attemptID), generation, terminalKind, settlementReason, attention.Bucket, attention.Kind, model.ShellQuote(taskID)), maxSettledNotificationPayload)
	message, err := insertMessage(tx, taskID, attemptID, "system", model.SettledMessageType, payload, "", false, true, sourceCursor, generation, "")
	if err != nil {
		return err
	}
	return publishNotificationTx(tx, message)
}

func scanDriverNotification(scanner interface{ Scan(...any) error }) (model.DriverNotification, error) {
	var notification model.DriverNotification
	var state string
	var claimedAt, claimUntil, ackedAt, supersededAt sql.NullString
	var created, updated string
	if err := scanner.Scan(&notification.NotificationID, &notification.MessageID, &notification.TaskID, &notification.TaskTitle, &notification.FeatureKey,
		&notification.TaskLabel, &notification.AttemptID, &notification.TargetDriverID, &notification.WorkerRunGeneration, &notification.SourceCursor, &notification.Kind, &state,
		&notification.Payload, &notification.Artifact, &created, &updated, &notification.ClaimOwner, &notification.DriverGeneration,
		&notification.ClaimToken, &claimedAt, &claimUntil, &notification.DeliveryAttempts, &ackedAt, &notification.AckOwner,
		&notification.AckDriverGeneration, &notification.HandlingID, &supersededAt, &notification.SupersedeReason, &notification.SubdriverEventID, &notification.RequestID, &notification.SubdriverID, &notification.SubdriverRepoName); err != nil {
		return model.DriverNotification{}, err
	}
	if notification.SubdriverEventID != 0 {
		notification.Kind = strings.Replace(notification.Kind, "coordinator-", "subdriver-", 1)
	}
	notification.State = model.NotificationState(state)
	notification.CreatedAt, notification.UpdatedAt = parseTime(created), parseTime(updated)
	notification.ClaimedAt = nullableTimePointer(claimedAt)
	notification.ClaimUntil = nullableTimePointer(claimUntil)
	notification.AckedAt = nullableTimePointer(ackedAt)
	notification.SupersededAt = nullableTimePointer(supersededAt)
	return notification, nil
}

func reclaimExpiredNotificationsTx(tx *sql.Tx, nowStamp, targetDriverID string) (int, error) {
	rows, err := tx.Query(`SELECT notification_id, claim_owner, driver_generation, claim_token FROM driver_notifications WHERE state='claimed' AND claim_until IS NOT NULL AND claim_until<=? AND target_driver_id=?`, nowStamp, targetDriverID)
	if err != nil {
		return 0, err
	}
	var notifications []struct{ id, owner, generation, token string }
	for rows.Next() {
		var item struct{ id, owner, generation, token string }
		if err := rows.Scan(&item.id, &item.owner, &item.generation, &item.token); err != nil {
			rows.Close()
			return 0, err
		}
		notifications = append(notifications, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, item := range notifications {
		if _, err := tx.Exec(`UPDATE driver_notifications SET state='pending', updated_at=?, claim_owner='', driver_generation='', claim_token='', claimed_at=NULL, claim_until=NULL WHERE notification_id=? AND state='claimed' AND claim_until<=?`, nowStamp, item.id, nowStamp); err != nil {
			return 0, err
		}
		if err := appendNotificationLogTx(tx, item.id, "reclaim", item.owner, item.generation, item.token, "", "reclaimed", "expired claim"); err != nil {
			return 0, err
		}
	}
	return len(notifications), rows.Err()
}

func supersedeStaleNotificationsTx(tx *sql.Tx, nowStamp string) (int, error) {
	rows, err := tx.Query(`SELECT d.notification_id, d.claim_owner, d.driver_generation, d.claim_token,
		CASE WHEN COALESCE(t.current_attempt_id,'')<>d.attempt_id THEN 'current attempt changed'
			WHEN a.released_at IS NOT NULL THEN 'attempt released'
			WHEN a.run_generation<>d.worker_run_generation THEN 'worker run generation changed'
			ELSE 'worker identity is no longer current' END
		FROM driver_notifications d JOIN tasks t ON t.id=d.task_id JOIN attempts a ON a.id=d.attempt_id
		WHERE d.state IN ('pending','claimed') AND (COALESCE(t.current_attempt_id,'')<>d.attempt_id
			OR a.run_generation<>d.worker_run_generation OR (a.released_at IS NOT NULL AND NOT (` + currentReleasedReportDoneNotification + `)))`)
	if err != nil {
		return 0, err
	}
	var items []struct{ id, owner, generation, token, reason string }
	for rows.Next() {
		var item struct{ id, owner, generation, token, reason string }
		if err := rows.Scan(&item.id, &item.owner, &item.generation, &item.token, &item.reason); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	superseded := 0
	for _, item := range items {
		if item.reason == "attempt released" {
			preserved, err := settledNotificationCurrentAfterReleaseTx(tx, item.id)
			if err != nil {
				return 0, err
			}
			if preserved {
				continue
			}
		}
		if err := supersedeNotificationTx(tx, item.id, item.reason, nowStamp, item.owner, item.generation, item.token); err != nil {
			return 0, err
		}
		superseded++
	}
	return superseded, rows.Err()
}

func supersedeNotificationsTx(tx *sql.Tx, predicate string, args []any, reason string) (int, error) {
	reason = boundedNotificationText(reason, 512)
	rows, err := tx.Query(`SELECT d.notification_id, d.claim_owner, d.driver_generation, d.claim_token FROM driver_notifications d WHERE d.state IN ('pending','claimed') AND `+predicate, args...)
	if err != nil {
		return 0, err
	}
	var items []struct{ id, owner, generation, token string }
	for rows.Next() {
		var item struct{ id, owner, generation, token string }
		if err := rows.Scan(&item.id, &item.owner, &item.generation, &item.token); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	timestamp := now()
	for _, item := range items {
		if err := supersedeNotificationTx(tx, item.id, reason, timestamp, item.owner, item.generation, item.token); err != nil {
			return 0, err
		}
	}
	return len(items), rows.Err()
}

func supersedeNotificationTx(tx *sql.Tx, notificationID, reason, timestamp, owner, generation, token string) error {
	if _, err := tx.Exec(`UPDATE driver_notifications SET state='superseded', updated_at=?, superseded_at=?, supersede_reason=? WHERE notification_id=? AND state IN ('pending','claimed')`, timestamp, timestamp, reason, notificationID); err != nil {
		return err
	}
	return appendNotificationLogTx(tx, notificationID, "supersede", owner, generation, token, "", "superseded", reason)
}

func appendNotificationLogTx(tx *sql.Tx, notificationID, operation, consumerID, driverGeneration, claimToken, handlingID, result, detail string) error {
	if !validNotificationOperation(operation) {
		return fmt.Errorf("invalid notification delivery operation %q", operation)
	}
	_, err := tx.Exec(`INSERT INTO notification_delivery_log(notification_id, target_driver_id, operation, consumer_id, driver_generation, claim_token, handling_id, result, detail, created_at)
		SELECT notification_id, target_driver_id, ?, ?, ?, ?, ?, ?, ?, ? FROM driver_notifications WHERE notification_id=?`, operation, boundedNotificationText(consumerID, MaxNotificationConsumerID), boundedNotificationText(driverGeneration, MaxNotificationConsumerID), boundedNotificationText(claimToken, 512), boundedNotificationText(handlingID, MaxNotificationHandlingID), boundedNotificationText(result, 128), boundedNotificationText(detail, 512), now(), notificationID)
	return err
}

func taskHasActiveClaimTx(tx *sql.Tx, taskID, nowStamp string) (bool, error) {
	if request, ok := strings.CutPrefix(taskID, "request:"); ok {
		var exists bool
		err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM driver_notifications d JOIN coordinator_events e ON e.id=d.coordinator_event_id WHERE e.request_id=? AND d.state='claimed' AND d.claim_until>?)`, request, nowStamp).Scan(&exists)
		return exists, err
	}
	var exists int
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM driver_notifications WHERE task_id=? AND state='claimed' AND claim_until>?)`, taskID, nowStamp).Scan(&exists)
	return exists != 0, err
}

type notificationIdentity struct {
	currentAttemptID string
	runGeneration    int
	released         bool
}

func currentNotificationIdentityTx(tx *sql.Tx, taskID, attemptID string, subdriverEventID ...int64) (notificationIdentity, error) {
	if len(subdriverEventID) > 0 && subdriverEventID[0] > 0 {
		var id int64
		err := tx.QueryRow(`SELECT id FROM coordinator_events WHERE id=?`, subdriverEventID[0]).Scan(&id)
		return notificationIdentity{}, err
	}
	var identity notificationIdentity
	var released sql.NullString
	if err := tx.QueryRow(`SELECT COALESCE(t.current_attempt_id,''), a.run_generation, a.released_at FROM tasks t JOIN attempts a ON a.id=? WHERE t.id=? AND a.task_id=t.id`, attemptID, taskID).Scan(&identity.currentAttemptID, &identity.runGeneration, &released); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return identity, fmt.Errorf("%w: notification identity no longer exists", ErrNotificationStale)
		}
		return identity, err
	}
	identity.released = released.Valid
	return identity, nil
}

func currentAfterReleaseTx(tx *sql.Tx, notificationID string) (bool, error) {
	var current int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM driver_notifications d WHERE d.notification_id=? AND (`+currentReleasedReportDoneNotification+`))`, notificationID).Scan(&current); err != nil || current != 0 {
		return current != 0, err
	}
	return settledNotificationCurrentAfterReleaseTx(tx, notificationID)
}

func settledNotificationCurrentAfterReleaseTx(tx *sql.Tx, notificationID string) (bool, error) {
	var taskID string
	err := tx.QueryRow(`SELECT d.task_id FROM driver_notifications d
		JOIN tasks t ON t.id=d.task_id AND COALESCE(t.current_attempt_id,'')=d.attempt_id
		JOIN attempts a ON a.id=d.attempt_id AND a.task_id=t.id AND a.run_generation=d.worker_run_generation
		WHERE d.notification_id=? AND d.kind=? AND a.released_at IS NOT NULL`, notificationID, model.SettledMessageType).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	attention, err := classifyTaskAttentionTx(tx, taskID, notificationID, time.Now().UTC())
	if err != nil {
		return false, err
	}
	return actionableTaskAttention(attention), nil
}

func supersedeReleasedAttemptNotificationsTx(tx *sql.Tx, attemptID, reason string) error {
	if _, err := supersedeNotificationsTx(tx, `d.attempt_id=? AND d.kind<>? AND NOT (`+currentReleasedReportDoneNotification+`)`, []any{attemptID, model.SettledMessageType}, reason); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT notification_id, claim_owner, driver_generation, claim_token FROM driver_notifications WHERE attempt_id=? AND kind=? AND state IN ('pending','claimed')`, attemptID, model.SettledMessageType)
	if err != nil {
		return err
	}
	var items []struct{ id, owner, generation, token string }
	for rows.Next() {
		var item struct{ id, owner, generation, token string }
		if err := rows.Scan(&item.id, &item.owner, &item.generation, &item.token); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range items {
		preserved, err := settledNotificationCurrentAfterReleaseTx(tx, item.id)
		if err != nil {
			return err
		}
		if !preserved {
			if err := supersedeNotificationTx(tx, item.id, reason, now(), item.owner, item.generation, item.token); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}

func validateDriverID(driverID string) error {
	if strings.TrimSpace(driverID) == "" || len([]byte(driverID)) > MaxNotificationConsumerID {
		return fmt.Errorf("driver ID must be between 1 and %d bytes", MaxNotificationConsumerID)
	}
	return nil
}

func validateConsumer(consumerID, generation string) error {
	if err := validateDriverID(consumerID); err != nil {
		return err
	}
	if strings.TrimSpace(generation) == "" || len([]byte(generation)) > MaxNotificationConsumerID {
		return fmt.Errorf("driver generation must be between 1 and %d bytes", MaxNotificationConsumerID)
	}
	return nil
}

func validateAckRequest(request model.NotificationAckRequest) error {
	if request.NotificationID == "" || request.ClaimToken == "" {
		return fmt.Errorf("notification ID and claim token are required")
	}
	if request.HandlingID == "" || len([]byte(request.HandlingID)) > MaxNotificationHandlingID {
		return fmt.Errorf("handling ID must be between 1 and %d bytes", MaxNotificationHandlingID)
	}
	return validateConsumer(request.ConsumerID, request.DriverGeneration)
}

func validateRenewRequest(request model.NotificationRenewRequest) error {
	if request.NotificationID == "" || request.ClaimToken == "" {
		return fmt.Errorf("notification ID and claim token are required")
	}
	return validateConsumer(request.ConsumerID, request.DriverGeneration)
}

func notificationTTL(requested ...time.Duration) (time.Duration, error) {
	ttl := DefaultNotificationClaimTTL
	if len(requested) > 1 {
		return 0, fmt.Errorf("only one notification claim TTL may be supplied")
	}
	if len(requested) == 1 && requested[0] != 0 {
		ttl = requested[0]
	}
	if ttl < MinNotificationClaimTTL || ttl > MaxNotificationClaimTTL {
		return 0, fmt.Errorf("notification claim TTL must be between %s and %s", MinNotificationClaimTTL, MaxNotificationClaimTTL)
	}
	return ttl, nil
}

func notificationToken() (string, error) {
	bytes := make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate notification claim token: %w", err)
	}
	return fmt.Sprintf("%x", bytes), nil
}

func validNotificationOperation(operation string) bool {
	switch operation {
	case "claim", "reclaim", "renew", "notify", "ack", "adopt", "supersede", "dedupe":
		return true
	default:
		return false
	}
}

func boundedNotificationText(value string, max int) string {
	value = strings.TrimSpace(value)
	if len([]byte(value)) <= max {
		return value
	}
	return string([]byte(value)[:max])
}

func nullableTimePointer(value sql.NullString) *time.Time {
	if !value.Valid || value.String == "" {
		return nil
	}
	parsed := parseTime(value.String)
	return &parsed
}

func timePointer(value string) *time.Time {
	parsed := parseTime(value)
	return &parsed
}

func valueTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}
