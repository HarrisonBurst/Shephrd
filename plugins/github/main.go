// Command shephrd-github is Shephrd's GitHub forge provider. It opens,
// merges, observes and retargets pull requests with the gh CLI; Shephrd
// itself pushes the branches and proves every merge.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
)

type request struct {
	Kind   string `json:"kind"`
	Plugin struct {
		Options map[string]any `json:"options"`
	} `json:"plugin"`
	Body json.RawMessage `json:"body"`
}

type forgeRequest struct {
	Operation string `json:"operation"`
	Repo      struct {
		Remote string `json:"remote"`
	} `json:"repo"`
	Branch        string `json:"branch"`
	Target        string `json:"target"`
	PullRequest   string `json:"pull_request"`
	Title         string `json:"title"`
	Body          string `json:"body"`
	TitleExplicit bool   `json:"title_explicit"`
	BodyExplicit  bool   `json:"body_explicit"`
	Draft         bool   `json:"draft"`
	DraftExplicit bool   `json:"draft_explicit"`
	RequireNew    bool   `json:"require_new"`
}

type response struct {
	Outcome     string `json:"outcome"`
	Reason      string `json:"reason,omitempty"`
	PullRequest string `json:"pull_request,omitempty"`
	URL         string `json:"url,omitempty"`
	State       string `json:"state,omitempty"`
	MergeCommit string `json:"merge_commit,omitempty"`
	Head        string `json:"head,omitempty"`
}

var githubRemote = regexp.MustCompile(`github\.com[:/]([^/]+)/([^/]+?)(\.git)?$`)

func main() {
	var req request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		fmt.Fprintln(os.Stderr, "decode request:", err)
		os.Exit(2)
	}
	if req.Kind != "provide" {
		json.NewEncoder(os.Stdout).Encode(map[string]any{})
		return
	}
	var forge forgeRequest
	if err := json.Unmarshal(req.Body, &forge); err != nil {
		fmt.Fprintln(os.Stderr, "decode forge request:", err)
		os.Exit(2)
	}
	method, _ := req.Plugin.Options["merge_method"].(string)
	if method == "" {
		method = "merge"
	}
	resp, err := handle(forge, method)
	if err != nil {
		resp = response{Outcome: "failed", Reason: err.Error()}
	}
	json.NewEncoder(os.Stdout).Encode(resp)
}

func repoSlug(remote string) string {
	if m := githubRemote.FindStringSubmatch(remote); m != nil {
		return m[1] + "/" + m[2]
	}
	return remote
}

func gh(args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh %s: %v: %s", args[1], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

type view struct {
	State       string `json:"state"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	IsDraft     bool   `json:"isDraft"`
	Mergeable   string `json:"mergeable"`
	URL         string `json:"url"`
	HeadRefOid  string `json:"headRefOid"`
	BaseRefName string `json:"baseRefName"`
	MergeCommit *struct {
		Oid string `json:"oid"`
	} `json:"mergeCommit"`
}

func viewPR(repo, number string) (view, error) {
	var v view
	out, err := gh("pr", "view", number, "--repo", repo, "--json", "state,title,body,isDraft,mergeable,url,headRefOid,baseRefName,mergeCommit")
	if err != nil {
		return v, err
	}
	return v, json.Unmarshal(out, &v)
}

func handle(req forgeRequest, method string) (response, error) {
	repo := repoSlug(req.Repo.Remote)
	switch req.Operation {
	case "publish":
		var number string
		if req.PullRequest != "" {
			number = req.PullRequest
		} else {
			out, err := gh("pr", "list", "--repo", repo, "--head", req.Branch, "--state", "open", "--json", "number,url")
			if err != nil {
				return response{}, err
			}
			var open []struct {
				Number int    `json:"number"`
				URL    string `json:"url"`
			}
			if err := json.Unmarshal(out, &open); err != nil {
				return response{}, err
			}
			if len(open) > 1 {
				return response{}, fmt.Errorf("multiple open pull requests for branch %s", req.Branch)
			}
			if len(open) == 1 {
				if req.RequireNew {
					return response{}, fmt.Errorf("pull request %d has no recorded publication for this task", open[0].Number)
				}
				number = fmt.Sprint(open[0].Number)
			}
		}
		if number != "" {
			v, err := viewPR(repo, number)
			if err != nil {
				return response{}, err
			}
			if v.State != "OPEN" {
				return response{}, fmt.Errorf("pull request %s is %s, not open", number, v.State)
			}
			if (req.TitleExplicit && req.Title != v.Title) || (req.BodyExplicit && req.Body != v.Body) || (req.DraftExplicit && req.Draft != v.IsDraft) {
				return response{}, fmt.Errorf("pull request %s already exists with different publication metadata; edit it on the forge or accept without metadata flags", number)
			}
			if v.BaseRefName != req.Target {
				if _, err := gh("pr", "edit", number, "--repo", repo, "--base", req.Target); err != nil {
					return response{}, err
				}
			}
			return response{Outcome: "ok", PullRequest: number, URL: v.URL}, nil
		}
		args := []string{"pr", "create", "--repo", repo, "--head", req.Branch, "--base", req.Target, "--title", req.Title, "--body", req.Body}
		if req.Draft {
			args = append(args, "--draft")
		}
		out, err := gh(args...)
		if err != nil {
			return response{}, err
		}
		url := strings.TrimSpace(string(out))
		return response{Outcome: "ok", PullRequest: path.Base(url), URL: url}, nil
	case "merge":
		v, err := viewPR(repo, req.PullRequest)
		if err != nil {
			return response{}, err
		}
		if v.State == "MERGED" {
			return response{Outcome: "ok", State: "merged"}, nil
		}
		if v.IsDraft {
			return response{Outcome: "pending", Reason: "the pull request is a draft"}, nil
		}
		if v.Mergeable != "MERGEABLE" {
			return response{Outcome: "pending", Reason: "the pull request is " + strings.ToLower(v.Mergeable)}, nil
		}
		if _, err := gh("pr", "merge", req.PullRequest, "--repo", repo, "--"+method); err != nil {
			return response{}, err
		}
		return response{Outcome: "ok", State: "merged"}, nil
	case "observe":
		v, err := viewPR(repo, req.PullRequest)
		if err != nil {
			return response{}, err
		}
		resp := response{Outcome: "ok", State: strings.ToLower(v.State), Head: v.HeadRefOid, URL: v.URL, PullRequest: req.PullRequest}
		if v.MergeCommit != nil {
			resp.MergeCommit = v.MergeCommit.Oid
		}
		return resp, nil
	case "retarget":
		if _, err := gh("pr", "edit", req.PullRequest, "--repo", repo, "--base", req.Target); err != nil {
			return response{}, err
		}
		return response{Outcome: "ok", PullRequest: req.PullRequest}, nil
	}
	return response{}, fmt.Errorf("unknown forge operation %q", req.Operation)
}
