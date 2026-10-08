package github

import (
	"testing"

	extensionhost "shephrd/internal/extension"
)

func TestManifestMatchesHostExpectation(t *testing.T) {
	if err := extensionhost.ValidateManifest(Manifest(), ExtensionID, []extensionhost.Capability{Capability()}); err != nil {
		t.Fatalf("manifest = %v", err)
	}
}

func TestValidateRequestBounds(t *testing.T) {
	valid := Request{Host: "github.com", Owner: "acme", Repository: "demo", Artifact: "https://github.com/acme/demo/pull/42", SealedCommit: "1111111111111111111111111111111111111111"}
	if err := ValidateRequest(valid); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	mixedCase := valid
	mixedCase.Owner = "Acme"
	mixedCase.Repository = "Demo"
	mixedCase.Artifact = "https://github.com/Acme/Demo/pull/42"
	if err := ValidateRequest(mixedCase); err != nil {
		t.Fatalf("mixed case repository identity rejected: %v", err)
	}
	for _, mutate := range []func(*Request){
		func(r *Request) { r.Host = "GitHub.com" },
		func(r *Request) { r.Host = "-bad" },
		func(r *Request) { r.Owner = "ac me" },
		func(r *Request) { r.Repository = "demo/" },
		func(r *Request) { r.Artifact = "" },
		func(r *Request) { r.SealedCommit = "111111111111111111111111111111111111111" },
		func(r *Request) { r.SealedCommit = "11111111111111111111111111111111111111111" },
		func(r *Request) { r.SealedCommit = "111111111111111111111111111111111111111A" },
	} {
		request := valid
		mutate(&request)
		if err := ValidateRequest(request); err == nil {
			t.Fatalf("request %+v accepted", request)
		}
	}
}

func TestParsePullRequestArtifact(t *testing.T) {
	reference, err := parsePullRequestArtifact("https://github.com/acme/demo/pull/42")
	if err != nil {
		t.Fatal(err)
	}
	if reference.Host != "github.com" || reference.Owner != "acme" || reference.Repository != "demo" || reference.Number != 42 || reference.URL != "https://github.com/acme/demo/pull/42" {
		t.Fatalf("reference = %+v", reference)
	}
	for _, invalid := range []string{
		"https://github.com/acme/demo/pull/42/",
		"http://github.com/acme/demo/pull/42",
		"https://github.com:8443/acme/demo/pull/42",
		"https://user:pass@github.com/acme/demo/pull/42",
		"https://github.com/acme/demo/pull/42?x=1",
		"https://github.com/acme/demo/pull/42#y",
		"https://github.com/acme/demo/pull/042",
		"https://github.com/acme/demo/pull/0",
		"https://github.com/acme/demo/pull",
		"https://github.com/acme/demo/issues/42",
		"https://github.com/acme/demo/pull/42/extra",
		"https://GITHUB.com/acme/demo/pull/42",
	} {
		if _, err := parsePullRequestArtifact(invalid); err == nil {
			t.Fatalf("artifact %q accepted", invalid)
		}
	}
}

func TestValidateObservationBounds(t *testing.T) {
	valid := Observation{
		SchemaVersion:      ObservationSchemaVersion,
		Repository:         RepositoryObservation{ID: "node", NameWithOwner: "acme/demo", URL: "https://github.com/acme/demo", DefaultBranchName: "main", DefaultBranchOID: "5555555555555555555555555555555555555555"},
		PullRequest:        PullRequestObservation{ID: "pr", URL: "https://github.com/acme/demo/pull/42", Number: 42, State: "MERGED", MergedAt: "2026-08-13T07:25:38Z", BaseRepository: "acme/demo", BaseRefName: "main", HeadRefName: "review", HeadRefOID: "3333333333333333333333333333333333333333", MergeCommitOID: "4444444444444444444444444444444444444444"},
		Commits:            []string{"1111111111111111111111111111111111111111"},
		PaginationComplete: true,
		SealedCompare:      CompareObservation{Status: "ahead", MergeBaseSHA: "1111111111111111111111111111111111111111"},
		MergeCompare:       CompareObservation{},
	}
	if err := ValidateObservation(valid); err != nil {
		t.Fatalf("valid observation rejected: %v", err)
	}
	mutations := []func(*Observation){
		func(o *Observation) { o.SchemaVersion = 2 },
		func(o *Observation) { o.Repository.ID = "" },
		func(o *Observation) { o.Repository.NameWithOwner = "acme" },
		func(o *Observation) { o.Repository.DefaultBranchName = "main\n" },
		func(o *Observation) { o.Repository.DefaultBranchOID = "xyz" },
		func(o *Observation) { o.PullRequest = PullRequestObservation{ID: "pr", Number: 42} },
		func(o *Observation) { o.PullRequest = PullRequestObservation{Number: 42, State: "MERGED"} },
		func(o *Observation) { o.PullRequest.Number = 0; o.PullRequest.ID = "pr" },
		func(o *Observation) { o.PullRequest.HeadRefOID = "xyz" },
		func(o *Observation) { o.PullRequest.MergeCommitOID = "xyz" },
		func(o *Observation) { o.Commits = []string{"not-an-oid"} },
		func(o *Observation) { o.SealedCompare = CompareObservation{Status: "ahead", MergeBaseSHA: "xyz"} },
		func(o *Observation) {
			o.SealedCompare = CompareObservation{Status: "", MergeBaseSHA: "1111111111111111111111111111111111111111"}
		},
	}
	for index, mutate := range mutations {
		observation := valid
		mutate(&observation)
		if err := ValidateObservation(observation); err == nil {
			t.Fatalf("mutation %d accepted", index)
		}
	}
	nullPR := valid
	nullPR.PullRequest = PullRequestObservation{Number: 42}
	if err := ValidateObservation(nullPR); err != nil {
		t.Fatalf("null pull request observation rejected: %v", err)
	}
}

func TestObservationDigestIsStableAndSensitive(t *testing.T) {
	observation := Observation{
		SchemaVersion:      ObservationSchemaVersion,
		Repository:         RepositoryObservation{ID: "node", NameWithOwner: "acme/demo", URL: "https://github.com/acme/demo", DefaultBranchName: "main", DefaultBranchOID: "5555555555555555555555555555555555555555"},
		PullRequest:        PullRequestObservation{ID: "pr", URL: "https://github.com/acme/demo/pull/42", Number: 42, State: "MERGED", MergedAt: "2026-08-13T07:25:38Z", BaseRepository: "acme/demo", BaseRefName: "main", HeadRefName: "review", HeadRefOID: "3333333333333333333333333333333333333333", MergeCommitOID: "4444444444444444444444444444444444444444"},
		Commits:            []string{"1111111111111111111111111111111111111111", "3333333333333333333333333333333333333333"},
		PaginationComplete: true,
		SealedCompare:      CompareObservation{Status: "ahead", MergeBaseSHA: "1111111111111111111111111111111111111111"},
		MergeCompare:       CompareObservation{Status: "ahead", MergeBaseSHA: "4444444444444444444444444444444444444444"},
	}
	first, err := ObservationDigest(observation)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ObservationDigest(observation)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 64 {
		t.Fatalf("digests = %s %s", first, second)
	}
	changed := observation
	changed.Commits = append(changed.Commits, "6666666666666666666666666666666666666666")
	if third, err := ObservationDigest(changed); err != nil || third == first {
		t.Fatalf("commit list change did not move the digest: %s", third)
	}
}
