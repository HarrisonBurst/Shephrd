package delivery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"shephrd/internal/config"
	"shephrd/internal/model"
)

const MaxReportInputBytes int64 = 64 * 1024
const ReportRecoveryValidation = "canonical-regular-stable-sha256+file-identity-v1"

type ReportFileEvidence struct {
	CanonicalPath       string
	FileIdentity        string
	FileMode            uint32
	FileModTimeUnixNano int64
	SHA256              string
	SizeBytes           int64
	ValidationKind      string
}

type LandingResult struct {
	Landed             bool      `json:"landed"`
	Kind               string    `json:"kind,omitempty"`
	SourceCommit       string    `json:"source_commit,omitempty"`
	TargetRef          string    `json:"target_ref,omitempty"`
	TargetCommit       string    `json:"target_commit,omitempty"`
	CheckpointRevision int       `json:"checkpoint_revision,omitempty"`
	VerifiedAt         time.Time `json:"verified_at,omitempty"`
}

type RemoteDeliveryResult struct {
	State    string `json:"state"`
	PRState  string `json:"pr_state,omitempty"`
	PRURL    string `json:"pr_url,omitempty"`
	MergedAt string `json:"merged_at,omitempty"`
}

type WorktreeResult struct {
	Released     bool   `json:"released"`
	ReleaseState string `json:"release_state"`
	RootDirty    bool   `json:"root_dirty"`
}

type Result struct {
	TaskID               string                            `json:"task_id,omitempty"`
	AttemptID            string                            `json:"attempt_id,omitempty"`
	Completion           string                            `json:"completion,omitempty"`
	CompletionProvenance string                            `json:"completion_provenance,omitempty"`
	BranchPushed         bool                              `json:"branch_pushed"`
	PRState              string                            `json:"pr_state,omitempty"`
	Landed               bool                              `json:"landed"`
	Reason               string                            `json:"reason"`
	Landing              LandingResult                     `json:"landing"`
	RemoteDelivery       RemoteDeliveryResult              `json:"remote_delivery"`
	Worktree             WorktreeResult                    `json:"worktree"`
	VerifiedArtifact     *model.VerifiedArtifact           `json:"verified_artifact,omitempty"`
	LifecycleHandlers    []model.ReportLifecycleInvocation `json:"lifecycle_handlers,omitempty"`
}

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

type Verifier struct {
	Runner  Runner
	DataDir string
	Forge   ForgeObserver
}

func New(dataDir string) Verifier {
	return Verifier{Runner: ExecRunner{}, DataDir: dataDir}
}

