package artifact

import (
	"context"
	"errors"
	"path/filepath"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/execution"
	"shephrd/internal/fault"
	"shephrd/internal/store"
)

type Service struct {
	DB  *store.Store
	Cfg *config.Config
}

type Delivered struct {
	Task     *coord.Task `json:"task"`
	Artifact *Artifact   `json:"artifact,omitempty"`
	Landing  *Landed     `json:"landing,omitempty"`
	Released []string    `json:"released"`
	Retained []string    `json:"retained"`
	Warnings []string    `json:"-"`
}

func (s *Service) repo(t *coord.Task) (path, branch string, err error) {
	err = s.DB.QueryRow(`SELECT path, default_branch FROM repos WHERE id = ?`, t.RepoID).Scan(&path, &branch)
	return path, branch, err
}

// Deliver accepts a done task's current result, or lands its code using
// the repository's landing mode, then closes it as delivered.
func (s *Service) Deliver(ctx context.Context, c coord.Caller, t *coord.Task) (*Delivered, error) {
	if t.State != "done" || t.ArtifactID == 0 {
		return nil, fault.New("invalid_state", "%s is %s; only a done task with a result can be delivered", t.Ref, t.State).WithNext("task", "show", t.Ref)
	}
	if n, err := coord.OpenChildren(s.DB, t.ID); err != nil || n > 0 {
		if err != nil {
			return nil, err
		}
		return nil, fault.New("open_children", "%s has %d open children; deliver or close them first", t.Ref, n).WithNext("task", "list", "--parent", t.Ref)
	}
	art, err := Load(s.DB, t.ArtifactID)
	if err != nil {
		return nil, err
	}
	out := &Delivered{Artifact: art, Released: []string{}, Retained: []string{}}
	if art.Kind != "code" {
		err := s.DB.Write(ctx, func(tx *store.Tx) error {
			t, err := coord.Load(tx, t.ID)
			if err != nil {
				return err
			}
			return s.close(tx, c, t, art, "accepted", "owner", false)
		})
		if err != nil {
			return nil, err
		}
		return s.finish(ctx, c, t.ID, out)
	}
	grant, err := Authorized(s.DB, s.Cfg, c, "land", t)
	if err != nil {
		return nil, err
	}
	landing := s.Cfg.Repos[t.Repo].Landing
	if landing.Mode == "" {
		return nil, fault.New("landing_not_configured", "repository %s has no landing mode; set [repos.%s.landing] in the home host's configuration", t.Repo, t.Repo)
	}
	if landing.Mode != "direct" {
		return nil, fault.New("landing_not_configured", "landing mode %s needs its forge provider", landing.Mode)
	}
	repo, branch, err := s.repo(t)
	if err != nil {
		return nil, err
	}
	landed, err := LandDirect(repo, branch, art.Commit, landing.Method, "Merge "+t.Ref+": "+t.Title, filepath.Join(s.Cfg.DataDir, "scratch"))
	if err != nil {
		s.DB.Write(ctx, func(tx *store.Tx) error {
			return recordLanding(tx, c, t, art, landing.Mode, "failed", nil, fault.As(err).Message)
		})
		return nil, err
	}
	out.Landing, out.Warnings = landed, landed.Warnings
	err = s.DB.Write(ctx, func(tx *store.Tx) error {
		t, err := coord.Load(tx, t.ID)
		if err != nil {
			return err
		}
		if err := RecordUse(tx, grant, "land", t, c); err != nil {
			return err
		}
		if err := recordLanding(tx, c, t, art, landing.Mode, "merged", landed, ""); err != nil {
			return err
		}
		return s.close(tx, c, t, art, landed.Proof, landed.Source, false)
	})
	if err != nil {
		return nil, err
	}
	return s.finish(ctx, c, t.ID, out)
}

