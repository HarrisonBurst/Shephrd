package artifact

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/execution"
	"shephrd/internal/fault"
	"shephrd/internal/plugin"
	"shephrd/internal/store"
)

// ForgeRequest is the forge provider contract: publish opens or finds the
// pull request for a branch Shephrd already pushed, merge merges it once
// the forge allows, observe reports whether and how it merged, and
// retarget moves it onto another base branch.
type ForgeRequest struct {
	Operation   string    `json:"operation"`
	Repo        ForgeRepo `json:"repo"`
	Branch      string    `json:"branch"`
	Target      string    `json:"target,omitempty"`
	Commit      string    `json:"commit"`
	PullRequest string    `json:"pull_request,omitempty"`
	Title       string    `json:"title,omitempty"`
	Body        string    `json:"body,omitempty"`
	Task        string    `json:"task"`
}

type ForgeRepo struct {
	Name   string `json:"name"`
	Remote string `json:"remote"`
}

type ForgeResponse struct {
	Outcome     string `json:"outcome"`
	Reason      string `json:"reason,omitempty"`
	PullRequest string `json:"pull_request,omitempty"`
	URL         string `json:"url,omitempty"`
	State       string `json:"state,omitempty"`
	MergeCommit string `json:"merge_commit,omitempty"`
	Head        string `json:"head,omitempty"`
}

type PullRequest struct {
	Ref    string `json:"ref"`
	URL    string `json:"url,omitempty"`
	Target string `json:"target"`
	State  string `json:"state"`
}

// Forges calls the forge provider a repository's landing names.
func Forges(reg *plugin.Registry, getenv config.Getenv) func(context.Context, string, ForgeRequest) (ForgeResponse, error) {
	return func(ctx context.Context, name string, req ForgeRequest) (ForgeResponse, error) {
		provider := reg.Provider("forge", name)
		if provider == nil {
			return ForgeResponse{}, fault.New("plugin_failed", "forge %q is not a declared plugin providing forge", name)
		}
		out, err := reg.Call(ctx, provider, plugin.Call{Kind: "provide", Type: "forge", Body: req, Getenv: getenv})
		if err != nil {
			return ForgeResponse{}, fault.New("plugin_failed", "forge %s: %v", name, err)
		}
		var resp ForgeResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			return ForgeResponse{}, fault.New("plugin_failed", "forge %s answered invalidly: %v", name, err)
		}
		return resp, nil
	}
}

// forge calls the repository's forge provider, naming the repository by
// its origin, read on the repository's host.
func (s *Service) forge(ctx context.Context, landing config.Landing, t *coord.Task, req ForgeRequest) (*ForgeResponse, error) {
	if req.Repo.Remote == "" {
		repo, _, host, err := s.repo(t)
		if err != nil {
			return nil, err
		}
		var remote execution.PushResponse
		if err := host.Call(ctx, "remote_url", execution.PushRequest{Repo: repo}, &remote); err != nil {
			return nil, err
		}
		req.Repo.Remote = remote.Remote
	}
	req.Repo.Name, req.Task = t.Repo, t.Ref
	resp, err := s.Forge(ctx, landing.Forge, req)
	if err != nil {
		return nil, err
	}
	switch resp.Outcome {
	case "ok", "pending":
		return &resp, nil
	case "failed":
		return nil, fault.New("landing_failed", "forge %s could not %s: %s", landing.Forge, req.Operation, resp.Reason)
	}
	return nil, fault.New("plugin_failed", "forge %s answered %s with outcome %q", landing.Forge, req.Operation, resp.Outcome)
}

// pullRequest is the task's latest pull request, if it has one.
func pullRequest(q coord.Querier, task int64) (ref, url string, err error) {
	err = q.QueryRow(`SELECT pull_request, detail FROM landings WHERE task = ? AND pull_request != '' ORDER BY id DESC LIMIT 1`, task).Scan(&ref, &url)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return ref, url, err
}

