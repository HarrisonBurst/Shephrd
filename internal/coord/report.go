package coord

import (
	"slices"

	"shephrd/internal/fault"
	"shephrd/internal/store"
)

const (
	MaxReport     = 8 << 10
	MaxReportNote = 4 << 10
)

var ReportKinds = []string{"progress", "question", "result", "blocker", "note"}

type Report struct {
	Kind    string
	Body    string
	Request int64
	Extra   map[string]any
}

// ReportTarget checks that a report can be made and resolves the request a
// driver's question or result answers.
func ReportTarget(q Querier, c Caller, r *Report) (*Task, error) {
	if c.Kind != "run" {
		return nil, fault.New("not_a_run", "only a session Shephrd started reports, with its run token")
	}
	if !slices.Contains(ReportKinds, r.Kind) {
		return nil, fault.New("usage", "report kinds are progress, question, result, blocker and note")
	}
	limit := MaxReport
	if r.Kind == "note" {
		limit = MaxReportNote
	}
	if len(r.Body) > limit {
		return nil, fault.New("too_large", "a %s is bounded at %d bytes; put longer material in the deliverable", r.Kind, limit)
	}
	if r.Body == "" {
		return nil, fault.New("usage", "a report needs text")
	}
	t, err := Load(q, c.Task)
	if err != nil {
		return nil, err
	}
	if t.State != "running" {
		return nil, fault.New("not_running", "%s is %s; reports are accepted only while it runs", t.Ref, t.State)
	}
	if t.Role != "driver" || (r.Kind != "question" && r.Kind != "result") {
		if r.Request != 0 {
			return nil, fault.New("usage", "--request is for a driver's question or result")
		}
		return t, nil
	}
	requests, err := Requests(q, t.ID)
	if err != nil {
		return nil, err
	}
	var open []int64
	for _, req := range requests {
		if req.State != "answered" {
			open = append(open, req.Seq)
		}
	}
	switch {
	case r.Request != 0 && !slices.Contains(open, r.Request):
		return nil, fault.New("invalid_request", "%d is not an open request on %s", r.Request, t.Ref)
	case r.Request == 0 && len(open) == 1:
		r.Request = open[0]
	case r.Request == 0:
		return nil, fault.New("request_required", "name the request this answers with --request <seq>")
	}
	return t, nil
}

// RecordReport applies a checked report: it records the event, applies the
// state change the report means and wakes the owner when it needs to know.
func RecordReport(tx *store.Tx, c Caller, t *Task, r *Report) (int64, error) {
	data := map[string]any{"body": r.Body}
	if r.Request != 0 {
		data["request"] = r.Request
	}
	for key, value := range r.Extra {
		data[key] = value
	}
	if t.Role == "driver" && r.Kind == "result" {
		children, err := Query(tx, `t.parent = ? AND t.request = ?`, t.ID, r.Request)
		if err != nil {
			return 0, err
		}
		states := []map[string]string{}
		for _, child := range children {
			states = append(states, map[string]string{"task": child.Ref, "state": child.State, "reason": child.Reason, "milestone": child.Milestone, "artifact": child.Artifact})
		}
		data["children"] = states
	}
	seq, err := tx.Emit("task."+r.Kind, c.String(), t.ID, c.Attempt, c.Run, data)
	if err != nil {
		return 0, err
	}
	now := store.Timestamp(tx.Now)
	if r.Kind == "progress" || t.Role == "worker" && r.Kind == "note" {
		_, err = tx.Exec(`UPDATE runs SET last_activity = ? WHERE id = ?`, now, c.Run)
	} else {
		endsTurn := t.Role == "worker" || r.Kind == "note" || r.Kind == "blocker"
		_, err = tx.Exec(`UPDATE runs SET last_activity = ?, turn_reported = ? WHERE id = ?`, now, endsTurn, c.Run)
	}
	if err != nil {
		return 0, err
	}
	switch {
	case r.Kind == "blocker":
		return seq, Hold(tx, t, "blocked")
	case t.Role == "worker" && r.Kind == "question":
		if err := SetState(tx, c.String(), t, "waiting", ""); err != nil {
			return 0, err
		}
		return seq, WakeOwner(tx, t, seq)
	case t.Role == "worker" && r.Kind == "result":
		if err := SetState(tx, c.String(), t, "done", ""); err != nil {
			return 0, err
		}
		return seq, WakeOwner(tx, t, seq)
	case t.Role == "driver" && r.Kind == "question":
		if _, err := tx.Exec(`UPDATE requests SET state = 'asked' WHERE task = ? AND seq = ?`, t.ID, r.Request); err != nil {
			return 0, err
		}
		return seq, WakeOwner(tx, t, seq)
	case t.Role == "driver" && r.Kind == "result":
		if _, err := tx.Exec(`UPDATE requests SET state = 'answered', result = ? WHERE task = ? AND seq = ?`, seq, t.ID, r.Request); err != nil {
			return 0, err
		}
		return seq, WakeOwner(tx, t, seq)
	}
	return seq, nil
}
