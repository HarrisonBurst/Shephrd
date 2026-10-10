package coord

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"shephrd/internal/fault"
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
