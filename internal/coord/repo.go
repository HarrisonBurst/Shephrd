package coord

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"

	"shephrd/internal/fault"
	"shephrd/internal/gitcmd"
	"shephrd/internal/store"
)

type Repo struct {
	ID            string `json:"id"`
	Host          string `json:"host"`
	Path          string `json:"path"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	Setup         string `json:"setup,omitempty"`
	Created       string `json:"created"`
}

func InspectRepo(path, defaultBranch string) (string, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", fault.New("not_a_repository", "%s does not exist", path)
	}
	top, err := gitcmd.Run(canonical, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", fault.New("not_a_repository", "%s is not a git work tree", canonical)
	}
	if top, err = filepath.EvalSymlinks(top); err != nil || top != canonical {
		return "", "", fault.New("not_a_repository", "%s is not the root of its git work tree", canonical)
	}
	if defaultBranch == "" {
		if remoteHead, err := gitcmd.Run(canonical, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
			defaultBranch = strings.TrimPrefix(remoteHead, "origin/")
		} else if head, err := gitcmd.Run(canonical, "symbolic-ref", "--short", "HEAD"); err == nil {
			defaultBranch = head
		} else {
			return "", "", fault.New("default_branch_unknown", "cannot determine the default branch of %s", canonical).
				WithNext("repo", "add", canonical, "--default-branch", "<branch>")
		}
	}
	if _, err := gitcmd.Run(canonical, "check-ref-format", "--branch", defaultBranch); err != nil {
		return "", "", fault.New("invalid_branch", "%q is not a valid branch name", defaultBranch)
	}
	_, local := gitcmd.Run(canonical, "rev-parse", "--verify", "--quiet", "refs/heads/"+defaultBranch)
	_, remote := gitcmd.Run(canonical, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+defaultBranch)
	if local != nil && remote != nil {
		return "", "", fault.New("invalid_branch", "branch %q does not exist in %s", defaultBranch, canonical)
	}
	return canonical, defaultBranch, nil
}

func AddRepo(tx *store.Tx, caller string, repo Repo) (Repo, error) {
	var existing int64
	err := tx.QueryRow(`SELECT id FROM repos WHERE (host = ? AND path = ?) OR name = ?`, repo.Host, repo.Path, repo.Name).Scan(&existing)
	if !errors.Is(err, sql.ErrNoRows) {
		if err != nil {
			return Repo{}, err
		}
		return Repo{}, fault.New("repo_exists", "a repository with path %s or name %q is already registered", repo.Path, repo.Name).
			WithNext("repo", "list")
	}
	repo.Created = store.Timestamp(tx.Now)
	result, err := tx.Exec(`INSERT INTO repos (host, path, name, default_branch, setup, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		repo.Host, repo.Path, repo.Name, repo.DefaultBranch, repo.Setup, repo.Created)
	if err != nil {
		return Repo{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Repo{}, err
	}
	repo.ID = "r_" + strconv.FormatInt(id, 10)
	_, err = tx.Emit("repo.added", caller, 0, 0, 0, map[string]string{
		"repo": repo.ID, "host": repo.Host, "path": repo.Path, "name": repo.Name,
	})
	return repo, err
}

func ListRepos(ctx context.Context, s *store.Store) ([]Repo, error) {
	rows, err := s.QueryContext(ctx, `SELECT id, host, path, name, default_branch, setup, created_at FROM repos ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	repos := []Repo{}
	for rows.Next() {
		var id int64
		var repo Repo
		if err := rows.Scan(&id, &repo.Host, &repo.Path, &repo.Name, &repo.DefaultBranch, &repo.Setup, &repo.Created); err != nil {
			return nil, err
		}
		repo.ID = "r_" + strconv.FormatInt(id, 10)
		repos = append(repos, repo)
	}
	return repos, rows.Err()
}