func (v Verifier) Verify(task model.Task, repo model.Repo, attempt model.Attempt, checkpoint model.AttemptCheckpoint) (Result, error) {
	result := Result{TaskID: task.ID, AttemptID: attempt.ID, Completion: "done", CompletionProvenance: model.CompletionProvenanceWorkerDone, RemoteDelivery: RemoteDeliveryResult{State: "unverified"}}
	if task.ArtifactRef == "" {
		return result, fmt.Errorf("task %s has no artifact ref; done cannot be verified", task.ID)
	}
	if err := model.ValidateDeliverableArtifact(task.Deliverable, task.ArtifactRef); err != nil {
		return result, err
	}
	if task.Deliverable == "report" {
		artifact, reason, err := v.snapshotReport(task, attempt)
		if err != nil {
			return result, err
		}
		result.Reason = reason
		if artifact == nil {
			return result, nil
		}
		result.Landed = true
		result.Landing = LandingResult{Landed: true, Kind: "report_artifact", VerifiedAt: time.Now().UTC()}
		result.VerifiedArtifact = artifact
		return result, nil
	}
	isPR := strings.Contains(task.ArtifactRef, "/pull/")
	if _, direct := v.Runner.(ExecRunner); direct {
		commands := []string{"git"}
		if isPR {
			commands = append(commands, "gh")
		}
		if err := config.Require(commands...); err != nil {
			return result, err
		}
	}
	if checkpoint.Producer != "worker" || checkpoint.AttemptID != attempt.ID || checkpoint.RunGeneration != attempt.RunGeneration || checkpoint.HeadCommit == "" || checkpoint.WorktreeDirty || checkpoint.WorkspaceFactsError != "" {
		return result, fmt.Errorf("attempt %s does not have a final clean worker checkpoint for remote verification", attempt.ID)
	}
	branch := ""
	var pr struct {
		State       string `json:"state"`
		MergedAt    string `json:"mergedAt"`
		BaseRefName string `json:"baseRefName"`
		HeadRefName string `json:"headRefName"`
		HeadRefOID  string `json:"headRefOid"`
		URL         string `json:"url"`
		MergeCommit struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if isPR {
		stdout, stderr, err := v.Runner.Run(task.RepoPath, "gh", "pr", "view", task.ArtifactRef, "--json", "state,mergedAt,baseRefName,headRefName,headRefOid,mergeCommit,url")
		if err != nil {
			result.Reason = "GitHub could not verify PR: " + strings.TrimSpace(string(stderr))
			return result, nil
		}
		if err := json.Unmarshal(stdout, &pr); err != nil {
			return result, fmt.Errorf("decode gh pr output: %w", err)
		}
		result.PRState = pr.State
		result.RemoteDelivery.PRState = pr.State
		result.RemoteDelivery.PRURL = pr.URL
		result.RemoteDelivery.MergedAt = pr.MergedAt
		branch = pr.HeadRefName
		if branch != attempt.Branch {
			result.Reason = fmt.Sprintf("PR head branch %q does not match attempt branch %q", branch, attempt.Branch)
			return result, nil
		}
	} else {
		if task.ArtifactRef != "branch:"+attempt.Branch {
			result.Reason = fmt.Sprintf("branch artifact must be branch:%s", attempt.Branch)
			return result, nil
		}
		branch = attempt.Branch
	}
	stdout, _, branchErr := v.Runner.Run(task.RepoPath, "git", "ls-remote", "--exit-code", "--heads", "origin", "refs/heads/"+branch)
	if branchErr == nil && strings.TrimSpace(string(stdout)) != "" {
		result.BranchPushed = true
		result.RemoteDelivery.State = "pushed"
	} else {
		result.RemoteDelivery.State = "not_pushed"
	}
	if !isPR {
		if result.BranchPushed {
			result.Reason = fmt.Sprintf("branch %s is delivered to origin but is not verified landed", branch)
		} else {
			result.Reason = fmt.Sprintf("branch %s is not delivered to origin", branch)
		}
		return result, nil
	}
	if pr.State != "MERGED" {
		result.Reason = fmt.Sprintf("PR state is %s; remote delivery is not landing", pr.State)
		return result, nil
	}
	if pr.BaseRefName != repo.DefaultBranch {
		result.Reason = fmt.Sprintf("merged PR base %q does not match registered default branch %q", pr.BaseRefName, repo.DefaultBranch)
		return result, nil
	}
	if pr.HeadRefOID != checkpoint.HeadCommit {
		result.Reason = fmt.Sprintf("merged PR head %s does not match sealed worker commit %s", pr.HeadRefOID, checkpoint.HeadCommit)
		return result, nil
	}
	if pr.MergeCommit.OID == "" || pr.MergedAt == "" {
		result.Reason = "merged PR is missing merge commit or merge time identity"
		return result, nil
	}
	result.Landed = true
	result.Landing = LandingResult{Landed: true, Kind: "github_pr", SourceCommit: checkpoint.HeadCommit,
		TargetRef: "refs/heads/" + repo.DefaultBranch, TargetCommit: pr.MergeCommit.OID,
		CheckpointRevision: checkpoint.Revision, VerifiedAt: time.Now().UTC()}
	result.Reason = fmt.Sprintf("PR is merged into %s with exact worker head %s", repo.DefaultBranch, checkpoint.HeadCommit)
	return result, nil
}

func (v Verifier) snapshotReport(task model.Task, attempt model.Attempt) (*model.VerifiedArtifact, string, error) {
	path := strings.TrimPrefix(task.ArtifactRef, "report:")
	expected, err := v.CanonicalReportPath(task.ID, attempt)
	if err != nil {
		return nil, "", err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	if filepath.Clean(absolute) != filepath.Clean(expected) {
		return nil, fmt.Sprintf("report artifact must be %s", expected), nil
	}
	dir := filepath.Join(v.DataDir, "artifacts", "sha256")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("create verified artifact directory: %w", err)
	}
	temporary, err := os.CreateTemp(dir, ".report-*")
	if err != nil {
		return nil, "", fmt.Errorf("create report snapshot: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	evidence, copyErr := hashStableReport(absolute, temporary, nil)
	if copyErr == nil {
		copyErr = temporary.Sync()
	}
	if copyErr == nil {
		copyErr = temporary.Chmod(0o444)
	}
	closeErr := temporary.Close()
	if copyErr != nil {
		return nil, "", fmt.Errorf("snapshot report artifact: %w", copyErr)
	}
	if closeErr != nil {
		return nil, "", fmt.Errorf("close report snapshot: %w", closeErr)
	}
	snapshotPath := filepath.Join(dir, evidence.SHA256+".md")
	if err := os.Link(temporaryPath, snapshotPath); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, "", fmt.Errorf("install report snapshot: %w", err)
		}
		if err := validateSnapshot(snapshotPath, evidence.SHA256, evidence.SizeBytes); err != nil {
			return nil, "", fmt.Errorf("existing content-addressed report snapshot is invalid: %w", err)
		}
	}
	artifact := &model.VerifiedArtifact{Kind: "report", OriginalRef: task.ArtifactRef, SHA256: evidence.SHA256, SizeBytes: evidence.SizeBytes, SnapshotPath: snapshotPath}
	return artifact, fmt.Sprintf("report artifact snapshotted with SHA-256 %s (%d bytes)", evidence.SHA256, evidence.SizeBytes), nil
}

func (v Verifier) CanonicalReportPath(taskID string, attempt model.Attempt) (string, error) {
	if attempt.ReportPath == "" {
		return filepath.Abs(filepath.Join(v.DataDir, taskID, "report.md"))
	}
	path, err := model.RunReportPath(v.DataDir, taskID, attempt.ID, attempt.RunGeneration)
	if err != nil {
		return "", err
	}
	if attempt.TaskID != taskID || attempt.RunGeneration < 1 || attempt.ReportPath != path {
		return "", fmt.Errorf("report destination does not match task %s attempt %s run %d", taskID, attempt.ID, attempt.RunGeneration)
	}
	return path, nil
}

func (v Verifier) ValidateReportRecovery(taskID string, attempt model.Attempt) (ReportFileEvidence, error) {
	path, err := v.CanonicalReportPath(taskID, attempt)
	if err != nil {
		return ReportFileEvidence{}, err
	}
	return hashStableReport(path, nil, nil)
}

func SameReportFileEvidence(attestation model.ReportRecoveryAttestation, evidence ReportFileEvidence) bool {
	return filepath.Clean(attestation.CanonicalReportPath) == filepath.Clean(evidence.CanonicalPath) &&
		attestation.FileIdentity == evidence.FileIdentity && attestation.FileMode == evidence.FileMode &&
		attestation.FileModTimeUnixNano == evidence.FileModTimeUnixNano && attestation.SHA256 == evidence.SHA256 &&
		attestation.SizeBytes == evidence.SizeBytes && attestation.ValidationKind == evidence.ValidationKind
}

func hashStableReport(path string, copyTo io.Writer, between func()) (ReportFileEvidence, error) {
	file, err := openPinnedRegular(path)
	if err != nil {
		return ReportFileEvidence{}, fmt.Errorf("report artifact %s does not exist or is not a pinned regular file: %w", path, err)
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return ReportFileEvidence{}, err
	}
	first := sha256.New()
	writer := io.Writer(first)
	if copyTo != nil {
		writer = io.MultiWriter(copyTo, first)
	}
	firstSize, err := io.Copy(writer, file)
	if err != nil {
		return ReportFileEvidence{}, err
	}
	if between != nil {
		between()
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return ReportFileEvidence{}, err
	}
	second := sha256.New()
	secondSize, err := io.Copy(second, file)
	if err != nil {
		return ReportFileEvidence{}, err
	}
	after, err := file.Stat()
	if err != nil {
		return ReportFileEvidence{}, err
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(after, current) {
		return ReportFileEvidence{}, fmt.Errorf("report artifact path changed during hashing")
	}
	firstDigest := hex.EncodeToString(first.Sum(nil))
	secondDigest := hex.EncodeToString(second.Sum(nil))
	if firstSize != secondSize || firstSize != before.Size() || secondSize != after.Size() || firstDigest != secondDigest || before.Mode() != after.Mode() || before.ModTime() != after.ModTime() || fileIdentity(file, before) != fileIdentity(file, after) {
		return ReportFileEvidence{}, fmt.Errorf("report artifact bytes or identity changed during hashing")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return ReportFileEvidence{}, err
	}
	return ReportFileEvidence{CanonicalPath: absolute, FileIdentity: fileIdentity(file, after), FileMode: uint32(after.Mode()),
		FileModTimeUnixNano: after.ModTime().UnixNano(), SHA256: secondDigest, SizeBytes: secondSize, ValidationKind: ReportRecoveryValidation}, nil
}

func (v Verifier) ReadVerifiedReport(artifact model.VerifiedArtifact) ([]byte, error) {
	return v.readVerifiedReport(artifact.ProducerTaskID, artifact.Kind, artifact.SHA256, artifact.SizeBytes, artifact.SnapshotPath)
}

func (v Verifier) ReadReportInput(input model.ReportInput) ([]byte, error) {
	return v.readVerifiedReport(input.ProducerTaskID, input.Kind, input.SHA256, input.SizeBytes, input.SnapshotPath)
}

func (v Verifier) readVerifiedReport(producerTaskID, kind, digest string, size int64, snapshotPath string) ([]byte, error) {
	if kind != "report" {
		return nil, fmt.Errorf("producer task %s artifact kind is %s, not report", producerTaskID, kind)
	}
	if size > MaxReportInputBytes {
		return nil, fmt.Errorf("producer report is %d bytes; explicit report inputs are limited to %d bytes, so create a bounded summary report", size, MaxReportInputBytes)
	}
	if size < 0 {
		return nil, fmt.Errorf("producer task %s report has an invalid negative byte count", producerTaskID)
	}
	expected, err := v.snapshotPath(digest)
	if err != nil {
		return nil, fmt.Errorf("producer task %s report has invalid provenance: %w", producerTaskID, err)
	}
	absolute, err := filepath.Abs(snapshotPath)
	if err != nil || filepath.Clean(absolute) != filepath.Clean(expected) {
		return nil, fmt.Errorf("producer task %s report snapshot path does not match its SHA-256 provenance", producerTaskID)
	}
	file, err := openPinnedRegular(absolute)
	if err != nil {
		return nil, fmt.Errorf("producer task %s verified report snapshot is missing or not a regular file", producerTaskID)
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, MaxReportInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read producer task %s verified report snapshot: %w", producerTaskID, err)
	}
	if int64(len(body)) != size {
		return nil, fmt.Errorf("producer task %s verified report byte count mismatch: got %d, want %d", producerTaskID, len(body), size)
	}
	actual := sha256.Sum256(body)
	if hex.EncodeToString(actual[:]) != digest {
		return nil, fmt.Errorf("producer task %s verified report SHA-256 mismatch", producerTaskID)
	}
	if !utf8.Valid(body) {
		return nil, fmt.Errorf("producer task %s verified report is not valid UTF-8", producerTaskID)
	}
	if bytes.IndexByte(body, 0) >= 0 {
		return nil, fmt.Errorf("producer task %s verified report contains a NUL byte", producerTaskID)
	}
	return body, nil
}

func (v Verifier) ValidateSnapshot(artifact model.VerifiedArtifact) error {
	expected, err := v.snapshotPath(artifact.SHA256)
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(artifact.SnapshotPath)
	if err != nil || filepath.Clean(absolute) != filepath.Clean(expected) {
		return fmt.Errorf("verified report snapshot path does not match its SHA-256 provenance")
	}
	return validateSnapshot(absolute, artifact.SHA256, artifact.SizeBytes)
}

func (v Verifier) snapshotPath(digest string) (string, error) {
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size || digest != strings.ToLower(digest) {
		return "", fmt.Errorf("invalid SHA-256 digest %q", digest)
	}
	return filepath.Abs(filepath.Join(v.DataDir, "artifacts", "sha256", digest+".md"))
}

func validateSnapshot(path, digest string, size int64) error {
	file, err := openPinnedRegular(path)
	if err != nil {
		return fmt.Errorf("snapshot is missing or not a regular file")
	}
	defer file.Close()
	hash := sha256.New()
	actualSize, err := io.Copy(hash, file)
	if err != nil {
		return err
	}
	if actualSize != size {
		return fmt.Errorf("byte count mismatch: got %d, want %d", actualSize, size)
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return fmt.Errorf("SHA-256 mismatch")
	}
	return nil
}

func openPinnedRegular(path string) (*os.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("not a regular file")
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		file.Close()
		return nil, fmt.Errorf("path changed or is not a regular file")
	}
	return file, nil
}
