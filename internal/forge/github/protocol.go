package github

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime/debug"
	"strings"

	extensionhost "shephrd/internal/extension"
)

const (
	ExtensionID       = "shephrd.github-observer"
	ExtensionVersion  = "1.0.0"
	CapabilityName    = "forge.github-observation"
	CapabilityVersion = 1
	ObserveOperation  = "observe"

	ObservationSchemaVersion = 1

	MaxArtifactBytes = 2048
	MaxCommitCount   = 8192
	MaxTextBytes     = 1024
)

var (
	gitOIDPattern    = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	ownerPartPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	hostPattern      = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)
)

type Request struct {
	Host         string `json:"host"`
	Owner        string `json:"owner"`
	Repository   string `json:"repository"`
	Artifact     string `json:"artifact"`
	SealedCommit string `json:"sealed_commit"`
}

type CompareObservation struct {
	Status       string `json:"status"`
	MergeBaseSHA string `json:"merge_base_sha"`
}

type RepositoryObservation struct {
	ID                string `json:"id"`
	NameWithOwner     string `json:"name_with_owner"`
	URL               string `json:"url"`
	DefaultBranchName string `json:"default_branch_name"`
	DefaultBranchOID  string `json:"default_branch_oid"`
}

type PullRequestObservation struct {
	ID             string `json:"id"`
	URL            string `json:"url"`
	Number         int    `json:"number"`
	State          string `json:"state"`
	MergedAt       string `json:"merged_at"`
	BaseRepository string `json:"base_repository"`
	BaseRefName    string `json:"base_ref_name"`
	HeadRefName    string `json:"head_ref_name"`
	HeadRefOID     string `json:"head_ref_oid"`
	MergeCommitOID string `json:"merge_commit_oid"`
}

type Observation struct {
	SchemaVersion      int                    `json:"schema_version"`
	Repository         RepositoryObservation  `json:"repository"`
	PullRequest        PullRequestObservation `json:"pull_request"`
	Commits            []string               `json:"commits"`
	PaginationComplete bool                   `json:"pagination_complete"`
	SealedCompare      CompareObservation     `json:"sealed_compare"`
	MergeCompare       CompareObservation     `json:"merge_compare"`
}

type Result struct {
	Observation Observation `json:"observation"`
}

func Capability() extensionhost.Capability {
	return extensionhost.Capability{Name: CapabilityName, Version: CapabilityVersion, Operations: []string{ObserveOperation}}
}

func Manifest() extensionhost.Manifest {
	return extensionhost.Manifest{
		Wire:         extensionhost.WireRange{Major: extensionhost.WireMajor, MinorMin: extensionhost.WireMinor, MinorMax: extensionhost.WireMinor},
		Extension:    extensionhost.Identity{ID: ExtensionID, Version: ExtensionVersion, BuildCommit: buildCommit()},
		Capabilities: []extensionhost.Capability{Capability()},
	}
}

func ValidateRequest(request Request) error {
	if !hostPattern.MatchString(request.Host) || len(request.Host) > 253 {
		return fmt.Errorf("GitHub observation host is invalid")
	}
	if !ownerPartPattern.MatchString(request.Owner) || !ownerPartPattern.MatchString(request.Repository) {
		return fmt.Errorf("GitHub observation repository identity is invalid")
	}
	if request.Artifact == "" || len(request.Artifact) > MaxArtifactBytes {
		return fmt.Errorf("GitHub observation artifact is invalid")
	}
	if !gitOIDPattern.MatchString(request.SealedCommit) {
		return fmt.Errorf("GitHub observation sealed commit is invalid")
	}
	return nil
}

func ValidateObservation(observation Observation) error {
	if observation.SchemaVersion != ObservationSchemaVersion {
		return fmt.Errorf("GitHub observation schema version is invalid")
	}
	if err := validateRepositoryObservation(observation.Repository); err != nil {
		return err
	}
	if err := validatePullRequestObservation(observation.PullRequest); err != nil {
		return err
	}
	if len(observation.Commits) > MaxCommitCount {
		return fmt.Errorf("GitHub observation commit count exceeds the limit")
	}
	for _, oid := range observation.Commits {
		if !gitOIDPattern.MatchString(oid) {
			return fmt.Errorf("GitHub observation commit identity is invalid")
		}
	}
	if err := validateCompareObservation(observation.SealedCompare); err != nil {
		return err
	}
	if err := validateCompareObservation(observation.MergeCompare); err != nil {
		return err
	}
	return nil
}

func ObservationDigest(observation Observation) (string, error) {
	canonical, err := json.Marshal(observation)
	if err != nil {
		return "", fmt.Errorf("canonicalize GitHub observation: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func validateRepositoryObservation(repository RepositoryObservation) error {
	if repository.ID == "" || len(repository.ID) > MaxTextBytes || repository.URL == "" || len(repository.URL) > MaxTextBytes {
		return fmt.Errorf("GitHub observation repository identity is invalid")
	}
	owner, name, ok := strings.Cut(repository.NameWithOwner, "/")
	if !ok || !ownerPartPattern.MatchString(owner) || !ownerPartPattern.MatchString(name) {
		return fmt.Errorf("GitHub observation repository name is invalid")
	}
	if !validBranchName(repository.DefaultBranchName) || !gitOIDPattern.MatchString(repository.DefaultBranchOID) {
		return fmt.Errorf("GitHub observation default branch is invalid")
	}
	return nil
}

func validatePullRequestObservation(pull PullRequestObservation) error {
	if pull.ID == "" {
		if pull.Number < 1 || pull.URL != "" || pull.State != "" || pull.MergedAt != "" ||
			pull.BaseRepository != "" || pull.BaseRefName != "" || pull.HeadRefName != "" || pull.HeadRefOID != "" || pull.MergeCommitOID != "" {
			return fmt.Errorf("GitHub observation pull request is invalid")
		}
		return nil
	}
	if pull.URL == "" || len(pull.URL) > MaxTextBytes || pull.Number < 1 || pull.State == "" || len(pull.State) > 64 ||
		len(pull.MergedAt) > 64 || pull.BaseRepository == "" || !validBranchName(pull.BaseRefName) ||
		!validBranchName(pull.HeadRefName) || !gitOIDPattern.MatchString(pull.HeadRefOID) {
		return fmt.Errorf("GitHub observation pull request is invalid")
	}
	if pull.MergeCommitOID != "" && !gitOIDPattern.MatchString(pull.MergeCommitOID) {
		return fmt.Errorf("GitHub observation pull request is invalid")
	}
	return nil
}

func validateCompareObservation(comparison CompareObservation) error {
	if comparison.MergeBaseSHA != "" && !gitOIDPattern.MatchString(comparison.MergeBaseSHA) {
		return fmt.Errorf("GitHub observation compare merge base is invalid")
	}
	if comparison.Status == "" && comparison.MergeBaseSHA == "" {
		return nil
	}
	if comparison.Status == "" || len(comparison.Status) > 64 {
		return fmt.Errorf("GitHub observation compare status is invalid")
	}
	return nil
}

func validBranchName(name string) bool {
	return name != "" && len(name) <= 255 && strings.IndexFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

func buildCommit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && len(setting.Value) <= 128 {
			return setting.Value
		}
	}
	return ""
}
