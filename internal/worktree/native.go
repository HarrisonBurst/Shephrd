package worktree

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"shephrd/internal/config"
	"shephrd/internal/model"
)

// NativeManager implements the native_git_worktree backend: one
// ephemeral, attempt-owned Git worktree created on demand from the freshly
// resolved registered default-branch head and removed only after a validated
// landing/report proof or an explicit discard. There is no pool, no reuse,
// and no lease allocator; non-reuse identity is the attempt-unique path plus
// the exact per-worktree Git admin directory and shared common directory.
type Runner interface {
	Run(dir, command string, args ...string) ([]byte, []byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(dir, command string, args ...string) ([]byte, []byte, error) {
	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

type NativeManager struct {
	Runner Runner
	// Root is the configured directory that owns every native attempt
	// worktree. All creation and removal is contained inside it.
	Root string
}

func NewNative(root string) NativeManager {
	return NativeManager{Runner: ExecRunner{}, Root: root}
}

// NativeIdentity is the exact durable identity of one native worktree.
type NativeIdentity struct {
	Path      string
	GitDir    string
	CommonDir string
	Branch    string
	Head      string
}

// BranchName preserves the exact historical attempt branch contract.
func BranchName(attempt model.Attempt) string {
	branch := "shephrd/" + attempt.TaskID
	if attempt.Number > 1 {
		branch = fmt.Sprintf("%s-attempt-%d", branch, attempt.Number)
	}
	return branch
}

// AllocatePath computes the attempt-unique worktree path
// <root>/<repo-id>/<attempt-id>/<repo-name>. Every component is validated as
// a single clean path element so a hostile name can never escape the root.
func (m NativeManager) AllocatePath(repo model.Repo, attempt model.Attempt) (string, error) {
	if strings.TrimSpace(m.Root) == "" || !filepath.IsAbs(m.Root) {
		return "", fmt.Errorf("native worktree root %q must be an absolute path", m.Root)
	}
	for _, element := range []string{repo.ID, attempt.ID, repo.Name} {
		if element == "" || element == "." || element == ".." || strings.ContainsAny(element, "/\\") || element != filepath.Clean(element) {
			return "", fmt.Errorf("native worktree path element %q is not a safe single path component", element)
		}
	}
	return filepath.Join(m.Root, repo.ID, attempt.ID, repo.Name), nil
}

func (m NativeManager) ResolveBase(repo model.Repo) (string, error) {
	return m.ResolveDefaultBase(repo)
}

func (m NativeManager) ResolveDefaultBase(repo model.Repo) (string, error) {
	if err := m.validateBranch(repo.Path, repo.DefaultBranch); err != nil {
		return "", fmt.Errorf("registered default branch %q is invalid: %w", repo.DefaultBranch, err)
	}
	local, err := m.resolveCommitRef(repo.Path, "refs/heads/"+repo.DefaultBranch)
	if err != nil {
		return "", fmt.Errorf("resolve registered default branch refs/heads/%s: %w", repo.DefaultBranch, err)
	}
	remote, err := m.authorizedRemote(repo.Path)
	if err != nil {
		return "", err
	}
	if remote == "" {
		return local, nil
	}
	if _, stderr, err := m.Runner.Run(repo.Path, "git", "fetch", "--no-tags", remote, repo.DefaultBranch); err != nil {
		return "", fmt.Errorf("fetch registered default branch %s from %s: %s: %w", repo.DefaultBranch, remote, strings.TrimSpace(string(stderr)), err)
	}
	remoteRef := "refs/remotes/" + remote + "/" + repo.DefaultBranch
	remoteTip, err := m.resolveCommitRef(repo.Path, remoteRef)
	if err != nil {
		return "", fmt.Errorf("resolve fetched default branch %s: %w", remoteRef, err)
	}
	relation, err := m.commitRelation(repo.Path, local, remoteTip)
	if err != nil {
		return "", err
	}
	switch relation {
	case "equal", "remote_ahead":
		return remoteTip, nil
	case "local_ahead":
		return local, nil
	default:
		return "", fmt.Errorf("registered default branch %s diverges from %s/%s (local %s, remote %s); refusing to choose a base", repo.DefaultBranch, remote, repo.DefaultBranch, local, remoteTip)
	}
}

func (m NativeManager) ResolveBranchBase(repo model.Repo, branch string) (string, error) {
	if err := m.validateBranch(repo.Path, branch); err != nil {
		return "", fmt.Errorf("explicit base branch %q is invalid: %w", branch, err)
	}
	commit, err := m.resolveCommitRef(repo.Path, "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("resolve explicit base branch %s: %w", branch, err)
	}
	return commit, nil
}

func (m NativeManager) ResolveCommitBase(repo model.Repo, commit string) (string, error) {
	commit = strings.TrimSpace(commit)
	if !model.ValidCommitID(commit) {
		return "", fmt.Errorf("explicit base commit %q must be a full lowercase Git object ID", commit)
	}
	resolved, err := m.resolveCommitRef(repo.Path, commit)
	if err != nil {
		return "", fmt.Errorf("resolve explicit base commit %s: %w", commit, err)
	}
	if resolved != commit {
		return "", fmt.Errorf("explicit base commit %s resolved to %s; refusing an ambiguous or rewritten commit", commit, resolved)
	}
	return resolved, nil
}

func (m NativeManager) authorizedRemote(path string) (string, error) {
	stdout, stderr, err := m.Runner.Run(path, "git", "remote")
	if err != nil {
		return "", fmt.Errorf("inspect configured Git remotes: %s: %w", strings.TrimSpace(string(stderr)), err)
	}
	remotes := strings.Fields(string(stdout))
	if len(remotes) == 0 {
		return "", nil
	}
	remote := ""
	for _, candidate := range remotes {
		if candidate == "origin" {
			remote = candidate
			break
		}
	}
	if remote == "" {
		if len(remotes) != 1 {
			return "", fmt.Errorf("configured Git remotes %q have no unambiguous authorized default-branch remote", remotes)
		}
		remote = remotes[0]
	}
	if _, stderr, err := m.Runner.Run(path, "git", "remote", "get-url", remote); err != nil {
		return "", fmt.Errorf("inspect configured %s remote: %s: %w", remote, strings.TrimSpace(string(stderr)), err)
	}
	return remote, nil
}

func (m NativeManager) validateBranch(path, branch string) error {
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("branch must not be empty")
	}
	if _, stderr, err := m.Runner.Run(path, "git", "check-ref-format", "--branch", branch); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(stderr)))
	}
	return nil
}

