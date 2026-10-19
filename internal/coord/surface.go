package coord

import (
	"crypto/sha256"
	"database/sql"
	"errors"

	"shephrd/internal/fault"
	"shephrd/internal/store"
)

type SurfaceInput struct {
	ID      int64  `json:"id"`
	Surface string `json:"surface"`
	Thread  string `json:"thread"`
	Body    string `json:"body"`
	Time    string `json:"time"`
}

type SurfaceReply struct {
	ID        int64  `json:"id"`
	Input     int64  `json:"input,omitempty"`
	InboxItem int64  `json:"inbox_item,omitempty"`
	Turn      int64  `json:"turn,omitempty"`
	Body      string `json:"body"`
	Time      string `json:"time"`
}

func SubmitSurface(tx *store.Tx, surface, thread, body string) (SurfaceInput, error) {
	var pending int
	if err := tx.QueryRow(`SELECT count(*) FROM surface_inputs WHERE answered = 0`).Scan(&pending); err != nil {
		return SurfaceInput{}, err
	}
	if pending >= 1024 {
		return SurfaceInput{}, fault.New("mailbox_full", "the coordinator input mailbox is full")
	}
	time := store.Timestamp(tx.Now)
	result, err := tx.Exec(`INSERT INTO surface_inputs(surface, thread, body, time) VALUES (?, ?, ?, ?)`, surface, thread, body, time)
	if err != nil {
		return SurfaceInput{}, err
	}
	id, err := result.LastInsertId()
	return SurfaceInput{ID: id, Surface: surface, Thread: thread, Body: body, Time: time}, err
}

func PendingSurface(q Querier, after int64) ([]SurfaceInput, error) {
	rows, err := q.Query(`SELECT id, surface, thread, body, time FROM surface_inputs WHERE answered = 0 AND id > ? ORDER BY id LIMIT 100`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []SurfaceInput{}
	for rows.Next() {
		var item SurfaceInput
		if err := rows.Scan(&item.ID, &item.Surface, &item.Thread, &item.Body, &item.Time); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func ReplySurface(tx *store.Tx, input int64, body string) (SurfaceReply, error) {
	var answered int
	err := tx.QueryRow(`SELECT answered FROM surface_inputs WHERE id = ?`, input).Scan(&answered)
	if errors.Is(err, sql.ErrNoRows) {
		return SurfaceReply{}, fault.New("not_found", "no pending surface input %d", input)
	}
	if err != nil {
		return SurfaceReply{}, err
	}
	if answered != 0 {
		return SurfaceReply{}, fault.New("already_answered", "surface input %d has a reply", input)
	}
	var replies int
	if err := tx.QueryRow(`SELECT count(*) FROM surface_replies`).Scan(&replies); err != nil {
		return SurfaceReply{}, err
	}
	if replies >= 1024 {
		return SurfaceReply{}, fault.New("mailbox_full", "the coordinator reply mailbox is full")
	}
	time := store.Timestamp(tx.Now)
	result, err := tx.Exec(`INSERT INTO surface_replies(surface, thread, input, body, time) SELECT surface, thread, ?, ?, ? FROM surface_inputs WHERE id = ?`, input, body, time, input)
	if err != nil {
		return SurfaceReply{}, err
	}
	if _, err := tx.Exec(`UPDATE surface_inputs SET answered = 1 WHERE id = ?`, input); err != nil {
		return SurfaceReply{}, err
	}
	id, err := result.LastInsertId()
	return SurfaceReply{ID: id, Input: input, Body: body, Time: time}, err
}

func RouteInbox(tx *store.Tx, surface, thread string, item, turn int64, body string) error {
	digest := sha256.Sum256([]byte(body))
	var existing int64
	err := tx.QueryRow(`SELECT turn FROM routed_inbox_outputs WHERE turn = ? AND inbox_item = ? AND surface = ? AND thread = ? AND digest = ?`, turn, item, surface, thread, digest[:]).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := tx.QueryRow(`SELECT count(*) FROM surface_replies`).Scan(&existing); err != nil {
		return err
	}
	if existing >= 1024 {
		return fault.New("mailbox_full", "the coordinator reply mailbox is full")
	}
	if _, err = tx.Exec(`INSERT INTO routed_inbox_outputs(turn, inbox_item, surface, thread, digest) VALUES (?, ?, ?, ?, ?)`, turn, item, surface, thread, digest[:]); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO surface_replies(surface, thread, inbox_item, turn, body, time) VALUES (?, ?, ?, ?, ?, ?)`, surface, thread, item, turn, body, store.Timestamp(tx.Now))
	return err
}

func AckSurface(tx *store.Tx, surface, thread string, cursor int64) (int64, error) {
	var saved int64
	err := tx.QueryRow(`SELECT cursor FROM surface_cursors WHERE surface = ? AND thread = ?`, surface, thread).Scan(&saved)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if cursor <= saved {
		return saved, nil
	}
	var exists int64
	if err := tx.QueryRow(`SELECT id FROM surface_replies WHERE surface = ? AND thread = ? AND id = ?`, surface, thread, cursor).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return 0, fault.New("invalid_cursor", "cursor does not name an output in this surface and thread")
	} else if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO surface_cursors(surface, thread, cursor) VALUES (?, ?, ?) ON CONFLICT(surface, thread) DO UPDATE SET cursor = excluded.cursor`, surface, thread, cursor); err != nil {
		return 0, err
	}
	_, err = tx.Exec(`DELETE FROM surface_replies WHERE surface = ? AND thread = ? AND id <= ?`, surface, thread, cursor)
	return cursor, err
}

func SurfaceReplies(q Querier, surface, thread string, after int64) ([]SurfaceReply, error) {
	rows, err := q.Query(`SELECT id, COALESCE(input, 0), COALESCE(inbox_item, 0), COALESCE(turn, 0), body, time FROM surface_replies
		WHERE surface = ? AND thread = ? AND id > ? ORDER BY id LIMIT 100`, surface, thread, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	replies := []SurfaceReply{}
	for rows.Next() {
		var reply SurfaceReply
		if err := rows.Scan(&reply.ID, &reply.Input, &reply.InboxItem, &reply.Turn, &reply.Body, &reply.Time); err != nil {
			return nil, err
		}
		replies = append(replies, reply)
	}
	return replies, rows.Err()
}