func recordLanding(tx *store.Tx, c coord.Caller, t *coord.Task, art *Artifact, mode, outcome string, landed *Landed, detail string) error {
	var tip, proof, source string
	if landed != nil {
		tip, proof, source = landed.Tip, landed.Proof, landed.Source
	}
	if _, err := tx.Exec(`INSERT INTO landings (task, artifact, mode, outcome, merge_commit, proof, proof_source, detail, time) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, art.ID, mode, outcome, tip, proof, source, detail, store.Timestamp(tx.Now)); err != nil {
		return err
	}
	_, err := tx.Emit("land.attempted", c.String(), t.ID, art.Attempt, 0, map[string]any{
		"task": t.Ref, "commit": art.Commit, "mode": mode, "outcome": outcome, "detail": detail,
	})
	return err
}

// close records delivery with its proof and makes the work's dependents
// ready. A delivered dependency is both published and merged.
func (s *Service) close(tx *store.Tx, c coord.Caller, t *coord.Task, art *Artifact, proof, source string, wake bool) error {
	if _, err := tx.Emit("task.published", c.String(), t.ID, 0, 0, map[string]any{"task": t.Ref, "artifact": art.Ref, "branch": art.Branch}); err != nil {
		return err
	}
	if err := coord.SetMilestone(tx, t, "merged", wake); err != nil {
		return err
	}
	if _, err := tx.Emit("task.delivered", c.String(), t.ID, 0, 0, map[string]any{"task": t.Ref, "artifact": art.Ref, "proof": proof, "source": source}); err != nil {
		return err
	}
	return coord.Close(tx, c.String(), t, "delivered")
}

func (s *Service) finish(ctx context.Context, c coord.Caller, id int64, out *Delivered) (*Delivered, error) {
	released, retained, err := s.Release(ctx, id, false)
	if err != nil {
		return nil, err
	}
	out.Released, out.Retained = released, retained
	out.Task, err = coord.Load(s.DB, id)
	return out, err
}

// Release removes a closed task's held workspaces that are clean and have
// no live process. A dirty one stays held and its owner is told.
func (s *Service) Release(ctx context.Context, id int64, force bool) (released, retained []string, err error) {
	t, err := coord.Load(s.DB, id)
	if err != nil {
		return nil, nil, err
	}
	attempts, err := coord.Attempts(s.DB, `task = ? AND workspace_state = 'held'`, t.ID)
	if err != nil {
		return nil, nil, err
	}
	released, retained = []string{}, []string{}
	for _, a := range attempts {
		ok, err := s.removable(t, a)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			retained = append(retained, a.Workspace)
			continue
		}
		path := ""
		if t.RepoID != 0 {
			if path, _, err = s.repo(t); err != nil {
				return nil, nil, err
			}
		}
		sealed, err := sealedFiles(s.DB, t.ID, a.N)
		if err != nil {
			return nil, nil, err
		}
		removeErr := execution.RemoveWorkspace(path, a, force, sealed)
		err = s.DB.Write(ctx, func(tx *store.Tx) error {
			switch {
			case removeErr == nil:
				return coord.SetWorkspaceState(tx, "system", a, "released", "")
			case errors.Is(removeErr, execution.ErrDirty):
				seq, err := tx.Emit("workspace.retained", "system", t.ID, a.N, 0, map[string]any{
					"attempt": a.N, "path": a.Workspace, "reason": "uncommitted changes",
				})
				if err != nil {
					return err
				}
				return coord.WakeOwner(tx, t, seq)
			default:
				return coord.SetWorkspaceState(tx, "system", a, "unknown", "")
			}
		})
		if err != nil {
			return nil, nil, err
		}
		if removeErr == nil {
			released = append(released, a.Workspace)
		} else {
			retained = append(retained, a.Workspace)
		}
	}
	return released, retained, nil
}

// removable is true only when no run of the attempt might still be alive.
func (s *Service) removable(t *coord.Task, a *coord.Attempt) (bool, error) {
	var alive int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM runs WHERE task = ? AND attempt = ? AND liveness != 'exited'`, t.ID, a.N).Scan(&alive)
	return alive == 0, err
}

