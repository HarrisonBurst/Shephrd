package delivery

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/model"
)

func (v Verifier) VerifyLocal(task model.Task, repo model.Repo, attempt model.Attempt, checkpoint model.AttemptCheckpoint, artifact string) (Result, error) {
	result := Result{TaskID: task.ID, AttemptID: attempt.ID, Completion: "done", CompletionProvenance: model.CompletionProvenanceWorkerDone, RemoteDelivery: RemoteDeliveryResult{State: "unverified"}, Worktree: WorktreeResult{ReleaseState: attempt.ReleaseState}}
	if _, direct := v.Runner.(ExecRunner); direct {
		if err := config.Require("git"); err != nil {
			return result, err
		}
	}
	if task.Deliverable != "code" {
		return result, fmt.Errorf("--local verifies code attempts only")
	}
	if artifact != "branch:"+attempt.Branch {
		return result, fmt.Errorf("local landing requires artifact branch:%s", attempt.Branch)
	}
	if checkpoint.AttemptID != attempt.ID || checkpoint.Producer != "worker" || checkpoint.RunGeneration != attempt.RunGeneration {
		return result, fmt.Errorf("attempt %s final checkpoint identity is stale or not worker-produced", attempt.ID)
	}
	if checkpoint.SessionID != attempt.SessionID || checkpoint.Branch != attempt.Branch {
		return result, fmt.Errorf("attempt %s final checkpoint session or branch identity does not match", attempt.ID)
	}
	if checkpoint.WorktreeDirty {
		return result, fmt.Errorf("attempt %s final worker checkpoint was dirty", attempt.ID)
	}
	if checkpoint.WorkspaceFactsError != "" {
		return result, fmt.Errorf("attempt %s final worker checkpoint has workspace fact error: %s", attempt.ID, checkpoint.WorkspaceFactsError)
	}
	if checkpoint.HeadCommit == "" {
		return result, fmt.Errorf("attempt %s final worker checkpoint has no sealed commit", attempt.ID)
	}
	if attempt.BaseCommit == "" {
		return result, fmt.Errorf("attempt %s has no recorded acquisition base commit", attempt.ID)
	}
	if attempt.WorktreePath == "" || attempt.Branch == "" {
		return result, fmt.Errorf("attempt %s has incomplete worker worktree identity", attempt.ID)
	}
	if _, _, err := v.Runner.Run(attempt.WorktreePath, "git", "check-ref-format", "--branch", attempt.Branch); err != nil {
		return result, fmt.Errorf("attempt %s has invalid branch %q", attempt.ID, attempt.Branch)
	}
	if _, _, err := v.Runner.Run(repo.Path, "git", "check-ref-format", "--branch", repo.DefaultBranch); err != nil {
		return result, fmt.Errorf("registered default branch %q is invalid", repo.DefaultBranch)
	}
	rootStatus, err := v.git(repo.Path, "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return result, fmt.Errorf("inspect registered root dirt: %w", err)
	}
	result.Worktree.RootDirty = strings.TrimSpace(rootStatus) != ""
	workerStatus, err := v.git(attempt.WorktreePath, "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return result, fmt.Errorf("inspect worker worktree dirt: %w", err)
	}
	if strings.TrimSpace(workerStatus) != "" {
		return result, fmt.Errorf("attempt %s worker worktree is currently dirty", attempt.ID)
	}
	rootCommon, err := v.commonDir(repo.Path)
	if err != nil {
		return result, fmt.Errorf("resolve registered common Git directory: %w", err)
	}
	workerCommon, err := v.commonDir(attempt.WorktreePath)
	if err != nil {
		return result, fmt.Errorf("resolve worker common Git directory: %w", err)
	}
	if rootCommon != workerCommon {
		return result, fmt.Errorf("attempt %s worker worktree does not share registered common Git directory", attempt.ID)
	}
	sourceOID, err := v.commit(attempt.WorktreePath, checkpoint.HeadCommit)
	if err != nil {
		return result, fmt.Errorf("sealed worker commit is absent from the registered repository: %w", err)
	}
	if sourceOID != checkpoint.HeadCommit {
		return result, fmt.Errorf("sealed worker commit resolved to %s instead of exact checkpoint OID %s", sourceOID, checkpoint.HeadCommit)
	}
	baseOID, err := v.commit(attempt.WorktreePath, attempt.BaseCommit)
	if err != nil {
		return result, fmt.Errorf("resolve attempt base commit: %w", err)
	}
	if sourceOID == baseOID {
		result.Reason = fmt.Sprintf("attempt %s has no code delta from its recorded base commit", attempt.ID)
		return result, nil
	}
	headOID, err := v.commit(attempt.WorktreePath, "HEAD")
	if err != nil {
		return result, fmt.Errorf("resolve worker HEAD: %w", err)
	}
	sourceRef := "refs/heads/" + attempt.Branch
	checkedOut, err := v.git(attempt.WorktreePath, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || strings.TrimSpace(checkedOut) != sourceRef {
		return result, fmt.Errorf("attempt %s worktree is not checked out on %s", attempt.ID, sourceRef)
	}
	branchOID, err := v.commit(attempt.WorktreePath, sourceRef)
	if err != nil {
		return result, fmt.Errorf("resolve worker branch ref %s: %w", sourceRef, err)
	}
	if headOID != sourceOID || branchOID != sourceOID {
		return result, fmt.Errorf("attempt %s worker HEAD or branch moved from sealed commit %s", attempt.ID, sourceOID)
	}
	targetRef := "refs/heads/" + repo.DefaultBranch
	targetOID, err := v.commit(repo.Path, targetRef)
	if err != nil {
		return result, fmt.Errorf("registered default branch ref %s does not resolve to a commit: %w", targetRef, err)
	}
	_, stderr, ancestorErr := v.Runner.Run(repo.Path, "git", "merge-base", "--is-ancestor", sourceOID, targetOID)
	if ancestorErr != nil {
		if exitCode(ancestorErr) == 1 {
			result.Reason = fmt.Sprintf("sealed worker commit %s is not contained in %s at %s", sourceOID, targetRef, targetOID)
			return result, nil
		}
		return result, fmt.Errorf("prove local landing ancestry: %s: %w", strings.TrimSpace(string(stderr)), ancestorErr)
	}
	branchAfter, err := v.commit(attempt.WorktreePath, sourceRef)
	if err != nil {
		return result, fmt.Errorf("reread worker branch ref %s: %w", sourceRef, err)
	}
	targetAfter, err := v.commit(repo.Path, targetRef)
	if err != nil {
		return result, fmt.Errorf("reread registered default branch ref %s: %w", targetRef, err)
	}
	if branchAfter != sourceOID || targetAfter != targetOID {
		return result, fmt.Errorf("Git refs changed concurrently during local landing verification; retry")
	}
	verifiedAt := time.Now().UTC()
	result.Landed = true
	result.Landing = LandingResult{Landed: true, Kind: "local_default_branch", SourceCommit: sourceOID,
		TargetRef: targetRef, TargetCommit: targetOID, CheckpointRevision: checkpoint.Revision, VerifiedAt: verifiedAt}
	result.Reason = fmt.Sprintf("landed locally: worker %s is contained in %s at %s; remote delivery was not verified", sourceOID, targetRef, targetOID)
	return result, nil
}

func (v Verifier) git(dir string, args ...string) (string, error) {
	stdout, stderr, err := v.Runner.Run(dir, "git", args...)
	if err != nil {
		detail := strings.TrimSpace(string(stderr))
		if detail != "" {
			return "", fmt.Errorf("%s: %w", detail, err)
		}
		return "", err
	}
	return strings.TrimSpace(string(stdout)), nil
}

func (v Verifier) commit(dir, ref string) (string, error) {
	return v.git(dir, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
}

func (v Verifier) commonDir(dir string) (string, error) {
	common, err := v.git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	return filepath.EvalSymlinks(filepath.Clean(common))
}

func exitCode(err error) int {
	type exitCoder interface {
		ExitCode() int
	}
	if code, ok := err.(exitCoder); ok {
		return code.ExitCode()
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode()
	}
	return -1
}
