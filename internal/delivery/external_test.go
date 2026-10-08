package delivery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	extensionhost "shephrd/internal/extension"
	forgegithub "shephrd/internal/forge/github"
	"shephrd/internal/model"
)

const (
	externalSealed  = "1111111111111111111111111111111111111111"
	externalFollow1 = "2222222222222222222222222222222222222222"
	externalHead    = "3333333333333333333333333333333333333333"
	externalMerge   = "4444444444444444444444444444444444444444"
	externalDefault = "5555555555555555555555555555555555555555"
)

type externalRunner struct {
	origin     string
	failBranch bool
	calls      []string
}

func (r *externalRunner) Run(dir, command string, args ...string) ([]byte, []byte, error) {
	call := command + " " + strings.Join(args, " ")
	r.calls = append(r.calls, call)
	if command != "git" {
		return nil, nil, errors.New("unexpected command")
	}
	if args[0] == "check-ref-format" {
		if r.failBranch {
			return nil, nil, errors.New("invalid branch")
		}
		return nil, nil, nil
	}
	if args[0] == "remote" {
		if r.origin == "" {
			return nil, nil, errors.New("missing")
		}
		return []byte(r.origin + "\n"), nil, nil
	}
	return nil, nil, errors.New("unexpected command")
}

type externalForge struct {
	observation forgegithub.Observation
	err         error
	request     forgegithub.Request
	calls       int
}

func (f *externalForge) Observe(ctx context.Context, request forgegithub.Request) (forgegithub.Observation, error) {
	f.calls++
	f.request = request
	return f.observation, f.err
}

func TestExternalDeliveryValidationSupportsFollowupsAndSquashMerge(t *testing.T) {
	for _, origin := range []string{
		"https://token:credential@github.com/acme/demo.git?auth=query-secret#fragment-secret",
		"git@github.com:acme/demo.git",
		"ssh://git@github.com/acme/demo.git",
	} {
		t.Run(strings.Split(origin, ":")[0], func(t *testing.T) {
			runner := &externalRunner{origin: origin}
			forge := &externalForge{observation: validExternalObservation()}
			evidence, err := (Verifier{Runner: runner, Forge: forge}).ValidateExternalDelivery(model.Repo{Path: "/repo", DefaultBranch: "main"}, "https://github.com/acme/demo/pull/42", externalSealed)
			if err != nil {
				t.Fatal(err)
			}
			if evidence.RemoteHost != "github.com" || evidence.RemoteRepository != "acme/demo" || evidence.PRNumber != 42 || evidence.PRHeadCommit != externalHead || evidence.MergeCommit != externalMerge || evidence.DefaultHeadAtValidation != externalDefault || evidence.GraphValidation != ExternalGraphValidation {
				t.Fatalf("evidence = %+v", evidence)
			}
			wantDigest, err := forgegithub.ObservationDigest(forge.observation)
			if err != nil {
				t.Fatal(err)
			}
			if evidence.EvidenceDigest != wantDigest || len(evidence.EvidenceDigest) != 64 {
				t.Fatalf("evidence digest = %q want %q", evidence.EvidenceDigest, wantDigest)
			}
			sent := fmt.Sprintf("%+v", forge.request)
			if forge.request.Host != "github.com" || forge.request.Owner != "acme" || forge.request.Repository != "demo" || forge.request.SealedCommit != externalSealed || forge.request.Artifact != "https://github.com/acme/demo/pull/42" {
				t.Fatalf("request = %+v", forge.request)
			}
			if strings.Contains(sent, "token:credential") || strings.Contains(sent, "query-secret") || strings.Contains(sent, "fragment-secret") {
				t.Fatalf("request leaked remote credentials: %s", sent)
			}
		})
	}
}

func TestExternalDeliveryValidationAcceptsExactFinalHead(t *testing.T) {
	observation := validExternalObservation()
	observation.PullRequest.HeadRefOID = externalSealed
	observation.SealedCompare = forgegithub.CompareObservation{Status: "identical", MergeBaseSHA: externalSealed}
	forge := &externalForge{observation: observation}
	evidence, err := (Verifier{Runner: &externalRunner{origin: "https://github.com/acme/demo.git"}, Forge: forge}).ValidateExternalDelivery(model.Repo{Path: "/repo", DefaultBranch: "main"}, "https://github.com/acme/demo/pull/42", externalSealed)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.PRHeadCommit != externalSealed {
		t.Fatalf("evidence = %+v", evidence)
	}
}

