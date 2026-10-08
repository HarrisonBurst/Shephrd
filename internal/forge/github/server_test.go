package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	testSealed  = "1111111111111111111111111111111111111111"
	testFollow  = "2222222222222222222222222222222222222222"
	testHead    = "3333333333333333333333333333333333333333"
	testMerge   = "4444444444444444444444444444444444444444"
	testDefault = "5555555555555555555555555555555555555555"
)

type fakeGH struct {
	pages       []graphPage
	compare     func(base, head string) (string, []byte)
	failGraph   error
	failCompare error
	calls       []string
	block       bool
}

func (f *fakeGH) invoke(ctx context.Context, args ...string) ([]byte, []byte, error) {
	call := strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if f.block {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	if strings.Contains(call, "graphql") {
		if f.failGraph != nil {
			return nil, []byte("gh: provider failure"), f.failGraph
		}
		body, err := json.Marshal(f.pages)
		return body, nil, err
	}
	if strings.Contains(call, "/compare/") {
		if f.failCompare != nil {
			return nil, []byte("gh: compare failure"), f.failCompare
		}
		parts := strings.SplitN(strings.Split(call, "/compare/")[1], "...", 2)
		status, body := "ahead", []byte(fmt.Sprintf(`{"status":"ahead","merge_base_commit":{"sha":"%s"}}`, parts[0]))
		if f.compare != nil {
			status, body = f.compare(parts[0], parts[1])
		}
		if status == "" {
			return nil, nil, errors.New("compare refused")
		}
		return body, nil, nil
	}
	return nil, nil, errors.New("unexpected gh call: " + call)
}

func testRequest() Request {
	return Request{Host: "github.com", Owner: "acme", Repository: "demo", Artifact: "https://github.com/acme/demo/pull/42", SealedCommit: testSealed}
}

func graphPageFixture(commits []string, hasNext bool, mutate func(*graphPage)) graphPage {
	var page graphPage
	repository := &page.Data.Repository
	repository.ID = "repo-node"
	repository.NameWithOwner = "acme/demo"
	repository.URL = "https://github.com/acme/demo"
	repository.DefaultBranchRef.Name = "main"
	repository.DefaultBranchRef.Target.OID = testDefault
	pull := &repository.PullRequest
	pull.ID = "pr-node"
	pull.URL = "https://github.com/acme/demo/pull/42"
	pull.State = "MERGED"
	pull.MergedAt = "2026-08-13T07:25:38Z"
	pull.BaseRefName = "main"
	pull.HeadRefName = "review"
	pull.HeadRefOID = testHead
	pull.BaseRepository.NameWithOwner = "acme/demo"
	pull.MergeCommit.OID = testMerge
	pull.Commits.PageInfo.HasNextPage = hasNext
	for _, oid := range commits {
		var node struct {
			Commit struct {
				OID string `json:"oid"`
			} `json:"commit"`
		}
		node.Commit.OID = oid
		pull.Commits.Nodes = append(pull.Commits.Nodes, node)
	}
	if mutate != nil {
		mutate(&page)
	}
	return page
}

func TestObserveParity(t *testing.T) {
	fake := &fakeGH{pages: []graphPage{
		graphPageFixture([]string{testSealed, testFollow}, true, nil),
		graphPageFixture([]string{testHead}, false, nil),
	}}
	observation, class, code, message := observe(context.Background(), testRequest(), fake.invoke)
	if class != "" {
		t.Fatalf("observe = %s %s: %s", class, code, message)
	}
	if observation.SchemaVersion != ObservationSchemaVersion {
		t.Fatalf("schema = %+v", observation)
	}
	if observation.Repository.ID != "repo-node" || observation.Repository.NameWithOwner != "acme/demo" || observation.Repository.DefaultBranchName != "main" || observation.Repository.DefaultBranchOID != testDefault {
		t.Fatalf("repository = %+v", observation.Repository)
	}
	pull := observation.PullRequest
	if pull.ID != "pr-node" || pull.Number != 42 || pull.State != "MERGED" || pull.URL != "https://github.com/acme/demo/pull/42" || pull.BaseRepository != "acme/demo" || pull.BaseRefName != "main" || pull.HeadRefName != "review" || pull.HeadRefOID != testHead || pull.MergeCommitOID != testMerge {
		t.Fatalf("pull = %+v", pull)
	}
	if strings.Join(observation.Commits, ",") != testSealed+","+testFollow+","+testHead || !observation.PaginationComplete {
		t.Fatalf("commits = %v complete=%v", observation.Commits, observation.PaginationComplete)
	}
	if observation.SealedCompare != (CompareObservation{Status: "ahead", MergeBaseSHA: testSealed}) || observation.MergeCompare != (CompareObservation{Status: "ahead", MergeBaseSHA: testMerge}) {
		t.Fatalf("compares = %+v %+v", observation.SealedCompare, observation.MergeCompare)
	}
	if len(fake.calls) != 3 || !strings.Contains(fake.calls[0], "--hostname github.com") || !strings.Contains(fake.calls[0], "graphql") || !strings.Contains(fake.calls[1], "repos/acme/demo/compare/"+testSealed+"..."+testHead) || !strings.Contains(fake.calls[2], "repos/acme/demo/compare/"+testMerge+"..."+testDefault) {
		t.Fatalf("calls = %v", fake.calls)
	}
	if err := ValidateObservation(observation); err != nil {
		t.Fatalf("observation invalid: %v", err)
	}
}

func TestObservePaginationCollectsEveryPage(t *testing.T) {
	third := "6666666666666666666666666666666666666666"
	fake := &fakeGH{pages: []graphPage{
		graphPageFixture([]string{testSealed}, true, nil),
		graphPageFixture([]string{testFollow}, true, nil),
		graphPageFixture([]string{third, testHead}, false, nil),
	}}
	observation, class, code, message := observe(context.Background(), testRequest(), fake.invoke)
	if class != "" {
		t.Fatalf("observe = %s %s: %s", class, code, message)
	}
	if strings.Join(observation.Commits, ",") != strings.Join([]string{testSealed, testFollow, third, testHead}, ",") {
		t.Fatalf("commits = %v", observation.Commits)
	}
}

func TestObserveTruncatedPaginationIsAnExplicitFact(t *testing.T) {
	fake := &fakeGH{pages: []graphPage{
		graphPageFixture([]string{testSealed}, true, nil),
		graphPageFixture([]string{testHead}, true, nil),
	}}
	observation, class, code, message := observe(context.Background(), testRequest(), fake.invoke)
	if class != "" {
		t.Fatalf("class=%s code=%s message=%s", class, code, message)
	}
	if observation.PaginationComplete {
		t.Fatalf("truncated pagination reported complete: %+v", observation)
	}
}

func TestObserveCancellationIsFailClosed(t *testing.T) {
	fake := &fakeGH{block: true}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, class, code, message := observe(ctx, testRequest(), fake.invoke)
	if class != "unavailable" || code != "provider_timeout" || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("class=%s code=%s message=%s", class, code, message)
	}
}

func TestObserveDishonestResponsesFailClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*fakeGH)
	}{
		{name: "repository identity changed mid-pagination", mutate: func(f *fakeGH) {
			f.pages = []graphPage{
				graphPageFixture([]string{testSealed}, true, nil),
				graphPageFixture([]string{testHead}, false, func(p *graphPage) { p.Data.Repository.ID = "other-node" }),
			}
		}},
		{name: "pull request identity changed mid-pagination", mutate: func(f *fakeGH) {
			f.pages = []graphPage{
				graphPageFixture([]string{testSealed}, true, nil),
				graphPageFixture([]string{testHead}, false, func(p *graphPage) { p.Data.Repository.PullRequest.State = "OPEN" }),
			}
		}},
		{name: "default branch moved mid-pagination", mutate: func(f *fakeGH) {
			f.pages = []graphPage{
				graphPageFixture([]string{testSealed}, true, nil),
				graphPageFixture([]string{testHead}, false, func(p *graphPage) { p.Data.Repository.DefaultBranchRef.Target.OID = testSealed }),
			}
		}},
		{name: "graphql errors array", mutate: func(f *fakeGH) {
			f.pages = []graphPage{graphPageFixture([]string{testSealed}, false, nil)}
			f.pages[0].Errors = []json.RawMessage{json.RawMessage(`{"message":"boom"}`)}
		}},
		{name: "repository unresolved", mutate: func(f *fakeGH) {
			f.pages = []graphPage{graphPageFixture(nil, false, func(p *graphPage) { p.Data.Repository.ID = "" })}
		}},
		{name: "empty pages", mutate: func(f *fakeGH) { f.pages = nil }},
		{name: "graphql failure", mutate: func(f *fakeGH) { f.failGraph = errors.New("exit status 1") }},
		{name: "compare failure", mutate: func(f *fakeGH) {
			f.pages = []graphPage{graphPageFixture([]string{testSealed, testHead}, false, nil)}
			f.failCompare = errors.New("exit status 1")
		}},
		{name: "compare undecodable", mutate: func(f *fakeGH) {
			f.pages = []graphPage{graphPageFixture([]string{testSealed, testHead}, false, nil)}
			f.compare = func(base, head string) (string, []byte) { return "ahead", []byte("not-json") }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeGH{pages: []graphPage{graphPageFixture([]string{testSealed, testHead}, false, nil)}}
			if test.mutate != nil {
				test.mutate(fake)
			}
			_, class, code, message := observe(context.Background(), testRequest(), fake.invoke)
			if class == "" || code == "" || message == "" {
				t.Fatalf("expected fail-closed error, got ok")
			}
		})
	}
}

