package execution

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"shephrd/internal/coord"
	"shephrd/internal/gitcmd"
	"shephrd/internal/store"
)

type Reconciled struct {
	Workspaces []map[string]string `json:"workspaces"`
	Runs       []map[string]string `json:"runs"`
}

// Reconcile classifies effects interrupted part way. It never starts a
// run, chooses resume or retry, or removes anything it cannot prove.
func (x *Executor) Reconcile(ctx context.Context) (*Reconciled, error) {
	out := &Reconciled{Workspaces: []map[string]string{}, Runs: []map[string]string{}}
	attempts, err := coord.Attempts(x.DB, `workspace_state = 'allocating'`)
	if err != nil {
		return nil, err
	}
	for _, a := range attempts {
		state := x.classifyAllocation(ctx, a)
		if err := x.write(ctx, func(tx *store.Tx) error { return coord.SetWorkspaceState(tx, "system", a, state, "") }); err != nil {
			return nil, err
		}
		out.Workspaces = append(out.Workspaces, map[string]string{"task": coord.TaskRef(a.Task), "path": a.Workspace, "state": state})
	}
	runs, err := coord.Runs(x.DB, `liveness IN ('starting', 'live', 'unknown')`)
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		observed, err := x.Probe(ctx, r)
		if err != nil {
			return nil, err
		}
		out.Runs = append(out.Runs, map[string]string{"task": coord.TaskRef(r.Task), "run": r.Dir, "liveness": observed})
	}
	return out, nil
}

func (x *Executor) classifyAllocation(ctx context.Context, a *coord.Attempt) string {
	_, statErr := os.Lstat(a.Workspace)
	if a.Branch == "" {
		if statErr == nil {
			return "held"
		}
		return "none"
	}
	var repo string
	if err := x.DB.QueryRowContext(ctx, `SELECT r.path FROM repos r JOIN tasks t ON t.repo = r.id WHERE t.id = ?`, a.Task).Scan(&repo); err != nil {
		return "unknown"
	}
	_, branchErr := gitcmd.Run(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+a.Branch)
	if errors.Is(statErr, fs.ErrNotExist) && branchErr != nil {
		return "none"
	}
	if statErr == nil && VerifyWorkspace(repo, a.Workspace, a.Branch, "") == nil {
		return "held"
	}
	return "unknown"
}
