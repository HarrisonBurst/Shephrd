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
	Branch      string `json:"branch"`
	Target      string `json:"target"`
	PullRequest string `json:"pull_request"`
	Title       string `json:"title"`
	Body        string `json:"body"`
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
	out, err := gh("pr", "view", number, "--repo", repo, "--json", "state,mergeable,url,headRefOid,baseRefName,mergeCommit")
	if err != nil {
		return v, err
	}
	return v, json.Unmarshal(out, &v)
}

func handle(req forgeRequest, method string) (response, error) {
	repo := repoSlug(req.Repo.Remote)
	switch req.Operation {
	case "publish":
		if req.PullRequest != "" {
			if v, err := viewPR(repo, req.PullRequest); err == nil && v.State == "OPEN" {
				if v.BaseRefName != req.Target {
					if _, err := gh("pr", "edit", req.PullRequest, "--repo", repo, "--base", req.Target); err != nil {
						return response{}, err
					}
				}
				return response{Outcome: "ok", PullRequest: req.PullRequest, URL: v.URL}, nil
			}
		}
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
		if len(open) > 0 {
			return response{Outcome: "ok", PullRequest: fmt.Sprint(open[0].Number), URL: open[0].URL}, nil
		}
		out, err = gh("pr", "create", "--repo", repo, "--head", req.Branch, "--base", req.Target, "--title", req.Title, "--body", req.Body)
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
