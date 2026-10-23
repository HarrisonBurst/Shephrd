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
	Operation     string    `json:"operation"`
	Repo          ForgeRepo `json:"repo"`
	Branch        string    `json:"branch"`
	Target        string    `json:"target,omitempty"`
	Commit        string    `json:"commit"`
	PullRequest   string    `json:"pull_request,omitempty"`
	Title         string    `json:"title,omitempty"`
	Body          string    `json:"body,omitempty"`
	TitleExplicit bool      `json:"title_explicit,omitempty"`
	BodyExplicit  bool      `json:"body_explicit,omitempty"`
	Draft         bool      `json:"draft,omitempty"`
	DraftExplicit bool      `json:"draft_explicit,omitempty"`
	RequireNew    bool      `json:"require_new,omitempty"`
	Task          string    `json:"task"`
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

func pullRequest(q coord.Querier, task, artifact int64) (ref, url string, err error) {
	err = q.QueryRow(`SELECT pull_request, detail FROM landings WHERE task = ? AND (? = 0 OR artifact = ?) AND mode = 'pull_request' AND outcome = 'published' AND pull_request != '' ORDER BY id DESC LIMIT 1`, task, artifact, artifact).Scan(&ref, &url)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return ref, url, err
}

func (s *Service) stackedTarget(ctx context.Context, t *coord.Task, landing config.Landing, branch string) (string, bool, error) {
	a, err := coord.LoadAttempt(s.DB, t.ID, t.Attempt)
	if err != nil || a.StackedOn == 0 {
		return branch, false, err
	}
	dep, err := coord.Load(s.DB, a.StackedOn)
	if err != nil {
		return "", false, err
	}
	if dep.Milestone == "merged" {
		return branch, false, nil
	}
	if dep.State != "done" || dep.Milestone != "published" || dep.ArtifactID == 0 {
		return "", false, fault.New("not_proven", "stacked dependency %s has no current published artifact", dep.Ref)
	}
	art, err := Load(s.DB, dep.ArtifactID)
	if err != nil {
		return "", false, err
	}
	if a.Base != art.Commit {
		return "", false, fault.New("not_proven", "stacked dependency %s has changed since %s started", dep.Ref, t.Ref)
	}
	var pinned int64
	if err := s.DB.QueryRow(`SELECT artifact FROM attempt_inputs WHERE task = ? AND attempt = ? AND dependency = ?`, t.ID, a.N, dep.ID).Scan(&pinned); err != nil {
		return "", false, err
	}
	if pinned != art.ID {
		return "", false, fault.New("not_proven", "stacked dependency %s no longer matches %s's pinned input", dep.Ref, t.Ref)
	}
	ref, url, err := pullRequest(s.DB, dep.ID, art.ID)
	if err != nil {
		return "", false, err
	}
	if ref == "" || url == "" {
		return "", false, fault.New("not_proven", "stacked dependency %s has no recorded pull request", dep.Ref)
	}
	observed, err := s.forge(ctx, landing, dep, ForgeRequest{Operation: "observe", Branch: art.Branch, Commit: art.Commit, PullRequest: ref})
	if err != nil {
		return "", false, fault.New("not_proven", "stacked dependency %s cannot be read: %v", dep.Ref, err)
	}
	if observed.PullRequest != ref || observed.URL != url {
		return "", false, fault.New("not_proven", "stacked dependency %s has no matching pull request", dep.Ref)
	}
	if observed.State == "open" && observed.Head == art.Commit {
		repo, _, host, err := s.repo(dep)
		if err != nil {
			return "", false, err
		}
		var remote struct {
			Tip string `json:"tip"`
		}
		if err := host.Call(ctx, "branch_tip", execution.TipRequest{Repo: repo, Branch: art.Branch}, &remote); err != nil || remote.Tip != art.Commit {
			return "", false, fault.New("not_proven", "stacked dependency %s has no matching live branch", dep.Ref)
		}
		return art.Branch, false, nil
	}
	if observed.State != "merged" {
		return "", false, fault.New("not_proven", "stacked dependency %s has no matching open or merged pull request", dep.Ref)
	}
	parent, err := coord.LoadAttempt(s.DB, dep.ID, art.Attempt)
	if err != nil {
		return "", false, err
	}
	if parent.StackedOn != 0 || parent.Base != art.Base || dep.RepoID != t.RepoID {
		return "", false, fault.New("not_proven", "stacked dependency %s has no proved default-branch base", dep.Ref)
	}
	_, _, _, _, err = s.recordedMerge(ctx, dep, art, landing, &PullRequest{Ref: ref, URL: url})
	if err != nil {
		return "", false, err
	}
	return branch, true, nil
}