func (m NativeManager) resolveCommitRef(path, ref string) (string, error) {
	stdout, stderr, err := m.Runner.Run(path, "git", "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(string(stderr)), err)
	}
	commit := strings.TrimSpace(string(stdout))
	if !model.ValidCommitID(commit) {
		return "", fmt.Errorf("resolved ref %s is not a full Git object ID", ref)
	}
	return commit, nil
}

func (m NativeManager) commitRelation(path, local, remote string) (string, error) {
	stdout, stderr, err := m.Runner.Run(path, "git", "rev-list", "--left-right", "--count", "--end-of-options", local+"..."+remote)
	if err != nil {
		return "", fmt.Errorf("compare local and fetched default branch commits: %s: %w", strings.TrimSpace(string(stderr)), err)
	}
	parts := strings.Fields(string(stdout))
	if len(parts) != 2 {
		return "", fmt.Errorf("compare local and fetched default branch commits returned invalid counts %q", strings.TrimSpace(string(stdout)))
	}
	left, leftErr := strconv.Atoi(parts[0])
	right, rightErr := strconv.Atoi(parts[1])
	if leftErr != nil || rightErr != nil {
		return "", fmt.Errorf("compare local and fetched default branch commits returned invalid counts %q", strings.TrimSpace(string(stdout)))
	}
	switch {
	case left == 0 && right == 0:
		return "equal", nil
	case left == 0:
		return "remote_ahead", nil
	case right == 0:
		return "local_ahead", nil
	default:
		return "diverged", nil
	}
}

