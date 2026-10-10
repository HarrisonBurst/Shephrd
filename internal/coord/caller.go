package coord

import (
	"database/sql"
	"strconv"
	"strings"

	"shephrd/internal/fault"
)

type Caller struct {
	Kind     string `json:"kind"`
	Name     string `json:"name,omitempty"`
	Operator bool   `json:"operator"`
	Task     int64  `json:"task,omitempty"`
	Attempt  int64  `json:"attempt,omitempty"`
	Run      int64  `json:"run,omitempty"`
}

func (c Caller) String() string {
	switch c.Kind {
	case "run":
		return "task:" + TaskRef(c.Task)
	case "operator":
		return "operator"
	}
	return c.Kind + ":" + c.Name
}

// Scope keys idempotency records: a run's keys never replay into another run.
func (c Caller) Scope() string {
	if c.Kind == "run" {
		return "run:" + strconv.FormatInt(c.Run, 10)
	}
	return c.String()
}

type Querier interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

func TaskRef(id int64) string {
	return "t_" + strconv.FormatInt(id, 10)
}

func ArtifactRef(id int64) string {
	return "a_" + strconv.FormatInt(id, 10)
}

func ParseTaskRef(ref string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(ref, "t_"), 10, 64)
	if err != nil || !strings.HasPrefix(ref, "t_") || id < 1 {
		return 0, fault.New("usage", "%q is not a task ID such as t_1", ref)
	}
	return id, nil
}

func CanRead(q Querier, c Caller, t *Task) (bool, error) {
	switch c.Kind {
	case "plugin":
		return true, nil
	case "operator":
		return true, nil
	case "driver":
		if c.Operator {
			return true, nil
		}
		var driver string
		if err := q.QueryRow(`SELECT driver FROM tasks WHERE id = ?`, t.Root).Scan(&driver); err != nil {
			return false, err
		}
		return driver == c.String(), nil
	case "run":
		return IsWithin(q, t.ID, c.Task)
	}
	return false, nil
}

// IsWithin reports whether task is ancestor or one of its descendants.
func IsWithin(q Querier, task, ancestor int64) (bool, error) {
	for id := task; id != 0; {
		if id == ancestor {
			return true, nil
		}
		var parent sql.NullInt64
		if err := q.QueryRow(`SELECT parent FROM tasks WHERE id = ?`, id).Scan(&parent); err != nil {
			return false, err
		}
		id = parent.Int64
	}
	return false, nil
}

func Owns(c Caller, t *Task) bool {
	if t.Parent == 0 {
		return (c.Kind == "driver" || c.Kind == "plugin") && t.Driver == c.String()
	}
	return c.Kind == "run" && c.Task == t.Parent
}

func Get(q Querier, c Caller, ref string) (*Task, error) {
	id, err := ParseTaskRef(ref)
	if err != nil {
		return nil, err
	}
	t, err := Load(q, id)
	if err != nil {
		return nil, err
	}
	readable, err := CanRead(q, c, t)
	if err != nil {
		return nil, err
	}
	if !readable {
		return nil, notFound(ref)
	}
	return t, nil
}

func GetOwned(q Querier, c Caller, ref string) (*Task, error) {
	t, err := Get(q, c, ref)
	if err != nil {
		return nil, err
	}
	if !Owns(c, t) {
		return nil, fault.New("not_owner", "%s is owned by %s, not %s", t.Ref, t.Owner(), c).WithNext("task", "show", t.Ref)
	}
	return t, nil
}

func notFound(ref string) error {
	return fault.New("not_found", "no task %s is visible to this caller", ref).WithNext("task", "list")
}
