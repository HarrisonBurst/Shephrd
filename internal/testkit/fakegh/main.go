// Command fakegh stands in for the gh CLI in tests. Pull requests live in
// $HOME/.fakegh/prs.json; --repo names a bare repository on disk, which
// merges really change. A file $HOME/.fakegh/blocked makes every pull
// request report itself not yet mergeable.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type pr struct {
	Number      int    `json:"number"`
	Repo        string `json:"repo"`
	Head        string `json:"head"`
	Base        string `json:"base"`
	Title       string `json:"title"`
	State       string `json:"state"`
	MergeCommit string `json:"merge_commit"`
}

var dir = filepath.Join(os.Getenv("HOME"), ".fakegh")

func main() {
	args := os.Args[1:]
	if len(args) < 2 || args[0] != "pr" {
		fail("fakegh supports pr commands only")
	}
	flags := map[string]string{}
	var positional []string
	for i := 2; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			name := strings.TrimPrefix(args[i], "--")
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				flags[name] = args[i+1]
				i++
			} else {
				flags[name] = "true"
			}
			continue
		}
		positional = append(positional, args[i])
	}
	prs := load()
	find := func() *pr {
		n, _ := strconv.Atoi(positional[0])
		for i := range prs {
			if prs[i].Number == n && prs[i].Repo == flags["repo"] {
				return &prs[i]
			}
		}
		fail("no pull request " + positional[0])
		return nil
	}
	switch args[1] {
	case "list":
		out := []map[string]any{}
		for _, p := range prs {
			if p.Repo == flags["repo"] && p.Head == flags["head"] && p.State == "OPEN" {
				out = append(out, map[string]any{"number": p.Number, "url": url(p)})
			}
		}
		json.NewEncoder(os.Stdout).Encode(out)
	case "create":
		p := pr{Number: len(prs) + 1, Repo: flags["repo"], Head: flags["head"], Base: flags["base"], Title: flags["title"], State: "OPEN"}
		prs = append(prs, p)
		save(prs)
		fmt.Println(url(p))
	case "view":
		p := find()
		mergeable := "MERGEABLE"
		if _, err := os.Stat(filepath.Join(dir, "blocked")); err == nil {
			mergeable = "BLOCKED"
		}
		out := map[string]any{"state": p.State, "mergeable": mergeable, "url": url(*p), "baseRefName": p.Base,
			"headRefOid": git(p.Repo, "rev-parse", "refs/heads/"+p.Head)}
		if p.MergeCommit != "" {
			out["mergeCommit"] = map[string]string{"oid": p.MergeCommit}
		}
		json.NewEncoder(os.Stdout).Encode(out)
	case "edit":
		p := find()
		p.Base = flags["base"]
		save(prs)
	case "merge":
		p := find()
		p.MergeCommit = merge(*p, flags)
		p.State = "MERGED"
		save(prs)
	default:
		fail("unsupported pr command " + args[1])
	}
}

// merge merges a pull request into its base in the bare repository, the
// way GitHub would for the chosen method.
func merge(p pr, flags map[string]string) string {
	work, err := os.MkdirTemp("", "fakegh-merge-")
	if err != nil {
		fail(err.Error())
	}
	defer os.RemoveAll(work)
	git(work, "clone", "-q", p.Repo, "clone")
	clone := filepath.Join(work, "clone")
	git(clone, "checkout", "-q", p.Base)
	switch {
	case flags["squash"] == "true":
		git(clone, "merge", "-q", "--squash", "origin/"+p.Head)
		git(clone, "commit", "-q", "-m", p.Title+" (#"+strconv.Itoa(p.Number)+")")
	case flags["rebase"] == "true":
		git(clone, "rebase", "-q", "origin/"+p.Head)
	default:
		git(clone, "merge", "-q", "--no-ff", "-m", "Merge pull request #"+strconv.Itoa(p.Number), "origin/"+p.Head)
	}
	git(clone, "push", "-q", "origin", p.Base)
	return git(clone, "rev-parse", "HEAD")
}

func git(dir string, args ...string) string {
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=GitHub", "-c", "user.email=gh@example.test", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fail(fmt.Sprintf("git %v: %v: %s", args, err, out))
	}
	return strings.TrimSpace(string(out))
}

func url(p pr) string {
	return "https://example.test/pull/" + strconv.Itoa(p.Number)
}

func load() []pr {
	var prs []pr
	body, _ := os.ReadFile(filepath.Join(dir, "prs.json"))
	json.Unmarshal(body, &prs)
	return prs
}

func save(prs []pr) {
	os.MkdirAll(dir, 0o700)
	body, _ := json.Marshal(prs)
	os.WriteFile(filepath.Join(dir, "prs.json"), body, 0o600)
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
