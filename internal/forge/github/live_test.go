package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestRealGitHubObserverReadProbeIsOptIn(t *testing.T) {
	if os.Getenv("SHEPHRD_REAL_GITHUB_OBSERVER_E2E") != "1" {
		t.Skip("set SHEPHRD_REAL_GITHUB_OBSERVER_E2E=1 to run the live read-only GitHub probe")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh is not installed")
	}
	if output, err := exec.Command("gh", "auth", "status").CombinedOutput(); err != nil {
		t.Skipf("gh is not authenticated: %s", output)
	}
	repo := os.Getenv("SHEPHRD_E2E_GITHUB_REPO")
	if repo == "" {
		repo = "octocat/Hello-World"
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		t.Skipf("SHEPHRD_E2E_GITHUB_REPO %q is not owner/repo", repo)
	}
	buildObserver(t)
	query := fmt.Sprintf(`query{repository(owner:%q,name:%q){pullRequests(states:MERGED,first:1){nodes{number url headRefOid}}}}`, owner, name)
	stdout, err := exec.Command("gh", "api", "graphql", "-f", "query="+query).Output()
	if err != nil {
		t.Skipf("live pull request lookup failed: %v", err)
	}
	var lookup struct {
		Data struct {
			Repository struct {
				PullRequests struct {
					Nodes []struct {
						Number     int    `json:"number"`
						URL        string `json:"url"`
						HeadRefOid string `json:"headRefOid"`
					} `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout, &lookup); err != nil {
		t.Fatalf("decode live pull request lookup: %v", err)
	}
	node := lookup.Data.Repository.PullRequests.Nodes
	if len(node) == 0 || node[0].HeadRefOid == "" {
		t.Skipf("no merged pull request in %s", repo)
	}
	host := "github.com"
	if after, found := strings.CutPrefix(node[0].URL, "https://"); found {
		if parsedHost, _, found := strings.Cut(after, "/"); found {
			host = parsedHost
		}
	}
	client := NewClient([]string{observerBinary}, observerSHA, os.Environ())
	observation, err := client.Observe(context.Background(), Request{Host: host, Owner: owner, Repository: name, Artifact: node[0].URL, SealedCommit: node[0].HeadRefOid})
	if err != nil {
		t.Fatalf("live observation: %v", err)
	}
	if observation.PullRequest.Number != node[0].Number || observation.PullRequest.URL != node[0].URL || observation.Repository.NameWithOwner != repo {
		t.Fatalf("live observation = %+v", observation)
	}
	if err := ValidateObservation(observation); err != nil {
		t.Fatalf("live observation invalid: %v", err)
	}
	if _, err := ObservationDigest(observation); err != nil {
		t.Fatalf("live observation digest: %v", err)
	}
}
