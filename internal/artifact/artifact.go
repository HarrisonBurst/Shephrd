package artifact

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/execution"
	"shephrd/internal/fault"
	"shephrd/internal/store"
)

type Artifact struct {
	ID      int64  `json:"-"`
	Ref     string `json:"id"`
	Task    string `json:"task"`
	TaskID  int64  `json:"-"`
	Attempt int64  `json:"attempt"`
	Run     int64  `json:"run"`
	Kind    string `json:"kind"`
	Commit  string `json:"commit,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Base    string `json:"base,omitempty"`
	Files   []File `json:"files,omitempty"`
	Text    string `json:"text"`
	Sealed  string `json:"sealed"`
}

type File struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

func ParseRef(ref string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(ref, "a_"), 10, 64)
	if err != nil || !strings.HasPrefix(ref, "a_") {
		return 0, fault.New("usage", "%q is not an artifact ID such as a_1", ref)
	}
	return id, nil
}

func Load(q coord.Querier, id int64) (*Artifact, error) {
	var a Artifact
	var files string
	err := q.QueryRow(`SELECT id, task, attempt, run, kind, commit_sha, branch, base, files, text, sealed_at FROM artifacts WHERE id = ?`, id).
		Scan(&a.ID, &a.TaskID, &a.Attempt, &a.Run, &a.Kind, &a.Commit, &a.Branch, &a.Base, &files, &a.Text, &a.Sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fault.New("not_found", "no artifact a_%d", id)
	}
	if err != nil {
		return nil, err
	}
	a.Ref, a.Task = coord.ArtifactRef(a.ID), coord.TaskRef(a.TaskID)
	return &a, json.Unmarshal([]byte(files), &a.Files)
}

func BlobPath(cfg *config.Config, digest string) string {
	return filepath.Join(cfg.DataDir, "artifacts", "sha256", digest)
}

// Seal checks a result against the task's deliverable on the attempt's
// host and captures it, so a mistake comes back to the session as a
// refusal it can fix. Report files are stored on the home host.
func Seal(ctx context.Context, cfg *config.Config, host execution.Host, t *coord.Task, a *coord.Attempt, text string, files []string) (*Artifact, error) {
	sealed := &Artifact{Kind: t.Deliverable, Text: text}
	if t.Role == "driver" {
		if len(files) > 0 {
			return nil, fault.New("result_refused", "a sub-driver's workspace is read-only; report its result as text")
		}
		return sealed, nil
	}
	var out execution.SealResponse
	err := host.Call(ctx, "seal", execution.SealRequest{Deliverable: t.Deliverable, Workspace: a.Workspace, Branch: a.Branch, Base: a.Base, Files: files}, &out)
	if err != nil {
		return nil, err
	}
	if t.Deliverable == "code" {
		sealed.Commit, sealed.Branch, sealed.Base = out.Commit, a.Branch, a.Base
	}
	for _, f := range out.Files {
		digest, err := storeBlob(cfg, f.Content)
		if err != nil {
			return nil, err
		}
		sealed.Files = append(sealed.Files, File{Path: f.Path, Digest: digest, Size: int64(len(f.Content))})
	}
	return sealed, nil
}

// storeBlob writes content into the content-addressed store, read-only.
func storeBlob(cfg *config.Config, body []byte) (string, error) {
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	blob := BlobPath(cfg, digest)
	if _, err := os.Stat(blob); err == nil {
		return digest, nil
	}
	if err := os.MkdirAll(filepath.Dir(blob), 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(blob), ".blob-")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Chmod(0o444)
	tmp.Close()
	return digest, os.Rename(tmp.Name(), blob)
}

// Record stores a sealed artifact as its task's current result.
func Record(tx *store.Tx, c coord.Caller, t *coord.Task, a *Artifact) error {
	files, err := json.Marshal(a.Files)
	if err != nil {
		return err
	}
	result, err := tx.Exec(`INSERT INTO artifacts (task, attempt, run, kind, commit_sha, branch, base, files, text, sealed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, c.Attempt, c.Run, a.Kind, a.Commit, a.Branch, a.Base, string(files), a.Text, store.Timestamp(tx.Now))
	if err != nil {
		return err
	}
	if a.ID, err = result.LastInsertId(); err != nil {
		return err
	}
	a.Ref, a.Task, a.TaskID, a.Attempt, a.Run = coord.ArtifactRef(a.ID), t.Ref, t.ID, c.Attempt, c.Run
	t.ArtifactID, t.Artifact = a.ID, a.Ref
	if _, err := tx.Exec(`UPDATE tasks SET artifact = ? WHERE id = ?`, a.ID, t.ID); err != nil {
		return err
	}
	_, err = tx.Emit("artifact.sealed", c.String(), t.ID, c.Attempt, c.Run, map[string]any{
		"artifact": a.Ref, "deliverable": a.Kind, "commit": a.Commit, "files": a.Files,
	})
	return err
}

