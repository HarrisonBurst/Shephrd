package github

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	extensionhost "shephrd/internal/extension"
)

var requestIDPattern = regexp.MustCompile(`^extension_request_[0-9a-f]{24}$`)

func Run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("expected exactly one command")
	}
	switch args[0] {
	case "describe":
		return writeJSON(stdout, Manifest())
	case "invoke":
		return invoke(stdin, stdout)
	default:
		return fmt.Errorf("unknown command")
	}
}

type ghInvoker func(ctx context.Context, args ...string) ([]byte, []byte, error)

func execGH(ctx context.Context, args ...string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func invoke(stdin io.Reader, stdout io.Writer) error {
	return handleInvoke(stdin, stdout, execGH)
}

func handleInvoke(stdin io.Reader, stdout io.Writer, invoke ghInvoker) error {
	frame, err := readFrame(bufio.NewReaderSize(stdin, 64*1024))
	if err != nil {
		return err
	}
	var request extensionhost.Request
	if err := extensionhost.StrictDecode(frame, &request); err != nil {
		return err
	}
	if err := validateEnvelope(request); err != nil {
		return err
	}
	var payload Request
	if err := extensionhost.StrictDecode(request.Payload, &payload); err != nil {
		return writeError(stdout, request, "malformed", "invalid_request", "GitHub observation request payload is invalid")
	}
	if err := ValidateRequest(payload); err != nil {
		return writeError(stdout, request, "malformed", "invalid_request", err.Error())
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.UnixMilli(request.DeadlineUnixMS))
	defer cancel()
	observation, class, code, message := observe(ctx, payload, invoke)
	if class != "" {
		return writeError(stdout, request, class, code, message)
	}
	return writeResponse(stdout, request, Result{Observation: observation})
}

func validateEnvelope(request extensionhost.Request) error {
	now := time.Now()
	if request.Wire != (extensionhost.WireVersion{Major: extensionhost.WireMajor, Minor: extensionhost.WireMinor}) ||
		!requestIDPattern.MatchString(request.RequestID) || request.Capability != CapabilityName ||
		request.CapabilityVersion != CapabilityVersion || request.Operation != ObserveOperation ||
		request.DeadlineUnixMS <= now.UnixMilli() || request.DeadlineUnixMS > now.Add(31*time.Second).UnixMilli() {
		return fmt.Errorf("request envelope is invalid or expired")
	}
	return nil
}

func observe(ctx context.Context, request Request, invoke ghInvoker) (Observation, string, string, string) {
	reference, err := parsePullRequestArtifact(request.Artifact)
	if err != nil {
		return Observation{}, "malformed", "pr_url_invalid", "GitHub observation artifact must be a canonical HTTPS pull request URL"
	}
	if !strings.EqualFold(reference.Host, request.Host) || !strings.EqualFold(reference.Owner, request.Owner) || !strings.EqualFold(reference.Repository, request.Repository) {
		return Observation{}, "malformed", "repository_mismatch", "GitHub observation artifact does not match the registered repository identity"
	}
	pages, class, code, message := queryPullRequest(ctx, request, reference, invoke)
	if class != "" {
		return Observation{}, class, code, message
	}
	observation, class, code, message := buildObservation(reference, pages)
	if class != "" {
		return Observation{}, class, code, message
	}
	if complete := observation.PullRequest.State == "MERGED" && observation.PullRequest.MergedAt != "" &&
		gitOIDPattern.MatchString(observation.PullRequest.HeadRefOID) && gitOIDPattern.MatchString(observation.PullRequest.MergeCommitOID) &&
		gitOIDPattern.MatchString(observation.Repository.DefaultBranchOID); complete {
		observation.SealedCompare, class, code, message = compareRange(ctx, request, request.SealedCommit, observation.PullRequest.HeadRefOID, invoke)
		if class != "" {
			return Observation{}, class, code, message
		}
		observation.MergeCompare, class, code, message = compareRange(ctx, request, observation.PullRequest.MergeCommitOID, observation.Repository.DefaultBranchOID, invoke)
		if class != "" {
			return Observation{}, class, code, message
		}
	}
	if err := ValidateObservation(observation); err != nil {
		return Observation{}, "malformed", "provider_response_invalid", err.Error()
	}
	return observation, "", "", ""
}

const pullRequestQuery = `query($owner:String!,$name:String!,$number:Int!,$endCursor:String){repository(owner:$owner,name:$name){id nameWithOwner url defaultBranchRef{name target{oid}} pullRequest(number:$number){id url state mergedAt baseRefName headRefName headRefOid baseRepository{nameWithOwner} mergeCommit{oid} commits(first:100,after:$endCursor){nodes{commit{oid}} pageInfo{hasNextPage endCursor}}}}}`

func queryPullRequest(ctx context.Context, request Request, reference pullRequestReference, invoke ghInvoker) ([]graphPage, string, string, string) {
	stdout, stderr, err := invoke(ctx, "api", "graphql", "--hostname", request.Host, "--paginate", "--slurp",
		"-f", "query="+pullRequestQuery, "-f", "owner="+request.Owner, "-f", "name="+request.Repository, "-F", "number="+strconv.Itoa(reference.Number))
	if err != nil {
		class, code, message := ghInvocationError(ctx, stderr, err, "GitHub CLI could not query the pull request")
		return nil, class, code, message
	}
	pages, err := decodePages(stdout)
	if err != nil {
		return nil, "malformed", "provider_response_invalid", "GitHub returned invalid pull request evidence"
	}
	for _, page := range pages {
		if len(page.Errors) != 0 || page.Data.Repository.ID == "" {
			return nil, "unavailable", "provider_unavailable", "GitHub could not resolve the registered repository and pull request"
		}
	}
	return pages, "", "", ""
}

type pullRequestReference struct {
	Host       string
	Owner      string
	Repository string
	Number     int
	URL        string
}

func parsePullRequestArtifact(raw string) (pullRequestReference, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.Port() != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return pullRequestReference{}, fmt.Errorf("pull request artifact must be a canonical HTTPS pull request URL")
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] != "pull" {
		return pullRequestReference{}, fmt.Errorf("pull request artifact must have shape https://<host>/<owner>/<repository>/pull/<number>")
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number < 1 || strconv.Itoa(number) != parts[3] {
		return pullRequestReference{}, fmt.Errorf("pull request artifact must contain a canonical positive pull request number")
	}
	canonical := "https://" + strings.ToLower(parsed.Hostname()) + "/" + parts[0] + "/" + parts[1] + "/pull/" + parts[3]
	if raw != canonical {
		return pullRequestReference{}, fmt.Errorf("pull request artifact must be canonical %s", canonical)
	}
	return pullRequestReference{Host: strings.ToLower(parsed.Hostname()), Owner: parts[0], Repository: parts[1], Number: number, URL: canonical}, nil
}

type graphPage struct {
	Data struct {
		Repository struct {
			ID               string `json:"id"`
			NameWithOwner    string `json:"nameWithOwner"`
			URL              string `json:"url"`
			DefaultBranchRef struct {
				Name   string `json:"name"`
				Target struct {
					OID string `json:"oid"`
				} `json:"target"`
			} `json:"defaultBranchRef"`
			PullRequest struct {
				ID             string `json:"id"`
				URL            string `json:"url"`
				State          string `json:"state"`
				MergedAt       string `json:"mergedAt"`
				BaseRefName    string `json:"baseRefName"`
				HeadRefName    string `json:"headRefName"`
				HeadRefOID     string `json:"headRefOid"`
				BaseRepository struct {
					NameWithOwner string `json:"nameWithOwner"`
				} `json:"baseRepository"`
				MergeCommit struct {
					OID string `json:"oid"`
				} `json:"mergeCommit"`
				Commits struct {
					Nodes []struct {
						Commit struct {
							OID string `json:"oid"`
						} `json:"commit"`
					} `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"commits"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []json.RawMessage `json:"errors"`
}

func decodePages(stdout []byte) ([]graphPage, error) {
	var pages []graphPage
	if err := json.Unmarshal(stdout, &pages); err != nil {
		var page graphPage
		if singleErr := json.Unmarshal(stdout, &page); singleErr != nil {
			return nil, singleErr
		}
		pages = []graphPage{page}
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("GitHub returned no pull request evidence")
	}
	return pages, nil
}

func buildObservation(reference pullRequestReference, pages []graphPage) (Observation, string, string, string) {
	first := pages[0].Data.Repository
	observation := Observation{
		SchemaVersion: ObservationSchemaVersion,
		Repository: RepositoryObservation{
			ID:                first.ID,
			NameWithOwner:     first.NameWithOwner,
			URL:               first.URL,
			DefaultBranchName: first.DefaultBranchRef.Name,
			DefaultBranchOID:  first.DefaultBranchRef.Target.OID,
		},
		PullRequest:        PullRequestObservation{Number: reference.Number},
		PaginationComplete: !pages[len(pages)-1].Data.Repository.PullRequest.Commits.PageInfo.HasNextPage,
	}
	firstPull := first.PullRequest
	for _, page := range pages {
		repo := page.Data.Repository
		pull := repo.PullRequest
		if repo.ID != first.ID || repo.NameWithOwner != first.NameWithOwner ||
			repo.DefaultBranchRef.Name != first.DefaultBranchRef.Name || repo.DefaultBranchRef.Target.OID != first.DefaultBranchRef.Target.OID ||
			pull.ID != firstPull.ID || pull.URL != firstPull.URL || pull.State != firstPull.State || pull.MergedAt != firstPull.MergedAt ||
			pull.BaseRefName != firstPull.BaseRefName || pull.HeadRefName != firstPull.HeadRefName || pull.HeadRefOID != firstPull.HeadRefOID ||
			pull.BaseRepository.NameWithOwner != firstPull.BaseRepository.NameWithOwner || pull.MergeCommit.OID != firstPull.MergeCommit.OID {
			return Observation{}, "malformed", "provider_response_invalid", "GitHub PR or default branch changed during paginated validation"
		}
		if observation.PullRequest.ID == "" && pull.ID != "" {
			observation.PullRequest = PullRequestObservation{
				ID:             pull.ID,
				URL:            pull.URL,
				Number:         reference.Number,
				State:          pull.State,
				MergedAt:       pull.MergedAt,
				BaseRepository: pull.BaseRepository.NameWithOwner,
				BaseRefName:    pull.BaseRefName,
				HeadRefName:    pull.HeadRefName,
				HeadRefOID:     pull.HeadRefOID,
				MergeCommitOID: pull.MergeCommit.OID,
			}
		}
		for _, node := range pull.Commits.Nodes {
			observation.Commits = append(observation.Commits, node.Commit.OID)
			if len(observation.Commits) > MaxCommitCount {
				return Observation{}, "malformed", "provider_response_invalid", "GitHub commit pagination exceeds the observation limit"
			}
		}
	}
	return observation, "", "", ""
}

func compareRange(ctx context.Context, request Request, base, head string, invoke ghInvoker) (CompareObservation, string, string, string) {
	stdout, stderr, err := invoke(ctx, "api", "--hostname", request.Host, "repos/"+request.Owner+"/"+request.Repository+"/compare/"+base+"..."+head)
	if err != nil {
		class, code, message := ghInvocationError(ctx, stderr, err, "GitHub CLI could not compare commits")
		return CompareObservation{}, class, code, message
	}
	var comparison struct {
		Status          string `json:"status"`
		MergeBaseCommit struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err := json.Unmarshal(stdout, &comparison); err != nil {
		return CompareObservation{}, "malformed", "provider_response_invalid", "GitHub returned invalid commit graph evidence"
	}
	return CompareObservation{Status: comparison.Status, MergeBaseSHA: comparison.MergeBaseCommit.SHA}, "", "", ""
}

func ghInvocationError(ctx context.Context, stderr []byte, err error, prefix string) (string, string, string) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "unavailable", "provider_timeout", prefix + " before the observation deadline"
	}
	detail := strings.TrimSpace(strings.ToValidUTF8(string(stderr), ""))
	if detail == "" {
		detail = strings.TrimSpace(err.Error())
	}
	message := prefix + ": " + detail
	if len(message) > 2048 {
		message = message[:2048]
	}
	return "unavailable", "provider_unavailable", message
}

func writeResponse(writer io.Writer, request extensionhost.Request, result Result) error {
	payload, err := json.Marshal(result)
	if err != nil || len(payload) > extensionhost.MaxFrameBytes {
		return fmt.Errorf("encode extension result")
	}
	return writeJSON(writer, extensionhost.Response{
		Wire: request.Wire, RequestID: request.RequestID, Capability: request.Capability,
		CapabilityVersion: request.CapabilityVersion, Operation: request.Operation, Status: "ok",
		Result: payload, Extension: Manifest().Extension,
	})
}

func writeError(writer io.Writer, request extensionhost.Request, class, code, message string) error {
	if len(message) > 2048 {
		message = message[:2048]
	}
	return writeJSON(writer, extensionhost.Response{
		Wire: request.Wire, RequestID: request.RequestID, Capability: request.Capability,
		CapabilityVersion: request.CapabilityVersion, Operation: request.Operation, Status: "error",
		Error:     &extensionhost.Failure{Class: class, Code: code, Message: message, Effect: "none"},
		Extension: Manifest().Extension,
	})
}

func writeJSON(writer io.Writer, value any) error {
	frame, err := json.Marshal(value)
	if err != nil || len(frame) > extensionhost.MaxFrameBytes {
		return fmt.Errorf("encode extension frame")
	}
	_, err = writer.Write(append(frame, '\n'))
	return err
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	var frame bytes.Buffer
	for {
		fragment, err := reader.ReadSlice('\n')
		if frame.Len()+len(fragment) > extensionhost.MaxFrameBytes {
			return nil, fmt.Errorf("extension request frame exceeds the size limit")
		}
		frame.Write(fragment)
		if err == nil {
			return bytes.TrimSuffix(frame.Bytes(), []byte{'\n'}), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && frame.Len() > 0 {
			return frame.Bytes(), nil
		}
		return nil, err
	}
}