// Verify re-checks proof for code merged outside Shephrd. It never
// changes repository history.
func (s *Service) Verify(ctx context.Context, c coord.Caller, t *coord.Task) (*Delivered, error) {
	if t.Deliverable != "code" || t.State != "done" || t.ArtifactID == 0 {
		return nil, fault.New("invalid_state", "%s has no current code result awaiting proof", t.Ref)
	}
	art, err := Load(s.DB, t.ArtifactID)
	if err != nil {
		return nil, err
	}
	repo, branch, err := s.repo(t)
	if err != nil {
		return nil, err
	}
	proven, err := ProveAncestry(repo, branch, art.Commit)
	if err != nil {
		return nil, err
	}
	if !proven {
		return nil, fault.New("not_proven", "%s is not on %s yet", art.Commit, branch).WithNext("task", "verify", t.Ref)
	}
	err = s.DB.Write(ctx, func(tx *store.Tx) error {
		t, err := coord.Load(tx, t.ID)
		if err != nil {
			return err
		}
		return s.close(tx, c, t, art, "ancestry", "git", !coord.Owns(c, t))
	})
	if err != nil {
		return nil, err
	}
	return s.finish(ctx, c, t.ID, &Delivered{Artifact: art})
}

// Discard closes a task as discarded and removes its workspaces even with
// uncommitted changes; committed work survives on its branches. With an
// attempt, it removes only that attempt's workspace.
func (s *Service) Discard(ctx context.Context, c coord.Caller, t *coord.Task, attempt int64) (*Delivered, error) {
	grant, err := Authorized(s.DB, s.Cfg, c, "discard", t)
	if err != nil {
		return nil, err
	}
	if attempt == 0 {
		err := s.DB.Write(ctx, func(tx *store.Tx) error {
			t, err := coord.Load(tx, t.ID)
			if err != nil {
				return err
			}
			if t.State == "running" || t.State == "waiting" {
				return fault.New("invalid_state", "%s is %s; stop it before discarding", t.Ref, t.State).WithNext("task", "stop", t.Ref)
			}
			if t.State != "closed" {
				if err := coord.Close(tx, c.String(), t, "discarded"); err != nil {
					return err
				}
			}
			if err := RecordUse(tx, grant, "discard", t, c); err != nil {
				return err
			}
			return dependentsChanged(tx, c, t, "discarded")
		})
		if err != nil {
			return nil, err
		}
	} else {
		a, err := coord.LoadAttempt(s.DB, t.ID, attempt)
		if err != nil {
			return nil, err
		}
		if ok, err := s.removable(t, a); err != nil || !ok {
			if err != nil {
				return nil, err
			}
			return nil, fault.New("run_live", "attempt %d of %s may still have a live run", attempt, t.Ref).WithNext("task", "stop", t.Ref)
		}
		if err := s.DB.Write(ctx, func(tx *store.Tx) error { return RecordUse(tx, grant, "discard", t, c) }); err != nil {
			return nil, err
		}
	}
	released, retained, err := s.Release(ctx, t.ID, true)
	if err != nil {
		return nil, err
	}
	t, err = coord.Load(s.DB, t.ID)
	return &Delivered{Task: t, Released: released, Retained: retained}, err
}

// dependentsChanged tells the owner of every task built on this one that
// it changed under them.
func dependentsChanged(tx *store.Tx, c coord.Caller, t *coord.Task, change string) error {
	dependents, err := coord.Query(tx, `t.state != 'closed' AND t.id IN (SELECT task FROM dependencies WHERE dependency = ?)`, t.ID)
	if err != nil {
		return err
	}
	for _, d := range dependents {
		seq, err := tx.Emit("dependency.changed", c.String(), d.ID, 0, 0, map[string]string{"task": d.Ref, "dependency": t.Ref, "change": change})
		if err != nil {
			return err
		}
		if !coord.Owns(c, d) {
			if err := coord.WakeOwner(tx, d, seq); err != nil {
				return err
			}
		}
	}
	return nil
}

// sealedFiles lists the report files snapshotted from an attempt.
func sealedFiles(q coord.Querier, task, attempt int64) ([]string, error) {
	rows, err := q.Query(`SELECT id FROM artifacts WHERE task = ? AND attempt = ? AND kind = 'report'`, task, attempt)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var files []string
	for _, id := range ids {
		art, err := Load(q, id)
		if err != nil {
			return nil, err
		}
		for _, f := range art.Files {
			files = append(files, f.Path)
		}
	}
	return files, nil
}
