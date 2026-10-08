package delivery

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"shephrd/internal/config"
	extensionhost "shephrd/internal/extension"
	forgegithub "shephrd/internal/forge/github"
	"shephrd/internal/model"
)

const ExternalGraphValidation = "github-pr-membership+compare-v1"

var externalGitOID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

type ForgeObserver interface {
	Observe(ctx context.Context, request forgegithub.Request) (forgegithub.Observation, error)
}

type remoteRepository struct {
	Host       string
	Owner      string
	Repository string
}

func (v Verifier) ValidateExternalDelivery(repo model.Repo, prURL, sealedCommit string) (model.ExternalDeliveryAttestation, error) {
	if v.Forge == nil {
		return model.ExternalDeliveryAttestation{}, model.Failure("forge_observation_not_configured", "the SHA-pinned GitHub observation extension is not configured")
	}
	if _, direct := v.Runner.(ExecRunner); direct {
		if err := config.Require("git"); err != nil {
			return model.ExternalDeliveryAttestation{}, model.Failure("registered_origin_missing", "Git is required to inspect the registered repository")
		}
	}
	if !externalGitOID.MatchString(sealedCommit) {
		return model.ExternalDeliveryAttestation{}, model.Failure("sealed_commit_mismatch", "--commit must be a full lowercase Git object ID")
	}
	if _, _, err := v.Runner.Run(repo.Path, "git", "check-ref-format", "--branch", repo.DefaultBranch); err != nil {
		return model.ExternalDeliveryAttestation{}, model.Failure("default_branch_mismatch", "registered default branch %q is invalid", repo.DefaultBranch)
	}
	origin, err := v.registeredOrigin(repo.Path)
	if err != nil {
		return model.ExternalDeliveryAttestation{}, err
	}
	observation, err := v.Forge.Observe(context.Background(), forgegithub.Request{Host: origin.Host, Owner: origin.Owner, Repository: origin.Repository, Artifact: prURL, SealedCommit: sealedCommit})
	if err != nil {
		return model.ExternalDeliveryAttestation{}, observationFailure(err)
	}
	return v.validateObservation(repo, origin, prURL, sealedCommit, observation)
}

func (v Verifier) validateObservation(repo model.Repo, origin remoteRepository, prURL, sealedCommit string, observation forgegithub.Observation) (model.ExternalDeliveryAttestation, error) {
	if err := forgegithub.ValidateObservation(observation); err != nil {
		return model.ExternalDeliveryAttestation{}, model.Failure("github_unavailable", "GitHub observation response is malformed or dishonest: %s", err)
	}
	remoteName := observation.Repository.NameWithOwner
	owner, name, ok := strings.Cut(remoteName, "/")
	if !ok || !strings.EqualFold(owner, origin.Owner) || !strings.EqualFold(name, origin.Repository) {
		return model.ExternalDeliveryAttestation{}, model.Failure("repository_mismatch", "GitHub canonical repository %q does not match registered origin %s/%s", remoteName, origin.Owner, origin.Repository)
	}
	if observation.Repository.DefaultBranchName != repo.DefaultBranch {
		return model.ExternalDeliveryAttestation{}, model.Failure("default_branch_mismatch", "GitHub default branch %q does not match registered default branch %q", observation.Repository.DefaultBranchName, repo.DefaultBranch)
	}
	pull := observation.PullRequest
	if pull.ID == "" || pull.URL == "" {
		return model.ExternalDeliveryAttestation{}, model.Failure("repository_mismatch", "pull request %d does not exist in registered repository %s", pull.Number, remoteName)
	}
	if pull.URL != prURL {
		return model.ExternalDeliveryAttestation{}, model.Failure("pr_url_invalid", "PR URL must be canonical %s", pull.URL)
	}
	if pull.State != "MERGED" || pull.MergedAt == "" {
		return model.ExternalDeliveryAttestation{}, model.Failure("pr_not_merged", "PR %s is %s, not merged", pull.URL, pull.State)
	}
	if _, err := time.Parse(time.RFC3339, pull.MergedAt); err != nil {
		return model.ExternalDeliveryAttestation{}, model.Failure("github_unavailable", "GitHub returned an invalid merge time for PR %s", pull.URL)
	}
	if !strings.EqualFold(pull.BaseRepository, remoteName) || pull.BaseRefName != repo.DefaultBranch {
		return model.ExternalDeliveryAttestation{}, model.Failure("pr_base_mismatch", "PR base %s:%s does not match registered repository %s default branch %s", pull.BaseRepository, pull.BaseRefName, remoteName, repo.DefaultBranch)
	}
	if !externalGitOID.MatchString(pull.HeadRefOID) || !externalGitOID.MatchString(pull.MergeCommitOID) || !externalGitOID.MatchString(observation.Repository.DefaultBranchOID) {
		return model.ExternalDeliveryAttestation{}, model.Failure("github_unavailable", "GitHub returned incomplete commit identity for PR %s", pull.URL)
	}
	if !observation.PaginationComplete {
		return model.ExternalDeliveryAttestation{}, model.Failure("github_unavailable", "GitHub commit pagination ended before all PR commits were validated")
	}
	member := false
	for _, oid := range observation.Commits {
		if oid == sealedCommit {
			member = true
		}
	}
	if !member {
		return model.ExternalDeliveryAttestation{}, model.Failure("sealed_commit_not_in_pr", "sealed commit %s is not an exact member of PR %s", sealedCommit, pull.URL)
	}
	if err := requireAncestry(observation.SealedCompare, sealedCommit, pull.HeadRefOID, "sealed_commit_not_pr_ancestor"); err != nil {
		return model.ExternalDeliveryAttestation{}, err
	}
	if err := requireAncestry(observation.MergeCompare, pull.MergeCommitOID, observation.Repository.DefaultBranchOID, "merge_not_on_default"); err != nil {
		return model.ExternalDeliveryAttestation{}, err
	}
	digest, err := forgegithub.ObservationDigest(observation)
	if err != nil {
		return model.ExternalDeliveryAttestation{}, err
	}
	return model.ExternalDeliveryAttestation{SchemaVersion: 1, Provider: "github", RemoteHost: origin.Host,
		RemoteRepository: remoteName, PRNumber: pull.Number, PRNodeID: pull.ID, PRURL: pull.URL,
		RegisteredDefaultBranch: repo.DefaultBranch, PRBaseRef: pull.BaseRefName, PRHeadRef: pull.HeadRefName,
		PRHeadCommit: pull.HeadRefOID, MergeCommit: pull.MergeCommitOID, MergedAt: pull.MergedAt,
		DefaultHeadAtValidation: observation.Repository.DefaultBranchOID, GraphValidation: ExternalGraphValidation,
		EvidenceDigest: digest, EvidenceValidatedAt: time.Now().UTC()}, nil
}