// CheckSupported fails with an actionable diagnostic for repository shapes
// the native backend does not yet claim to handle safely: active submodules
// (Git documents multiple superproject checkouts as incomplete) and Git LFS
// content (each worktree needs a tested checkout/materialization path).
func (m NativeManager) CheckSupported(repo model.Repo, base string) error {
	if _, _, err := m.Runner.Run(repo.Path, "git", "cat-file", "-e", base+":.gitmodules"); err == nil {
		return fmt.Errorf("repository %s has active submodules at %s; submodule superprojects are not supported", repo.Name, base)
	}
	stdout, _, err := m.Runner.Run(repo.Path, "git", "grep", "-l", "-e", "filter=lfs", base, "--", ".gitattributes", "*/.gitattributes")
	if err == nil && strings.TrimSpace(string(stdout)) != "" {
		return fmt.Errorf("repository %s uses Git LFS at %s; Git LFS worktrees are not supported", repo.Name, base)
	}
	return nil
}

// Add creates the attempt branch and worktree at the exact allocated path.
// It fails closed before touching Git when the branch already exists or the
// path is already occupied, and never deletes an existing branch.
func (m NativeManager) Add(repo model.Repo, path, branch, base string) error {
	if err := m.verifyIntendedContainment(path); err != nil {
		return err
	}
	if _, direct := m.Runner.(ExecRunner); direct {
		if err := config.Require("git"); err != nil {
			return err
		}
	}
	if _, _, err := m.Runner.Run(repo.Path, "git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return fmt.Errorf("branch %s already exists in %s; refusing to reuse or delete it for a new attempt worktree", branch, repo.Name)
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("native worktree path %s already exists; refusing to reuse it", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect native worktree path %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create native worktree container: %w", err)
	}
	if _, stderr, err := m.Runner.Run(repo.Path, "git", "worktree", "add", "-b", branch, path, base); err != nil {
		return fmt.Errorf("git worktree add failed for %s: %s: %w", path, strings.TrimSpace(string(stderr)), err)
	}
	return nil
}

// Inspect reads the worktree identity back from Git itself and verifies it
// against the registered repository: exact resolved path, shared common Git
// directory, per-worktree admin directory under <common>/worktrees, and the
// exact checked-out branch.
func (m NativeManager) Inspect(repo model.Repo, path, branch string) (NativeIdentity, error) {
	identity := NativeIdentity{Path: path, Branch: branch}
	repoCommon, err := m.commonDir(repo.Path)
	if err != nil {
		return identity, fmt.Errorf("resolve registered common Git directory: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return identity, fmt.Errorf("resolve native worktree path %s: %w", path, err)
	}
	top, err := m.git(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return identity, fmt.Errorf("read native worktree toplevel: %w", err)
	}
	resolvedTop, err := filepath.EvalSymlinks(top)
	if err != nil {
		return identity, fmt.Errorf("resolve native worktree toplevel %s: %w", top, err)
	}
	if resolvedTop != resolvedPath {
		return identity, fmt.Errorf("native worktree toplevel %s does not match attempt path %s", top, path)
	}
	gitDir, err := m.git(path, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return identity, fmt.Errorf("read native worktree git dir: %w", err)
	}
	common, err := m.commonDir(path)
	if err != nil {
		return identity, fmt.Errorf("resolve native worktree common Git directory: %w", err)
	}
	if common != repoCommon {
		return identity, fmt.Errorf("native worktree common Git directory %s does not match registered repository common directory %s", common, repoCommon)
	}
	resolvedGitDir, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		return identity, fmt.Errorf("resolve native worktree git dir %s: %w", gitDir, err)
	}
	worktreesDir := filepath.Join(common, "worktrees") + string(filepath.Separator)
	if !strings.HasPrefix(resolvedGitDir+string(filepath.Separator), worktreesDir) {
		return identity, fmt.Errorf("native worktree admin directory %s is not inside %s", gitDir, filepath.Join(common, "worktrees"))
	}
	checkedOut, err := m.git(path, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || checkedOut != "refs/heads/"+branch {
		return identity, fmt.Errorf("native worktree %s is not checked out on refs/heads/%s", path, branch)
	}
	head, err := m.git(path, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil {
		return identity, fmt.Errorf("read native worktree HEAD: %w", err)
	}
	identity.GitDir = resolvedGitDir
	identity.CommonDir = common
	identity.Head = head
	return identity, nil
}

// VerifyHeld re-verifies the exact recorded identity of a held worktree. It
// is the containment gate for relaunch, release, and recovery: any mismatch
// between the recorded path/gitdir/common-dir/branch and current Git evidence
// fails closed.
func (m NativeManager) VerifyHeld(repo model.Repo, attempt model.Attempt) (NativeIdentity, error) {
	if attempt.WorktreePath == "" || attempt.WorktreeGitDir == "" || attempt.WorktreeCommonDir == "" || attempt.Branch == "" {
		return NativeIdentity{}, fmt.Errorf("attempt %s has incomplete native worktree identity", attempt.ID)
	}
	if err := m.verifyContainment(attempt.WorktreePath); err != nil {
		return NativeIdentity{}, err
	}
	identity, err := m.Inspect(repo, attempt.WorktreePath, attempt.Branch)
	if err != nil {
		return identity, err
	}
	if identity.GitDir != attempt.WorktreeGitDir {
		return identity, fmt.Errorf("attempt %s worktree admin directory changed from %s to %s", attempt.ID, attempt.WorktreeGitDir, identity.GitDir)
	}
	if identity.CommonDir != attempt.WorktreeCommonDir {
		return identity, fmt.Errorf("attempt %s common Git directory changed from %s to %s", attempt.ID, attempt.WorktreeCommonDir, identity.CommonDir)
	}
	return identity, nil
}

// RegistrationState reports whether the attempt path exists on disk and
// whether Git still registers a linked worktree at that path. Recovery uses
// the pair to classify crashes without guessing.
func (m NativeManager) RegistrationState(repo model.Repo, path string) (pathExists, registered bool, err error) {
	if _, statErr := os.Lstat(path); statErr == nil {
		pathExists = true
	} else if !os.IsNotExist(statErr) {
		return false, false, fmt.Errorf("inspect native worktree path %s: %w", path, statErr)
	}
	stdout, stderr, runErr := m.Runner.Run(repo.Path, "git", "worktree", "list", "--porcelain")
	if runErr != nil {
		return pathExists, false, fmt.Errorf("list registered worktrees: %s: %w", strings.TrimSpace(string(stderr)), runErr)
	}
	resolved := resolveThroughExistingAncestor(path)
	for _, line := range strings.Split(string(stdout), "\n") {
		candidate, found := strings.CutPrefix(strings.TrimSpace(line), "worktree ")
		if !found {
			continue
		}
		if candidate == path || candidate == resolved || resolveThroughExistingAncestor(candidate) == resolved {
			return pathExists, true, nil
		}
	}
	return pathExists, false, nil
}

// resolveThroughExistingAncestor resolves symlinks through the deepest
// existing ancestor so a removed directory still compares equal to its
// recorded registration path.
func resolveThroughExistingAncestor(path string) string {
	path = filepath.Clean(path)
	missing := make([]string, 0)
	current := path
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// Dirty reports whether the worktree currently has tracked or untracked
// visible changes.
func (m NativeManager) Dirty(path string) (bool, error) {
	stdout, stderr, err := m.Runner.Run(path, "git", "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return false, fmt.Errorf("inspect native worktree dirt: %s: %w", strings.TrimSpace(string(stderr)), err)
	}
	return strings.TrimSpace(string(stdout)) != "", nil
}

// LiveProcessPIDs is a best-effort defense that lists live processes holding
// files or working directories under the worktree path. When lsof is
// unavailable it reports nothing rather than failing the release.
func (m NativeManager) LiveProcessPIDs(path string) []int {
	stdout, _, err := m.Runner.Run("", "lsof", "-t", "+D", path)
	if err != nil && strings.TrimSpace(string(stdout)) == "" {
		return nil
	}
	pids := make([]int, 0)
	self := os.Getpid()
	for _, line := range strings.Fields(string(stdout)) {
		pid, parseErr := strconv.Atoi(line)
		if parseErr != nil || pid <= 0 || pid == self {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// Remove removes the exact attempt worktree. Non-forced removal is used for
// clean landed/report work; force is reserved for explicit discard. After
// Git succeeds only the attempt-owned empty containers are removed, and the
// attempt branch ref is always left in place as artifact history.
func (m NativeManager) Remove(repo model.Repo, path string, force bool) error {
	if err := m.verifyContainment(path); err != nil {
		return err
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	if _, stderr, err := m.Runner.Run(repo.Path, "git", args...); err != nil {
		return fmt.Errorf("git worktree remove failed for %s: %s: %w", path, strings.TrimSpace(string(stderr)), err)
	}
	m.removeEmptyContainers(path)
	return nil
}

// RemoveAbsentRegistration prunes a linked-worktree registration whose
// directory no longer exists, completing an interrupted removal.
func (m NativeManager) RemoveAbsentRegistration(repo model.Repo, path string) error {
	if err := m.verifyIntendedContainment(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("native worktree path %s still exists; refusing registration-only cleanup", path)
	}
	if _, stderr, err := m.Runner.Run(repo.Path, "git", "worktree", "remove", "--force", path); err != nil {
		return fmt.Errorf("remove absent worktree registration for %s: %s: %w", path, strings.TrimSpace(string(stderr)), err)
	}
	m.removeEmptyContainers(path)
	return nil
}

func (m NativeManager) removeEmptyContainers(path string) {
	// <root>/<repo-id>/<attempt-id>/<repo-name> -> remove the attempt and
	// repo containers only when empty; os.Remove refuses non-empty
	// directories, so unrelated content is never touched.
	attemptDir := filepath.Dir(path)
	repoDir := filepath.Dir(attemptDir)
	if m.pathInsideRoot(attemptDir) {
		_ = os.Remove(attemptDir)
	}
	if m.pathInsideRoot(repoDir) && filepath.Clean(repoDir) != filepath.Clean(m.Root) {
		_ = os.Remove(repoDir)
	}
}

// verifyIntendedContainment checks that a path is lexically owned by the
// configured root and that no already-existing component under the root is a
// symlink, rejecting containment attacks before creation or removal.
func (m NativeManager) verifyIntendedContainment(path string) error {
	if !m.pathInsideRoot(path) {
		return fmt.Errorf("native worktree path %s is outside the configured worktree root %s", path, m.Root)
	}
	current := filepath.Clean(path)
	root := filepath.Clean(m.Root)
	for current != root {
		info, err := os.Lstat(current)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("native worktree path component %s is a symlink; refusing symlinked attempt paths", current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return nil
}

// verifyContainment additionally requires the fully resolved path to stay
// inside the resolved root, so a symlinked ancestor cannot redirect removal.
func (m NativeManager) verifyContainment(path string) error {
	if err := m.verifyIntendedContainment(path); err != nil {
		return err
	}
	resolvedRoot, err := filepath.EvalSymlinks(m.Root)
	if err != nil {
		return fmt.Errorf("resolve native worktree root %s: %w", m.Root, err)
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve native worktree path %s: %w", path, err)
	}
	if resolvedPath != resolvedRoot && !strings.HasPrefix(resolvedPath+string(filepath.Separator), resolvedRoot+string(filepath.Separator)) {
		return fmt.Errorf("native worktree path %s resolves outside the configured worktree root %s", path, m.Root)
	}
	return nil
}

func (m NativeManager) pathInsideRoot(path string) bool {
	root := filepath.Clean(m.Root)
	cleaned := filepath.Clean(path)
	return filepath.IsAbs(cleaned) && cleaned != root && strings.HasPrefix(cleaned+string(filepath.Separator), root+string(filepath.Separator))
}

func (m NativeManager) git(dir string, args ...string) (string, error) {
	stdout, stderr, err := m.Runner.Run(dir, "git", args...)
	if err != nil {
		detail := strings.TrimSpace(string(stderr))
		if detail != "" {
			return "", fmt.Errorf("%s: %w", detail, err)
		}
		return "", err
	}
	return strings.TrimSpace(string(stdout)), nil
}

func (m NativeManager) commonDir(dir string) (string, error) {
	common, err := m.git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	return filepath.EvalSymlinks(filepath.Clean(common))
}

// RunSetupHook executes the registered repository setup hook inside the
// freshly created attempt worktree.
func (m NativeManager) RunSetupHook(repo model.Repo, path string) error {
	if repo.SetupHook == "" {
		return nil
	}
	if _, stderr, err := m.Runner.Run(path, "/bin/sh", "-c", repo.SetupHook); err != nil {
		return fmt.Errorf("repo setup hook failed: %s: %w", strings.TrimSpace(string(stderr)), err)
	}
	return nil
}
