package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"shephrd/internal/model"
)

const subdriverSelect = `SELECT id,COALESCE(repo_id,''),context,generation,state,session_id,runner_pid,harness_pid,harness,model,runtime,checkpoint,failure,updated_at,endpoint_json FROM coordinators`
const subdriverRequestSelect = `SELECT id,coordinator_id,driver_id,origin_driver_id,request_key,original,context,COALESCE(lead_request_id,''),state,created_at FROM coordinator_requests`
const subdriverEventSelect = `SELECT id,request_id,event_key,kind,payload,COALESCE(reply_to,0),handled,created_at FROM coordinator_events`

func scanSubdriver(row interface{ Scan(...any) error }) (c model.Subdriver, err error) {
	var updated, endpoint string
	err = row.Scan(&c.ID, &c.RepoID, &c.Context, &c.Generation, &c.State, &c.SessionID, &c.RunnerPID, &c.HarnessPID, &c.Harness, &c.Model, &c.Runtime, &c.Checkpoint, &c.Failure, &updated, &endpoint)
	if err == nil && endpoint != "" {
		err = json.Unmarshal([]byte(endpoint), &c.Endpoint)
	}
	c.UpdatedAt = parseTime(updated)
	return
}
func scanSubdriverRequest(row interface{ Scan(...any) error }) (r model.SubdriverRequest, err error) {
	var created string
	err = row.Scan(&r.ID, &r.SubdriverID, &r.DriverID, &r.OriginDriverID, &r.Key, &r.Original, &r.Context, &r.LeadRequestID, &r.State, &created)
	r.CreatedAt = parseTime(created)
	return
}
func scanSubdriverEvent(row interface{ Scan(...any) error }) (e model.SubdriverEvent, err error) {
	var created string
	err = row.Scan(&e.ID, &e.RequestID, &e.Key, &e.Kind, &e.Payload, &e.ReplyTo, &e.Handled, &created)
	e.CreatedAt = parseTime(created)
	return
}
func (s *Store) Subdriver(id string) (model.Subdriver, error) {
	return scanSubdriver(s.db.QueryRow(subdriverSelect+` WHERE id=?`, id))
}
func (s *Store) SubdriverRequest(id string) (model.SubdriverRequest, error) {
	return scanSubdriverRequest(s.db.QueryRow(subdriverRequestSelect+` WHERE id=?`, id))
}

