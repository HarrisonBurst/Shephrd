package artifact

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/fault"
	"shephrd/internal/store"
)

type Grant struct {
	ID      int64    `json:"-"`
	Ref     string   `json:"id"`
	Grantor string   `json:"grantor"`
	Task    string   `json:"to"`
	TaskID  int64    `json:"-"`
	Actions []string `json:"actions"`
	Reason  string   `json:"reason"`
	Created string   `json:"created"`
	Revoked string   `json:"revoked,omitempty"`
}

func GrantRef(id int64) string { return "g_" + strconv.FormatInt(id, 10) }

// Authorized names the grant that lets a caller take an irreversible
// action on a task it owns. Owning the task is never enough by itself.
func Authorized(q coord.Querier, cfg *config.Config, c coord.Caller, action string, t *coord.Task) (string, error) {
	for i, g := range cfg.Grants {
		if !slices.Contains(g.Actions, action) || g.Repo != "" && g.Repo != t.Repo {
			continue
		}
		if g.To == c.String() && (c.Kind == "driver" || c.Kind == "plugin") || g.To == "subdriver" && c.Kind == "run" {
			return fmt.Sprintf("config:%d", i), nil
		}
	}
	if c.Kind == "run" {
		grants, err := Grants(q, `revoked_at IS NULL`)
		if err != nil {
			return "", err
		}
		for _, g := range grants {
			if !slices.Contains(g.Actions, action) {
				continue
			}
			within, err := coord.IsWithin(q, c.Task, g.TaskID)
			if err != nil {
				return "", err
			}
			if within {
				return g.Ref, nil
			}
		}
	}
	return "", fault.New("not_granted", "%s holds no %s grant for %s; the user grants it in configuration or through its owner", c, action, t.Ref).
		WithNext("grant", action, "--to", t.Ref, "--reason", "-")
}

func RecordUse(tx *store.Tx, ref, action string, t *coord.Task, c coord.Caller) error {
	if _, err := tx.Exec(`INSERT INTO grant_uses (grant_ref, action, task, caller, time) VALUES (?, ?, ?, ?, ?)`,
		ref, action, t.ID, c.String(), store.Timestamp(tx.Now)); err != nil {
		return err
	}
	_, err := tx.Emit("grant.used", c.String(), t.ID, 0, 0, map[string]string{"grant": ref, "action": action, "task": t.Ref})
	return err
}

// Delegate passes an action the caller holds down to one of its child
// driver tasks, covering that task's subtree and never more.
func Delegate(tx *store.Tx, cfg *config.Config, c coord.Caller, action string, t *coord.Task, reason string) (*Grant, error) {
	if action != "land" && action != "discard" {
		return nil, fault.New("usage", "the grantable actions are land and discard")
	}
	if t.Role != "driver" {
		return nil, fault.New("usage", "grants go to driver tasks, which deliver their workers' results")
	}
	if strings.TrimSpace(reason) == "" {
		return nil, fault.New("usage", "a grant records why: pass --reason, such as the user's approval")
	}
	if _, err := Authorized(tx, cfg, c, action, t); err != nil {
		return nil, err
	}
	actions, _ := json.Marshal([]string{action})
	result, err := tx.Exec(`INSERT INTO grants (grantor, task, actions, reason, created_at) VALUES (?, ?, ?, ?, ?)`,
		c.String(), t.ID, string(actions), reason, store.Timestamp(tx.Now))
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	g := &Grant{ID: id, Ref: GrantRef(id), Grantor: c.String(), Task: t.Ref, TaskID: t.ID, Actions: []string{action}, Reason: reason, Created: store.Timestamp(tx.Now)}
	_, err = tx.Emit("grant.added", c.String(), t.ID, 0, 0, map[string]any{
		"grant": g.Ref, "grantor": g.Grantor, "grantee": t.Ref, "actions": g.Actions, "scope": "subtree", "reason": reason,
	})
	return g, err
}

func Revoke(tx *store.Tx, c coord.Caller, ref string) (*Grant, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(ref, "g_"), 10, 64)
	if err != nil || !strings.HasPrefix(ref, "g_") {
		return nil, fault.New("usage", "%q is not a grant ID such as g_1", ref)
	}
	grants, err := Grants(tx, `id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(grants) == 0 || grants[0].Grantor != c.String() && !c.Operator {
		return nil, fault.New("not_found", "no grant %s was made by %s", ref, c)
	}
	g := grants[0]
	if g.Revoked != "" {
		return g, nil
	}
	g.Revoked = store.Timestamp(tx.Now)
	if _, err := tx.Exec(`UPDATE grants SET revoked_at = ?, revoked_by = ? WHERE id = ?`, g.Revoked, c.String(), id); err != nil {
		return nil, err
	}
	_, err = tx.Emit("grant.revoked", c.String(), g.TaskID, 0, 0, map[string]any{"grant": g.Ref, "grantor": g.Grantor, "grantee": g.Task, "actions": g.Actions})
	return g, err
}

func Grants(q coord.Querier, where string, args ...any) ([]*Grant, error) {
	rows, err := q.Query(`SELECT id, grantor, task, actions, reason, created_at, COALESCE(revoked_at, '') FROM grants WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []*Grant
	for rows.Next() {
		var g Grant
		var actions string
		if err := rows.Scan(&g.ID, &g.Grantor, &g.TaskID, &actions, &g.Reason, &g.Created, &g.Revoked); err != nil {
			return nil, err
		}
		g.Ref, g.Task = GrantRef(g.ID), coord.TaskRef(g.TaskID)
		json.Unmarshal([]byte(actions), &g.Actions)
		grants = append(grants, &g)
	}
	return grants, rows.Err()
}
