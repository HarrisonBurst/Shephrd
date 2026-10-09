package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"shephrd/internal/model"
)

func (s *Store) SetSubdriverEndpoint(f model.SubdriverFence, endpoint model.TerminalEndpoint) error {
	body, err := json.Marshal(endpoint)
	if err != nil {
		return err
	}
	result, err := s.db.Exec(`UPDATE coordinators SET endpoint_json=?,updated_at=? WHERE id=? AND generation=? AND token=? AND state='starting' AND endpoint_json=''`, string(body), now(), f.ID, f.Generation, f.Token)
	return subdriverChanged(result, err)
}
func (s *Store) HoldSubdriverObservation(c model.Subdriver, reason string) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = acquireImmediateTransactionLock(tx); err != nil {
		return err
	}
	var token, checkpoint string
	err = tx.QueryRow(`SELECT token,checkpoint FROM coordinators WHERE id=? AND generation=? AND state=? AND updated_at=? AND state IN ('starting','running','idle')`, c.ID, c.Generation, c.State, stamp(c.UpdatedAt)).Scan(&token, &checkpoint)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = finishSubdriverTx(tx, model.SubdriverFence{ID: c.ID, Generation: c.Generation, Token: token}, checkpoint, reason); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) SubdriverEvent(id int64) (model.SubdriverEvent, error) {
	e, err := scanSubdriverEvent(s.db.QueryRow(subdriverEventSelect+` WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return e, model.Failure("not_found", "sub-driver event %d does not exist", id)
	}
	return e, err
}
func (s *Store) SubdriverSession(f model.SubdriverFence, session string) error {
	if session == "" {
		return fmt.Errorf("empty sub-driver harness session")
	}
	result, err := s.db.Exec(`UPDATE coordinators SET session_id=?,updated_at=? WHERE id=? AND generation=? AND token=? AND state='running'`, session, now(), f.ID, f.Generation, f.Token)
	return subdriverChanged(result, err)
}
func (s *Store) Subdrivers(driver string, offset int) ([]model.Subdriver, error) {
	if offset < 0 {
		return nil, fmt.Errorf("offset must be nonnegative")
	}
	query := subdriverSelect
	args := []any{}
	if driver != "" {
		query += ` WHERE id IN (SELECT coordinator_id FROM coordinator_requests WHERE driver_id=?)`
		args = append(args, driver)
	}
	query += ` ORDER BY id LIMIT 21 OFFSET ?`
	args = append(args, offset)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.Subdriver{}
	for rows.Next() {
		c, err := scanSubdriver(rows)
		if err != nil {
			return nil, err
		}
		c.Context = ""
		c.Checkpoint = ""
		result = append(result, c)
	}
	return result, rows.Err()
}
func (s *Store) SubdriverEventHandled(id int64) (bool, error) {
	var handled bool
	err := s.db.QueryRow(`SELECT handled FROM coordinator_events WHERE id=?`, id).Scan(&handled)
	return handled, err
}