func requireAncestry(comparison forgegithub.CompareObservation, base, head string, failureKind string) error {
	if (comparison.Status != "ahead" && comparison.Status != "identical") || comparison.MergeBaseSHA != base {
		if failureKind == "sealed_commit_not_pr_ancestor" {
			return model.Failure(failureKind, "sealed commit %s is not an ancestor of final PR head %s", base, head)
		}
		return model.Failure(failureKind, "PR merge commit %s is not reachable from current registered default head %s", base, head)
	}
	return nil
}

func observationFailure(err error) error {
	var remote *extensionhost.RemoteError
	if errors.As(err, &remote) {
		switch remote.Failure.Code {
		case "pr_url_invalid":
			return model.Failure("pr_url_invalid", "%s", remote.Failure.Message)
		case "repository_mismatch":
			return model.Failure("repository_mismatch", "%s", remote.Failure.Message)
		}
		return model.Failure("github_unavailable", "%s", remote.Failure.Message)
	}
	var hostError *extensionhost.HostError
	if errors.As(err, &hostError) {
		switch hostError.Kind {
		case extensionhost.ErrorDeadlineExceeded:
			return model.Failure("github_unavailable", "GitHub observation timed out before returning evidence")
		case extensionhost.ErrorCancelled:
			return model.Failure("github_unavailable", "GitHub observation was cancelled before returning evidence")
		default:
			return model.Failure("github_unavailable", "GitHub observation failed: %s", hostError)
		}
	}
	return model.Failure("github_unavailable", "%v", err)
}

func (v Verifier) registeredOrigin(path string) (remoteRepository, error) {
	stdout, _, err := v.Runner.Run(path, "git", "remote", "get-url", "--all", "origin")
	if err != nil || strings.TrimSpace(string(stdout)) == "" {
		return remoteRepository{}, model.Failure("registered_origin_missing", "registered repository has no readable origin fetch URL")
	}
	var identity remoteRepository
	for _, raw := range strings.Split(strings.TrimSpace(string(stdout)), "\n") {
		candidate, err := parseRemoteRepository(strings.TrimSpace(raw))
		if err != nil {
			return remoteRepository{}, model.Failure("registered_origin_missing", "registered origin is not a canonical GitHub SSH or HTTPS repository URL")
		}
		if identity.Host == "" {
			identity = candidate
			continue
		}
		if !strings.EqualFold(identity.Host, candidate.Host) || !strings.EqualFold(identity.Owner, candidate.Owner) || !strings.EqualFold(identity.Repository, candidate.Repository) {
			return remoteRepository{}, model.Failure("repository_mismatch", "registered origin contains conflicting repository identities")
		}
	}
	return identity, nil
}

func parseRemoteRepository(raw string) (remoteRepository, error) {
	if raw == "" {
		return remoteRepository{}, fmt.Errorf("empty remote")
	}
	if !strings.Contains(raw, "://") {
		at := strings.LastIndex(raw, "@")
		colon := strings.Index(raw, ":")
		if colon < 1 || (at >= 0 && at > colon) {
			return remoteRepository{}, fmt.Errorf("invalid SSH remote")
		}
		host := raw[:colon]
		if at >= 0 {
			host = raw[at+1 : colon]
		}
		return remoteRepositoryFromParts(host, raw[colon+1:])
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.Port() != "" {
		return remoteRepository{}, fmt.Errorf("invalid remote URL")
	}
	switch parsed.Scheme {
	case "https", "ssh":
	default:
		return remoteRepository{}, fmt.Errorf("unsupported remote URL")
	}
	return remoteRepositoryFromParts(parsed.Hostname(), parsed.Path)
}

func remoteRepositoryFromParts(host, path string) (remoteRepository, error) {
	path = strings.Trim(strings.TrimSpace(path), "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if host == "" || len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return remoteRepository{}, fmt.Errorf("invalid repository path")
	}
	return remoteRepository{Host: strings.ToLower(host), Owner: parts[0], Repository: parts[1]}, nil
}