// stackedOn returns the unmerged dependency the task's current attempt was
// built on, or nil.
func (s *Service) stackedOn(t *coord.Task) (*coord.Task, *Artifact, error) {
	a, err := coord.LoadAttempt(s.DB, t.ID, t.Attempt)
	if err != nil || a.StackedOn == 0 {
		return nil, nil, err
	}
	dep, err := coord.Load(s.DB, a.StackedOn)
	if err != nil || dep.Milestone == "merged" || dep.ArtifactID == 0 {
		return nil, nil, err
	}
	art, err := Load(s.DB, dep.ArtifactID)
	return dep, art, err
}

// deliverPullRequest pushes the sealed commit to the attempt branch and
// opens its pull request, against the stacked dependency's branch while
// that is unmerged. It merges right away only when Shephrd merges.
func (s *Service) deliverPullRequest(ctx context.Context, c coord.Caller, t *coord.Task, art *Artifact, landing config.Landing, grant string) (*Delivered, error) {
	repo, branch, host, err := s.repo(t)
	if err != nil {
		return nil, err
	}
	target := branch
	if _, depArt, err := s.stackedOn(t); err != nil {
		return nil, err
	} else if depArt != nil {
		target = depArt.Branch
	}
	var push execution.PushResponse
	if err := host.Call(ctx, "push_branch", execution.PushRequest{Repo: repo, Branch: art.Branch, Commit: art.Commit}, &push); err != nil {
		s.DB.Write(ctx, func(tx *store.Tx) error {
			return recordLanding(tx, c, t, art, landing.Mode, "failed", nil, fault.As(err).Message)
		})
		return nil, err
	}
	existing, _, err := pullRequest(s.DB, t.ID)
	if err != nil {
		return nil, err
	}
	published, err := s.forge(ctx, landing, t, ForgeRequest{
		Operation: "publish", Repo: ForgeRepo{Remote: push.Remote}, Branch: art.Branch, Target: target,
		Commit: art.Commit, PullRequest: existing, Title: t.Ref + ": " + t.Title, Body: art.Text,
	})
	if err != nil {
		s.DB.Write(ctx, func(tx *store.Tx) error {
			return recordLanding(tx, c, t, art, landing.Mode, "failed", nil, fault.As(err).Message)
		})
		return nil, err
	}
	pr := &PullRequest{Ref: published.PullRequest, URL: published.URL, Target: target, State: "open"}
	err = s.DB.Write(ctx, func(tx *store.Tx) error {
		t, err := coord.Load(tx, t.ID)
		if err != nil {
			return err
		}
		if err := RecordUse(tx, grant, "land", t, c); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO landings (task, artifact, mode, outcome, pull_request, detail, time) VALUES (?, ?, ?, 'published', ?, ?, ?)`,
			t.ID, art.ID, landing.Mode, pr.Ref, pr.URL, store.Timestamp(tx.Now)); err != nil {
			return err
		}
		if _, err := tx.Emit("land.attempted", c.String(), t.ID, art.Attempt, 0, map[string]any{
			"task": t.Ref, "commit": art.Commit, "mode": landing.Mode, "outcome": "published",
		}); err != nil {
			return err
		}
		if _, err := tx.Emit("task.published", c.String(), t.ID, 0, 0, map[string]any{
			"task": t.Ref, "artifact": art.Ref, "branch": art.Branch, "pull_request": pr.Ref, "url": pr.URL, "target": target,
		}); err != nil {
			return err
		}
		return coord.SetMilestone(tx, t, "published", false)
	})
	if err != nil {
		return nil, err
	}
	if landing.Merge == "shephrd" {
		return s.mergePullRequest(ctx, c, t, art, landing, pr)
	}
	t, err = coord.Load(s.DB, t.ID)
	return &Delivered{Task: t, Artifact: art, PullRequest: pr, Released: []string{}, Retained: []string{}}, err
}

func (s *Service) mergePullRequest(ctx context.Context, c coord.Caller, t *coord.Task, art *Artifact, landing config.Landing, pr *PullRequest) (*Delivered, error) {
	merged, err := s.forge(ctx, landing, t, ForgeRequest{Operation: "merge", Branch: art.Branch, Commit: art.Commit, PullRequest: pr.Ref})
	if err != nil {
		return nil, err
	}
	if merged.Outcome == "pending" {
		t, err := coord.Load(s.DB, t.ID)
		pr.State = "awaiting_merge"
		return &Delivered{Task: t, Artifact: art, PullRequest: pr, Pending: merged.Reason, Released: []string{}, Retained: []string{}}, err
	}
	return s.provePullRequest(ctx, c, t, art, landing, pr, false)
}

// provePullRequest closes a task whose pull request merged. Core proves
// it: by ancestry, or for squash and rebase merges by the forge's record
// that the sealed commit's pull request merged into the default branch.
func (s *Service) provePullRequest(ctx context.Context, c coord.Caller, t *coord.Task, art *Artifact, landing config.Landing, pr *PullRequest, wake bool) (*Delivered, error) {
	observed, err := s.forge(ctx, landing, t, ForgeRequest{Operation: "observe", Branch: art.Branch, Commit: art.Commit, PullRequest: pr.Ref})
	if err != nil {
		return nil, err
	}
	if observed.State != "merged" {
		return nil, fault.New("not_proven", "pull request %s is %s", pr.Ref, observed.State).WithNext("task", "verify", t.Ref)
	}
	if observed.Head != art.Commit {
		return nil, fault.New("not_proven", "pull request %s merged %s, not the sealed commit %s", pr.Ref, observed.Head, art.Commit)
	}
	repo, branch, host, err := s.repo(t)
	if err != nil {
		return nil, err
	}
	proof, source, rewritten := "ancestry", "git", false
	if !s.onBranch(ctx, host, repo, branch, art.Commit) {
		if observed.MergeCommit == "" || !s.onBranch(ctx, host, repo, branch, observed.MergeCommit) {
			return nil, fault.New("not_proven", "the merge of %s is not on %s", pr.Ref, branch).WithNext("task", "verify", t.Ref)
		}
		proof, source, rewritten = "recorded_merge", "forge:"+landing.Forge, true
	}
	err = s.DB.Write(ctx, func(tx *store.Tx) error {
		t, err := coord.Load(tx, t.ID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO landings (task, artifact, mode, outcome, pull_request, merge_commit, proof, proof_source, detail, time)
			VALUES (?, ?, ?, 'merged', ?, ?, ?, ?, ?, ?)`, t.ID, art.ID, landing.Mode, pr.Ref, observed.MergeCommit, proof, source, pr.URL, store.Timestamp(tx.Now)); err != nil {
			return err
		}
		if _, err := tx.Emit("land.attempted", c.String(), t.ID, art.Attempt, 0, map[string]any{
			"task": t.Ref, "commit": art.Commit, "mode": landing.Mode, "outcome": "merged", "pull_request": pr.Ref,
		}); err != nil {
			return err
		}
		if rewritten {
			if err := stackedChanged(tx, c, t, "merged_rewritten"); err != nil {
				return err
			}
		}
		return s.close(tx, c, t, art, proof, source, wake)
	})
	if err != nil {
		return nil, err
	}
	s.retargetStacked(ctx, t, landing, branch)
	pr.State = "merged"
	out, err := s.finish(ctx, c, t.ID, &Delivered{Artifact: art, PullRequest: pr})
	return out, err
}

