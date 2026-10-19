package coord

import (
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
	ID    int64  `json:"id"`
	Input int64  `json:"input"`
	Body  string `json:"body"`
	Time  string `json:"time"`
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
	result, err := tx.Exec(`INSERT INTO surface_replies(input, body, time) VALUES (?, ?, ?)`, input, body, time)
	if err != nil {
		return SurfaceReply{}, err
	}
	if _, err := tx.Exec(`UPDATE surface_inputs SET answered = 1 WHERE id = ?`, input); err != nil {
		return SurfaceReply{}, err
	}
	id, err := result.LastInsertId()
	return SurfaceReply{ID: id, Input: input, Body: body, Time: time}, err
}

func SurfaceReplies(q Querier, surface, thread string, after int64) ([]SurfaceReply, error) {
	rows, err := q.Query(`SELECT r.id, r.input, r.body, r.time FROM surface_replies r
		JOIN surface_inputs i ON i.id = r.input
		WHERE i.surface = ? AND i.thread = ? AND r.id > ? ORDER BY r.id LIMIT 100`, surface, thread, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	replies := []SurfaceReply{}
	for rows.Next() {
		var reply SurfaceReply
		if err := rows.Scan(&reply.ID, &reply.Input, &reply.Body, &reply.Time); err != nil {
			return nil, err
		}
		replies = append(replies, reply)
	}
	return replies, rows.Err()
}