func (s *Service) deliverPullRequest(ctx context.Context, c coord.Caller, t *coord.Task, art *Artifact, landing config.Landing, grant string, options PublishOptions) (*Delivered, error) {
	repo, branch, host, err := s.repo(t)
	if err != nil {
		return nil, err
	}
	target, fallback, err := s.stackedTarget(ctx, t, landing, branch)
	if err != nil {
		return nil, err
	}
	var push execution.PushResponse
	if err := host.Call(ctx, "push_branch", execution.PushRequest{Repo: repo, Branch: art.Branch, Commit: art.Commit}, &push); err != nil {
		s.DB.Write(ctx, func(tx *store.Tx) error {
			return recordLanding(tx, c, t, art, landing.Mode, "failed", nil, fault.As(err).Message)
		})
		return nil, err
	}
	existing, existingURL, err := pullRequest(s.DB, t.ID, 0)
	if err != nil {
		return nil, err
	}
	title, body := t.Ref+": "+t.Title, art.Text
	if options.Title != nil {
		title = *options.Title
	}
	if options.Body != nil {
		body = *options.Body
	}
	published, err := s.forge(ctx, landing, t, ForgeRequest{
		Operation: "publish", Repo: ForgeRepo{Remote: push.Remote}, Branch: art.Branch, Target: target,
		Commit: art.Commit, PullRequest: existing, Title: title, Body: body,
		TitleExplicit: options.Title != nil, BodyExplicit: options.Body != nil, Draft: options.Draft, DraftExplicit: options.DraftExplicit,
		RequireNew: fallback && existing == "",
	})
	if err != nil {
		s.DB.Write(ctx, func(tx *store.Tx) error {
			return recordLanding(tx, c, t, art, landing.Mode, "failed", nil, fault.As(err).Message)
		})
		return nil, err
	}
	if published.PullRequest == "" || published.URL == "" || (fallback && existing != "" && (published.PullRequest != existing || published.URL != existingURL)) {
		return nil, fault.New("landing_failed", "forge %s did not identify the recorded pull request", landing.Forge)
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
		if _, err := tx.Exec(`INSERT INTO landings (task, artifact, mode, outcome, pull_request, detail, time, effective_base) VALUES (?, ?, ?, 'published', ?, ?, ?, ?)`,
			t.ID, art.ID, landing.Mode, pr.Ref, pr.URL, store.Timestamp(tx.Now), target); err != nil {
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

func (s *Service) provePullRequest(ctx context.Context, c coord.Caller, t *coord.Task, art *Artifact, landing config.Landing, pr *PullRequest, wake bool) (*Delivered, error) {
	observed, delta, proof, source, err := s.recordedMerge(ctx, t, art, landing, pr)
	if err != nil {
		return nil, err
	}
	rewritten := proof == "recorded_merge"
	err = s.DB.Write(ctx, func(tx *store.Tx) error {
		t, err := coord.Load(tx, t.ID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO landings (task, artifact, mode, outcome, pull_request, merge_commit, proof, proof_source, detail, time, sealed_base, sealed_commit, premerge_parent, expected_tree, carrier_tree)
			VALUES (?, ?, ?, 'merged', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, t.ID, art.ID, landing.Mode, pr.Ref, observed.MergeCommit, proof, source, pr.URL, store.Timestamp(tx.Now), art.Base, art.Commit, delta.Premerge, delta.ExpectedTree, delta.CarrierTree); err != nil {
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
	_, branch, _, err := s.repo(t)
	if err != nil {
		return nil, err
	}
	s.retargetStacked(ctx, t, landing, branch)
	pr.State = "merged"
	out, err := s.finish(ctx, c, t.ID, &Delivered{Artifact: art, PullRequest: pr})
	return out, err
}

func (s *Service) recordedMerge(ctx context.Context, t *coord.Task, art *Artifact, landing config.Landing, pr *PullRequest) (*ForgeResponse, execution.DeltaProof, string, string, error) {
	var delta execution.DeltaProof
	observed, err := s.forge(ctx, landing, t, ForgeRequest{Operation: "observe", Branch: art.Branch, Commit: art.Commit, PullRequest: pr.Ref})
	if err != nil {
		return nil, delta, "", "", fault.New("not_proven", "pull request %s cannot be read: %v", pr.Ref, err).WithNext("task", "verify", t.Ref)
	}
	if observed.State != "merged" || observed.PullRequest != pr.Ref || observed.URL == "" || observed.URL != pr.URL {
		return nil, delta, "", "", fault.New("not_proven", "pull request %s has no matching merged record", pr.Ref).WithNext("task", "verify", t.Ref)
	}
	repo, branch, host, err := s.repo(t)
	if err != nil {
		return nil, delta, "", "", err
	}
	proof, source := "ancestry", "git"
	if !s.onBranch(ctx, host, repo, branch, art.Commit) {
		if observed.MergeCommit == "" {
			return nil, delta, "", "", fault.New("not_proven", "pull request %s has no merge carrier", pr.Ref)
		}
		if err := host.Call(ctx, "prove_delta", execution.DeltaRequest{
			Repo: repo, Branch: branch, Base: art.Base, Commit: art.Commit, Carrier: observed.MergeCommit,
		}, &delta); err != nil {
			return nil, delta, "", "", fault.New("not_proven", "pull request %s has no matching sealed delta: %v", pr.Ref, err).WithNext("task", "verify", t.Ref)
		}
		proof, source = "recorded_merge", "forge:"+landing.Forge
	}
	return observed, delta, proof, source, nil
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
		ref, _, err := pullRequest(s.DB, d.ID, d.ArtifactID)
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
	EffectiveBase  string `json:"effective_base,omitempty"`
	Mode           string `json:"mode"`
	Outcome        string `json:"outcome"`
	PullRequest    string `json:"pull_request,omitempty"`
	MergeCommit    string `json:"merge_commit,omitempty"`
	Proof          string `json:"proof,omitempty"`
	Source         string `json:"proof_source,omitempty"`
	Detail         string `json:"detail,omitempty"`
	Time           string `json:"time"`
	SealedBase     string `json:"sealed_base,omitempty"`
	SealedCommit   string `json:"sealed_commit,omitempty"`
	PremergeParent string `json:"premerge_parent,omitempty"`
	ExpectedTree   string `json:"expected_tree,omitempty"`
	CarrierTree    string `json:"carrier_tree,omitempty"`
}

// Landings lists a task's landing attempts, oldest first.
func Landings(q coord.Querier, task int64) ([]Landing, error) {
	rows, err := q.Query(`SELECT effective_base, mode, outcome, pull_request, merge_commit, proof, proof_source, detail, time, sealed_base, sealed_commit, premerge_parent, expected_tree, carrier_tree FROM landings WHERE task = ? ORDER BY id`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	landings := []Landing{}
	for rows.Next() {
		var l Landing
		if err := rows.Scan(&l.EffectiveBase, &l.Mode, &l.Outcome, &l.PullRequest, &l.MergeCommit, &l.Proof, &l.Source, &l.Detail, &l.Time, &l.SealedBase, &l.SealedCommit, &l.PremergeParent, &l.ExpectedTree, &l.CarrierTree); err != nil {
			return nil, err
		}
		landings = append(landings, l)
	}
	return landings, rows.Err()
}