// Read returns an artifact's content: one report file, or the answer text.
func Read(cfg *config.Config, a *Artifact, path string) (string, error) {
	switch a.Kind {
	case "code":
		return "", fault.New("usage", "a code artifact is commit %s on %s; read it with git", a.Commit, a.Branch)
	case "answer":
		return a.Text, nil
	}
	if path == "" && len(a.Files) == 1 {
		path = a.Files[0].Path
	}
	for _, f := range a.Files {
		if f.Path == path {
			file, err := os.Open(BlobPath(cfg, f.Digest))
			if err != nil {
				return "", err
			}
			defer file.Close()
			body, err := io.ReadAll(file)
			return string(body), err
		}
	}
	if len(a.Files) == 0 {
		return a.Text, nil
	}
	return "", fault.New("usage", "name one of the artifact's files with --file: %v", a.Files)
}

// Inputs pins each dependency's current artifact into a starting attempt
// and lists them for its brief. Report files are copied read-only beside
// the workspace on its host; pinned inputs never change afterwards.
func Inputs(db *store.Store, cfg *config.Config, hosts func(string) (execution.Host, error)) func(context.Context, *coord.Task, *coord.Attempt) ([]string, error) {
	return func(ctx context.Context, t *coord.Task, a *coord.Attempt) ([]string, error) {
		var pinned int
		if err := db.QueryRow(`SELECT COUNT(*) FROM attempt_inputs WHERE task = ? AND attempt = ?`, t.ID, a.N).Scan(&pinned); err != nil {
			return nil, err
		}
		if pinned == 0 {
			host, err := hosts(a.Host)
			if err != nil {
				return nil, err
			}
			if err := pin(ctx, db, cfg, host, t, a); err != nil {
				return nil, err
			}
		}
		rows, err := db.Query(`SELECT dependency, artifact, path FROM attempt_inputs WHERE task = ? AND attempt = ? ORDER BY dependency`, t.ID, a.N)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var dep, id int64
			var path string
			if err := rows.Scan(&dep, &id, &path); err != nil {
				return nil, err
			}
			art, err := Load(db, id)
			if err != nil {
				return nil, err
			}
			switch art.Kind {
			case "report":
				lines = append(lines, fmt.Sprintf("%s report %s, files in `%s`", coord.TaskRef(dep), art.Ref, path))
			case "code":
				lines = append(lines, fmt.Sprintf("%s code %s at commit `%s`, already in your base", coord.TaskRef(dep), art.Ref, art.Commit))
			default:
				lines = append(lines, fmt.Sprintf("%s answer %s: %s", coord.TaskRef(dep), art.Ref, art.Text))
			}
		}
		return lines, rows.Err()
	}
}

func pin(ctx context.Context, db *store.Store, cfg *config.Config, host execution.Host, t *coord.Task, a *coord.Attempt) error {
	deps, err := coord.Query(db, `t.id IN (SELECT dependency FROM dependencies WHERE task = ?) AND t.artifact IS NOT NULL`, t.ID)
	if err != nil {
		return err
	}
	for _, dep := range deps {
		art, err := Load(db, dep.ArtifactID)
		if err != nil {
			return err
		}
		dir := ""
		if art.Kind == "report" {
			dir = filepath.Join(a.Workspace+".inputs", dep.Ref)
			files := map[string][]byte{}
			for _, f := range art.Files {
				if files[f.Path], err = os.ReadFile(BlobPath(cfg, f.Digest)); err != nil {
					return err
				}
			}
			if err := host.Call(ctx, "pin_inputs", execution.PinRequest{Dir: dir, Files: files}, nil); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`INSERT OR IGNORE INTO attempt_inputs (task, attempt, dependency, artifact, path) VALUES (?, ?, ?, ?, ?)`,
			t.ID, a.N, dep.ID, art.ID, dir); err != nil {
			return err
		}
	}
	return nil
}