func TestExternalDeliveryValidationFailuresAreClassified(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*externalForge)
		runner func(*externalRunner)
		pr     string
		want   string
	}{
		{name: "repository mismatch", mutate: func(f *externalForge) { f.observation.Repository.NameWithOwner = "acme/other" }, want: "repository_mismatch"},
		{name: "open", mutate: func(f *externalForge) {
			f.observation.PullRequest.State = "OPEN"
			f.observation.PullRequest.MergedAt = ""
		}, want: "pr_not_merged"},
		{name: "wrong default", mutate: func(f *externalForge) { f.observation.Repository.DefaultBranchName = "release" }, want: "default_branch_mismatch"},
		{name: "wrong base", mutate: func(f *externalForge) { f.observation.PullRequest.BaseRefName = "release" }, want: "pr_base_mismatch"},
		{name: "commit absent", mutate: func(f *externalForge) { f.observation.Commits = []string{externalFollow1, externalHead} }, want: "sealed_commit_not_in_pr"},
		{name: "sealed diverged", mutate: func(f *externalForge) { f.observation.SealedCompare.Status = "diverged" }, want: "sealed_commit_not_pr_ancestor"},
		{name: "merge missing from default", mutate: func(f *externalForge) { f.observation.MergeCompare.Status = "diverged" }, want: "merge_not_on_default"},
		{name: "pagination incomplete", mutate: func(f *externalForge) { f.observation.PaginationComplete = false }, want: "github_unavailable"},
		{name: "malformed observation", mutate: func(f *externalForge) { f.observation.SchemaVersion = 99 }, want: "github_unavailable"},
		{name: "invalid merge time", mutate: func(f *externalForge) { f.observation.PullRequest.MergedAt = "not-a-time" }, want: "github_unavailable"},
		{name: "incomplete identity", mutate: func(f *externalForge) { f.observation.PullRequest.MergeCommitOID = "" }, want: "github_unavailable"},
		{name: "pull request missing", mutate: func(f *externalForge) {
			f.observation.PullRequest = forgegithub.PullRequestObservation{Number: 42}
			f.observation.SealedCompare = forgegithub.CompareObservation{}
			f.observation.MergeCompare = forgegithub.CompareObservation{}
		}, want: "repository_mismatch"},
		{name: "non canonical observed url", mutate: func(f *externalForge) { f.observation.PullRequest.URL = "https://github.com/acme/demo/pull/99" }, want: "pr_url_invalid"},
		{name: "invalid default branch", runner: func(r *externalRunner) { r.failBranch = true }, want: "default_branch_mismatch"},
		{name: "invalid sealed commit", want: "sealed_commit_mismatch", pr: "https://github.com/acme/demo/pull/42"},
		{name: "observation not configured", want: "forge_observation_not_configured"},
		{name: "remote pr url invalid", mutate: func(f *externalForge) {
			f.err = &extensionhost.RemoteError{Failure: extensionhost.Failure{Class: "malformed", Code: "pr_url_invalid", Message: "GitHub observation artifact must be a canonical HTTPS pull request URL", Effect: "none"}, Operation: "observe"}
		}, want: "pr_url_invalid"},
		{name: "remote repository mismatch", mutate: func(f *externalForge) {
			f.err = &extensionhost.RemoteError{Failure: extensionhost.Failure{Class: "malformed", Code: "repository_mismatch", Message: "GitHub observation artifact does not match the registered repository identity", Effect: "none"}, Operation: "observe"}
		}, want: "repository_mismatch"},
		{name: "provider unavailable", mutate: func(f *externalForge) {
			f.err = &extensionhost.RemoteError{Failure: extensionhost.Failure{Class: "unavailable", Code: "provider_unavailable", Message: "GitHub CLI could not query the pull request: gh: exit status 1", Effect: "none"}, Operation: "observe"}
		}, want: "github_unavailable"},
		{name: "observation deadline", mutate: func(f *externalForge) {
			f.err = &extensionhost.HostError{Kind: extensionhost.ErrorDeadlineExceeded, Operation: "observe"}
		}, want: "github_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &externalRunner{origin: "https://github.com/acme/demo.git"}
			if test.runner != nil {
				test.runner(runner)
			}
			var forge ForgeObserver
			if test.want != "forge_observation_not_configured" {
				forge = &externalForge{observation: validExternalObservation()}
				if test.mutate != nil {
					test.mutate(forge.(*externalForge))
				}
			}
			pr := test.pr
			if pr == "" {
				pr = "https://github.com/acme/demo/pull/42"
			}
			sealed := externalSealed
			if test.want == "sealed_commit_mismatch" {
				sealed = "not-a-commit"
			}
			_, err := (Verifier{Runner: runner, Forge: forge}).ValidateExternalDelivery(model.Repo{Path: "/repo", DefaultBranch: "main"}, pr, sealed)
			if err == nil || errKind(err) != test.want {
				t.Fatalf("error = %v kind=%q, want %q", err, errKind(err), test.want)
			}
			if test.want == "default_branch_mismatch" && strings.Contains(err.Error(), "digest") {
				t.Fatalf("invalid default branch must preserve the hard failure: %v", err)
			}
			if (test.want == "sealed_commit_mismatch" || test.runner != nil) && forge != nil && forge.(*externalForge).calls != 0 {
				t.Fatalf("observation launched before pre-observation validation: calls=%d", forge.(*externalForge).calls)
			}
		})
	}
}

func TestExternalDeliveryCanonicalInputs(t *testing.T) {
	for _, remote := range []string{"file:///tmp/repo", "git://github.com/acme/demo.git", "https://github.com/acme/demo/extra", "git@github.com:demo"} {
		if _, err := parseRemoteRepository(remote); err == nil {
			t.Fatalf("remote %q accepted", remote)
		}
	}
}

func validExternalObservation() forgegithub.Observation {
	return forgegithub.Observation{
		SchemaVersion:      forgegithub.ObservationSchemaVersion,
		Repository:         forgegithub.RepositoryObservation{ID: "repo-node", NameWithOwner: "acme/demo", URL: "https://github.com/acme/demo", DefaultBranchName: "main", DefaultBranchOID: externalDefault},
		PullRequest:        forgegithub.PullRequestObservation{ID: "pr-node", URL: "https://github.com/acme/demo/pull/42", Number: 42, State: "MERGED", MergedAt: "2026-08-13T07:25:38Z", BaseRepository: "acme/demo", BaseRefName: "main", HeadRefName: "review", HeadRefOID: externalHead, MergeCommitOID: externalMerge},
		Commits:            []string{externalSealed, externalFollow1, externalHead},
		PaginationComplete: true,
		SealedCompare:      forgegithub.CompareObservation{Status: "ahead", MergeBaseSHA: externalSealed},
		MergeCompare:       forgegithub.CompareObservation{Status: "ahead", MergeBaseSHA: externalMerge},
	}
}

func errKind(err error) string {
	type classified interface {
		ErrorKind() string
	}
	if value, ok := err.(classified); ok {
		return value.ErrorKind()
	}
	return ""
}
