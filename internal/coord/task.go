package coord

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"shephrd/internal/config"
	"shephrd/internal/fault"
	"shephrd/internal/store"
)

const (
	MaxObjective  = 64 << 10
	MaxAcceptance = 16 << 10
	MaxTitle      = 200
	MaxMessage    = 64 << 10
	MaxNote       = 4 << 10
	MaxPluginData = 4 << 10
)

type Task struct {
	ID          int64         `json:"-"`
	Ref         string        `json:"id"`
	Parent      int64         `json:"-"`
	ParentRef   string        `json:"parent,omitempty"`
	Driver      string        `json:"driver,omitempty"`
	Root        int64         `json:"-"`
	Depth       int           `json:"depth"`
	Request     int64         `json:"request,omitempty"`
	Role        string        `json:"role"`
	RepoID      int64         `json:"-"`
	Repo        string        `json:"repo,omitempty"`
	Title       string        `json:"title"`
	Objective   string        `json:"objective,omitempty"`
	Acceptance  string        `json:"acceptance,omitempty"`
	Deliverable string        `json:"deliverable"`
	Target      config.Target `json:"target"`
	State       string        `json:"state"`
	Reason      string        `json:"reason,omitempty"`
	Attempt     int64         `json:"attempt"`
	Revision    int64         `json:"revision"`
	Milestone   string        `json:"milestone,omitempty"`
	Created     string        `json:"created"`
	Updated     string        `json:"updated"`
}

func (t *Task) Owner() string {
	if t.Parent == 0 {
		return t.Driver
	}
	return "task:" + t.ParentRef
}

func (t *Task) Summary() *Task {
	s := *t
	s.Objective, s.Acceptance = "", ""
	return &s
}

const taskColumns = `t.id, COALESCE(t.parent, 0), COALESCE(t.driver, ''), t.root, t.depth, COALESCE(t.request, 0), t.role,
	COALESCE(t.repo, 0), COALESCE(r.name, ''), t.title, t.objective, t.acceptance, t.deliverable, t.host, t.harness, t.model,
	t.state, t.reason, t.attempt, t.revision, t.milestone, t.created_at, t.updated_at
	FROM tasks t LEFT JOIN repos r ON r.id = t.repo`

func scanTask(row interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.Parent, &t.Driver, &t.Root, &t.Depth, &t.Request, &t.Role, &t.RepoID, &t.Repo,
		&t.Title, &t.Objective, &t.Acceptance, &t.Deliverable, &t.Target.Host, &t.Target.Harness, &t.Target.Model,
		&t.State, &t.Reason, &t.Attempt, &t.Revision, &t.Milestone, &t.Created, &t.Updated)
	if err != nil {
		return nil, err
	}
	t.Ref = TaskRef(t.ID)
	if t.Parent != 0 {
		t.ParentRef = TaskRef(t.Parent)
	}
	return &t, nil
}