func (s *Service) onBranch(ctx context.Context, host execution.Host, repo, branch, commit string) bool {
	var proof struct {
		Proven bool `json:"proven"`
	}
	return host.Call(ctx, "prove_ancestry", execution.AncestryRequest{Repo: repo, Branch: branch, Commit: commit}, &proof) == nil && proof.Proven
}

// retargetStacked moves the open pull requests stacked on a merged task
// onto the default branch.
func (s *Service) retargetStacked(ctx context.Context, t *coord.Task, landing config.Landing, branch string) {
	dependents, err := coord.Query(s.DB, `t.state != 'closed' AND t.id IN (SELECT task FROM attempts a WHERE a.stacked_on = ? AND a.n = (SELECT attempt FROM tasks WHERE id = a.task))`, t.ID)
	if err != nil {
		return
	}
	for _, d := range dependents {
		ref, _, err := pullRequest(s.DB, d.ID)
		if err != nil || ref == "" || d.ArtifactID == 0 {
			continue
		}
		art, err := Load(s.DB, d.ArtifactID)
		if err != nil {
			continue
		}
		_, err = s.forge(ctx, landing, d, ForgeRequest{Operation: "retarget", Branch: art.Branch, Target: branch, Commit: art.Commit, PullRequest: ref})
		s.DB.Write(ctx, func(tx *store.Tx) error {
			_, werr := tx.Emit("pull_request.retargeted", "system", d.ID, 0, 0, map[string]any{"task": d.Ref, "pull_request": ref, "target": branch, "error": errString(err)})
			return werr
		})
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// stackedChanged tells the owner of every task whose current attempt was
// built on this task that the work under it changed.
func stackedChanged(tx *store.Tx, c coord.Caller, t *coord.Task, change string) error {
	dependents, err := coord.Query(tx, `t.state != 'closed' AND t.id IN (SELECT task FROM attempts a WHERE a.stacked_on = ? AND a.n = (SELECT attempt FROM tasks WHERE id = a.task))`, t.ID)
	if err != nil {
		return err
	}
	for _, d := range dependents {
		seq, err := tx.Emit("dependency.changed", c.String(), d.ID, 0, 0, map[string]string{"task": d.Ref, "dependency": t.Ref, "change": change})
		if err != nil {
			return err
		}
		if err := coord.WakeOwner(tx, d, seq); err != nil {
			return err
		}
	}
	return nil
}

// Sealed tells stacked dependents when their dependency seals a new commit.
func Sealed(tx *store.Tx, c coord.Caller, t *coord.Task, a *Artifact) error {
	if a.Kind != "code" {
		return nil
	}
	return stackedChanged(tx, c, t, "new_commit")
}

// StackBase chooses a new attempt's base in a pull_request repository: the
// sealed commit of the one unmerged code dependency it waits on as
// published. Tasks waiting with --until merged never stack.
func StackBase(db *store.Store, cfg *config.Config) func(*coord.Task) (string, int64, error) {
	return func(t *coord.Task) (string, int64, error) {
		if cfg.Repos[t.Repo].Landing.Mode != "pull_request" {
			return "", 0, nil
		}
		deps, err := coord.Query(db, `t.deliverable = 'code' AND t.milestone = 'published' AND t.artifact IS NOT NULL
			AND t.id IN (SELECT dependency FROM dependencies WHERE task = ? AND until = 'published')`, t.ID)
		if err != nil || len(deps) == 0 {
			return "", 0, err
		}
		if len(deps) > 1 {
			return "", 0, fault.New("stacking_ambiguous", "%s waits on %d unmerged pull requests; a task stacks on one. Wait for the others with --until merged", t.Ref, len(deps))
		}
		art, err := Load(db, deps[0].ArtifactID)
		if err != nil {
			return "", 0, err
		}
		return art.Commit, deps[0].ID, nil
	}
}

type Landing struct {
	Mode        string `json:"mode"`
	Outcome     string `json:"outcome"`
	PullRequest string `json:"pull_request,omitempty"`
	MergeCommit string `json:"merge_commit,omitempty"`
	Proof       string `json:"proof,omitempty"`
	Source      string `json:"proof_source,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Time        string `json:"time"`
}

// Landings lists a task's landing attempts, oldest first.
func Landings(q coord.Querier, task int64) ([]Landing, error) {
	rows, err := q.Query(`SELECT mode, outcome, pull_request, merge_commit, proof, proof_source, detail, time FROM landings WHERE task = ? ORDER BY id`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	landings := []Landing{}
	for rows.Next() {
		var l Landing
		if err := rows.Scan(&l.Mode, &l.Outcome, &l.PullRequest, &l.MergeCommit, &l.Proof, &l.Source, &l.Detail, &l.Time); err != nil {
			return nil, err
		}
		landings = append(landings, l)
	}
	return landings, rows.Err()
}
