package coord

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"shephrd/internal/fault"
	"shephrd/internal/store"
)

type Item struct {
	ID        int64               `json:"id"`
	Driver    string              `json:"driver"`
	TaskID    int64               `json:"-"`
	Task      string              `json:"task"`
	TaskTitle string              `json:"title"`
	TaskState string              `json:"task_state"`
	Reason    string              `json:"reason,omitempty"`
	Event     store.Event         `json:"event"`
	State     string              `json:"state"`
	AckedBy   string              `json:"acked_by,omitempty"`
	Delivery  Delivery            `json:"delivery"`
	Created   string              `json:"created"`
	Next      map[string][]string `json:"next"`
}

type Delivery struct {
	State    string `json:"state,omitempty"`
	Attempts int    `json:"attempts"`
	Detail   string `json:"detail,omitempty"`
	Next     string `json:"next,omitempty"`
}

// AddItem records that a driver's root task needs its attention.
func AddItem(tx *store.Tx, driver string, t *Task, seq int64) error {
	result, err := tx.Exec(`INSERT OR IGNORE INTO inbox_items (driver, task, event, state, created_at) VALUES (?, ?, ?, 'pending', ?)`,
		driver, t.ID, seq, store.Timestamp(tx.Now))
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return nil
	}
	id, err := result.LastInsertId()
	if err != nil {
		return err
	}
	_, err = tx.Emit("inbox.added", "system", t.ID, 0, 0, map[string]any{"driver": driver, "item": id, "task": t.Ref, "event": seq})
	return err
}

func AckItem(tx *store.Tx, c Caller, id int64) (*Item, error) {
	items, err := Items(tx, `i.id = ? AND i.driver = ?`, id, c.String())
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fault.New("not_found", "no inbox item %d belongs to %s", id, c).WithNext("inbox")
	}
	item := items[0]
	if item.State == "acked" {
		return item, nil
	}
	if err := ack(tx, item, c.String(), false); err != nil {
		return nil, err
	}
	item.State, item.AckedBy = "acked", c.String()
	return item, nil
}

// AckTask acknowledges a closing task's pending items for its driver.
func AckTask(tx *store.Tx, t *Task) error {
	items, err := Items(tx, `i.task = ? AND i.state = 'pending'`, t.ID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if err := ack(tx, item, "system", true); err != nil {
			return err
		}
	}
	return nil
}

func ack(tx *store.Tx, item *Item, by string, system bool) error {
	if _, err := tx.Exec(`UPDATE inbox_items SET state = 'acked', acked_by = ?, acked_at = ? WHERE id = ?`, by, store.Timestamp(tx.Now), item.ID); err != nil {
		return err
	}
	_, err := tx.Emit("inbox.acked", by, item.TaskID, 0, 0, map[string]any{"driver": item.Driver, "item": item.ID, "system": system})
	return err
}

func Items(q Querier, where string, args ...any) ([]*Item, error) {
	rows, err := q.Query(`SELECT i.id, i.driver, i.task, t.title, t.state, t.reason, i.state, i.acked_by, i.delivery, i.delivery_attempts,
		i.delivery_detail, COALESCE(i.next_delivery, ''), i.created_at, e.seq, e.name, e.time, COALESCE(e.attempt, 0), COALESCE(e.run, 0), e.caller, e.data
		FROM inbox_items i JOIN tasks t ON t.id = i.task JOIN events e ON e.seq = i.event WHERE `+where+` ORDER BY i.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []*Item{}
	for rows.Next() {
		var item Item
		var data string
		if err := rows.Scan(&item.ID, &item.Driver, &item.TaskID, &item.TaskTitle, &item.TaskState, &item.Reason, &item.State, &item.AckedBy,
			&item.Delivery.State, &item.Delivery.Attempts, &item.Delivery.Detail, &item.Delivery.Next, &item.Created,
			&item.Event.Seq, &item.Event.Name, &item.Event.Time, &item.Event.Attempt, &item.Event.Run, &item.Event.Caller, &data); err != nil {
			return nil, err
		}
		item.Task = TaskRef(item.TaskID)
		item.Event.Task, item.Event.TaskID, item.Event.Data = item.Task, item.TaskID, json.RawMessage(data)
		id := strconv.FormatInt(item.ID, 10)
		item.Next = map[string][]string{
			"read": {"shephrd", "task", "show", item.Task},
			"ack":  {"shephrd", "inbox", "ack", id},
		}
		if item.Event.Name == "task.question" {
			item.Next["reply"] = []string{"shephrd", "task", "send", item.Task, "-", "--reply-to", strconv.FormatInt(item.Event.Seq, 10)}
		}
		items = append(items, &item)
	}
	return items, rows.Err()
}

func lastSeq(tx *store.Tx, task int64) (int64, error) {
	var seq sql.NullInt64
	err := tx.QueryRow(`SELECT MAX(seq) FROM events WHERE task = ?`, task).Scan(&seq)
	if err == nil && !seq.Valid {
		err = errors.New("task has no events")
	}
	return seq.Int64, err
}