func Load(q Querier, id int64) (*Task, error) {
	t, err := scanTask(q.QueryRow(`SELECT `+taskColumns+` WHERE t.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound(TaskRef(id))
	}
	return t, err
}

func Query(q Querier, where string, args ...any) ([]*Task, error) {
	rows, err := q.Query(`SELECT `+taskColumns+` WHERE `+where+` ORDER BY t.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []*Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

var transitions = map[string][]string{
	"queued":  {"running", "closed"},
	"running": {"waiting", "done", "held"},
	"waiting": {"running", "held"},
	"held":    {"running", "closed"},
	"done":    {"running", "closed"},
}

func SetState(tx *store.Tx, caller string, t *Task, to, reason string) error {
	if t.State == to && t.Reason == reason {
		return nil
	}
	if t.State != to && !slices.Contains(transitions[t.State], to) {
		return fault.New("invalid_state", "%s cannot go from %s to %s", t.Ref, t.State, to).WithNext("task", "show", t.Ref)
	}
	from := t.State
	t.State, t.Reason = to, reason
	if err := Touch(tx, t, `state = ?, reason = ?`, to, reason); err != nil {
		return err
	}
	if _, err := tx.Emit("task.state", caller, t.ID, t.Attempt, 0, map[string]string{"from": from, "to": to, "reason": reason}); err != nil {
		return err
	}
	if to == "closed" {
		return AckTask(tx, t)
	}
	return nil
}

// Touch applies an update to a task and advances its revision.
func Touch(tx *store.Tx, t *Task, set string, args ...any) error {
	t.Revision++
	t.Updated = store.Timestamp(tx.Now)
	_, err := tx.Exec(`UPDATE tasks SET `+set+`, revision = ?, updated_at = ? WHERE id = ?`, append(args, t.Revision, t.Updated, t.ID)...)
	return err
}

func CheckRevision(t *Task, want int64) error {
	if want != 0 && want != t.Revision {
		return fault.New("revision_conflict", "%s is at revision %d, not %d", t.Ref, t.Revision, want).WithNext("task", "show", t.Ref)
	}
	return nil
}

func OpenChildren(q Querier, id int64) (int, error) {
	var n int
	err := q.QueryRow(`SELECT COUNT(*) FROM tasks WHERE parent = ? AND state != 'closed'`, id).Scan(&n)
	return n, err
}

type NewTask struct {
	Role        string
	Repo        string
	Title       string
	Objective   string
	Acceptance  string
	Deliverable string
	After       []string
	Until       string
	Request     int64
	Target      config.Target
}

type Proposal struct {
	NewTask
	Parent  *Task
	Driver  string
	RepoID  int64
	Host    string
	Depth   int
	After   []int64
	Request int64
}

// Propose validates a new task against the caller and the tree. It runs
// before intercept hooks and again inside the creating transaction.
func Propose(q Querier, cfg *config.Config, c Caller, spec NewTask) (*Proposal, error) {
	p := &Proposal{NewTask: spec}
	switch c.Kind {
	case "driver", "plugin":
		p.Driver = c.String()
	case "run":
		parent, err := Load(q, c.Task)
		if err != nil {
			return nil, err
		}
		if parent.Role != "driver" {
			return nil, fault.New("not_a_driver", "a worker cannot create tasks")
		}
		p.Parent, p.Depth = parent, parent.Depth+1
	default:
		return nil, fault.New("no_driver", "creating a task needs a driver identity; configure driver or pass --as driver:<name>")
	}
	if p.Depth > cfg.MaxDepth {
		return nil, fault.New("depth_exceeded", "tasks may be at most %d levels below a root", cfg.MaxDepth)
	}
	if p.Role == "" {
		p.Role = "worker"
	}
	if p.Role != "worker" && p.Role != "driver" {
		return nil, fault.New("usage", "role must be worker or driver, not %q", p.Role)
	}
	p.Objective = strings.TrimSpace(p.Objective)
	if p.Objective == "" {
		return nil, fault.New("usage", "a task needs an objective")
	}
	if len(p.Objective) > MaxObjective || len(p.Acceptance) > MaxAcceptance {
		return nil, fault.New("too_large", "the objective is bounded at %d bytes and acceptance at %d", MaxObjective, MaxAcceptance)
	}
	if p.Title == "" {
		p.Title, _, _ = strings.Cut(p.Objective, "\n")
		if len(p.Title) > 80 {
			p.Title = strings.TrimSpace(p.Title[:77]) + "..."
		}
	}
	if len(p.Title) > MaxTitle {
		return nil, fault.New("too_large", "the title is bounded at %d bytes", MaxTitle)
	}
	if err := p.resolveRepo(q); err != nil {
		return nil, err
	}
	if err := p.resolveDeliverable(); err != nil {
		return nil, err
	}
	if err := p.resolveRequest(q); err != nil {
		return nil, err
	}
	if err := p.resolveDependencies(q, c); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Proposal) resolveRepo(q Querier) error {
	if p.Repo != "" {
		err := q.QueryRow(`SELECT id, host FROM repos WHERE name = ? OR 'r_' || id = ?`, p.Repo, p.Repo).Scan(&p.RepoID, &p.Host)
		if errors.Is(err, sql.ErrNoRows) {
			return fault.New("not_found", "no repository %q is registered", p.Repo).WithNext("repo", "list")
		}
		if err != nil {
			return err
		}
	}
	if p.Role == "worker" && p.RepoID == 0 {
		return fault.New("usage", "a worker task needs --repo")
	}
	if p.Parent != nil && p.Parent.RepoID != p.RepoID {
		if p.Parent.RepoID == 0 {
			return fault.New("not_granted", "a general sub-driver has no repository authority")
		}
		return fault.New("not_granted", "a sub-driver for %s can only delegate within %s", p.Parent.Repo, p.Parent.Repo)
	}
	return nil
}

func (p *Proposal) resolveDeliverable() error {
	allowed := map[string][]string{"worker": {"code", "report"}, "driver": {"answer", "report"}}[p.Role]
	if p.Deliverable == "" {
		p.Deliverable = allowed[0]
	}
	if !slices.Contains(allowed, p.Deliverable) {
		return fault.New("usage", "a %s task delivers %s, not %q", p.Role, strings.Join(allowed, " or "), p.Deliverable)
	}
	return nil
}

func (p *Proposal) resolveRequest(q Querier) error {
	if p.Parent == nil {
		if p.NewTask.Request != 0 {
			return fault.New("usage", "--request is only for children of a driver task")
		}
		return nil
	}
	rows, err := q.Query(`SELECT seq FROM requests WHERE task = ? AND state != 'answered' ORDER BY seq`, p.Parent.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var open []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return err
		}
		open = append(open, seq)
	}
	switch {
	case p.NewTask.Request != 0 && slices.Contains(open, p.NewTask.Request):
		p.Request = p.NewTask.Request
	case p.NewTask.Request != 0:
		return fault.New("invalid_request", "%d is not an open request on %s", p.NewTask.Request, p.Parent.Ref).WithNext("task", "show", p.Parent.Ref)
	case len(open) == 1:
		p.Request = open[0]
	case len(open) == 0:
		return fault.New("invalid_request", "%s has no open request to serve", p.Parent.Ref)
	default:
		return fault.New("request_required", "%s has several open requests; pass --request", p.Parent.Ref).WithNext("task", "show", p.Parent.Ref)
	}
	return rows.Err()
}

func (p *Proposal) resolveDependencies(q Querier, c Caller) error {
	if p.Until == "" {
		p.Until = "published"
	}
	if p.Until != "published" && p.Until != "merged" {
		return fault.New("usage", "--until must be published or merged")
	}
	for _, ref := range p.NewTask.After {
		id, err := ParseTaskRef(ref)
		if err != nil {
			return err
		}
		dep, err := Get(q, c, ref)
		if err != nil {
			return err
		}
		sibling := dep.Parent == 0 && p.Parent == nil && dep.Driver == p.Driver ||
			p.Parent != nil && dep.Parent == p.Parent.ID
		if !sibling {
			return fault.New("invalid_dependency", "%s is not a sibling; dependencies never cross owners", ref)
		}
		if !slices.Contains(p.After, id) {
			p.After = append(p.After, id)
		}
	}
	return nil
}

func Create(tx *store.Tx, c Caller, p *Proposal, target config.Target) (*Task, error) {
	now := store.Timestamp(tx.Now)
	var parent, driver, request, repo any
	root := int64(0)
	if p.Parent != nil {
		parent, root, request = p.Parent.ID, p.Parent.Root, p.Request
	} else {
		driver = p.Driver
	}
	if p.RepoID != 0 {
		repo = p.RepoID
	}
	result, err := tx.Exec(`INSERT INTO tasks (parent, driver, root, depth, request, role, repo, title, objective, acceptance, deliverable,
		host, harness, model, state, reason, attempt, revision, milestone, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', '', 0, 1, '', ?, ?)`,
		parent, driver, root, p.Depth, request, p.Role, repo, p.Title, p.Objective, p.Acceptance, p.Deliverable,
		target.Host, target.Harness, target.Model, now, now)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	if p.Parent == nil {
		if _, err := tx.Exec(`UPDATE tasks SET root = id WHERE id = ?`, id); err != nil {
			return nil, err
		}
	}
	deps := []string{}
	for _, dep := range p.After {
		if _, err := tx.Exec(`INSERT INTO dependencies (task, dependency, until) VALUES (?, ?, ?)`, id, dep, p.Until); err != nil {
			return nil, err
		}
		deps = append(deps, TaskRef(dep))
	}
	t, err := Load(tx, id)
	if err != nil {
		return nil, err
	}
	data := map[string]any{
		"owner": t.Owner(), "parent": t.ParentRef, "role": t.Role, "repo": t.Repo, "title": t.Title,
		"deliverable": t.Deliverable, "target": target, "dependencies": deps, "until": p.Until,
	}
	if t.Request != 0 {
		data["request"] = t.Request
	}
	seq, err := tx.Emit("task.created", c.String(), id, 0, 0, data)
	if err != nil {
		return nil, err
	}
	if t.Role == "driver" {
		if _, err := tx.Exec(`INSERT INTO requests (task, seq, state) VALUES (?, ?, 'open')`, id, seq); err != nil {
			return nil, err
		}
	}
	return t, nil
}

type Dependency struct {
	Task          string `json:"task"`
	Until         string `json:"until"`
	State         string `json:"state"`
	Milestone     string `json:"milestone,omitempty"`
	Satisfied     bool   `json:"satisfied"`
	Unsatisfiable bool   `json:"unsatisfiable,omitempty"`
}

func Dependencies(q Querier, id int64) ([]Dependency, bool, error) {
	rows, err := q.Query(`SELECT d.dependency, d.until, t.state, t.milestone FROM dependencies d JOIN tasks t ON t.id = d.dependency
		WHERE d.task = ? ORDER BY d.dependency`, id)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	deps := []Dependency{}
	ready := true
	for rows.Next() {
		var dep Dependency
		var depID int64
		if err := rows.Scan(&depID, &dep.Until, &dep.State, &dep.Milestone); err != nil {
			return nil, false, err
		}
		dep.Task = TaskRef(depID)
		dep.Satisfied = dep.Milestone == "merged" || dep.Milestone == "published" && dep.Until == "published"
		dep.Unsatisfiable = !dep.Satisfied && dep.State == "closed"
		ready = ready && dep.Satisfied
		deps = append(deps, dep)
	}
	return deps, ready, rows.Err()
}

type Request struct {
	Seq    int64  `json:"seq"`
	State  string `json:"state"`
	Result int64  `json:"result,omitempty"`
}

func Requests(q Querier, id int64) ([]Request, error) {
	rows, err := q.Query(`SELECT seq, state, COALESCE(result, 0) FROM requests WHERE task = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	requests := []Request{}
	for rows.Next() {
		var r Request
		if err := rows.Scan(&r.Seq, &r.State, &r.Result); err != nil {
			return nil, err
		}
		requests = append(requests, r)
	}
	return requests, rows.Err()
}

func Send(tx *store.Tx, c Caller, t *Task, body string, replyTo int64) (int64, error) {
	if t.State == "closed" {
		return 0, fault.New("invalid_state", "%s is closed", t.Ref)
	}
	if len(body) > MaxMessage {
		return 0, fault.New("too_large", "a message is bounded at %d bytes", MaxMessage)
	}
	data := map[string]any{"body": body}
	name := "task.message"
	var request int64
	if replyTo != 0 {
		var question string
		err := tx.QueryRow(`SELECT data FROM events WHERE seq = ? AND task = ? AND name = 'task.question'`, replyTo, t.ID).Scan(&question)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fault.New("invalid_reply", "%d is not a question on %s", replyTo, t.Ref).WithNext("task", "show", t.Ref)
		}
		if err != nil {
			return 0, err
		}
		var answered int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM events WHERE task = ? AND name = 'task.reply' AND json_extract(data, '$.reply_to') = ?`, t.ID, replyTo).Scan(&answered); err != nil {
			return 0, err
		}
		if answered > 0 {
			return 0, fault.New("invalid_reply", "question %d on %s is already answered", replyTo, t.Ref)
		}
		var q struct {
			Request int64 `json:"request"`
		}
		json.Unmarshal([]byte(question), &q)
		request = q.Request
		name, data["reply_to"] = "task.reply", replyTo
		if request != 0 {
			data["request"] = request
		}
	}
	seq, err := tx.Emit(name, c.String(), t.ID, t.Attempt, 0, data)
	if err != nil {
		return 0, err
	}
	switch {
	case t.Role == "driver" && replyTo == 0:
		_, err = tx.Exec(`INSERT INTO requests (task, seq, state) VALUES (?, ?, 'open')`, t.ID, seq)
	case t.Role == "driver" && request != 0:
		_, err = tx.Exec(`UPDATE requests SET state = 'open' WHERE task = ? AND seq = ? AND state = 'asked'`, t.ID, request)
	}
	if err != nil {
		return 0, err
	}
	return seq, Wake(tx, t)
}

// Wake marks a started task as having something new for its next turn.
func Wake(tx *store.Tx, t *Task) error {
	if t.Attempt == 0 {
		return nil
	}
	_, err := tx.Exec(`UPDATE tasks SET wake_at = COALESCE(wake_at, ?) WHERE id = ?`, store.Timestamp(tx.Now), t.ID)
	return err
}

func Cancel(tx *store.Tx, c Caller, t *Task) error {
	if t.State != "queued" {
		return fault.New("invalid_state", "only a task that never started can be cancelled; %s is %s", t.Ref, t.State).
			WithNext("task", "discard", t.Ref)
	}
	if err := requireNoOpenChildren(tx, t); err != nil {
		return err
	}
	return SetState(tx, c.String(), t, "closed", "cancelled")
}

func requireNoOpenChildren(q Querier, t *Task) error {
	n, err := OpenChildren(q, t.ID)
	if err != nil {
		return err
	}
	if n > 0 {
		return fault.New("open_children", "%s has %d open children; a task cannot close before them", t.Ref, n).
			WithNext("task", "list", "--parent", t.Ref)
	}
	return nil
}

func Adopt(tx *store.Tx, c Caller, t *Task, to string) error {
	if t.Parent != 0 {
		return fault.New("not_root", "only a root task can change owner; %s belongs to %s", t.Ref, t.ParentRef)
	}
	kind, name, _ := strings.Cut(to, ":")
	if (kind != "driver" && kind != "plugin") || !config.NamePattern.MatchString(name) {
		return fault.New("usage", "a root is owned by driver:<name> or plugin:<name>, not %q", to)
	}
	if !Owns(c, t) && !c.Operator {
		return fault.New("not_owner", "only %s or the operator can hand %s to another driver", t.Driver, t.Ref)
	}
	from := t.Driver
	if from == to {
		return nil
	}
	t.Driver = to
	if err := Touch(tx, t, `driver = ?`, to); err != nil {
		return err
	}
	_, err := tx.Emit("task.adopted", c.String(), t.ID, 0, 0, map[string]string{"from": from, "to": to})
	return err
}

func Note(tx *store.Tx, c Caller, t *Task, body string) (int64, error) {
	if len(body) > MaxNote {
		return 0, fault.New("too_large", "a note is bounded at %d bytes", MaxNote)
	}
	return tx.Emit("task.note", c.String(), t.ID, t.Attempt, c.Run, map[string]string{"body": body})
}

func SetData(tx *store.Tx, c Caller, t *Task, pluginName string, set map[string]json.RawMessage, unset []string) (map[string]json.RawMessage, error) {
	data := map[string]json.RawMessage{}
	var current string
	err := tx.QueryRow(`SELECT data FROM plugin_data WHERE task = ? AND plugin = ?`, t.ID, pluginName).Scan(&current)
	if err == nil {
		if err := json.Unmarshal([]byte(current), &data); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	keys := []string{}
	for key, value := range set {
		data[key] = value
		keys = append(keys, key)
	}
	for _, key := range unset {
		delete(data, key)
		keys = append(keys, key)
	}
	body, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if len(body) > MaxPluginData {
		return nil, fault.New("too_large", "plugin data is bounded at %d bytes per plugin", MaxPluginData)
	}
	if _, err := tx.Exec(`INSERT INTO plugin_data (task, plugin, data) VALUES (?, ?, ?)
		ON CONFLICT (task, plugin) DO UPDATE SET data = excluded.data`, t.ID, pluginName, string(body)); err != nil {
		return nil, err
	}
	slices.Sort(keys)
	_, err = tx.Emit("task.data", c.String(), t.ID, 0, 0, map[string]any{"plugin": pluginName, "keys": keys})
	return data, err
}

func PluginData(q Querier, id int64) (map[string]json.RawMessage, error) {
	rows, err := q.Query(`SELECT plugin, data FROM plugin_data WHERE task = ? ORDER BY plugin`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var name, data string
		if err := rows.Scan(&name, &data); err != nil {
			return nil, err
		}
		out[name] = json.RawMessage(data)
	}
	return out, rows.Err()
}