func subdriverText(name, value string, limit int, required bool) error {
	if (required && strings.TrimSpace(value) == "") || len(value) > limit {
		return fmt.Errorf("%s must be %s at most %d bytes", name, map[bool]string{true: "nonblank and", false: ""}[required], limit)
	}
	return nil
}
func ordinaryDriver(id string) error {
	if err := validateDriverID(id); err != nil {
		return err
	}
	if model.IsSubdriverOwner(id) {
		return fmt.Errorf("sub-drivers cannot create sub-drivers or route sibling requests; ask the original driver")
	}
	return nil
}
func (s *Store) HandoffSubdriver(repoID, generalContext, driver, key, original, contextText, lead string) (model.SubdriverRequest, error) {
	var empty model.SubdriverRequest
	if repoID != "" && generalContext != "" {
		return empty, fmt.Errorf("choose repository or general scope, not both")
	}
	if err := ordinaryDriver(driver); err != nil {
		return empty, err
	}
	for _, v := range []struct {
		n, v string
		l    int
		r    bool
	}{{"key", key, 256, true}, {"original request", original, 256 * 1024, true}, {"context", contextText, 64 * 1024, false}, {"general context", generalContext, 8192, repoID == ""}} {
		if err := subdriverText(v.n, v.v, v.l, v.r); err != nil {
			return empty, err
		}
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	if err = acquireImmediateTransactionLock(tx); err != nil {
		return empty, err
	}
	previous, err := scanSubdriverRequest(tx.QueryRow(subdriverRequestSelect+` WHERE origin_driver_id=? AND request_key=?`, driver, key))
	if err == nil {
		c, err := scanSubdriver(tx.QueryRow(subdriverSelect+` WHERE id=?`, previous.SubdriverID))
		if err != nil {
			return empty, err
		}
		if c.RepoID != repoID || c.Context != generalContext || previous.Original != original || previous.Context != contextText || previous.LeadRequestID != lead {
			return empty, fmt.Errorf("handoff idempotency key conflicts with recorded request")
		}
		return previous, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return empty, err
	}
	if lead != "" {
		var owner string
		if err = tx.QueryRow(`SELECT driver_id FROM coordinator_requests WHERE id=?`, lead).Scan(&owner); err != nil {
			return empty, err
		}
		if owner != driver {
			return empty, fmt.Errorf("lead request belongs to another driver")
		}
	}
	c := model.Subdriver{ID: NewID("coord"), RepoID: repoID, Context: generalContext}
	if repoID != "" {
		existing, e := scanSubdriver(tx.QueryRow(subdriverSelect+` WHERE repo_id=?`, repoID))
		if e == nil {
			c = existing
		} else if !errors.Is(e, sql.ErrNoRows) {
			return empty, e
		}
	}
	if _, err = tx.Exec(`INSERT INTO coordinators(id,repo_id,context,updated_at) VALUES(?,NULLIF(?,''),?,?) ON CONFLICT(id) DO NOTHING`, c.ID, repoID, generalContext, now()); err != nil {
		return empty, err
	}
	r := model.SubdriverRequest{ID: NewID("request"), SubdriverID: c.ID, DriverID: driver, OriginDriverID: driver, Key: key, Original: original, Context: contextText, LeadRequestID: lead, State: "open", CreatedAt: time.Now().UTC()}
	if _, err = tx.Exec(`INSERT INTO coordinator_requests(id,coordinator_id,driver_id,origin_driver_id,request_key,original,context,lead_request_id,created_at) VALUES(?,?,?,?,?,?,?,NULLIF(?,''),?)`, r.ID, c.ID, driver, driver, key, original, contextText, lead, stamp(r.CreatedAt)); err != nil {
		return empty, err
	}
	if _, err = tx.Exec(`INSERT INTO coordinator_events(request_id,event_key,kind,payload,created_at) VALUES(?,'initial','request','Original request is in the request record.',?)`, r.ID, now()); err != nil {
		return empty, err
	}
	return r, tx.Commit()
}

func subdriverConsumerTx(tx *sql.Tx, consumer, generation string) error {
	if strings.HasPrefix(strings.TrimSpace(consumer), "subdriver:") {
		return fmt.Errorf("%w: use the recorded sub-driver owner identity, not a renamed prefix", ErrNotificationConflict)
	}
	id, ok := strings.CutPrefix(consumer, "coordinator:")
	if !ok {
		return nil
	}
	var valid bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM coordinators WHERE id=? AND 'coordinator:'||generation=? AND state='running')`, id, generation).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("%w: sub-driver notification consumer generation is not current", ErrNotificationConflict)
	}
	return nil
}

func subdriverFenceTx(tx *sql.Tx, f model.SubdriverFence) error {
	var valid int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM coordinators WHERE id=? AND generation=? AND token=? AND token<>'' AND state='running')`, f.ID, f.Generation, f.Token).Scan(&valid); err != nil {
		return err
	}
	if valid == 0 {
		return fmt.Errorf("sub-driver session is stale or not running; inspect sub-driver %s", f.ID)
	}
	return nil
}
func (s *Store) CheckSubdriverFence(f model.SubdriverFence) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return subdriverFenceTx(tx, f)
}
func requestFenceTx(tx *sql.Tx, f model.SubdriverFence, request string) error {
	if err := subdriverFenceTx(tx, f); err != nil {
		return err
	}
	var owner string
	if err := tx.QueryRow(`SELECT coordinator_id FROM coordinator_requests WHERE id=?`, request).Scan(&owner); err != nil {
		return err
	}
	if owner != f.ID {
		return fmt.Errorf("request is outside sub-driver scope")
	}
	return nil
}
func (s *Store) SubdriverReply(request, driver, key, payload string, replyTo int64) (model.SubdriverEvent, error) {
	if err := ordinaryDriver(driver); err != nil {
		return model.SubdriverEvent{}, err
	}
	return s.subdriverEvent(model.SubdriverFence{}, request, driver, key, "reply", payload, replyTo)
}
func (s *Store) SubdriverReturn(f model.SubdriverFence, request, key, kind, payload string) (model.SubdriverEvent, error) {
	switch kind {
	case "question", "result", "blocker", "handoff":
	default:
		return model.SubdriverEvent{}, fmt.Errorf("return kind must be question, result, blocker, or handoff")
	}
	return s.subdriverEvent(f, request, "", key, kind, payload, 0)
}
func (s *Store) subdriverEvent(f model.SubdriverFence, request, driver, key, kind, payload string, replyTo int64) (model.SubdriverEvent, error) {
	var empty model.SubdriverEvent
	if err := subdriverText("event key", key, 256, true); err != nil {
		return empty, err
	}
	if err := subdriverText("payload", payload, 8192, true); err != nil {
		return empty, err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	if err = acquireImmediateTransactionLock(tx); err != nil {
		return empty, err
	}
	r, err := scanSubdriverRequest(tx.QueryRow(subdriverRequestSelect+` WHERE id=?`, request))
	if err != nil {
		return empty, err
	}
	if kind == "reply" {
		if r.DriverID != driver {
			return empty, fmt.Errorf("request belongs to %s", r.DriverID)
		}
	} else if err = requestFenceTx(tx, f, request); err != nil {
		return empty, err
	}
	prior, err := scanSubdriverEvent(tx.QueryRow(subdriverEventSelect+` WHERE request_id=? AND event_key=?`, request, key))
	if err == nil {
		if prior.Kind != kind || prior.Payload != payload || prior.ReplyTo != replyTo {
			return empty, fmt.Errorf("event key conflicts with recorded content")
		}
		return prior, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return empty, err
	}
	if kind == "reply" {
		var count int
		if err = tx.QueryRow(`SELECT count(*) FROM coordinator_events WHERE id=? AND request_id=? AND kind IN ('question','blocker','handoff')`, replyTo, request).Scan(&count); err != nil {
			return empty, err
		}
		if count != 1 {
			return empty, fmt.Errorf("reply-to must identify a question, blocker, or handoff on this request")
		}
		if r.State == "done" {
			return empty, fmt.Errorf("request is complete; create a new correlated handoff")
		}
	}
	handled := kind != "reply"
	result, err := tx.Exec(`INSERT INTO coordinator_events(request_id,event_key,kind,payload,reply_to,handled,created_at) VALUES(?,?,?,?,NULLIF(?,0),?,?)`, request, key, kind, payload, replyTo, handled, now())
	if err != nil {
		return empty, err
	}
	id, _ := result.LastInsertId()
	if kind != "reply" {
		if _, err = tx.Exec(`INSERT INTO driver_notifications(notification_id,coordinator_event_id,target_driver_id,worker_run_generation,source_cursor,kind,state,created_at,updated_at) VALUES(?,?,?,0,? ,?,'pending',?,?)`, NewID("wake"), id, r.DriverID, id, "subdriver-"+kind, now(), now()); err != nil {
			return empty, err
		}
	}
	state := "open"
	if kind == "question" || kind == "blocker" || kind == "handoff" {
		state = "waiting"
	}
	if kind == "result" {
		state = "done"
	}
	if _, err = tx.Exec(`UPDATE coordinator_requests SET state=? WHERE id=? AND state<>'done'`, state, request); err != nil {
		return empty, err
	}
	e, err := scanSubdriverEvent(tx.QueryRow(subdriverEventSelect+` WHERE id=?`, id))
	if err != nil {
		return empty, err
	}
	return e, tx.Commit()
}
func (s *Store) HandleSubdriverEvent(f model.SubdriverFence, id int64) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var request string
	if err = tx.QueryRow(`SELECT request_id FROM coordinator_events WHERE id=?`, id).Scan(&request); err != nil {
		return err
	}
	if err = requestFenceTx(tx, f, request); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE coordinator_events SET handled=1 WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DispatchSubdriverWorker(f model.SubdriverFence, request, key string, task model.Task) (model.Task, error) {
	if err := subdriverText("dispatch key", key, 256, true); err != nil {
		return model.Task{}, err
	}
	if strings.TrimSpace(task.Objective) == "" || task.FeatureKey == "" || (task.Deliverable != "code" && task.Deliverable != "report") {
		return model.Task{}, fmt.Errorf("worker requires objective, feature, and code or report deliverable")
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.Task{}, err
	}
	defer tx.Rollback()
	if err = acquireImmediateTransactionLock(tx); err != nil {
		return model.Task{}, err
	}
	if err = requestFenceTx(tx, f, request); err != nil {
		return model.Task{}, err
	}
	c, err := scanSubdriver(tx.QueryRow(subdriverSelect+` WHERE id=?`, f.ID))
	if err != nil {
		return model.Task{}, err
	}
	if c.RepoID == "" || (task.RepoID != "" && task.RepoID != c.RepoID) {
		return model.Task{}, fmt.Errorf("worker dispatch requires the sub-driver's own repository; general research is direct")
	}
	task.RepoID = c.RepoID
	task.DriverID = c.DriverID()
	task.Title = model.DeriveTitle(task.Title, task.Objective, task.FeatureKey)
	spec, _ := json.Marshal(task)
	var id, previous string
	err = tx.QueryRow(`SELECT task_id,specification FROM coordinator_workers WHERE request_id=? AND dispatch_key=?`, request, key).Scan(&id, &previous)
	if err == nil {
		if previous != string(spec) {
			return model.Task{}, fmt.Errorf("dispatch key conflicts with recorded worker specification")
		}
		if err = tx.Commit(); err != nil {
			return model.Task{}, err
		}
		return s.Task(id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.Task{}, err
	}
	var state string
	if err = tx.QueryRow(`SELECT state FROM coordinator_requests WHERE id=?`, request).Scan(&state); err != nil {
		return model.Task{}, err
	}
	if state != "open" {
		return model.Task{}, fmt.Errorf("request is %s, not open", state)
	}
	task.ID = NewID("task")
	task.Status = model.TaskStatusQueued
	task.CreatedAt = time.Now().UTC()
	task.UpdatedAt = task.CreatedAt
	if err = insertTaskTx(tx, task, task.CreatedAt); err != nil {
		return model.Task{}, err
	}
	if _, err = tx.Exec(`INSERT INTO coordinator_workers(request_id,dispatch_key,task_id,specification) VALUES(?,?,?,?)`, request, key, task.ID, string(spec)); err != nil {
		return model.Task{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Task{}, err
	}
	return s.Task(task.ID)
}

func (s *Store) SubdriverPage(id string, offset int) (model.SubdriverPage, error) {
	p := model.SubdriverPage{Offset: offset, Requests: []model.SubdriverRequest{}, Events: []model.SubdriverEvent{}, Workers: []model.SubdriverWorker{}}
	if offset < 0 {
		return p, fmt.Errorf("offset must be nonnegative")
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	p.Subdriver, err = scanSubdriver(tx.QueryRow(subdriverSelect+` WHERE id=?`, id))
	if err != nil {
		return p, err
	}
	summarySelect := strings.Replace(subdriverRequestSelect, "original,context", "'',''", 1)
	rows, err := tx.Query(summarySelect+` WHERE coordinator_id=? ORDER BY created_at,id LIMIT 21 OFFSET ?`, id, offset)
	if err != nil {
		return p, err
	}
	more := false
	for rows.Next() {
		r, e := scanSubdriverRequest(rows)
		if e != nil {
			rows.Close()
			return p, e
		}
		if len(p.Requests) == 20 {
			more = true
			break
		}
		r.ReadCommand = "shephrd subdriver request " + r.ID + " --json"
		r.Original = ""
		r.Context = ""
		p.Requests = append(p.Requests, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return p, err
	}
	rows, err = tx.Query(subdriverEventSelect+` WHERE request_id IN (SELECT id FROM coordinator_requests WHERE coordinator_id=?) AND handled=0 ORDER BY id LIMIT 21 OFFSET ?`, id, offset)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		e, err := scanSubdriverEvent(rows)
		if err != nil {
			rows.Close()
			return p, err
		}
		if len(p.Events) == 20 {
			more = true
			break
		}
		if len(e.Payload) > 1024 {
			e.Payload = ""
			e.ReadCommand = fmt.Sprintf("shephrd subdriver event %d --json", e.ID)
		}
		p.Events = append(p.Events, e)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return p, err
	}
	rows, err = tx.Query(`SELECT request_id,dispatch_key,task_id FROM coordinator_workers WHERE request_id IN (SELECT id FROM coordinator_requests WHERE coordinator_id=?) ORDER BY task_id LIMIT 21 OFFSET ?`, id, offset)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var w model.SubdriverWorker
		if err = rows.Scan(&w.RequestID, &w.Key, &w.TaskID); err != nil {
			rows.Close()
			return p, err
		}
		if len(p.Workers) == 20 {
			more = true
			break
		}
		p.Workers = append(p.Workers, w)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return p, err
	}
	if more {
		p.Next = fmt.Sprintf("shephrd subdriver inspect %s --offset %d --json", id, offset+20)
	}
	return p, tx.Commit()
}

func (s *Store) SubdriverTaskFence(f model.SubdriverFence, taskID string) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = subdriverFenceTx(tx, f); err != nil {
		return err
	}
	var valid int
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM coordinator_workers w JOIN coordinator_requests r ON r.id=w.request_id JOIN tasks t ON t.id=w.task_id WHERE w.task_id=? AND r.coordinator_id=? AND t.driver_id=?)`, taskID, f.ID, "coordinator:"+f.ID).Scan(&valid); err != nil {
		return err
	}
	if valid != 1 {
		return fmt.Errorf("task is not owned by this sub-driver")
	}
	return nil
}

func (s *Store) SubdriverCandidates(driver string) ([]model.Subdriver, error) {
	rows, err := s.db.Query(subdriverSelect+` WHERE id IN (SELECT coordinator_id FROM coordinator_requests WHERE driver_id=?) ORDER BY updated_at`, driver)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.Subdriver{}
	for rows.Next() {
		c, e := scanSubdriver(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, c)
	}
	return result, rows.Err()
}
func (s *Store) SubdriverPending(id string) (bool, error) {
	var pending bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM coordinator_events e JOIN coordinator_requests r ON r.id=e.request_id WHERE r.coordinator_id=? AND e.handled=0) OR EXISTS(SELECT 1 FROM driver_notifications d WHERE d.target_driver_id=? AND (d.state='pending' OR (d.state='claimed' AND d.claim_until<?)) AND `+reportDoneNotificationPresentable+`)`, id, "coordinator:"+id, now()).Scan(&pending)
	return pending, err
}
func (s *Store) ReserveSubdriver(id string, generation int, harness, modelID, runtime string) (model.SubdriverFence, error) {
	f := model.SubdriverFence{ID: id, Token: NewID("session")}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return f, err
	}
	defer tx.Rollback()
	if err = acquireImmediateTransactionLock(tx); err != nil {
		return f, err
	}
	result, err := tx.Exec(`UPDATE coordinators SET generation=generation+1,state='starting',token=?,runner_pid=0,harness_pid=0,session_id='',endpoint_json='',harness=?,model=?,runtime=?,failure='',updated_at=? WHERE id=? AND generation=? AND state='idle'`, f.Token, harness, modelID, runtime, now(), id, generation)
	if err != nil {
		return f, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return f, fmt.Errorf("sub-driver generation or state conflict; inspect before recovery")
	}
	if err = tx.QueryRow(`SELECT generation FROM coordinators WHERE id=?`, id).Scan(&f.Generation); err != nil {
		return f, err
	}
	return f, tx.Commit()
}
func (s *Store) StartSubdriver(f model.SubdriverFence, pid int, session string) error {
	result, err := s.db.Exec(`UPDATE coordinators SET state='running',runner_pid=?,session_id=?,updated_at=? WHERE id=? AND generation=? AND token=? AND state='starting'`, pid, session, now(), f.ID, f.Generation, f.Token)
	return subdriverChanged(result, err)
}
func subdriverChanged(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("sub-driver generation or state conflict")
	}
	return nil
}
func (s *Store) SubdriverHarnessPID(f model.SubdriverFence, pid int) error {
	result, err := s.db.Exec(`UPDATE coordinators SET harness_pid=?,updated_at=? WHERE id=? AND generation=? AND token=? AND state='running'`, pid, now(), f.ID, f.Generation, f.Token)
	return subdriverChanged(result, err)
}
func (s *Store) FinishSubdriver(f model.SubdriverFence, checkpoint, failure string) error {
	if err := subdriverText("checkpoint", checkpoint, 4096, false); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = finishSubdriverTx(tx, f, checkpoint, failure); err != nil {
		return err
	}
	return tx.Commit()
}
func finishSubdriverTx(tx *sql.Tx, f model.SubdriverFence, checkpoint, failure string) error {
	state := "idle"
	if failure != "" {
		state = "held"
	}
	result, err := tx.Exec(`UPDATE coordinators SET state=?,checkpoint=?,failure=?,updated_at=? WHERE id=? AND generation=? AND token=? AND state IN ('running','starting','idle') AND (state<>'idle' OR ?='held')`, state, checkpoint, failure, now(), f.ID, f.Generation, f.Token, state)
	if err = subdriverChanged(result, err); err != nil {
		return err
	}
	if failure != "" {
		key := fmt.Sprintf("system-held:%d", f.Generation)
		payload := fmt.Sprintf("Sub-driver %s session %d is held: %s. Inspect: shephrd subdriver inspect %s --json. Workers retain their owner; do not adopt or redispatch them.", f.ID, f.Generation, boundedNotificationText(failure, 1024), f.ID)
		if _, err = tx.Exec(`INSERT INTO coordinator_events(request_id,event_key,kind,payload,handled,created_at) SELECT id,?,'blocker',?,1,? FROM coordinator_requests WHERE coordinator_id=? AND state<>'done' ON CONFLICT(request_id,event_key) DO NOTHING`, key, payload, now(), f.ID); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO driver_notifications(notification_id,coordinator_event_id,target_driver_id,worker_run_generation,source_cursor,kind,state,created_at,updated_at) SELECT 'coordinator-held:'||e.id,e.id,r.driver_id,0,e.id,'subdriver-blocker','pending',?,? FROM coordinator_events e JOIN coordinator_requests r ON r.id=e.request_id WHERE r.coordinator_id=? AND e.event_key=? ON CONFLICT(coordinator_event_id) DO NOTHING`, now(), now(), f.ID, key); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) RecoverSubdriver(id string, generation int, reasons ...string) error {
	reason := "Recorded runner, harness and endpoint absence verified."
	if len(reasons) > 0 && reasons[0] != "" {
		reason = reasons[0]
	}
	if err := subdriverText("recovery reason", reason, 1024, true); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE coordinators SET state='idle',token='',failure='',updated_at=? WHERE id=? AND generation=? AND state IN ('held','starting','running')`, now(), id, generation)
	if err = subdriverChanged(result, err); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO notification_delivery_log(notification_id,target_driver_id,operation,consumer_id,driver_generation,claim_token,result,detail,created_at) SELECT notification_id,target_driver_id,'reclaim',claim_owner,driver_generation,claim_token,'reclaimed',?,? FROM driver_notifications WHERE target_driver_id=? AND state='claimed'`, boundedNotificationText(reason, 512), now(), "coordinator:"+id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE driver_notifications SET state='pending',claim_owner='',driver_generation='',claim_token='',claimed_at=NULL,claim_until=NULL,updated_at=? WHERE target_driver_id=? AND state='claimed'`, now(), "coordinator:"+id); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO coordinator_events(request_id,event_key,kind,payload,handled,created_at) SELECT id,?,'recovery',?,0,? FROM coordinator_requests WHERE coordinator_id=? AND state<>'done'`, fmt.Sprintf("system-recovery:%d", generation), reason, now(), id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) AdoptSubdriverRequest(id, from, to string) error {
	if err := ordinaryDriver(to); err != nil {
		return err
	}
	if err := ordinaryDriver(from); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE coordinator_requests SET driver_id=? WHERE id=? AND driver_id=?`, to, id, from)
	if err = subdriverChanged(result, err); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO notification_delivery_log(notification_id,target_driver_id,operation,consumer_id,driver_generation,claim_token,result,detail,created_at) SELECT notification_id,target_driver_id,'adopt',claim_owner,driver_generation,claim_token,'retargeted',?,? FROM driver_notifications WHERE coordinator_event_id IN (SELECT id FROM coordinator_events WHERE request_id=?) AND state IN ('pending','claimed')`, boundedNotificationText("request return route adopted from "+from+" to "+to, 512), now(), id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE driver_notifications SET target_driver_id=?,state='pending',claim_owner='',driver_generation='',claim_token='',claimed_at=NULL,claim_until=NULL,updated_at=? WHERE coordinator_event_id IN (SELECT id FROM coordinator_events WHERE request_id=?) AND state IN ('pending','claimed')`, to, now(), id); err != nil {
		return err
	}
	return tx.Commit()
}