func TestObserveNullPullRequestReturnsEmptyIdentity(t *testing.T) {
	fake := &fakeGH{pages: []graphPage{graphPageFixture(nil, false, func(p *graphPage) {
		pull := &p.Data.Repository.PullRequest
		pull.ID = ""
		pull.URL = ""
		pull.State = ""
		pull.MergedAt = ""
		pull.BaseRefName = ""
		pull.HeadRefName = ""
		pull.HeadRefOID = ""
		pull.BaseRepository.NameWithOwner = ""
		pull.MergeCommit.OID = ""
	})}}
	observation, class, code, message := observe(context.Background(), testRequest(), fake.invoke)
	if class != "" {
		t.Fatalf("observe = %s %s: %s", class, code, message)
	}
	if observation.PullRequest.ID != "" || observation.PullRequest.Number != 42 || len(observation.Commits) != 0 {
		t.Fatalf("pull = %+v commits=%v", observation.PullRequest, observation.Commits)
	}
	if observation.SealedCompare.Status != "" || observation.MergeCompare.Status != "" {
		t.Fatalf("null PR must not run compares: %+v %+v", observation.SealedCompare, observation.MergeCompare)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("calls = %v", fake.calls)
	}
}

func TestObserveUnmergedPullRequestSkipsCompares(t *testing.T) {
	fake := &fakeGH{pages: []graphPage{graphPageFixture([]string{testSealed, testHead}, false, func(p *graphPage) {
		pull := &p.Data.Repository.PullRequest
		pull.State = "OPEN"
		pull.MergedAt = ""
	})}}
	observation, class, code, message := observe(context.Background(), testRequest(), fake.invoke)
	if class != "" {
		t.Fatalf("observe = %s %s: %s", class, code, message)
	}
	if observation.SealedCompare.Status != "" || observation.MergeCompare.Status != "" {
		t.Fatalf("unmerged PR must not run compares: %+v %+v", observation.SealedCompare, observation.MergeCompare)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("calls = %v", fake.calls)
	}
}

func TestObserveRequestValidation(t *testing.T) {
	fake := &fakeGH{pages: []graphPage{graphPageFixture([]string{testSealed}, false, nil)}}
	for _, test := range []struct {
		name   string
		mutate func(*Request)
		class  string
		code   string
	}{
		{name: "non canonical artifact", mutate: func(r *Request) { r.Artifact = "https://github.com/acme/demo/pull/42/" }, class: "malformed", code: "pr_url_invalid"},
		{name: "insecure scheme", mutate: func(r *Request) { r.Artifact = "http://github.com/acme/demo/pull/42" }, class: "malformed", code: "pr_url_invalid"},
		{name: "leading zero number", mutate: func(r *Request) { r.Artifact = "https://github.com/acme/demo/pull/042" }, class: "malformed", code: "pr_url_invalid"},
		{name: "artifact identity mismatch", mutate: func(r *Request) { r.Artifact = "https://github.com/acme/other/pull/42" }, class: "malformed", code: "repository_mismatch"},
		{name: "artifact host mismatch", mutate: func(r *Request) { r.Artifact = "https://gitlab.com/acme/demo/pull/42" }, class: "malformed", code: "repository_mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := testRequest()
			test.mutate(&request)
			_, class, code, _ := observe(context.Background(), request, fake.invoke)
			if class != test.class || code != test.code {
				t.Fatalf("class=%s code=%s want %s %s", class, code, test.class, test.code)
			}
		})
	}
}
