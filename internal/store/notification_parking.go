package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"shephrd/internal/model"
)

const (
	DefaultMaxRejectedClaims = 3
	MaxMaxRejectedClaims     = 20
	unparkedResult           = "unparked"
	unclaimedNotification    = `(d.state='pending' OR (d.state='claimed' AND d.claim_until<=?))`
)

type notificationQueryer interface {
	Query(string, ...any) (*sql.Rows, error)
}

// parkedNotifications derives watcher parking from notify rows: each watcher
// claim ends with at most one delivered, rejected or undeliverable row, and a
// delivered claim or an owner unpark marker resets the consecutive count.
func parkedNotifications(queryer notificationQueryer, owner string, threshold int) (map[string]model.ParkedNotification, error) {
	parked := make(map[string]model.ParkedNotification)
	if threshold <= 0 {
		return parked, nil
	}
	rows, err := queryer.Query(`SELECT l.notification_id, l.claim_token, l.result, l.detail, l.created_at FROM notification_delivery_log l
		JOIN driver_notifications d ON d.notification_id=l.notification_id
		WHERE l.target_driver_id=? AND d.target_driver_id=? AND `+unclaimedNotification+` AND l.operation='notify'
			AND (l.result=? OR (l.driver_generation LIKE 'watch:%' AND l.result IN ('delivered','rejected','undeliverable')))
		ORDER BY l.id`, owner, owner, stamp(time.Now().UTC()), unparkedResult)
	if err != nil {
		return nil, err
	}
	type progress struct {
		parked model.ParkedNotification
		claims map[string]bool
	}
	states := make(map[string]*progress)
	for rows.Next() {
		var id, token, result, detail, created string
		if err := rows.Scan(&id, &token, &result, &detail, &created); err != nil {
			rows.Close()
			return nil, err
		}
		state := states[id]
		if state == nil || result == unparkedResult || result == "delivered" {
			state = &progress{parked: model.ParkedNotification{NotificationID: id}, claims: make(map[string]bool)}
			states[id] = state
		}
		if result != "rejected" && result != "undeliverable" || state.claims[token] {
			continue
		}
		state.claims[token] = true
		state.parked.RejectedClaims++
		state.parked.LastResult, state.parked.LastDetail, state.parked.LastAt = result, detail, parseTime(created)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for id, state := range states {
		if state.parked.RejectedClaims >= threshold {
			parked[id] = state.parked
		}
	}
	return parked, nil
}

func (s *Store) ParkedNotifications(owner string, threshold int) ([]model.ParkedNotification, error) {
	if err := validateDriverID(owner); err != nil {
		return nil, err
	}
	parked, err := parkedNotifications(s.db, owner, threshold)
	if err != nil {
		return nil, err
	}
	result := make([]model.ParkedNotification, 0, len(parked))
	if len(parked) == 0 {
		return result, nil
	}
	rows, err := s.db.Query(notificationSelect+` WHERE `+unclaimedNotification+` AND d.target_driver_id=? ORDER BY d.created_at, d.message_id`, stamp(time.Now().UTC()), owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		notification, err := scanDriverNotification(rows)
		if err != nil {
			return nil, err
		}
		if entry, ok := parked[notification.NotificationID]; ok {
			result = append(result, describeParked(entry, notification))
		}
	}
	return result, rows.Err()
}

func (s *Store) UnparkNotification(owner, notificationID string, threshold int) (model.ParkedNotification, error) {
	if err := validateDriverID(owner); err != nil {
		return model.ParkedNotification{}, err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.ParkedNotification{}, err
	}
	defer tx.Rollback()
	if err := acquireImmediateTransactionLock(tx); err != nil {
		return model.ParkedNotification{}, err
	}
	parked, err := parkedNotifications(tx, owner, threshold)
	if err != nil {
		return model.ParkedNotification{}, err
	}
	entry, ok := parked[notificationID]
	if !ok {
		return model.ParkedNotification{}, fmt.Errorf("%w: notification %s is not parked for driver %s", ErrNotificationConflict, notificationID, owner)
	}
	notification, err := scanDriverNotification(tx.QueryRow(notificationSelect+` WHERE d.notification_id=?`, notificationID))
	if err != nil {
		return model.ParkedNotification{}, err
	}
	detail := fmt.Sprintf("unparked by %s after %d consecutive rejected or undeliverable claims; the next watcher drain delivers it under a new claim", owner, entry.RejectedClaims)
	if err := appendNotificationLogTx(tx, notificationID, "notify", owner, "", "", "", unparkedResult, detail); err != nil {
		return model.ParkedNotification{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.ParkedNotification{}, err
	}
	return describeParked(entry, notification), nil
}

func describeParked(entry model.ParkedNotification, notification model.DriverNotification) model.ParkedNotification {
	entry.Kind = notification.Kind
	entry.TaskID = notification.TaskID
	entry.RequestID = notification.RequestID
	entry.CreatedAt = notification.CreatedAt
	return entry
}
